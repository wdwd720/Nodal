//go:build integration

package adminplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

// testAdvisoryLockID is shared with the other suites that migrate the same
// database, so two packages never run goose concurrently.
const testAdvisoryLockID = 424242

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	lockConn, err := pgx.Connect(ctx, testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "adminplane integration: connect for advisory lock:", err)
		return 1
	}
	defer func() { _ = lockConn.Close(ctx) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock_shared($1)", testAdvisoryLockID); err != nil {
		fmt.Fprintln(os.Stderr, "adminplane integration: advisory lock:", err)
		return 1
	}
	defer func() { _, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock_shared($1)", testAdvisoryLockID) }()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "adminplane integration: migrate up:", err)
		return 1
	}
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "adminplane-itest", MaxConns: 16})
	if err != nil {
		fmt.Fprintln(os.Stderr, "adminplane integration: open pool:", err)
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

// t0 is the fake clock's origin. Every fixture is created here so a run can
// never depend on the wall clock.
var t0 = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

type harness struct {
	t   *testing.T
	clk *clock.Fake
	svc *admin.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	requireEnv(t)
	clk := clock.NewFake(t0)
	return &harness{t: t, clk: clk, svc: admin.NewService(clk, audit.NewWriter())}
}

// newUser inserts a users row and returns its id. Rows accumulate across runs,
// which is fine: nothing here queries "all users".
func (h *harness) newUser() string {
	h.t.Helper()
	uid := id.New[id.Any]().String()
	_, err := testDB.Exec(context.Background(),
		`INSERT INTO users (id, idp_issuer, idp_subject, status) VALUES ($1, 'adminplane-itest', $2, 'ACTIVE')`,
		uid, "sub-"+uid)
	require.NoError(h.t, err)
	return uid
}

// ctxFor attaches p to a context.
func ctxFor(p security.Principal) context.Context {
	return security.WithPrincipal(context.Background(), p)
}

// attempt runs fn inside a transaction that is always rolled back, so the same
// stored action can be attacked from every angle without its state advancing.
// The authorization, state-machine and CHECK-constraint verdicts all happen
// before COMMIT, so rolling back loses none of them.
func attempt(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := testDB.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return fn(ctx, tx)
}

// commit runs fn in a transaction that commits on success.
func (h *harness) commit(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(ctx, db.TxOptions{}, fn)
}

func noopExec(_ context.Context, _ pgx.Tx, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// persona is one shape of caller the console might hold a session for.
type persona struct {
	name string
	// build produces the principal as it would look at now. The step-up
	// freshness is relative to now, so a persona keeps its meaning as the fake
	// clock advances.
	build func(now time.Time) (security.Principal, bool)
}

// personas covers the bypasses a real attacker or a careless operator reaches
// for: no session, an agent session, a stale or password-only session, the
// proposer holding the elevation, an elevation that has expired, a principal
// whose subject is not a user, and the read-only operator.
func personas(h *harness, proposer, approver string) []persona {
	base := func(uid string, roles ...security.Role) func(time.Time) (security.Principal, bool) {
		return func(now time.Time) (security.Principal, bool) {
			return security.Principal{
				SubjectID: uid, ActorType: security.ActorOperator, Roles: roles,
				SessionID: "sess-" + uid, AuthTime: now.Add(-time.Minute), AMR: []string{"mfa"},
			}, true
		}
	}
	withBreakGlass := func(inner func(time.Time) (security.Principal, bool), until func(time.Time) time.Time) func(time.Time) (security.Principal, bool) {
		return func(now time.Time) (security.Principal, bool) {
			p, ok := inner(now)
			u := until(now)
			p.Roles = append(p.Roles, security.RoleBreakGlass)
			p.BreakGlassUntil = &u
			return p, ok
		}
	}
	return []persona{
		{name: "anonymous", build: func(time.Time) (security.Principal, bool) { return security.Principal{}, false }},
		{name: "agent", build: func(time.Time) (security.Principal, bool) {
			return security.AgentPrincipal(id.New[id.Any]().String(), "acct-1"), true
		}},
		{name: "support_read_only", build: base(h.newUser(), security.RoleSupportReadOnly)},
		{name: "proposer_admin", build: base(proposer, security.RoleAdmin)},
		{
			name:  "proposer_admin_elevated",
			build: withBreakGlass(base(proposer, security.RoleAdmin), func(now time.Time) time.Time { return now.Add(time.Hour) }),
		},
		{name: "other_admin", build: base(approver, security.RoleAdmin)},
		{
			name:  "other_admin_elevated",
			build: withBreakGlass(base(approver, security.RoleAdmin), func(now time.Time) time.Time { return now.Add(time.Hour) }),
		},
		{
			name:  "other_admin_elevation_expired",
			build: withBreakGlass(base(approver, security.RoleAdmin), func(now time.Time) time.Time { return now.Add(-time.Second) }),
		},
		{name: "other_admin_stale_step_up", build: func(now time.Time) (security.Principal, bool) {
			p, _ := base(approver, security.RoleAdmin)(now)
			p.Roles = append(p.Roles, security.RoleBreakGlass)
			u := now.Add(time.Hour)
			p.BreakGlassUntil = &u
			p.AuthTime = now.Add(-24 * time.Hour)
			return p, true
		}},
		{name: "other_admin_password_only", build: func(now time.Time) (security.Principal, bool) {
			p, _ := base(approver, security.RoleAdmin)(now)
			p.Roles = append(p.Roles, security.RoleBreakGlass)
			u := now.Add(time.Hour)
			p.BreakGlassUntil = &u
			p.AMR = []string{"pwd"}
			return p, true
		}},
		{name: "subject_is_not_a_user", build: func(now time.Time) (security.Principal, bool) {
			p, _ := base("dev:admin", security.RoleAdmin)(now)
			p.Roles = append(p.Roles, security.RoleBreakGlass)
			u := now.Add(time.Hour)
			p.BreakGlassUntil = &u
			return p, true
		}},
	}
}

// TestIntegration_AffordanceAgreesWithEnforcement is the Stage 15 exit
// criterion in its strongest form. For every action kind, every stored state
// and every caller shape, the console's affordance and the enforcing service
// must reach the same verdict — and, when they refuse, the same errs.Code.
//
// Both directions are failures. An affordance that says yes where the service
// says no is a dead button. An affordance that says no where the service says
// yes hides a real capability behind a greyed-out control, and the operator
// then reaches for a worse tool.
func TestIntegration_AffordanceAgreesWithEnforcement(t *testing.T) {
	h := newHarness(t)
	proposer, approver := h.newUser(), h.newUser()
	people := personas(h, proposer, approver)

	proposerCtx := func(now time.Time) context.Context {
		return ctxFor(security.Principal{
			SubjectID: proposer, ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin},
			SessionID: "sess", AuthTime: now.Add(-time.Minute), AMR: []string{"mfa"},
		})
	}
	elevatedCtx := func(uid string, now time.Time) context.Context {
		u := now.Add(time.Hour)
		return ctxFor(security.Principal{
			SubjectID: uid, ActorType: security.ActorOperator,
			Roles:     []security.Role{security.RoleAdmin, security.RoleBreakGlass},
			SessionID: "sess", AuthTime: now.Add(-time.Minute), AMR: []string{"mfa"}, BreakGlassUntil: &u,
		})
	}

	// Two stored actions per kind: one left PROPOSED, one moved to APPROVED by
	// a distinct elevated principal.
	type fixture struct {
		proposed admin.Action
		approved admin.Action
	}
	fixtures := map[admin.Kind]fixture{}
	for _, kind := range admin.Kinds() {
		var f fixture
		require.NoError(t, h.commit(proposerCtx(t0), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			f.proposed, err = h.svc.Propose(ctx, tx, admin.Proposal{
				Kind: kind, TargetType: "target", TargetID: "left-" + string(kind),
				Reason: "left open for the affordance matrix",
			})
			return err
		}), kind)
		require.NoError(t, h.commit(proposerCtx(t0), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			f.approved, err = h.svc.Propose(ctx, tx, admin.Proposal{
				Kind: kind, TargetType: "target", TargetID: "right-" + string(kind),
				Reason: "moved forward for the affordance matrix",
			})
			return err
		}), kind)
		spec, _ := admin.Spec(kind)
		if spec.RequiresDual {
			require.NoError(t, h.commit(elevatedCtx(approver, t0), func(ctx context.Context, tx pgx.Tx) error {
				var err error
				f.approved, err = h.svc.Approve(ctx, tx, f.approved.ID.String(), "approved for the affordance matrix")
				return err
			}), kind)
			require.Equal(t, admin.StatusApproved, f.approved.Status, kind)
		}
		fixtures[kind] = f
	}

	// Three clock positions: now (fresh), and past the longest expiry so every
	// action in the table is dead.
	clockPoints := []struct {
		name string
		at   time.Time
	}{
		{"fresh", t0},
		{"after every expiry", t0.Add(48 * time.Hour)},
	}

	checked := 0
	byReason := map[Reason]int{}
	for _, cp := range clockPoints {
		h.clk.Set(cp.at)
		now := cp.at
		for _, kind := range admin.Kinds() {
			f := fixtures[kind]
			states := map[string]admin.Action{"proposed": f.proposed, "approved": f.approved}
			for stateName, stored := range states {
				// Re-read so the affordance judges exactly what the service will lock.
				var action admin.Action
				require.NoError(t, h.commit(elevatedCtx(approver, now), func(ctx context.Context, tx pgx.Tx) error {
					var err error
					action, err = h.svc.Get(ctx, tx, stored.ID.String())
					return err
				}))
				for _, who := range people {
					p, present := who.build(now)
					ctx := context.Background()
					if present {
						ctx = ctxFor(p)
					}
					actor := NewActor(p)
					if !present {
						actor = Actor{}
					}
					for _, verb := range []Verb{VerbApprove, VerbReject, VerbExecute, VerbCancel} {
						checked++
						want := Decide(actor, action, verb, now)
						byReason[want.Reason]++
						err := invoke(ctx, h, action.ID.String(), verb)
						label := fmt.Sprintf("%s | %s | %s/%s | %s", cp.name, kind, stateName, verb, who.name)
						if want.Allowed {
							assert.NoError(t, err, "affordance offered a refused action: %s", label)
							continue
						}
						if assert.Error(t, err, "affordance hid a permitted action: %s (%s)", label, want.Reason) {
							assert.Equal(t, want.Code, errs.CodeOf(err),
								"%s: affordance said %s/%s, service said %v", label, want.Reason, want.Code, err)
						}
					}
				}
			}
		}
	}
	t.Logf("checked %d (persona, kind, state, verb, clock) combinations: %v", checked, byReason)
	// A matrix in which nothing is ever permitted would agree with an
	// enforcing layer that refuses everything, and prove nothing. Pin the
	// arms that must actually occur.
	for _, want := range []Reason{
		ReasonAllowed, ReasonUnauthenticated, ReasonAgentPrincipal, ReasonSubjectNotUser,
		ReasonMissingPermission, ReasonStepUpRequired, ReasonSelfApproval, ReasonNotProposer,
		ReasonKindTakesNoApproval, ReasonExpired, ReasonWrongStatus, ReasonAwaitingApproval,
	} {
		assert.Positive(t, byReason[want], "the matrix never exercised %s", want)
	}
}

// invoke performs verb against the real service in a rolled-back transaction.
func invoke(ctx context.Context, h *harness, actionID string, verb Verb) error {
	return attempt(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		switch verb {
		case VerbApprove:
			_, err = h.svc.Approve(ctx, tx, actionID, "approval note")
		case VerbReject:
			_, err = h.svc.Reject(ctx, tx, actionID, "a rejection reason")
		case VerbCancel:
			_, err = h.svc.Cancel(ctx, tx, actionID, "a cancellation note")
		case VerbExecute:
			_, err = h.svc.Execute(ctx, tx, actionID, noopExec)
		default:
			err = fmt.Errorf("unsupported verb %q", verb)
		}
		return err
	})
}

// TestIntegration_SelfApprovalIsRefusedByTheDatabaseToo proves the second lock.
// The service refuses self-approval in Go; if that check were ever removed or
// bypassed, the CHECK constraint on admin_actions still refuses the write.
func TestIntegration_SelfApprovalIsRefusedByTheDatabaseToo(t *testing.T) {
	h := newHarness(t)
	proposer := h.newUser()
	ctx := ctxFor(security.Principal{
		SubjectID: proposer, ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin},
		SessionID: "sess", AuthTime: t0.Add(-time.Minute), AMR: []string{"mfa"},
	})
	var a admin.Action
	require.NoError(t, h.commit(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = h.svc.Propose(ctx, tx, admin.Proposal{
			Kind: admin.KindKillSwitchRelease, TargetType: "kill_switch",
			TargetID: "GLOBAL_NEW_RISK_KILL:*", Reason: "release after the incident closed",
		})
		return err
	}))

	// Straight past the service, straight at the table.
	err := attempt(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`UPDATE admin_actions SET status = 'APPROVED', approved_by_user_id = proposed_by_user_id, approved_at = now() WHERE id = $1`,
			a.ID)
		return e
	})
	require.Error(t, err, "the table accepted a self-approved row")
	assert.True(t, db.IsCheckViolation(err), "expected a CHECK violation, got %v", err)

	// A distinct approver is accepted by the same statement, so the refusal
	// above is the constraint and not some other failure.
	other := h.newUser()
	require.NoError(t, attempt(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`UPDATE admin_actions SET status = 'APPROVED', approved_by_user_id = $2, approved_at = now() WHERE id = $1`,
			a.ID, other)
		return e
	}))
}

// TestIntegration_ApprovalNeverCrossesRecordOrKind is the replay attack: take a
// genuine approval and present it for a different record, or for a different
// kind of action against the same record. Every consumer of an approval id
// (kill-switch release, gate activation, material reconciliation resolution,
// ledger correction) goes through VerifyApproved, so this is the one place the
// guard has to hold.
func TestIntegration_ApprovalNeverCrossesRecordOrKind(t *testing.T) {
	h := newHarness(t)
	proposer, approver := h.newUser(), h.newUser()
	pctx := ctxFor(security.Principal{
		SubjectID: proposer, ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin},
		SessionID: "sess", AuthTime: t0.Add(-time.Minute), AMR: []string{"mfa"},
	})
	until := t0.Add(time.Hour)
	actx := ctxFor(security.Principal{
		SubjectID: approver, ActorType: security.ActorOperator,
		Roles:     []security.Role{security.RoleAdmin, security.RoleBreakGlass},
		SessionID: "sess", AuthTime: t0.Add(-time.Minute), AMR: []string{"mfa"}, BreakGlassUntil: &until,
	})

	// One genuine, complete approval per dual-control kind, each naming its own
	// target.
	target := func(k admin.Kind) string { return "record-of-" + string(k) }
	approvals := map[admin.Kind]admin.Action{}
	for _, kind := range admin.Kinds() {
		spec, _ := admin.Spec(kind)
		if !spec.RequiresDual {
			continue
		}
		var a admin.Action
		require.NoError(t, h.commit(pctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			a, err = h.svc.Propose(ctx, tx, admin.Proposal{
				Kind: kind, TargetType: "target", TargetID: target(kind),
				Reason: "a genuine approval for the replay matrix",
			})
			return err
		}), kind)
		require.NoError(t, h.commit(actx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			a, err = h.svc.Approve(ctx, tx, a.ID.String(), "genuinely approved")
			return err
		}), kind)
		approvals[kind] = a
	}
	require.NotEmpty(t, approvals)

	bg := context.Background()
	for mintedFor, a := range approvals {
		// It verifies for exactly its own kind and its own target.
		got, err := h.svc.VerifyApproved(bg, testDB, a.ID.String(), mintedFor, target(mintedFor))
		require.NoError(t, err, mintedFor)
		require.Equal(t, approver, *got.ApprovedBy)

		for presentedAs := range approvals {
			for presentedFor := range approvals {
				if presentedAs == mintedFor && presentedFor == mintedFor {
					continue
				}
				_, err := h.svc.VerifyApproved(bg, testDB, a.ID.String(), presentedAs, target(presentedFor))
				assert.Error(t, err, "approval minted for %s/%s was accepted as %s/%s",
					mintedFor, target(mintedFor), presentedAs, target(presentedFor))
				assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
			}
		}
		// And never for a target that simply does not exist.
		_, err = h.svc.VerifyApproved(bg, testDB, a.ID.String(), mintedFor, "record-of-something-else")
		assert.Error(t, err)
	}
}

// TestIntegration_ExpiredElevationIsRefusedByTheService pairs the unit-level
// affordance test with the real service: a session that still carries the
// BREAK_GLASS role after its deadline approves nothing.
func TestIntegration_ExpiredElevationIsRefusedByTheService(t *testing.T) {
	h := newHarness(t)
	proposer, approver := h.newUser(), h.newUser()
	pctx := ctxFor(security.Principal{
		SubjectID: proposer, ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin},
		SessionID: "sess", AuthTime: t0.Add(-time.Minute), AMR: []string{"mfa"},
	})
	var a admin.Action
	require.NoError(t, h.commit(pctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = h.svc.Propose(ctx, tx, admin.Proposal{
			Kind: admin.KindReconciliationResolveMaterial, TargetType: "reconciliation_record",
			TargetID: id.New[id.Any]().String(), Reason: "material mismatch needs two people",
		})
		return err
	}))

	elevationEnds := t0.Add(10 * time.Minute)
	session := func(now time.Time) context.Context {
		u := elevationEnds
		return ctxFor(security.Principal{
			SubjectID: approver, ActorType: security.ActorOperator,
			Roles:     []security.Role{security.RoleAdmin, security.RoleBreakGlass},
			SessionID: "sess", AuthTime: now.Add(-time.Minute), AMR: []string{"mfa"}, BreakGlassUntil: &u,
		})
	}

	// Live: refused only because we roll back, not because of authority.
	h.clk.Set(elevationEnds.Add(-time.Minute))
	require.NoError(t, invoke(session(h.clk.Now()), h, a.ID.String(), VerbApprove))

	// Dead: the permission is simply not held any more.
	for _, at := range []time.Time{elevationEnds, elevationEnds.Add(time.Second), elevationEnds.Add(time.Hour)} {
		h.clk.Set(at)
		err := invoke(session(at), h, a.ID.String(), VerbApprove)
		require.Error(t, err, "an elevation expired at %s was honored at %s", elevationEnds, at)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

		p, _ := security.PrincipalFrom(session(at))
		assert.Equal(t, ElevationExpired, ElevationOf(p, at).State)
		assert.Equal(t, ReasonMissingPermission, Decide(NewActor(p), a, VerbApprove, at).Reason)
	}
}

// TestIntegration_BreakGlassGrantIsItselfDualControlled closes the loop: the
// elevation that unlocks every approve permission is obtained through the same
// two-person process, and the requester can never approve their own request.
func TestIntegration_BreakGlassGrantIsItselfDualControlled(t *testing.T) {
	h := newHarness(t)
	requester, granter, grantee := h.newUser(), h.newUser(), h.newUser()
	sess := func(uid string, roles ...security.Role) context.Context {
		return ctxFor(security.Principal{
			SubjectID: uid, ActorType: security.ActorOperator, Roles: roles,
			SessionID: "sess", AuthTime: h.clk.Now().Add(-time.Minute), AMR: []string{"mfa"},
		})
	}
	params, err := json.Marshal(admin.BreakGlassParams{
		UserID: grantee, Scope: "incident-2026-09-06", DurationSeconds: int64(time.Hour / time.Second),
	})
	require.NoError(t, err)

	var a admin.Action
	require.NoError(t, h.commit(sess(requester, security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) error {
		var e error
		a, e = h.svc.Propose(ctx, tx, admin.Proposal{
			Kind: admin.KindBreakGlassGrant, TargetType: "user", TargetID: grantee,
			Params: params, Reason: "incident response needs a second pair of hands",
		})
		return e
	}))

	// The requester holds break_glass:approve as an ADMIN, and is still refused.
	require.Equal(t, ReasonSelfApproval,
		Decide(NewActor(mustPrincipal(sess(requester, security.RoleAdmin), t)), a, VerbApprove, h.clk.Now()).Reason)
	err = invoke(sess(requester, security.RoleAdmin), h, a.ID.String(), VerbApprove)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// A distinct SECURITY principal approves and executes it.
	require.NoError(t, h.commit(sess(granter, security.RoleSecurity), func(ctx context.Context, tx pgx.Tx) error {
		var e error
		a, e = h.svc.Approve(ctx, tx, a.ID.String(), "incident confirmed in the bridge")
		return e
	}))
	var grant admin.Grant
	require.NoError(t, h.commit(sess(granter, security.RoleSecurity), func(ctx context.Context, tx pgx.Tx) error {
		var e error
		grant, e = admin.NewBreakGlass(h.svc).Grant(ctx, tx, a.ID.String())
		return e
	}))
	require.Equal(t, grantee, grant.UserID)
	assert.True(t, grant.Active(h.clk.Now()))

	// The grant elevates exactly one person, and dies on the clock.
	granteeP, _ := security.PrincipalFrom(sess(grantee, security.RoleOperations))
	elevatedP := admin.PrincipalWithBreakGlass(granteeP, grant)
	assert.Equal(t, ElevationActive, ElevationOf(elevatedP, h.clk.Now()).State)
	assert.Equal(t, ElevationExpired, ElevationOf(elevatedP, grant.ExpiresAt).State)

	requesterP, _ := security.PrincipalFrom(sess(requester, security.RoleAdmin))
	assert.Equal(t, ElevationNone, ElevationOf(admin.PrincipalWithBreakGlass(requesterP, grant), h.clk.Now()).State,
		"the requester was elevated by a grant that names someone else")
}

func mustPrincipal(ctx context.Context, t *testing.T) security.Principal {
	t.Helper()
	p, ok := security.PrincipalFrom(ctx)
	require.True(t, ok)
	return p
}
