//go:build integration

package gates

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// These tests need an isolated database (go run ./scripts/testdb -name gates)
// because cp_app cannot delete rows: every test resets the gate it uses by
// revoking it, and Bootstrap is idempotent.
var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testOwnerDB    *db.DB
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
		fmt.Fprintln(os.Stderr, "gates integration: migrate up:", err)
		return 1
	}
	var err error
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "gates-itest", MaxConns: 8})
	if err != nil {
		fmt.Fprintln(os.Stderr, "gates integration: open pool:", err)
		return 1
	}
	// The owning role. cp_app holds only UPDATE (version) on capability_gates --
	// deliberately, so the state can move only through cp_gate_transition -- which
	// means an evidence column cannot be blanked as the application at all. The
	// GT003 guard exists for the cases that privilege does not cover: a defect in
	// the Go layer, or anything running as the owner. This pool is how those are
	// reached.
	testMigrateDB, err := db.Open(ctx, db.Config{URL: testMigrateURL, AppName: "gates-itest-owner", MaxConns: 4})
	if err != nil {
		fmt.Fprintln(os.Stderr, "gates integration: open owner pool:", err)
		testDB.Close()
		return 1
	}
	testOwnerDB = testMigrateDB
	code := m.Run()
	testMigrateDB.Close()
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

func transitionsOf(t *testing.T, gateID GateID) []Transition {
	t.Helper()
	ts, err := Transitions(context.Background(), testDB, gateID)
	require.NoError(t, err)
	return ts
}

// transitionsSince returns the gate's transitions not present in baseline,
// in (occurred_at, id) order. Rows persist across runs, so tests diff
// against a baseline instead of asserting absolute positions or counts.
func transitionsSince(t *testing.T, gateID GateID, baseline []Transition) []Transition {
	t.Helper()
	seen := map[TransitionID]bool{}
	for _, tr := range baseline {
		seen[tr.ID] = true
	}
	var out []Transition
	for _, tr := range transitionsOf(t, gateID) {
		if !seen[tr.ID] {
			out = append(out, tr)
		}
	}
	return out
}

type syncAudit struct {
	mu     sync.Mutex
	events []AuditEvent
	fail   error
}

func (s *syncAudit) Append(_ context.Context, _ pgx.Tx, e AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.events = append(s.events, e)
	return nil
}

func (s *syncAudit) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

type fixture struct {
	env     string
	clk     *clock.Fake
	audit   *syncAudit
	admin   *Admin
	checker *Checker
}

func newFixture(t *testing.T, env string) *fixture {
	t.Helper()
	requireEnv(t)
	f := &fixture{env: env, clk: newClock(), audit: &syncAudit{}}
	var err error
	f.admin, err = NewAdmin(env, f.clk, f.audit)
	require.NoError(t, err)
	f.checker, err = NewChecker(env, func(Capability) bool { return true }, f.clk)
	require.NoError(t, err)
	require.NoError(t, f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Bootstrap(ctx, tx, env)
		return err
	}))
	return f
}

func (f *fixture) inTx(t *testing.T, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return testDB.InTx(ctx, db.TxOptions{}, fn)
}

// do runs op inside a transaction with the principal attached.
func (f *fixture) do(t *testing.T, p security.Principal, op func(ctx context.Context, tx pgx.Tx) (Gate, error)) (Gate, error) {
	t.Helper()
	var g Gate
	err := f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		g, err = op(security.WithPrincipal(ctx, p), tx)
		return err
	})
	return g, err
}

func (f *fixture) verdict(t *testing.T, c Capability) Verdict {
	t.Helper()
	v, err := f.checker.IsActive(context.Background(), testDB, c)
	require.NoError(t, err)
	return v
}

func (f *fixture) get(t *testing.T, c Capability) Gate {
	t.Helper()
	g, err := Get(context.Background(), testDB, c, f.env)
	require.NoError(t, err)
	return g
}

// reset puts the gate into a proposable state, revoking it if a previous
// run left it elsewhere.
func (f *fixture) reset(t *testing.T, c Capability) {
	t.Helper()
	g, err := Get(context.Background(), testDB, c, f.env)
	if errs.CodeOf(err) == errs.CodeNotFound {
		return
	}
	require.NoError(t, err)
	switch g.State {
	case StateDisabled, StateRevoked, StateExpired:
		return
	}
	_, err = f.do(t, f.bg("reset-"+g.ID.String()), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Revoke(ctx, tx, c, "test reset")
	})
	require.NoError(t, err)
}

func highRiskProposal(reason string) Proposal {
	return Proposal{
		LegalReviewRef: "LEGAL-2026-01", ProviderContractRef: "STRIPE-MSA-7", RiskApprovalRef: "RISK-11", SecurityApprovalRef: "SEC-3",
		EvidenceHashes: []string{"0f" + repeat("a", 62)}, Reason: reason,
	}
}

func TestIntegration_Bootstrap_Idempotent(t *testing.T) {
	requireEnv(t)
	const env = "STAGING"
	before, err := List(context.Background(), testDB, env)
	require.NoError(t, err)
	var first, second int
	require.NoError(t, testDB.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		first, err = Bootstrap(ctx, tx, env)
		return err
	}))
	assert.Equal(t, len(AllCapabilities())-len(before), first)
	require.NoError(t, testDB.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		second, err = Bootstrap(ctx, tx, env)
		return err
	}))
	assert.Zero(t, second)

	after, err := List(context.Background(), testDB, env)
	require.NoError(t, err)
	require.Len(t, after, len(AllCapabilities()))
	seen := map[Capability]bool{}
	for _, g := range after {
		seen[g.Capability] = true
		if !seen[g.Capability] || len(before) == 0 {
			assert.Equal(t, StateDisabled, g.State, g.Capability)
			assert.Zero(t, g.ApprovalVersion)
			assert.Empty(t, g.Approvers)
		}
	}
	assert.Len(t, seen, len(AllCapabilities()))

	// Fresh deployment: with configuration enabling everything, every
	// capability is still inactive because the persisted row is DISABLED.
	checker, err := NewChecker(env, func(Capability) bool { return true }, newClock())
	require.NoError(t, err)
	for _, c := range AllCapabilities() {
		v, err := checker.IsActive(context.Background(), testDB, c)
		require.NoError(t, err)
		assert.False(t, v.Active, c)
		if len(before) == 0 {
			assert.Equal(t, ReasonStateNotActive, v.Reason, c)
		}
		assert.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(checker.RequireActive(context.Background(), testDB, c)))
	}
	// Unknown environment refused.
	_, err = Bootstrap(context.Background(), nil, "MOON")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestIntegration_Lifecycle_ThreePrincipals(t *testing.T) {
	f := newFixture(t, "TEST")
	const c = LiveFunding
	f.reset(t, c)
	prev := f.get(t, c)
	base := f.audit.count() // reset may itself have audited a revoke
	baseline := transitionsOf(t, prev.ID)
	proposer := f.op("risk-alice", security.RoleRisk)
	bg1, bg2 := f.bg("bg-bob"), f.bg("bg-carol")

	v := f.verdict(t, c)
	assert.False(t, v.Active)

	// Propose.
	g, err := f.do(t, proposer, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, c, highRiskProposal("go live with Stripe funding"))
	})
	require.NoError(t, err)
	assert.Equal(t, StatePendingApproval, g.State)
	assert.Equal(t, prev.ApprovalVersion+1, g.ApprovalVersion)
	assert.Equal(t, "risk-alice", g.ProposedBy)
	require.Len(t, g.Approvers, 1)
	assert.Equal(t, StepPropose, g.Approvers[0].Step)
	assert.Equal(t, "RISK", g.Approvers[0].Role)
	assert.Nil(t, g.RevokedAt)
	assert.Equal(t, ReasonStateNotActive, f.verdict(t, c).Reason)

	// Re-propose while pending is not a legal transition.
	_, err = f.do(t, proposer, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, c, highRiskProposal("again"))
	})
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	// Proposer holding break-glass still cannot approve their own proposal.
	_, err = f.do(t, f.bg("risk-alice"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Approve(ctx, tx, c, "self")
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// Activate before approve is refused.
	_, err = f.do(t, bg1, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Activate(ctx, tx, c, "too early")
	})
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	// Approve.
	g, err = f.do(t, bg1, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Approve(ctx, tx, c, "legal and contract reviewed")
	})
	require.NoError(t, err)
	assert.Equal(t, StateApproved, g.State)
	require.Len(t, g.Approvers, 2)
	assert.Equal(t, "bg-bob", g.Approvers[1].UserID)
	assert.Equal(t, g.EvidenceDigestHex(), g.Approvers[1].EvidenceHash)
	assert.Equal(t, ReasonStateNotActive, f.verdict(t, c).Reason)

	// Same principal cannot activate.
	_, err = f.do(t, bg1, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Activate(ctx, tx, c, "me again")
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	// Nor the proposer.
	_, err = f.do(t, f.bg("risk-alice"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Activate(ctx, tx, c, "me")
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// Activate by a third principal.
	f.clk.Advance(time.Minute)
	g, err = f.do(t, bg2, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Activate(ctx, tx, c, "second approval")
	})
	require.NoError(t, err)
	assert.Equal(t, StateActive, g.State)
	require.NotNil(t, g.EffectiveAt)
	assert.Equal(t, f.clk.Now(), g.EffectiveAt.UTC())
	assert.Equal(t, []string{"bg-bob", "bg-carol"}, g.DistinctApprovers())

	v = f.verdict(t, c)
	assert.True(t, v.Active, v.Reason)
	assert.Equal(t, g.ApprovalVersion, v.ApprovalVersion)
	assert.Equal(t, g.EvidenceHashes, v.EvidenceHashes)
	assert.NoError(t, f.checker.RequireActive(context.Background(), testDB, c))

	// Configuration alone decides nothing: the same row is inactive for a
	// deployment that does not enable it.
	off, err := NewChecker(f.env, func(Capability) bool { return false }, f.clk)
	require.NoError(t, err)
	vv, err := off.IsActive(context.Background(), testDB, c)
	require.NoError(t, err)
	assert.Equal(t, ReasonConfigDisabled, vv.Reason)

	// Transitions and audit so far: propose, approve, activate.
	ts := transitionsSince(t, g.ID, baseline)
	require.Len(t, ts, 3)
	last3 := ts
	assert.Equal(t, []GateState{StateApproved, StateActive}, []GateState{last3[1].To, last3[2].To})
	assert.Equal(t, StatePendingApproval, last3[0].To)
	assert.Equal(t, "risk-alice", last3[0].ActorID)
	assert.Equal(t, security.ActorOperator, last3[0].ActorType)
	assert.Equal(t, "bg-bob", last3[1].ActorID)
	assert.Equal(t, "bg-carol", last3[2].ActorID)
	digest := g.EvidenceDigest()
	assert.Equal(t, digest[:], last3[2].EvidenceHash)
	assert.Equal(t, base+3, f.audit.count())
	assert.Equal(t, "capability_gate.activate", f.audit.events[base+2].Action)
	assert.Equal(t, g.ID.String(), f.audit.events[base+2].ResourceID)

	// Suspend: single operator, no step-up.
	ops := f.op("ops-dave", security.RoleOperations)
	ops.AuthTime = f.clk.Now().Add(-24 * time.Hour)
	ops.AMR = []string{"pwd"}
	g, err = f.do(t, ops, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Suspend(ctx, tx, c, "provider incident")
	})
	require.NoError(t, err)
	assert.Equal(t, StateSuspended, g.State)
	assert.Equal(t, ReasonStateNotActive, f.verdict(t, c).Reason)
	assert.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(f.checker.RequireActive(context.Background(), testDB, c)))
	// Suspending again is not a legal transition; a customer cannot suspend.
	_, err = f.do(t, ops, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Suspend(ctx, tx, c, "again")
	})
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	// Resume goes back to APPROVED, never straight to ACTIVE.
	g, err = f.do(t, bg1, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Resume(ctx, tx, c, "incident resolved")
	})
	require.NoError(t, err)
	assert.Equal(t, StateApproved, g.State)
	assert.False(t, f.verdict(t, c).Active)
	// The resumer cannot activate; another principal can.
	_, err = f.do(t, bg1, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Activate(ctx, tx, c, "me")
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	g, err = f.do(t, bg2, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Activate(ctx, tx, c, "re-activate")
	})
	require.NoError(t, err)
	assert.Equal(t, StateActive, g.State)
	assert.True(t, f.verdict(t, c).Active)

	// Revoke ends the version; a new proposal starts the next one.
	g, err = f.do(t, bg1, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Revoke(ctx, tx, c, "contract terminated")
	})
	require.NoError(t, err)
	assert.Equal(t, StateRevoked, g.State)
	require.NotNil(t, g.RevokedAt)
	assert.Equal(t, "contract terminated", g.RevokeReason)
	assert.False(t, f.verdict(t, c).Active)
	_, err = f.do(t, bg1, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Revoke(ctx, tx, c, "twice")
	})
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	version := g.ApprovalVersion
	g, err = f.do(t, proposer, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, c, highRiskProposal("new contract"))
	})
	require.NoError(t, err)
	assert.Equal(t, version+1, g.ApprovalVersion)
	assert.Len(t, g.Approvers, 1, "chain resets per approval version")
	assert.Nil(t, g.RevokedAt)
	assert.Nil(t, g.EffectiveAt)

	all := transitionsSince(t, g.ID, baseline)
	assert.Len(t, all, 8, "propose, approve, activate, suspend, resume, activate, revoke, propose")
	assert.Equal(t, base+8, f.audit.count(), "one audit event per transition")
}

func TestIntegration_Expiry(t *testing.T) {
	f := newFixture(t, "TEST")
	const c = LiveManualTrading
	f.reset(t, c)
	baseline := transitionsOf(t, f.get(t, c).ID)
	proposer := f.op("risk-alice", security.RoleRisk)
	bg1, bg2 := f.bg("bg-bob"), f.bg("bg-carol")

	p := highRiskProposal("time-boxed pilot")
	p.ExpiresAt = f.clk.Now().Add(time.Hour)
	_, err := f.do(t, proposer, func(ctx context.Context, tx pgx.Tx) (Gate, error) { return f.admin.Propose(ctx, tx, c, p) })
	require.NoError(t, err)
	_, err = f.do(t, bg1, func(ctx context.Context, tx pgx.Tx) (Gate, error) { return f.admin.Approve(ctx, tx, c, "ok") })
	require.NoError(t, err)
	g, err := f.do(t, bg2, func(ctx context.Context, tx pgx.Tx) (Gate, error) { return f.admin.Activate(ctx, tx, c, "ok") })
	require.NoError(t, err)
	assert.True(t, f.verdict(t, c).Active)

	// The checker fails closed the instant the window ends, before any
	// worker runs.
	f.clk.Advance(time.Hour)
	v := f.verdict(t, c)
	assert.False(t, v.Active)
	assert.Equal(t, ReasonExpired, v.Reason)
	assert.Equal(t, StateActive, v.State)

	// ExpireDue persists the fact with a SYSTEM transition. Other gates a
	// previous run left with a past expires_at are swept too, so look for
	// ours rather than for a count.
	expiredCaps := func() map[Capability]GateState {
		var expired []Gate
		require.NoError(t, f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			expired, err = f.admin.ExpireDue(ctx, tx, f.clk.Now())
			return err
		}))
		out := map[Capability]GateState{}
		for _, g := range expired {
			out[g.Capability] = g.State
		}
		return out
	}
	swept := expiredCaps()
	assert.Equal(t, StateExpired, swept[c])
	assert.Equal(t, StateExpired, f.get(t, c).State)
	assert.Equal(t, ReasonStateNotActive, f.verdict(t, c).Reason)
	ts := transitionsSince(t, g.ID, baseline)
	require.Len(t, ts, 4, "propose, approve, activate, expire")
	last := ts[len(ts)-1]
	assert.Equal(t, StateExpired, last.To)
	assert.Equal(t, security.ActorSystem, last.ActorType)
	assert.Equal(t, "gates.expire_due", last.ActorID)

	// Idempotent.
	_, again := expiredCaps()[c]
	assert.False(t, again)

	// EXPIRED → PENDING_APPROVAL with a new version.
	//
	// A freshly authenticated proposer, not the one from an hour ago: Propose
	// demands a recent step-up like every other step of the ceremony (F-99),
	// and the clock has moved an hour since `proposer` authenticated.
	renewer := f.op("risk-alice", security.RoleRisk)
	g2, err := f.do(t, renewer, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, c, highRiskProposal("renewal"))
	})
	require.NoError(t, err)
	assert.Equal(t, g.ApprovalVersion+1, g2.ApprovalVersion)
	assert.Equal(t, StatePendingApproval, g2.State)
}

func TestIntegration_ActivateRefusesExpiredWindow(t *testing.T) {
	f := newFixture(t, "TEST")
	const c = Withdrawals
	f.reset(t, c)
	p := highRiskProposal("short window")
	p.ExpiresAt = f.clk.Now().Add(30 * time.Minute)
	_, err := f.do(t, f.op("risk-alice", security.RoleRisk), func(ctx context.Context, tx pgx.Tx) (Gate, error) { return f.admin.Propose(ctx, tx, c, p) })
	require.NoError(t, err)
	_, err = f.do(t, f.bg("bg-bob"), func(ctx context.Context, tx pgx.Tx) (Gate, error) { return f.admin.Approve(ctx, tx, c, "ok") })
	require.NoError(t, err)
	f.clk.Advance(time.Hour)
	carol := f.bg("bg-carol")
	carol.AuthTime = f.clk.Now()
	until := f.clk.Now().Add(time.Hour)
	carol.BreakGlassUntil = &until
	_, err = f.do(t, carol, func(ctx context.Context, tx pgx.Tx) (Gate, error) { return f.admin.Activate(ctx, tx, c, "late") })
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
	assert.Equal(t, StateApproved, f.get(t, c).State)
}

func TestIntegration_AuditFailureRollsBackTransition(t *testing.T) {
	f := newFixture(t, "TEST")
	const c = Marketplace
	f.reset(t, c)
	before := f.get(t, c)
	f.audit.fail = fmt.Errorf("audit store unavailable")
	_, err := f.do(t, f.op("compliance-eve", security.RoleCompliance), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, c, Proposal{Reason: "marketplace beta"})
	})
	require.Error(t, err)
	after := f.get(t, c)
	assert.Equal(t, before.State, after.State)
	assert.Equal(t, before.ApprovalVersion, after.ApprovalVersion)
	assert.Equal(t, before.Version, after.Version)
	ts, err := Transitions(context.Background(), testDB, before.ID)
	require.NoError(t, err)
	for _, tr := range ts {
		assert.NotEqual(t, "marketplace beta", tr.Reason)
	}
}

func TestIntegration_ProposeCreatesMissingRow(t *testing.T) {
	requireEnv(t)
	// DEV is never bootstrapped by these tests; Propose persists the
	// implicit DISABLED row before transitioning it.
	f := &fixture{env: "DEV", clk: newClock(), audit: &syncAudit{}}
	var err error
	f.admin, err = NewAdmin("DEV", f.clk, f.audit)
	require.NoError(t, err)
	f.reset(t, CrossChain)
	g, err := f.do(t, f.op("risk-alice", security.RoleRisk), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, CrossChain, highRiskProposal("cctp pilot"))
	})
	require.NoError(t, err)
	assert.Equal(t, StatePendingApproval, g.State)
	ts, err := Transitions(context.Background(), testDB, g.ID)
	require.NoError(t, err)
	require.NotEmpty(t, ts)
	assert.Equal(t, StateDisabled, ts[0].From)
	// An agent principal is refused even though the row now exists.
	_, err = f.do(t, security.AgentPrincipal("agent-1", "acct-1"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Suspend(ctx, tx, CrossChain, "agent says stop")
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}

// forgedChain is an approval chain that satisfies every check in Evaluate: a
// proposer and two distinct approvers, none of whom is the proposer.
const forgedChain = `[{"user_id":"mallory","step":"PROPOSE"},` +
	`{"user_id":"accomplice-1","step":"APPROVE"},{"user_id":"accomplice-2","step":"ACTIVATE"}]`

// execAsApp runs sql on the application pool (role cp_app), bypassing this
// package's repository functions, and returns the error verbatim.
func execAsApp(t *testing.T, sql string, args ...any) error {
	t.Helper()
	_, err := testDB.Exec(context.Background(), sql, args...)
	return err
}

// callGateTransition invokes cp_gate_transition as cp_app with a forged
// request. Only the fields the forgeries vary are parameters; the rest are
// the well-formed values a real call would carry.
func callGateTransition(t *testing.T, gateID GateID, version int64, op, actorID, approver string) error {
	t.Helper()
	_, err := testDB.Exec(context.Background(), `SELECT * FROM cp_gate_transition(
		$1::uuid, $2::bigint, $3::text, 'OPERATOR'::text, $4::text, 'forged'::text, $5::uuid,
		NULL::bytea, now()::timestamptz, $6::jsonb,
		NULL::text, NULL::text, NULL::text, NULL::text, NULL::jsonb, NULL::timestamptz, NULL::timestamptz)`,
		gateID, version, op, actorID, NewTransitionID(), approver)
	return err
}

// TestIntegration_DatabaseRefusesForgedActivation is the adversarial control
// for migration 00701. Every case here is what an attacker holding the
// application's database credential would try: raw SQL issued as cp_app,
// bypassing internal/gates entirely. Before 00701, cases 3 and 4 succeeded and
// a live-money capability came out ACTIVE with one actor and no approval,
// because gates.Evaluate re-derives all five conditions from the same row the
// forgery had just written.
//
// Every case must be refused by the database, and after every case the
// capability must still evaluate inactive with deployment configuration fully
// permissive (the fixture's checker enables everything, so only conditions 2-5
// are doing the work).
func TestIntegration_DatabaseRefusesForgedActivation(t *testing.T) {
	f := newFixture(t, "LOCAL")
	const c = CEXTrading // used by no other test, in an environment no other test writes
	ctx := context.Background()
	f.reset(t, c)
	g := f.get(t, c)
	require.NotEqual(t, StateActive, g.State, "fixture must not start ACTIVE")

	stillInactive := func(what string) {
		t.Helper()
		row := f.get(t, c)
		assert.NotEqual(t, StateActive, row.State, "%s: row reached ACTIVE", what)
		// Evaluate the stored row directly, with deployment configuration
		// enabled and a clock past any effective_at a forgery could have
		// written, so the verdict turns on conditions 2-5 and not on the
		// fixture's fake clock happening to sit before the forged instant.
		v := Evaluate(&row, true, time.Now().UTC().Add(time.Minute))
		assert.False(t, v.Active, "%s: the forged row evaluates ACTIVE (%+v)", what, v)
		assert.False(t, f.verdict(t, c).Active, "%s: the checker reports ACTIVE", what)
	}

	// 1. cp_app holds no UPDATE privilege on any column that decides anything.
	//    Only `version` is granted, and only so SELECT ... FOR UPDATE can take
	//    a row lock; it appears in none of the five conditions.
	for _, col := range []string{
		"state", "approval_version", "approvers", "proposed_by_user_id", "evidence_hashes",
		"legal_review_ref", "provider_contract_ref", "risk_approval_ref", "security_approval_ref",
		"effective_at", "expires_at", "revoked_at", "revoke_reason",
	} {
		var granted bool
		require.NoError(t, testDB.QueryRow(ctx,
			`SELECT has_column_privilege(current_user, 'capability_gates', $1, 'UPDATE')`, col).Scan(&granted))
		assert.False(t, granted, "cp_app must not be able to UPDATE capability_gates.%s", col)
	}
	var canDelete, canInsertHistory, isMigrate bool
	require.NoError(t, testDB.QueryRow(ctx, `SELECT
		has_table_privilege(current_user, 'capability_gates', 'DELETE'),
		has_table_privilege(current_user, 'capability_gate_transitions', 'INSERT'),
		pg_has_role(current_user, 'cp_migrate', 'USAGE')`).Scan(&canDelete, &canInsertHistory, &isMigrate))
	assert.False(t, canDelete, "cp_app must not DELETE a gate row")
	assert.False(t, canInsertHistory, "cp_app must not write gate history directly")
	assert.False(t, isMigrate, "cp_app must not be able to become the owner of cp_gate_transition")

	// 2. The bare rewrite of the row every condition is read from. Before
	//    00701 this was refused only by 00603's AU001 binding, which case 3
	//    then satisfied; now the privilege is simply absent.
	err := execAsApp(t, `UPDATE capability_gates SET state = 'ACTIVE', proposed_by_user_id = 'mallory',
		approvers = $2::jsonb, legal_review_ref = 'L', provider_contract_ref = 'P',
		risk_approval_ref = 'R', security_approval_ref = 'S', effective_at = now(), expires_at = NULL, revoked_at = NULL
		WHERE id = $1`, g.ID, forgedChain)
	assert.Error(t, err, "raw UPDATE of capability_gates must be refused")
	assert.Equal(t, db.SQLStateInsufficientPrivilege, db.SQLState(err), "got %v", err)
	t.Logf("raw UPDATE refused: %v", err)
	stillInactive("raw UPDATE")

	// 3. THE DEFECT: the same UPDATE paired with its own transition row in one
	//    transaction, which is what satisfied AU001. cp_app no longer holds
	//    INSERT on the immutable history table, so it cannot write its own
	//    permission slip.
	err = testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO capability_gate_transitions
			(id, gate_id, from_state, to_state, actor_type, actor_id, reason)
			VALUES ($1, $2, $3, 'ACTIVE', 'OPERATOR', 'mallory', 'self-signed')`,
			NewTransitionID(), g.ID, string(g.State)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE capability_gates SET state = 'ACTIVE', proposed_by_user_id = 'mallory',
			approvers = $2::jsonb, legal_review_ref = 'L', provider_contract_ref = 'P',
			risk_approval_ref = 'R', security_approval_ref = 'S', effective_at = now()
			WHERE id = $1`, g.ID, forgedChain)
		return err
	})
	assert.Error(t, err, "self-signed transition + UPDATE must be refused")
	assert.Equal(t, db.SQLStateInsufficientPrivilege, db.SQLState(err), "got %v", err)
	t.Logf("self-signed transition + UPDATE refused: %v", err)
	stillInactive("self-signed transition + UPDATE")

	// 4. THE DEFECT, second path: a fresh deployment has no gate rows at all
	//    (gates.Bootstrap has no callers), so UNIQUE (capability, environment)
	//    was no obstacle and a single INSERT could create a gate already
	//    ACTIVE. PART 244 is now a database fact: a gate is born DISABLED.
	err = execAsApp(t, `INSERT INTO capability_gates (id, capability, environment, state, approval_version,
		legal_review_ref, provider_contract_ref, risk_approval_ref, security_approval_ref,
		proposed_by_user_id, approvers, evidence_hashes, effective_at)
		VALUES ($1, 'SECURITIES', 'PROD', 'ACTIVE', 1, 'L', 'P', 'R', 'S', 'mallory', $2::jsonb, '[]'::jsonb, now())`,
		NewGateID(), forgedChain)
	assert.Error(t, err, "a gate row born ACTIVE must be refused")
	assert.Equal(t, "GT005", db.SQLState(err), "got %v", err)
	t.Logf("INSERT of a gate born ACTIVE refused: %v", err)
	var born int
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT count(*) FROM capability_gates WHERE capability = 'SECURITIES' AND environment = 'PROD'`).Scan(&born))
	assert.Zero(t, born, "no SECURITIES/PROD row may exist")

	// 5. The gate authority itself cannot be talked into a transition the
	//    state machine forbids: DISABLED does not reach ACTIVE, whatever the
	//    caller claims about approvals.
	err = callGateTransition(t, g.ID, g.Version, opActivate, "mallory", `{"user_id":"mallory","step":"ACTIVATE"}`)
	assert.Error(t, err)
	assert.Equal(t, "GT002", db.SQLState(err), "got %v", err)
	t.Logf("cp_gate_transition(activate) from %s refused: %v", g.State, err)
	stillInactive("direct activate from " + string(g.State))

	// 6. Dual control is the database's rule too, not only the Go layer's.
	//    Drive the gate legitimately to APPROVED, then try to close the last
	//    step with a principal that is not entitled to.
	proposer, bob := f.op("risk-alice", security.RoleRisk), f.bg("bg-bob")
	_, err = f.do(t, proposer, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, c, highRiskProposal("cex pilot"))
	})
	require.NoError(t, err)
	_, err = f.do(t, bob, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Approve(ctx, tx, c, "reviewed")
	})
	require.NoError(t, err)
	g = f.get(t, c)
	require.Equal(t, StateApproved, g.State)

	// The approver activating their own approval.
	err = callGateTransition(t, g.ID, g.Version, opActivate, "bg-bob", `{"user_id":"bg-bob","step":"ACTIVATE"}`)
	assert.Error(t, err)
	assert.Equal(t, "GT004", db.SQLState(err), "got %v", err)
	t.Logf("cp_gate_transition(activate) by the approver refused: %v", err)
	stillInactive("approver self-activation")

	// The proposer activating their own proposal.
	err = callGateTransition(t, g.ID, g.Version, opActivate, "risk-alice", `{"user_id":"risk-alice","step":"ACTIVATE"}`)
	assert.Error(t, err)
	assert.Equal(t, "GT004", db.SQLState(err), "got %v", err)
	stillInactive("proposer self-activation")

	// A chain entry naming someone other than the acting principal, so one
	// actor could pose as two: the entry must be the actor's own.
	err = callGateTransition(t, g.ID, g.Version, opActivate, "bg-bob", `{"user_id":"bg-carol","step":"ACTIVATE"}`)
	assert.Error(t, err)
	assert.Equal(t, "GT001", db.SQLState(err), "got %v", err)
	t.Logf("cp_gate_transition with a chain entry naming another principal refused: %v", err)
	stillInactive("impersonated chain entry")

	// 7. The approved path still works: the control refuses forgeries, not
	//    legitimate activation by a distinct third principal.
	f.clk.Advance(time.Minute)
	got, err := f.do(t, f.bg("bg-carol"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Activate(ctx, tx, c, "second approval")
	})
	require.NoError(t, err)
	assert.Equal(t, StateActive, got.State)
	assert.True(t, f.verdict(t, c).Active)

	// 8. An ACTIVE gate's validity window cannot be widened behind the state
	//    machine's back either.
	err = execAsApp(t, `UPDATE capability_gates SET expires_at = now() + interval '100 years' WHERE id = $1`, got.ID)
	assert.Error(t, err)
	assert.Equal(t, db.SQLStateInsufficientPrivilege, db.SQLState(err), "got %v", err)

	// Leave the fixture where the next run expects it.
	_, err = f.do(t, f.bg("bg-bob"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Revoke(ctx, tx, c, "end of adversarial control")
	})
	require.NoError(t, err)
	assert.False(t, f.verdict(t, c).Active)
}
