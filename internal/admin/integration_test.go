//go:build integration

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

const testAdvisoryLockID = 424242

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	lockConn, err := pgx.Connect(ctx, testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "admin integration: connect for advisory lock:", err)
		return 1
	}
	defer func() { _ = lockConn.Close(ctx) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock_shared($1)", testAdvisoryLockID); err != nil {
		fmt.Fprintln(os.Stderr, "admin integration: advisory lock:", err)
		return 1
	}
	defer func() { _, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock_shared($1)", testAdvisoryLockID) }()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "admin integration: migrate up:", err)
		return 1
	}
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "admin-itest", MaxConns: 16})
	if err != nil {
		fmt.Fprintln(os.Stderr, "admin integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}

// fixture holds one service with a fake clock and a set of operator users.
type fixture struct {
	t   *testing.T
	clk *clock.Fake
	svc *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	clk := clock.NewFake(t0)
	return &fixture{t: t, clk: clk, svc: NewService(clk, audit.NewWriter())}
}

// newUser inserts a users row and returns its id (a v4 uuid for one in two
// users, to prove subjects need not be v7).
func (f *fixture) newUser() string {
	f.t.Helper()
	var uid string
	if time.Now().UnixNano()%2 == 0 {
		uid = uuid.NewString()
	} else {
		uid = id.New[id.Any]().String()
	}
	_, err := testDB.Exec(context.Background(),
		`INSERT INTO users (id, idp_issuer, idp_subject, status) VALUES ($1, 'itest', $2, 'ACTIVE')`, uid, "sub-"+uid)
	require.NoError(f.t, err)
	return uid
}

// operator returns a context whose principal is an OPERATOR with the given
// roles, strongly authenticated one minute ago on the fake clock.
func (f *fixture) operator(userID string, roles ...security.Role) context.Context {
	p := security.Principal{
		SubjectID: userID, ActorType: security.ActorOperator, Roles: roles,
		SessionID: "sess-" + userID, AuthTime: f.clk.Now().Add(-time.Minute), AMR: []string{"mfa"},
	}
	return security.WithPrincipal(context.Background(), p)
}

// breakGlass returns a context whose principal holds a live BREAK_GLASS
// elevation (plus ADMIN), the only way to hold dual-control approve rights.
func (f *fixture) breakGlass(userID string) context.Context {
	until := f.clk.Now().Add(time.Hour)
	p := security.Principal{
		SubjectID: userID, ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin, security.RoleBreakGlass},
		SessionID: "sess-" + userID, AuthTime: f.clk.Now().Add(-time.Minute), AMR: []string{"mfa"}, BreakGlassUntil: &until,
	}
	return security.WithPrincipal(context.Background(), p)
}

func inTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(ctx, db.TxOptions{}, fn)
}

func (f *fixture) propose(ctx context.Context, p Proposal) (Action, error) {
	var a Action
	err := inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = f.svc.Propose(ctx, tx, p)
		return err
	})
	return a, err
}

func (f *fixture) approve(ctx context.Context, id, note string) (Action, error) {
	var a Action
	err := inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = f.svc.Approve(ctx, tx, id, note)
		return err
	})
	return a, err
}

func (f *fixture) execute(ctx context.Context, id string, exec ExecFunc) (Action, error) {
	var a Action
	err := inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = f.svc.Execute(ctx, tx, id, exec)
		return err
	})
	return a, err
}

func (f *fixture) get(ctx context.Context, id string) Action {
	f.t.Helper()
	a, err := f.svc.Get(ctx, testDB, id)
	require.NoError(f.t, err)
	return a
}

func okExec(result string) ExecFunc {
	return func(ctx context.Context, tx pgx.Tx, params json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(result), nil
	}
}

func ledgerProposal(target string) Proposal {
	return Proposal{
		Kind: KindLedgerCorrection, TargetType: "account", TargetID: target,
		Params: json.RawMessage(`{ "amount" : "12.50", "account_id" : "` + target + `", "legs" : [ {"z": "1", "a": "2"} ] }`),
		Reason: "compensating entry for INC-42", CorrelationID: "corr-" + target,
	}
}

type auditRow struct {
	Seq          int64
	Action       string
	ActorID      string
	ResourceID   string
	Reason       *string
	Payload      []byte
	Correlation  *string
	BeforeHash   []byte
	AfterHash    []byte
	BuildVersion *string
}

func adminAuditFor(t *testing.T, actionID string) []auditRow {
	t.Helper()
	rows, err := testDB.Query(context.Background(), `
		SELECT stream_seq, action, actor_id, resource_id, reason, payload, correlation_id, before_hash, after_hash, build_version
		FROM audit_events WHERE stream = 'admin' AND (resource_id = $1 OR evidence_ref = $1) ORDER BY stream_seq`, actionID)
	require.NoError(t, err)
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		require.NoError(t, rows.Scan(&r.Seq, &r.Action, &r.ActorID, &r.ResourceID, &r.Reason, &r.Payload, &r.Correlation, &r.BeforeHash, &r.AfterHash, &r.BuildVersion))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func transitionsFor(t *testing.T, actionID string) [][2]string {
	t.Helper()
	rows, err := testDB.Query(context.Background(),
		`SELECT from_status, to_status FROM admin_action_transitions WHERE action_id = $1 ORDER BY occurred_at, id`, actionID)
	require.NoError(t, err)
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var from, to string
		require.NoError(t, rows.Scan(&from, &to))
		out = append(out, [2]string{from, to})
	}
	require.NoError(t, rows.Err())
	return out
}

func requireCode(t *testing.T, err error, code errs.Code) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, code, errs.CodeOf(err), "%v", err)
}

func TestIntegration_ProposeApproveExecute(t *testing.T) {
	f := newFixture(t)
	proposer, approver := f.newUser(), f.newUser()
	pctx := f.operator(proposer, security.RoleFinance)
	target := "acct-" + uuid.NewString()

	a, err := f.propose(pctx, ledgerProposal(target))
	require.NoError(t, err)
	assert.Equal(t, StatusProposed, a.Status)
	assert.True(t, a.RequiresDual)
	assert.Equal(t, proposer, a.ProposedBy)
	assert.Equal(t, t0, a.ProposedAt)
	assert.Equal(t, t0.Add(-time.Minute), a.ProposerStepUpAt, "proposer_step_up_at is Principal.AuthTime")
	assert.Equal(t, t0.Add(4*time.Hour), a.ExpiresAt)
	assert.JSONEq(t, `{"account_id":"`+target+`","amount":"12.50","legs":[{"a":"2","z":"1"}]}`, string(a.Params))
	wantHash, err := ParamsHash(ledgerProposal(target).Params)
	require.NoError(t, err)
	assert.Equal(t, wantHash, a.ParamsHash)
	assert.Equal(t, "corr-"+target, *a.CorrelationID)
	stored := f.get(pctx, a.ID.String())
	assert.Equal(t, a.ParamsHash, stored.ParamsHash)
	assert.Equal(t, a.ProposedAt, stored.ProposedAt)
	assert.Equal(t, a.ExpiresAt, stored.ExpiresAt)
	assert.JSONEq(t, string(a.Params), string(stored.Params))

	// Execute before approval is refused for a dual-control kind.
	_, err = f.execute(pctx, a.ID.String(), okExec(`{}`))
	requireCode(t, err, errs.CodeInvalidStateTransition)

	f.clk.Advance(2 * time.Minute)
	actx := f.breakGlass(approver)
	approved, err := f.approve(actx, a.ID.String(), "reviewed evidence")
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, approved.Status)
	require.NotNil(t, approved.ApprovedBy)
	assert.Equal(t, approver, *approved.ApprovedBy)
	assert.Equal(t, f.clk.Now(), *approved.ApprovedAt)
	assert.Equal(t, f.clk.Now().Add(-time.Minute), *approved.ApproverStepUpAt)
	assert.Equal(t, "reviewed evidence", *approved.ApprovalNote)

	// Approving twice is a state error.
	_, err = f.approve(actx, a.ID.String(), "again")
	requireCode(t, err, errs.CodeInvalidStateTransition)

	// Pending lists show it to any operator with an admin permission.
	pending, err := f.svc.ListPending(pctx, testDB, 0)
	require.NoError(t, err)
	found := false
	for _, p := range pending {
		if p.ID == a.ID {
			found = true
			assert.Equal(t, StatusApproved, p.Status)
		}
	}
	assert.True(t, found)

	f.clk.Advance(time.Minute)
	pctx = f.operator(proposer, security.RoleFinance)
	var sawParams json.RawMessage
	executed, err := f.execute(pctx, a.ID.String(), func(ctx context.Context, tx pgx.Tx, params json.RawMessage) (json.RawMessage, error) {
		sawParams = params
		// The consumer verifies the approval inside the callback, where the
		// action is still APPROVED, using the callback's transaction.
		ap, err := f.svc.VerifyApproved(ctx, tx, a.ID.String(), KindLedgerCorrection, target)
		if err != nil {
			return nil, err
		}
		if ap.Status != StatusApproved || *ap.ApprovedBy != approver {
			return nil, fmt.Errorf("unexpected approval %+v", ap)
		}
		return json.RawMessage(`{"journal_transaction_id":"jt-1","posted":true}`), nil
	})
	require.NoError(t, err)
	assert.Equal(t, StatusExecuted, executed.Status)
	assert.JSONEq(t, string(a.Params), string(sawParams))
	assert.Equal(t, f.clk.Now(), *executed.ExecutedAt)
	assert.JSONEq(t, `{"journal_transaction_id":"jt-1","posted":true}`, string(executed.ExecutionResult))
	stored = f.get(pctx, a.ID.String())
	assert.Equal(t, StatusExecuted, stored.Status)
	assert.JSONEq(t, `{"journal_transaction_id":"jt-1","posted":true}`, string(stored.ExecutionResult))
	assert.Nil(t, stored.ExecutionError)

	// Terminal: nothing else applies.
	_, err = f.execute(pctx, a.ID.String(), okExec(`{}`))
	requireCode(t, err, errs.CodeInvalidStateTransition)
	_, err = f.approve(actx, a.ID.String(), "late")
	requireCode(t, err, errs.CodeInvalidStateTransition)

	// Every transition is recorded and audited on the admin stream, in order.
	assert.Equal(t, [][2]string{{"NONE", "PROPOSED"}, {"PROPOSED", "APPROVED"}, {"APPROVED", "EXECUTED"}}, transitionsFor(t, a.ID.String()))
	events := adminAuditFor(t, a.ID.String())
	require.Len(t, events, 3)
	assert.Equal(t, []string{AuditProposed, AuditApproved, AuditExecuted}, []string{events[0].Action, events[1].Action, events[2].Action})
	assert.Equal(t, proposer, events[0].ActorID)
	assert.Equal(t, approver, events[1].ActorID)
	assert.Equal(t, proposer, events[2].ActorID)
	assert.Nil(t, events[0].BeforeHash)
	assert.Equal(t, events[0].AfterHash, events[1].BeforeHash, "after-hash of one transition is the before-hash of the next")
	assert.Equal(t, events[1].AfterHash, events[2].BeforeHash)
	assert.Equal(t, "corr-"+target, *events[0].Correlation)
	assert.Equal(t, "compensating entry for INC-42", *events[0].Reason)
	assert.Equal(t, "reviewed evidence", *events[1].Reason)
	assert.Equal(t, "dev", *events[0].BuildVersion)
	var payload auditPayload
	require.NoError(t, json.Unmarshal(events[1].Payload, &payload))
	assert.Equal(t, KindLedgerCorrection, payload.Kind)
	assert.Equal(t, StatusProposed, payload.FromStatus)
	assert.Equal(t, StatusApproved, payload.ToStatus)
	assert.Equal(t, approver, *payload.ApprovedBy)
	assert.True(t, payload.RequiresDual)
	for i := 1; i < len(events); i++ {
		assert.Greater(t, events[i].Seq, events[i-1].Seq)
	}
	rep, err := audit.NewVerifier().VerifyStream(context.Background(), testDB, audit.AdminStream)
	require.NoError(t, err)
	assert.True(t, rep.OK, rep.Reason)
}

func TestIntegration_SameProposerCannotApprove(t *testing.T) {
	f := newFixture(t)
	user := f.newUser()
	// Even a principal holding both permissions (ADMIN + live break-glass)
	// cannot approve their own proposal.
	ctx := f.breakGlass(user)
	a, err := f.propose(ctx, ledgerProposal("acct-"+uuid.NewString()))
	require.NoError(t, err)

	_, err = f.approve(ctx, a.ID.String(), "self")
	requireCode(t, err, errs.CodeForbidden)
	assert.True(t, errors.Is(err, security.ErrSelfApproval), "%v", err)
	assert.Equal(t, StatusProposed, f.get(ctx, a.ID.String()).Status)
	assert.Len(t, transitionsFor(t, a.ID.String()), 1)

	// The database refuses it too, independently of this package.
	_, err = testDB.Exec(context.Background(),
		`UPDATE admin_actions SET approved_by_user_id = proposed_by_user_id, status = 'APPROVED' WHERE id = $1`, a.ID)
	require.Error(t, err)
	assert.True(t, db.IsCheckViolation(err), "SQLSTATE %q", db.SQLState(err))
}

func TestIntegration_ExpiredActionsAreDead(t *testing.T) {
	f := newFixture(t)
	proposer, approver := f.newUser(), f.newUser()
	pctx := f.operator(proposer, security.RoleOperations)
	a, err := f.propose(pctx, Proposal{
		Kind: KindKillSwitchRelease, TargetType: "kill_switch", TargetID: "GLOBAL_NEW_RISK_KILL:*",
		Params: json.RawMessage(`{"kind":"GLOBAL_NEW_RISK_KILL","scope":"*"}`), Reason: "incident resolved, release global kill",
	})
	require.NoError(t, err)
	assert.Equal(t, t0.Add(time.Hour), a.ExpiresAt)

	// One microsecond before expiry the approval is still possible; at
	// expiry it is not.
	f.clk.Set(a.ExpiresAt)
	actx := f.breakGlass(approver)
	_, err = f.approve(actx, a.ID.String(), "too late")
	requireCode(t, err, errs.CodeInvalidStateTransition)
	assert.Contains(t, err.Error(), "expired")
	pctx = f.operator(proposer, security.RoleOperations)
	_, err = f.execute(pctx, a.ID.String(), okExec(`{}`))
	requireCode(t, err, errs.CodeInvalidStateTransition)

	// An approved action that expires before execution is equally dead.
	f.clk.Set(t0)
	b, err := f.propose(f.operator(proposer, security.RoleOperations), Proposal{
		Kind: KindKillSwitchRelease, TargetType: "kill_switch", TargetID: "FUNDING_DISABLE:*",
		Params: json.RawMessage(`{}`), Reason: "release funding disable",
	})
	require.NoError(t, err)
	_, err = f.approve(f.breakGlass(approver), b.ID.String(), "ok")
	require.NoError(t, err)
	f.clk.Set(b.ExpiresAt.Add(time.Second))
	_, err = f.execute(f.operator(proposer, security.RoleOperations), b.ID.String(), okExec(`{}`))
	requireCode(t, err, errs.CodeInvalidStateTransition)
	_, err = f.svc.VerifyApproved(context.Background(), testDB, b.ID.String(), KindKillSwitchRelease, "FUNDING_DISABLE:*")
	assert.True(t, errors.Is(err, ErrApprovalExpired), "%v", err)

	// The worker sweeps them into EXPIRED with a transition and an audit event.
	var n int
	require.NoError(t, inTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = f.svc.ExpireDue(ctx, tx, f.clk.Now())
		return err
	}))
	assert.GreaterOrEqual(t, n, 2)
	for _, x := range []Action{a, b} {
		got := f.get(f.operator(proposer, security.RoleOperations), x.ID.String())
		assert.Equal(t, StatusExpired, got.Status)
		tr := transitionsFor(t, x.ID.String())
		assert.Equal(t, "EXPIRED", tr[len(tr)-1][1])
		ev := adminAuditFor(t, x.ID.String())
		assert.Equal(t, AuditExpired, ev[len(ev)-1].Action)
		assert.Equal(t, "system", ev[len(ev)-1].ActorID)
	}
	// Idempotent: nothing left to expire for these.
	require.NoError(t, inTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = f.svc.ExpireDue(ctx, tx, f.clk.Now())
		return err
	}))
	assert.Equal(t, 0, n)
}

func TestIntegration_NonDualKindExecutesWithoutApproval(t *testing.T) {
	f := newFixture(t)
	compliance := f.newUser()
	ctx := f.operator(compliance, security.RoleCompliance)
	a, err := f.propose(ctx, Proposal{
		Kind: KindAccountUnfreeze, TargetType: "account", TargetID: "acct-" + uuid.NewString(),
		Params: json.RawMessage(`{"account_id":"x"}`), Reason: "sanctions review cleared",
	})
	require.NoError(t, err)
	assert.False(t, a.RequiresDual)

	// There is no approve permission: approval is not a transition of this kind.
	_, err = f.approve(f.breakGlass(f.newUser()), a.ID.String(), "n/a")
	requireCode(t, err, errs.CodeInvalidStateTransition)

	executed, err := f.execute(ctx, a.ID.String(), okExec(`{"unfrozen":true}`))
	require.NoError(t, err)
	assert.Equal(t, StatusExecuted, executed.Status)
	assert.Nil(t, executed.ApprovedBy)
	assert.Equal(t, [][2]string{{"NONE", "PROPOSED"}, {"PROPOSED", "EXECUTED"}}, transitionsFor(t, a.ID.String()))

	ap, err := f.svc.VerifyApproved(context.Background(), testDB, a.ID.String(), KindAccountUnfreeze, a.TargetID)
	require.NoError(t, err)
	assert.Equal(t, StatusExecuted, ap.Status)
	assert.False(t, ap.RequiresDual)
}

func TestIntegration_ExecutionFailureIsRecordedAndCallbackRolledBack(t *testing.T) {
	f := newFixture(t)
	proposer, approver := f.newUser(), f.newUser()
	pctx := f.operator(proposer, security.RoleFinance)
	a, err := f.propose(pctx, ledgerProposal("acct-"+uuid.NewString()))
	require.NoError(t, err)
	_, err = f.approve(f.breakGlass(approver), a.ID.String(), "ok")
	require.NoError(t, err)

	side := audit.AgentStream(uuid.NewString())
	business := errs.New(errs.CodeInsufficientBuyingPower, "cannot post: would overdraw")
	var got Action
	// The caller commits despite the failure so the FAILED record persists.
	err = inTx(pctx, func(ctx context.Context, tx pgx.Tx) error {
		var execErr error
		got, execErr = f.svc.Execute(ctx, tx, a.ID.String(), func(ctx context.Context, tx pgx.Tx, params json.RawMessage) (json.RawMessage, error) {
			// A write inside the callback...
			_, err := audit.NewWriter().Append(ctx, tx, audit.Event{
				Stream: side, ActorType: "SYSTEM", ActorID: "exec", Action: "x", ResourceType: "y", ResourceID: "z", OccurredAt: f.clk.Now(),
			})
			if err != nil {
				return nil, err
			}
			// ...followed by a business failure.
			return nil, business
		})
		require.Error(t, execErr)
		assert.Equal(t, errs.CodeInsufficientBuyingPower, errs.CodeOf(execErr), "the callback's code is preserved")
		assert.True(t, errors.Is(execErr, business))
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, got.Status)
	require.NotNil(t, got.ExecutionError)
	assert.Contains(t, *got.ExecutionError, "would overdraw")

	stored := f.get(pctx, a.ID.String())
	assert.Equal(t, StatusFailed, stored.Status)
	assert.Contains(t, *stored.ExecutionError, "would overdraw")
	rep, err := audit.NewVerifier().VerifyStream(context.Background(), testDB, side)
	require.NoError(t, err)
	assert.Equal(t, 0, rep.Events, "the callback's write was rolled back with the savepoint")
	assert.Equal(t, [][2]string{{"NONE", "PROPOSED"}, {"PROPOSED", "APPROVED"}, {"APPROVED", "FAILED"}}, transitionsFor(t, a.ID.String()))
	ev := adminAuditFor(t, a.ID.String())
	assert.Equal(t, AuditFailed, ev[len(ev)-1].Action)

	// FAILED is terminal.
	_, err = f.execute(pctx, a.ID.String(), okExec(`{}`))
	requireCode(t, err, errs.CodeInvalidStateTransition)
	_, err = f.svc.VerifyApproved(context.Background(), testDB, a.ID.String(), KindLedgerCorrection, a.TargetID)
	assert.True(t, errors.Is(err, ErrApprovalNotActive))

	// When the caller rolls back instead, nothing about the attempt survives
	// and the action stays APPROVED.
	b, err := f.propose(pctx, ledgerProposal("acct-"+uuid.NewString()))
	require.NoError(t, err)
	_, err = f.approve(f.breakGlass(approver), b.ID.String(), "ok")
	require.NoError(t, err)
	_, err = f.execute(pctx, b.ID.String(), func(ctx context.Context, tx pgx.Tx, params json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("plain failure")
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
	assert.Equal(t, StatusApproved, f.get(pctx, b.ID.String()).Status)
	assert.Len(t, transitionsFor(t, b.ID.String()), 2)

	// An invalid result is a failure as well.
	_, err = f.execute(pctx, b.ID.String(), okExec(`{"n":1.5}`))
	require.Error(t, err)
	assert.Equal(t, StatusApproved, f.get(pctx, b.ID.String()).Status, "rolled back by the InTx error path")
}

func TestIntegration_TamperedParamsAreRefused(t *testing.T) {
	f := newFixture(t)
	proposer, approver := f.newUser(), f.newUser()
	pctx := f.operator(proposer, security.RoleFinance)
	a, err := f.propose(pctx, ledgerProposal("acct-"+uuid.NewString()))
	require.NoError(t, err)
	_, err = f.approve(f.breakGlass(approver), a.ID.String(), "ok")
	require.NoError(t, err)

	// cp_app may UPDATE admin_actions (status changes), so a bug or a stolen
	// app credential could rewrite params; the hash check catches it.
	_, err = testDB.Exec(context.Background(),
		`UPDATE admin_actions SET params = jsonb_set(params, '{amount}', '"999999.00"') WHERE id = $1`, a.ID)
	require.NoError(t, err)

	_, err = f.execute(pctx, a.ID.String(), okExec(`{}`))
	requireCode(t, err, errs.CodeConflict)
	assert.True(t, errors.Is(err, ErrParamsTampered))
	assert.Equal(t, StatusApproved, f.get(pctx, a.ID.String()).Status, "no transition on integrity failure")
	assert.Len(t, transitionsFor(t, a.ID.String()), 2)

	// Restoring the exact params (any key order) makes it executable again.
	_, err = testDB.Exec(context.Background(),
		`UPDATE admin_actions SET params = jsonb_set(params, '{amount}', '"12.50"') WHERE id = $1`, a.ID)
	require.NoError(t, err)
	executed, err := f.execute(pctx, a.ID.String(), okExec(`{}`))
	require.NoError(t, err)
	assert.Equal(t, StatusExecuted, executed.Status)
}

func TestIntegration_VerifyApprovedMatrix(t *testing.T) {
	f := newFixture(t)
	proposer, approver := f.newUser(), f.newUser()
	pctx := f.operator(proposer, security.RoleRisk)
	target := "LIVE_AGENT_TRADING:PROD"
	gate := func() Proposal {
		return Proposal{
			Kind: KindCapabilityGateApprove, TargetType: "capability_gate", TargetID: target,
			Params: json.RawMessage(`{"capability":"LIVE_AGENT_TRADING","environment":"PROD"}`), Reason: "evidence pack v3 attached",
		}
	}
	bg := context.Background()

	proposed, err := f.propose(pctx, gate())
	require.NoError(t, err)
	_, err = f.svc.VerifyApproved(bg, testDB, proposed.ID.String(), KindCapabilityGateApprove, target)
	assert.True(t, errors.Is(err, ErrApprovalNotActive), "%v", err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	approved, err := f.approve(f.breakGlass(approver), proposed.ID.String(), "ok")
	require.NoError(t, err)
	ap, err := f.svc.VerifyApproved(bg, testDB, approved.ID.String(), KindCapabilityGateApprove, target)
	require.NoError(t, err)
	assert.Equal(t, approved.ID, ap.ActionID)
	assert.Equal(t, StatusApproved, ap.Status)
	assert.Equal(t, proposer, ap.ProposedBy)
	assert.Equal(t, approver, *ap.ApprovedBy)
	assert.Equal(t, approved.ParamsHash, ap.ParamsHash)
	assert.JSONEq(t, `{"capability":"LIVE_AGENT_TRADING","environment":"PROD"}`, string(ap.Params))
	assert.True(t, ap.RequiresDual)

	_, err = f.svc.VerifyApproved(bg, testDB, approved.ID.String(), KindKillSwitchRelease, target)
	assert.True(t, errors.Is(err, ErrApprovalMismatch), "wrong kind")
	_, err = f.svc.VerifyApproved(bg, testDB, approved.ID.String(), KindCapabilityGateApprove, "LIVE_FUNDING:PROD")
	assert.True(t, errors.Is(err, ErrApprovalMismatch), "wrong target")
	_, err = f.svc.VerifyApproved(bg, testDB, approved.ID.String(), KindCapabilityGateApprove, "")
	assert.True(t, errors.Is(err, ErrApprovalMismatch), "empty target")
	_, err = f.svc.VerifyApproved(bg, testDB, NewActionID().String(), KindCapabilityGateApprove, target)
	assert.True(t, errors.Is(err, ErrApprovalNotFound), "unknown id")
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	_, err = f.svc.VerifyApproved(bg, testDB, "not-an-id", KindCapabilityGateApprove, target)
	assert.True(t, errors.Is(err, ErrApprovalNotFound), "malformed id")
	_, err = f.svc.VerifyApproved(security.WithPrincipal(bg, security.AgentPrincipal("agent-1", "acct-1")), testDB, approved.ID.String(), KindCapabilityGateApprove, target)
	requireCode(t, err, errs.CodeForbidden)

	// Executed approvals verify (until expiry).
	executed, err := f.execute(f.operator(proposer, security.RoleRisk), approved.ID.String(), okExec(`{"gate_version":2}`))
	require.NoError(t, err)
	ap, err = f.svc.VerifyApproved(bg, testDB, executed.ID.String(), KindCapabilityGateApprove, target)
	require.NoError(t, err)
	assert.Equal(t, StatusExecuted, ap.Status)
	assert.NotNil(t, ap.ExecutedAt)
	f.clk.Set(executed.ExpiresAt)
	_, err = f.svc.VerifyApproved(bg, testDB, executed.ID.String(), KindCapabilityGateApprove, target)
	assert.True(t, errors.Is(err, ErrApprovalExpired))
	f.clk.Set(t0)

	// Rejected and cancelled never verify.
	rejected, err := f.propose(f.operator(proposer, security.RoleRisk), gate())
	require.NoError(t, err)
	var r Action
	require.NoError(t, inTx(f.breakGlass(approver), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.Reject(ctx, tx, rejected.ID.String(), "insufficient evidence")
		return err
	}))
	assert.Equal(t, StatusRejected, r.Status)
	assert.Equal(t, approver, *r.RejectedBy)
	assert.Equal(t, "insufficient evidence", *r.RejectedReason)
	_, err = f.svc.VerifyApproved(bg, testDB, rejected.ID.String(), KindCapabilityGateApprove, target)
	assert.True(t, errors.Is(err, ErrApprovalNotActive))

	cancelled, err := f.propose(f.operator(proposer, security.RoleRisk), gate())
	require.NoError(t, err)
	err = inTx(f.breakGlass(approver), func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Cancel(ctx, tx, cancelled.ID.String(), "not mine")
		return err
	})
	requireCode(t, err, errs.CodeForbidden)
	require.NoError(t, inTx(f.operator(proposer, security.RoleRisk), func(ctx context.Context, tx pgx.Tx) error {
		c, err := f.svc.Cancel(ctx, tx, cancelled.ID.String(), "filed against the wrong gate")
		if err == nil {
			assert.Equal(t, StatusCancelled, c.Status)
		}
		return err
	}))
	_, err = f.svc.VerifyApproved(bg, testDB, cancelled.ID.String(), KindCapabilityGateApprove, target)
	assert.True(t, errors.Is(err, ErrApprovalNotActive))
	assert.Equal(t, AuditCancelled, adminAuditFor(t, cancelled.ID.String())[1].Action)
	assert.Equal(t, AuditRejected, adminAuditFor(t, rejected.ID.String())[1].Action)
}

func TestIntegration_AuthorizationMatrix(t *testing.T) {
	f := newFixture(t)
	user := f.newUser()

	t.Run("agents are refused before any query", func(t *testing.T) {
		actx := security.WithPrincipal(context.Background(), security.AgentPrincipal("agent-1", "acct-1"))
		// A nil transaction proves no statement runs: any query would panic.
		_, err := f.svc.Propose(actx, nil, ledgerProposal("x"))
		requireCode(t, err, errs.CodeForbidden)
		_, err = f.svc.Approve(actx, nil, NewActionID().String(), "")
		requireCode(t, err, errs.CodeForbidden)
		_, err = f.svc.Reject(actx, nil, NewActionID().String(), "")
		requireCode(t, err, errs.CodeForbidden)
		_, err = f.svc.Cancel(actx, nil, NewActionID().String(), "")
		requireCode(t, err, errs.CodeForbidden)
		_, err = f.svc.Execute(actx, nil, NewActionID().String(), okExec(`{}`))
		requireCode(t, err, errs.CodeForbidden)
		_, err = f.svc.ExpireDue(actx, nil, f.clk.Now())
		requireCode(t, err, errs.CodeForbidden)
		_, err = f.svc.Get(actx, nil, NewActionID().String())
		requireCode(t, err, errs.CodeForbidden)
		_, err = f.svc.ListPending(actx, nil, 10)
		requireCode(t, err, errs.CodeForbidden)
		_, err = f.svc.VerifyApproved(actx, nil, NewActionID().String(), KindLedgerCorrection, "x")
		requireCode(t, err, errs.CodeForbidden)
		_, err = NewBreakGlass(f.svc).Grant(actx, nil, NewActionID().String())
		requireCode(t, err, errs.CodeForbidden)
	})

	t.Run("anonymous", func(t *testing.T) {
		_, err := f.svc.Propose(context.Background(), nil, ledgerProposal("x"))
		requireCode(t, err, errs.CodeUnauthenticated)
		_, err = f.svc.Get(context.Background(), testDB, NewActionID().String())
		requireCode(t, err, errs.CodeUnauthenticated)
	})

	t.Run("missing permission", func(t *testing.T) {
		_, err := f.propose(f.operator(user, security.RoleSupportReadOnly), ledgerProposal("x"))
		requireCode(t, err, errs.CodeForbidden)
		_, err = f.svc.ListPending(f.operator(user, security.RoleSupportReadOnly), testDB, 10)
		requireCode(t, err, errs.CodeForbidden)
		customer := security.WithPrincipal(context.Background(), security.Principal{
			SubjectID: user, ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer}, AuthTime: f.clk.Now(), AMR: []string{"mfa"},
		})
		_, err = f.propose(customer, ledgerProposal("x"))
		requireCode(t, err, errs.CodeForbidden)
	})

	t.Run("step-up", func(t *testing.T) {
		stale := security.WithPrincipal(context.Background(), security.Principal{
			SubjectID: user, ActorType: security.ActorOperator, Roles: []security.Role{security.RoleFinance},
			AuthTime: f.clk.Now().Add(-time.Hour), AMR: []string{"mfa"},
		})
		_, err := f.propose(stale, ledgerProposal("x"))
		requireCode(t, err, errs.CodeStepUpRequired)
		weak := security.WithPrincipal(context.Background(), security.Principal{
			SubjectID: user, ActorType: security.ActorOperator, Roles: []security.Role{security.RoleFinance},
			AuthTime: f.clk.Now(), AMR: []string{"pwd"},
		})
		_, err = f.propose(weak, ledgerProposal("x"))
		requireCode(t, err, errs.CodeStepUpRequired)

		// LEDGER_CORRECTION needs a 5-minute step-up: 6 minutes is stale.
		sixMin := security.WithPrincipal(context.Background(), security.Principal{
			SubjectID: user, ActorType: security.ActorOperator, Roles: []security.Role{security.RoleFinance},
			AuthTime: f.clk.Now().Add(-6 * time.Minute), AMR: []string{"mfa"},
		})
		_, err = f.propose(sixMin, ledgerProposal("x"))
		requireCode(t, err, errs.CodeStepUpRequired)
	})

	t.Run("subject must be a user id", func(t *testing.T) {
		alice := security.WithPrincipal(context.Background(), security.Principal{
			SubjectID: "alice", ActorType: security.ActorOperator, Roles: []security.Role{security.RoleFinance},
			AuthTime: f.clk.Now(), AMR: []string{"mfa"},
		})
		_, err := f.propose(alice, ledgerProposal("x"))
		requireCode(t, err, errs.CodeForbidden)
	})

	t.Run("validation", func(t *testing.T) {
		ctx := f.operator(user, security.RoleAdmin)
		cases := map[string]Proposal{
			"unknown kind":     {Kind: "PATCH_BALANCE", TargetType: "account", TargetID: "x", Reason: "long enough reason"},
			"short reason":     {Kind: KindLedgerCorrection, TargetType: "account", TargetID: "x", Reason: "short"},
			"missing target":   {Kind: KindLedgerCorrection, TargetType: "account", Reason: "long enough reason"},
			"array params":     {Kind: KindLedgerCorrection, TargetType: "account", TargetID: "x", Reason: "long enough reason", Params: json.RawMessage(`[1]`)},
			"float params":     {Kind: KindLedgerCorrection, TargetType: "account", TargetID: "x", Reason: "long enough reason", Params: json.RawMessage(`{"amount":1.5}`)},
			"malformed params": {Kind: KindLedgerCorrection, TargetType: "account", TargetID: "x", Reason: "long enough reason", Params: json.RawMessage(`{`)},
			"nul in reason":    {Kind: KindLedgerCorrection, TargetType: "account", TargetID: "x", Reason: "long enough \x00 reason"},
		}
		for name, p := range cases {
			_, err := f.propose(ctx, p)
			requireCode(t, err, errs.CodeValidationFailed)
			_ = name
		}
		_, err := f.approve(ctx, "nope", "")
		requireCode(t, err, errs.CodeValidationFailed)
		_, err = f.approve(f.breakGlass(user), NewActionID().String(), "")
		requireCode(t, err, errs.CodeNotFound)
		_, err = f.svc.Get(ctx, testDB, NewActionID().String())
		requireCode(t, err, errs.CodeNotFound)
	})

	t.Run("approver needs the approve permission and step-up", func(t *testing.T) {
		proposer := f.newUser()
		a, err := f.propose(f.operator(proposer, security.RoleFinance), ledgerProposal("acct-"+uuid.NewString()))
		require.NoError(t, err)
		// ADMIN without a live break-glass elevation does not hold ledger:approve_correction.
		_, err = f.approve(f.operator(user, security.RoleAdmin), a.ID.String(), "x")
		requireCode(t, err, errs.CodeForbidden)
		// An expired break-glass elevation is no elevation.
		past := f.clk.Now().Add(-time.Second)
		expired := security.WithPrincipal(context.Background(), security.Principal{
			SubjectID: user, ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin, security.RoleBreakGlass},
			AuthTime: f.clk.Now(), AMR: []string{"mfa"}, BreakGlassUntil: &past,
		})
		_, err = f.approve(expired, a.ID.String(), "x")
		requireCode(t, err, errs.CodeForbidden)
		// Live elevation but stale authentication.
		until := f.clk.Now().Add(time.Hour)
		stale := security.WithPrincipal(context.Background(), security.Principal{
			SubjectID: user, ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin, security.RoleBreakGlass},
			AuthTime: f.clk.Now().Add(-time.Hour), AMR: []string{"mfa"}, BreakGlassUntil: &until,
		})
		_, err = f.approve(stale, a.ID.String(), "x")
		requireCode(t, err, errs.CodeStepUpRequired)
		assert.Equal(t, StatusProposed, f.get(f.operator(proposer, security.RoleFinance), a.ID.String()).Status)
	})
}

func TestIntegration_ConcurrentApprovalHappensOnce(t *testing.T) {
	f := newFixture(t)
	proposer := f.newUser()
	a, err := f.propose(f.operator(proposer, security.RoleFinance), ledgerProposal("acct-"+uuid.NewString()))
	require.NoError(t, err)

	const n = 8
	approvers := make([]string, n)
	for i := range approvers {
		approvers[i] = f.newUser()
	}
	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			_, results[i] = f.approve(f.breakGlass(approvers[i]), a.ID.String(), fmt.Sprintf("approver %d", i))
		}(i)
	}
	start.Done()
	wg.Wait()
	ok := 0
	for i, err := range results {
		if err == nil {
			ok++
			continue
		}
		requireCode(t, err, errs.CodeInvalidStateTransition)
		_ = i
	}
	assert.Equal(t, 1, ok, "exactly one approval wins the row lock")
	assert.Len(t, transitionsFor(t, a.ID.String()), 2)
	assert.Len(t, adminAuditFor(t, a.ID.String()), 2)
}

func TestIntegration_BreakGlassGrant(t *testing.T) {
	f := newFixture(t)
	adminA, adminB, grantee := f.newUser(), f.newUser(), f.newUser()
	actx := f.operator(adminA, security.RoleAdmin)
	bctx := f.operator(adminB, security.RoleAdmin)

	a, err := f.propose(actx, Proposal{
		Kind: KindBreakGlassGrant, TargetType: "user", TargetID: grantee,
		Params: json.RawMessage(`{"user_id":"` + grantee + `","scope":"INC-77 release WITHDRAWALS_DISABLE","duration_seconds":1800}`),
		Reason: "INC-77: withdrawals must be re-enabled tonight",
	})
	require.NoError(t, err)
	assert.Equal(t, t0.Add(30*time.Minute), a.ExpiresAt)

	// Two distinct admins: the proposer cannot approve, a second admin can
	// (break_glass:request is the two-person form for this kind).
	_, err = f.approve(actx, a.ID.String(), "me")
	requireCode(t, err, errs.CodeForbidden)
	_, err = f.approve(bctx, a.ID.String(), "confirmed with on-call lead")
	require.NoError(t, err)

	// A non-break-glass action cannot be executed as a grant.
	other, err := f.propose(f.operator(adminA, security.RoleAdmin), Proposal{
		Kind: KindAccountUnfreeze, TargetType: "account", TargetID: "acct-x", Reason: "not a grant at all",
	})
	require.NoError(t, err)
	err = inTx(actx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := NewBreakGlass(f.svc).Grant(ctx, tx, other.ID.String())
		return err
	})
	requireCode(t, err, errs.CodeInvalidStateTransition)

	f.clk.Advance(time.Minute)
	actx = f.operator(adminA, security.RoleAdmin)
	var grant Grant
	require.NoError(t, inTx(actx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		grant, err = NewBreakGlass(f.svc).Grant(ctx, tx, a.ID.String())
		return err
	}))
	assert.Equal(t, Grant{ActionID: a.ID.String(), UserID: grantee, Scope: "INC-77 release WITHDRAWALS_DISABLE", ExpiresAt: f.clk.Now().Add(30 * time.Minute)}, grant)

	stored := f.get(actx, a.ID.String())
	assert.Equal(t, StatusExecuted, stored.Status)
	parsed, err := ParseGrant(stored.ExecutionResult)
	require.NoError(t, err)
	assert.Equal(t, grant, parsed)

	// The grant elevates only the grantee, until its expiry.
	gp := security.Principal{SubjectID: grantee, ActorType: security.ActorOperator, Roles: []security.Role{security.RoleOperations}, AuthTime: f.clk.Now(), AMR: []string{"mfa"}}
	elevated := PrincipalWithBreakGlass(gp, grant)
	require.NoError(t, elevated.Validate())
	assert.True(t, elevated.Has(security.PermKillRelease, f.clk.Now()))
	assert.False(t, elevated.Has(security.PermKillRelease, grant.ExpiresAt))
	assert.False(t, PrincipalWithBreakGlass(security.Principal{SubjectID: adminA, ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin}}, grant).Has(security.PermKillRelease, f.clk.Now()))

	// It is audited on the admin stream, and verifiable.
	events := adminAuditFor(t, a.ID.String())
	actions := make([]string, 0, len(events))
	for _, e := range events {
		actions = append(actions, e.Action)
	}
	assert.Equal(t, []string{AuditProposed, AuditApproved, AuditBreakGlassGranted, AuditExecuted}, actions)
	assert.Equal(t, grantee, events[2].ResourceID)
	ap, err := f.svc.VerifyApproved(context.Background(), testDB, a.ID.String(), KindBreakGlassGrant, grantee)
	require.NoError(t, err)
	assert.Equal(t, StatusExecuted, ap.Status)

	// A grant with malformed params fails at execution and is recorded.
	bad, err := f.propose(f.operator(adminA, security.RoleAdmin), Proposal{
		Kind: KindBreakGlassGrant, TargetType: "user", TargetID: grantee,
		Params: json.RawMessage(`{"user_id":"` + grantee + `","scope":"x","duration_seconds":99999}`), Reason: "too long a grant",
	})
	require.NoError(t, err)
	_, err = f.approve(f.operator(adminB, security.RoleAdmin), bad.ID.String(), "ok")
	require.NoError(t, err)
	err = inTx(f.operator(adminA, security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) error {
		_, err := NewBreakGlass(f.svc).Grant(ctx, tx, bad.ID.String())
		requireCode(t, err, errs.CodeValidationFailed)
		return nil // keep the FAILED record
	})
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, f.get(actx, bad.ID.String()).Status)

	rep, err := audit.NewVerifier().VerifyStream(context.Background(), testDB, audit.AdminStream)
	require.NoError(t, err)
	assert.True(t, rep.OK, rep.Reason)
}

func TestIntegration_AdminStreamVerifiesAfterEverything(t *testing.T) {
	requireEnv(t)
	rep, err := audit.NewVerifier().VerifyStream(context.Background(), testDB, audit.AdminStream)
	require.NoError(t, err)
	assert.True(t, rep.OK, rep.Reason)
	assert.Positive(t, rep.Events)
	var transitions, events int
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT count(*) FROM admin_action_transitions`).Scan(&transitions))
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE stream = 'admin' AND resource_type = 'admin_action'`).Scan(&events))
	assert.Equal(t, transitions, events, "one admin_action audit event per transition")
}
