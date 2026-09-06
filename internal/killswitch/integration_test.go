//go:build integration

package killswitch

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// Needs an isolated database (go run ./scripts/testdb -name gates): cp_app
// cannot delete rows, so id-scoped tests use fresh scopes and global
// switches are released before use.
var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "killswitch integration: migrate up:", err)
		return 1
	}
	var err error
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "killswitch-itest", MaxConns: 8})
	if err != nil {
		fmt.Fprintln(os.Stderr, "killswitch integration: open pool:", err)
		return 1
	}
	code := m.Run()
	testDB.Close()
	return code
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}

// newClock seeds the fake clock from the wall clock: rows persist across
// runs (cp_app cannot delete), so each run's transitions must sort after
// the previous run's.
func newClock() *clock.Fake {
	return clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
}

// op is an operator principal authenticated strongly at the fixture's
// current time.
func (f *fixture) op(sub string, roles ...security.Role) security.Principal {
	return security.Principal{SubjectID: sub, ActorType: security.ActorOperator, Roles: roles, AuthTime: f.clk.Now(), AMR: []string{"mfa"}}
}

// bg is an operator holding a live BREAK_GLASS elevation for one hour from
// the fixture's current time.
func (f *fixture) bg(sub string) security.Principal {
	p := f.op(sub, security.RoleOperations, security.RoleBreakGlass)
	until := f.clk.Now().Add(time.Hour)
	p.BreakGlassUntil = &until
	return p
}

// transitionsSince returns the switch's transitions not present in
// baseline, in (occurred_at, id) order. Rows persist across runs, so tests
// diff against a baseline instead of asserting absolute positions.
func transitionsSince(t *testing.T, switchID SwitchID, baseline []Transition) []Transition {
	t.Helper()
	seen := map[TransitionID]bool{}
	for _, tr := range baseline {
		seen[tr.ID] = true
	}
	all, err := Transitions(context.Background(), testDB, switchID)
	require.NoError(t, err)
	var out []Transition
	for _, tr := range all {
		if !seen[tr.ID] {
			out = append(out, tr)
		}
	}
	return out
}

type syncAudit struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (s *syncAudit) Append(_ context.Context, _ pgx.Tx, e AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

func (s *syncAudit) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

type fixture struct {
	clk     *clock.Fake
	audit   *syncAudit
	ver     *fakeVerifier
	ctl     *Controller
	checker *Checker
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	f := &fixture{clk: newClock(), audit: &syncAudit{}, ver: &fakeVerifier{approvals: map[string]Approval{}}}
	var err error
	f.ctl, err = NewController(f.clk, f.audit, f.ver)
	require.NoError(t, err)
	f.checker = NewChecker(Policy{})
	return f
}

func (f *fixture) inTx(t *testing.T, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return testDB.InTx(ctx, db.TxOptions{}, fn)
}

func (f *fixture) activate(t *testing.T, p security.Principal, kind Kind, scope, reason string) (Switch, error) {
	t.Helper()
	var s Switch
	err := f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, err = f.ctl.Activate(security.WithPrincipal(ctx, p), tx, kind, scope, reason)
		return err
	})
	return s, err
}

func (f *fixture) release(t *testing.T, p security.Principal, kind Kind, scope, reason string, approvalID *string) (Switch, error) {
	t.Helper()
	var s Switch
	err := f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, err = f.ctl.Release(security.WithPrincipal(ctx, p), tx, kind, scope, reason, approvalID)
		return err
	})
	return s, err
}

// approve registers a dual-controlled release approval with the fake
// verifier and returns its id.
func (f *fixture) approve(kind Kind, scope string) string {
	id := uuid.Must(uuid.NewV7()).String()
	f.ver.approvals[id] = Approval{
		ID: id, Kind: ApprovalKindRelease, TargetID: ReleaseTargetID(kind, scope),
		ProposedBy: "ops-dave", ApprovedBy: "bg-erin", ApprovedAt: f.clk.Now(), ExpiresAt: f.clk.Now().Add(time.Hour),
	}
	return id
}

// ensureReleased clears a switch a previous run may have left active.
func (f *fixture) ensureReleased(t *testing.T, kind Kind, scope string) {
	t.Helper()
	s, err := Get(context.Background(), testDB, kind, scope)
	if errs.CodeOf(err) == errs.CodeNotFound || (err == nil && !s.Active) {
		return
	}
	require.NoError(t, err)
	var ap *string
	if kind.Severity() == SeveritySevere {
		id := f.approve(kind, scope)
		ap = &id
	}
	_, err = f.release(t, f.bg("reset"), kind, scope, "test reset", ap)
	require.NoError(t, err)
}

func (f *fixture) check(a Action) error {
	return f.checker.Check(context.Background(), testDB, a)
}

func manualTrade(account string) Action {
	return Action{Class: NewRisk, AccountID: account, Venue: "jupiter", InstrumentID: "SOL-USDC", Chain: "solana", Provider: "helius"}
}

func TestIntegration_GlobalKill_BlocksNewRiskNotReconciliation(t *testing.T) {
	f := newFixture(t)
	f.ensureReleased(t, GlobalNewRiskKill, "*")
	ops := f.op("ops-dave", security.RoleOperations)
	ops.AuthTime = f.clk.Now().Add(-48 * time.Hour) // no step-up needed to activate
	ops.AMR = []string{"pwd"}
	acct := "acct-" + uuid.NewString()

	require.NoError(t, f.check(manualTrade(acct)), "clean state")
	var baseline []Transition
	if prior, err := Get(context.Background(), testDB, GlobalNewRiskKill, "*"); err == nil {
		baseline, err = Transitions(context.Background(), testDB, prior.ID)
		require.NoError(t, err)
	}

	s, err := f.activate(t, ops, GlobalNewRiskKill, "", "market-wide anomaly")
	require.NoError(t, err)
	assert.True(t, s.Active)
	assert.Equal(t, GlobalScope, s.ScopeID)
	assert.Equal(t, SeveritySevere, s.Severity)
	assert.Equal(t, "ops-dave", s.ActivatedBy)
	assert.Equal(t, 1, f.audit.count())

	// PART 165: new manual trades and new agent intents are rejected...
	err = f.check(manualTrade(acct))
	require.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err))
	e, _ := errs.As(err)
	assert.Equal(t, "GLOBAL_NEW_RISK_KILL", e.Fields["switch"])
	assert.Equal(t, "*", e.Fields["scope"])
	agentIntent := Action{Class: NewRisk, AccountID: acct, AgentID: "agent-1", StrategyVersionID: "sv-1", ModelID: "m-1"}
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(f.check(agentIntent)))
	funding := Action{Class: NewRisk, AccountID: acct, Provider: "stripe", Funding: true}
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(f.check(funding)))
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(f.check(Action{Class: Withdraw, AccountID: acct})))
	// ...while reconciliation, fills, settlement, ledger posting, cancels and
	// risk reduction continue.
	for _, class := range []ActionClass{Reconcile, Settle, LedgerPost, Observe, Cancel, ReduceRisk} {
		a := manualTrade(acct)
		a.Class = class
		assert.NoErrorf(t, f.check(a), "%s during global kill", class)
	}

	// Operator sees state.
	snap, err := f.checker.Snapshot(context.Background(), testDB)
	require.NoError(t, err)
	assert.True(t, snap.Global())
	active, err := f.ctl.Active(context.Background(), testDB)
	require.NoError(t, err)
	found := false
	for _, sw := range active {
		if sw.Kind == GlobalNewRiskKill {
			found = true
			assert.Equal(t, s.ID, sw.ID)
		}
	}
	assert.True(t, found)

	// Pressing it again is a harmless no-op.
	again, err := f.activate(t, ops, GlobalNewRiskKill, "*", "still bad")
	require.NoError(t, err)
	assert.Equal(t, s.Version, again.Version)
	assert.Equal(t, "market-wide anomaly", again.Reason)
	ts := transitionsSince(t, s.ID, baseline)
	require.Len(t, ts, 1, "exactly one new activation transition")
	assert.True(t, ts[0].ToActive)
	assert.Equal(t, "ops-dave", ts[0].ActorID)
	assert.Equal(t, 1, f.audit.count())

	// Re-enable requires the configured approval: the activator cannot
	// release; break-glass without an approval cannot either; a wrong
	// approval is refused; the right one works.
	_, err = f.release(t, ops, GlobalNewRiskKill, "*", "all clear", nil)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	bg := f.bg("bg-frank")
	_, err = f.release(t, bg, GlobalNewRiskKill, "*", "all clear", nil)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	wrong := f.approve(FundingDisable, "*")
	_, err = f.release(t, bg, GlobalNewRiskKill, "*", "all clear", &wrong)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	unknown := uuid.NewString()
	_, err = f.release(t, bg, GlobalNewRiskKill, "*", "all clear", &unknown)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(f.check(manualTrade(acct))), "still active after failed releases")

	ok := f.approve(GlobalNewRiskKill, "*")
	f.clk.Advance(time.Minute)
	rel, err := f.release(t, bg, GlobalNewRiskKill, "*", "all clear", &ok)
	require.NoError(t, err)
	assert.False(t, rel.Active)
	assert.Equal(t, "bg-frank", rel.ReleasedBy)
	assert.Equal(t, ok, rel.ReleaseApprovalID)
	assert.Equal(t, "all clear", rel.ReleaseReason)
	require.NotNil(t, rel.ReleasedAt)
	assert.NoError(t, f.check(manualTrade(acct)))
	assert.Equal(t, 2, f.audit.count())
	assert.Equal(t, "kill_switch.release", f.audit.events[1].Action)
	assert.Equal(t, ok, f.audit.events[1].EvidenceRef)

	ts = transitionsSince(t, s.ID, baseline)
	require.Len(t, ts, 2, "activate, release")
	last := ts[1]
	assert.False(t, last.ToActive)
	assert.Equal(t, ok, last.ApprovalID)
	assert.Equal(t, security.ActorOperator, last.ActorType)

	// Releasing twice is not a legal transition.
	_, err = f.release(t, bg, GlobalNewRiskKill, "*", "again", &ok)
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	// Re-activation creates a new activation on the same row.
	s2, err := f.activate(t, ops, GlobalNewRiskKill, "*", "second incident")
	require.NoError(t, err)
	assert.Equal(t, s.ID, s2.ID)
	assert.True(t, s2.Active)
	assert.Empty(t, s2.ReleasedBy)
	assert.Nil(t, s2.ReleasedAt)
	f.ensureReleased(t, GlobalNewRiskKill, "*")
}

func TestIntegration_StandardSwitch_ScopedAndReleasedWithStepUp(t *testing.T) {
	f := newFixture(t)
	sec := f.op("sec-gina", security.RoleSecurity)
	acct, other := "acct-"+uuid.NewString(), "acct-"+uuid.NewString()

	s, err := f.activate(t, sec, AccountFreeze, acct, "suspected account takeover")
	require.NoError(t, err)
	assert.Equal(t, SeverityStandard, s.Severity)
	assert.Equal(t, acct, s.ScopeID)

	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(f.check(manualTrade(acct))))
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(f.check(Action{Class: Withdraw, AccountID: acct})))
	assert.NoError(t, f.check(manualTrade(other)), "other accounts unaffected")
	reduce := manualTrade(acct)
	reduce.Class = ReduceRisk
	assert.NoError(t, f.check(reduce), "frozen account may still reduce risk by default")
	strict := NewChecker(Policy{AccountFreezeBlocksRiskReduction: true})
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(strict.Check(context.Background(), testDB, reduce)))
	for _, class := range []ActionClass{Reconcile, Settle, LedgerPost, Observe, Cancel} {
		a := manualTrade(acct)
		a.Class = class
		assert.NoError(t, f.check(a), class)
	}

	// STANDARD release: kill:release + step-up, no approval.
	_, err = f.release(t, sec, AccountFreeze, acct, "cleared", nil)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "SECURITY lacks kill:release")
	rel, err := f.release(t, f.bg("bg-hank"), AccountFreeze, acct, "cleared", nil)
	require.NoError(t, err)
	assert.False(t, rel.Active)
	assert.Empty(t, rel.ReleaseApprovalID)
	assert.NoError(t, f.check(manualTrade(acct)))
	assert.Zero(t, f.ver.calls, "no approval consulted for a STANDARD release")

	// Unknown switch.
	_, err = f.release(t, f.bg("bg-hank"), AccountFreeze, "acct-"+uuid.NewString(), "x", nil)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	_, err = Get(context.Background(), testDB, AccountFreeze, "acct-"+uuid.NewString())
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

func TestIntegration_AgentCannotActivate(t *testing.T) {
	f := newFixture(t)
	scope := "acct-" + uuid.NewString()
	_, err := f.activate(t, security.AgentPrincipal("agent-1", scope), AccountFreeze, scope, "agent panic")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	_, err = Get(context.Background(), testDB, AccountFreeze, scope)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	assert.Zero(t, f.audit.count())
}

func TestIntegration_CachedChecker_PreChecksOnly(t *testing.T) {
	f := newFixture(t)
	inst := "inst-" + uuid.NewString()
	cc, err := NewCachedChecker(f.checker, testDB, f.clk, 500*time.Millisecond)
	require.NoError(t, err)
	a := Action{Class: NewRisk, AccountID: "acct", InstrumentID: inst}
	require.NoError(t, cc.PreCheck(context.Background(), a), "primes the cache")

	_, err = f.activate(t, f.op("risk-ivy", security.RoleRisk), InstrumentHalt, inst, "bad price feed")
	require.NoError(t, err)

	// The authoritative check sees it immediately; the cache may lag up to
	// its TTL, which is why it serves pre-checks only.
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(f.check(a)))
	assert.NoError(t, cc.PreCheck(context.Background(), a), "stale within ttl")
	f.clk.Advance(500 * time.Millisecond)
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(cc.PreCheck(context.Background(), a)), "refreshed after ttl")
	snap, err := cc.Snapshot(context.Background())
	require.NoError(t, err)
	assert.True(t, snap.Has(InstrumentHalt, inst))

	// Invalidate forces a refresh regardless of ttl.
	_, err = f.release(t, f.bg("bg-jack"), InstrumentHalt, inst, "feed fixed", nil)
	require.NoError(t, err)
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(cc.PreCheck(context.Background(), a)), "still cached")
	cc.Invalidate()
	assert.NoError(t, cc.PreCheck(context.Background(), a))
}

func TestIntegration_ConcurrentActivationsOneRow(t *testing.T) {
	f := newFixture(t)
	scope := "agent-" + uuid.NewString()
	ops := f.op("ops-dave", security.RoleOperations)
	var wg sync.WaitGroup
	results := make([]Switch, 8)
	errsCh := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errsCh[i] = f.activate(t, ops, AgentPause, scope, fmt.Sprintf("press %d", i))
		}(i)
	}
	wg.Wait()
	var id SwitchID
	for i := range results {
		if errsCh[i] != nil {
			// A loser of the insert race reports CONFLICT; the switch is
			// nonetheless active, which is what matters in an emergency.
			assert.Equal(t, errs.CodeConflict, errs.CodeOf(errsCh[i]))
			continue
		}
		if id.IsZero() {
			id = results[i].ID
		}
		assert.Equal(t, id, results[i].ID)
		assert.True(t, results[i].Active)
	}
	s, err := Get(context.Background(), testDB, AgentPause, scope)
	require.NoError(t, err)
	assert.True(t, s.Active)
	ts, err := Transitions(context.Background(), testDB, s.ID)
	require.NoError(t, err)
	assert.Len(t, ts, 1, "exactly one activation transition")
}
