package killswitch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func operator(sub string, roles ...security.Role) security.Principal {
	return security.Principal{SubjectID: sub, ActorType: security.ActorOperator, Roles: roles, AuthTime: t0, AMR: []string{"mfa"}}
}

func breakGlass(sub string) security.Principal {
	until := t0.Add(time.Hour)
	p := operator(sub, security.RoleOperations, security.RoleBreakGlass)
	p.BreakGlassUntil = &until
	return p
}

func ctxWith(p security.Principal) context.Context {
	return security.WithPrincipal(context.Background(), p)
}

type fakeAudit struct{ events []AuditEvent }

func (f *fakeAudit) Append(_ context.Context, _ pgx.Tx, e AuditEvent) error {
	f.events = append(f.events, e)
	return nil
}

type fakeVerifier struct {
	approvals map[string]Approval
	err       error
	calls     int
}

func (f *fakeVerifier) VerifyApproved(_ context.Context, _ db.Querier, approvalID, kind, targetID string) (Approval, error) {
	f.calls++
	if f.err != nil {
		return Approval{}, f.err
	}
	ap, ok := f.approvals[approvalID]
	if !ok {
		return Approval{}, errs.New(errs.CodeNotFound, "admin action not found")
	}
	if ap.Kind != kind || ap.TargetID != targetID {
		return Approval{}, errs.New(errs.CodeForbidden, "admin action does not match")
	}
	return ap, nil
}

func newTestController(t *testing.T) (*Controller, *clock.Fake, *fakeAudit, *fakeVerifier) {
	t.Helper()
	clk := clock.NewFake(t0)
	audit := &fakeAudit{}
	ver := &fakeVerifier{approvals: map[string]Approval{}}
	c, err := NewController(clk, audit, ver)
	require.NoError(t, err)
	return c, clk, audit, ver
}

func active(kind Kind, scope string) Switch {
	return Switch{Kind: kind, ScopeID: scope, Active: true, Severity: kind.Severity()}
}

// --- kinds, scopes, classes ---------------------------------------------------

func TestKinds_SeverityAndScopeRules(t *testing.T) {
	kinds := AllKinds()
	require.Len(t, kinds, 12)
	severe := map[Kind]bool{GlobalNewRiskKill: true, ChainDisableNewActions: true, ProviderDisableNewActions: true, FundingDisable: true, WithdrawalsDisable: true}
	for _, k := range kinds {
		assert.True(t, k.Valid())
		if severe[k] {
			assert.Equalf(t, SeveritySevere, k.Severity(), "%s", k)
		} else {
			assert.Equalf(t, SeverityStandard, k.Severity(), "%s", k)
		}
	}
	assert.False(t, Kind("EVERYTHING_OFF").Valid())
	assert.Equal(t, SeveritySevere, Kind("EVERYTHING_OFF").Severity(), "unknown kinds are severe (fail closed)")

	// Global-only kinds.
	for _, k := range []Kind{GlobalNewRiskKill, FundingDisable, WithdrawalsDisable} {
		s, err := k.NormalizeScope("")
		require.NoError(t, err)
		assert.Equal(t, GlobalScope, s)
		s, err = k.NormalizeScope(" * ")
		require.NoError(t, err)
		assert.Equal(t, GlobalScope, s)
		_, err = k.NormalizeScope("acct-1")
		assert.Equalf(t, errs.CodeValidationFailed, errs.CodeOf(err), "%s must be global", k)
	}
	// Id-only kinds.
	for _, k := range []Kind{AccountFreeze, AgentPause, StrategyVersionDisable, VenueDisable, InstrumentCloseOnly, InstrumentHalt, ChainDisableNewActions, ProviderDisableNewActions} {
		s, err := k.NormalizeScope(" x-1 ")
		require.NoError(t, err)
		assert.Equal(t, "x-1", s)
		_, err = k.NormalizeScope("*")
		assert.Equalf(t, errs.CodeValidationFailed, errs.CodeOf(err), "%s needs an id", k)
		_, err = k.NormalizeScope("")
		assert.Equalf(t, errs.CodeValidationFailed, errs.CodeOf(err), "%s needs an id", k)
	}
	// MODEL_DISABLE takes either.
	s, err := ModelDisable.NormalizeScope("")
	require.NoError(t, err)
	assert.Equal(t, GlobalScope, s)
	s, err = ModelDisable.NormalizeScope("model-7")
	require.NoError(t, err)
	assert.Equal(t, "model-7", s)
	// Junk.
	_, err = AccountFreeze.NormalizeScope("a b")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = AccountFreeze.NormalizeScope("a\x00b")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = AccountFreeze.NormalizeScope(string(make([]byte, maxScopeLen+1)))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = Kind("NOPE").NormalizeScope("*")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestActionClasses(t *testing.T) {
	classes := AllActionClasses()
	require.Len(t, classes, 8)
	never := map[ActionClass]bool{Observe: true, Settle: true, Reconcile: true, LedgerPost: true, Cancel: true}
	for _, c := range classes {
		assert.True(t, c.Valid())
		assert.Equalf(t, never[c], c.NeverBlocked(), "%s", c)
	}
	assert.False(t, ActionClass("YOLO").Valid())
	assert.False(t, ActionClass("YOLO").NeverBlocked())
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(Action{Class: "YOLO"}.Validate()))
	assert.NoError(t, Action{Class: NewRisk}.Validate())
}

// --- blocking matrix ------------------------------------------------------------

// trade is a fully-dimensioned manual trade action; every id-scoped switch
// with a matching scope applies to it.
func trade(class ActionClass) Action {
	return Action{
		Class: class, AccountID: "acct", AgentID: "agent", StrategyVersionID: "sv", Venue: "jupiter",
		InstrumentID: "SOL-USDC", Chain: "solana", Provider: "helius", ModelID: "model",
	}
}

func TestMatrix_NewRisk(t *testing.T) {
	a := trade(NewRisk)
	matching := map[Kind]string{
		GlobalNewRiskKill: "*", AccountFreeze: "acct", AgentPause: "agent", StrategyVersionDisable: "sv", VenueDisable: "jupiter",
		InstrumentCloseOnly: "SOL-USDC", InstrumentHalt: "SOL-USDC", ChainDisableNewActions: "solana", ProviderDisableNewActions: "helius",
		FundingDisable: "*", WithdrawalsDisable: "*", ModelDisable: "model",
	}
	for _, k := range AllKinds() {
		sw := active(k, matching[k])
		want := true
		switch k {
		case WithdrawalsDisable:
			want = false // withdrawals are not new risk
		case FundingDisable:
			want = false // only funding sessions
		}
		assert.Equalf(t, want, Blocks(sw, a, Policy{}), "NEW_RISK vs matching %s", k)
		// Inactive rows never block.
		sw.Active = false
		assert.Falsef(t, Blocks(sw, a, Policy{}), "inactive %s", k)
	}
	// Non-matching scopes never block.
	for _, k := range []Kind{AccountFreeze, AgentPause, StrategyVersionDisable, VenueDisable, InstrumentCloseOnly, InstrumentHalt, ChainDisableNewActions, ProviderDisableNewActions, ModelDisable} {
		assert.Falsef(t, Blocks(active(k, "other"), a, Policy{}), "non-matching %s", k)
	}
	// A funding session is blocked by FUNDING_DISABLE and by its provider's
	// switch, and a plain trade is not blocked by FUNDING_DISABLE.
	funding := Action{Class: NewRisk, AccountID: "acct", Provider: "stripe", Funding: true}
	assert.True(t, Blocks(active(FundingDisable, "*"), funding, Policy{}))
	assert.True(t, Blocks(active(ProviderDisableNewActions, "stripe"), funding, Policy{}))
	assert.True(t, Blocks(active(GlobalNewRiskKill, "*"), funding, Policy{}))
	assert.False(t, Blocks(active(FundingDisable, "*"), a, Policy{}))
	// MODEL_DISABLE(*) stops every model-driven action and no manual one.
	assert.True(t, Blocks(active(ModelDisable, "*"), a, Policy{}))
	manual := a
	manual.ModelID = ""
	assert.False(t, Blocks(active(ModelDisable, "*"), manual, Policy{}))
	assert.False(t, Blocks(active(ModelDisable, "model"), manual, Policy{}))
	// An action with no dimensions is only stopped by the global kill.
	bare := Action{Class: NewRisk}
	for _, k := range AllKinds() {
		want := k == GlobalNewRiskKill
		assert.Equalf(t, want, Blocks(active(k, matching[k]), bare, Policy{}), "bare NEW_RISK vs %s", k)
	}
}

func TestMatrix_ReduceRisk(t *testing.T) {
	a := trade(ReduceRisk)
	blockedBy := map[Kind]bool{InstrumentHalt: true, ChainDisableNewActions: true, ProviderDisableNewActions: true}
	matching := map[Kind]string{
		GlobalNewRiskKill: "*", AccountFreeze: "acct", AgentPause: "agent", StrategyVersionDisable: "sv", VenueDisable: "jupiter",
		InstrumentCloseOnly: "SOL-USDC", InstrumentHalt: "SOL-USDC", ChainDisableNewActions: "solana", ProviderDisableNewActions: "helius",
		FundingDisable: "*", WithdrawalsDisable: "*", ModelDisable: "model",
	}
	for _, k := range AllKinds() {
		assert.Equalf(t, blockedBy[k], Blocks(active(k, matching[k]), a, Policy{}), "REDUCE_RISK vs %s (default policy)", k)
	}
	// PART 52: the global kill must never stop risk reduction.
	assert.False(t, Blocks(active(GlobalNewRiskKill, "*"), a, Policy{}))
	assert.False(t, Blocks(active(InstrumentCloseOnly, "SOL-USDC"), a, Policy{}), "close-only allows closing")
	// ACCOUNT_FREEZE blocks reduction only when policy says so.
	strict := Policy{AccountFreezeBlocksRiskReduction: true}
	assert.True(t, Blocks(active(AccountFreeze, "acct"), a, strict))
	assert.False(t, Blocks(active(AccountFreeze, "other"), a, strict))
	for _, k := range AllKinds() {
		want := blockedBy[k] || k == AccountFreeze
		assert.Equalf(t, want, Blocks(active(k, matching[k]), a, strict), "REDUCE_RISK vs %s (strict policy)", k)
	}
	// Non-matching halts do not block.
	assert.False(t, Blocks(active(InstrumentHalt, "BTC-USDC"), a, Policy{}))
}

func TestMatrix_Withdraw(t *testing.T) {
	a := Action{Class: Withdraw, AccountID: "acct", Provider: "stripe", Chain: "solana", Funding: false}
	blockedBy := map[Kind]bool{WithdrawalsDisable: true, AccountFreeze: true, GlobalNewRiskKill: true}
	matching := map[Kind]string{
		GlobalNewRiskKill: "*", AccountFreeze: "acct", AgentPause: "agent", StrategyVersionDisable: "sv", VenueDisable: "jupiter",
		InstrumentCloseOnly: "SOL-USDC", InstrumentHalt: "SOL-USDC", ChainDisableNewActions: "solana", ProviderDisableNewActions: "stripe",
		FundingDisable: "*", WithdrawalsDisable: "*", ModelDisable: "*",
	}
	for _, k := range AllKinds() {
		assert.Equalf(t, blockedBy[k], Blocks(active(k, matching[k]), a, Policy{}), "WITHDRAW vs %s", k)
		assert.Equalf(t, blockedBy[k], Blocks(active(k, matching[k]), a, Policy{AccountFreezeBlocksRiskReduction: true}), "WITHDRAW vs %s (strict)", k)
	}
	assert.False(t, Blocks(active(AccountFreeze, "other"), a, Policy{}))
}

func TestBlocking_DeterministicFirstMatch(t *testing.T) {
	a := trade(NewRisk)
	sws := []Switch{active(VenueDisable, "jupiter"), active(GlobalNewRiskKill, "*"), active(AccountFreeze, "acct")}
	got, ok := Blocking(sws, a, Policy{})
	require.True(t, ok)
	assert.Equal(t, AccountFreeze, got.Kind, "sorted by kind: ACCOUNT_FREEZE < GLOBAL_NEW_RISK_KILL < VENUE_DISABLE")
	// Input order does not matter and the input is not mutated.
	rev := []Switch{sws[2], sws[1], sws[0]}
	got2, ok := Blocking(rev, a, Policy{})
	require.True(t, ok)
	assert.Equal(t, got, got2)
	assert.Equal(t, VenueDisable, sws[0].Kind)
	_, ok = Blocking(nil, a, Policy{})
	assert.False(t, ok)

	err := BlockedError(got, a)
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err))
	e, _ := errs.As(err)
	assert.Equal(t, "ACCOUNT_FREEZE", e.Fields["switch"])
	assert.Equal(t, "acct", e.Fields["scope"])
	assert.Equal(t, "NEW_RISK", e.Fields["action_class"])
}

func TestSnapshot_Helpers(t *testing.T) {
	s := Snapshot{Switches: []Switch{active(GlobalNewRiskKill, "*"), active(AccountFreeze, "acct")}}
	assert.True(t, s.Global())
	assert.True(t, s.Has(AccountFreeze, "acct"))
	assert.False(t, s.Has(AccountFreeze, "other"))
	_, blocked := s.Blocking(trade(NewRisk), Policy{})
	assert.True(t, blocked)
	_, blocked = s.Blocking(trade(Reconcile), Policy{})
	assert.False(t, blocked)
	assert.False(t, Snapshot{}.Global())
}

// --- property: never-blocked classes are never blocked ---------------------

var scopePool = []string{"*", "acct", "agent", "sv", "jupiter", "SOL-USDC", "solana", "helius", "model", "other"}

func genSwitches(t *rapid.T) []Switch {
	n := rapid.IntRange(0, 24).Draw(t, "n")
	out := make([]Switch, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Switch{
			Kind:    rapid.SampledFrom(AllKinds()).Draw(t, "kind"),
			ScopeID: rapid.SampledFrom(scopePool).Draw(t, "scope"),
			Active:  rapid.Bool().Draw(t, "active"),
		})
	}
	return out
}

func genAction(t *rapid.T, classes []ActionClass) Action {
	pick := func(label string) string {
		return rapid.SampledFrom(append([]string{""}, scopePool...)).Draw(t, label)
	}
	return Action{
		Class: rapid.SampledFrom(classes).Draw(t, "class"), AccountID: pick("account"), AgentID: pick("agent"),
		StrategyVersionID: pick("sv"), Venue: pick("venue"), InstrumentID: pick("instrument"), Chain: pick("chain"),
		Provider: pick("provider"), ModelID: pick("model"), Funding: rapid.Bool().Draw(t, "funding"),
	}
}

func genPolicy(t *rapid.T) Policy {
	return Policy{AccountFreezeBlocksRiskReduction: rapid.Bool().Draw(t, "freeze_blocks_reduce")}
}

// TestProp_NeverBlockedClasses proves PART 52 for every combination of
// active switches: OBSERVE, SETTLE, RECONCILE, LEDGER_POST and CANCEL are
// never blocked, and Check for them never even needs a database.
func TestProp_NeverBlockedClasses(t *testing.T) {
	never := []ActionClass{Observe, Settle, Reconcile, LedgerPost, Cancel}
	checker := NewChecker(Policy{})
	rapid.Check(t, func(t *rapid.T) {
		sws := genSwitches(t)
		a := genAction(t, never)
		p := genPolicy(t)
		if _, blocked := Blocking(sws, a, p); blocked {
			t.Fatalf("%s blocked by %+v", a.Class, sws)
		}
		for _, sw := range sws {
			if Blocks(sw, a, p) {
				t.Fatalf("%s blocked by %+v", a.Class, sw)
			}
		}
		// Querier is nil: any query would panic.
		if err := checker.Check(context.Background(), nil, a); err != nil {
			t.Fatalf("Check(%s) = %v", a.Class, err)
		}
	})
}

// TestProp_EveryActiveSwitchCombination also enumerates all 2^12 subsets of
// kinds (each with every scope in the pool matching the fully-dimensioned
// action) exhaustively rather than randomly.
func TestProp_EveryActiveSwitchCombination(t *testing.T) {
	kinds := AllKinds()
	matching := map[Kind]string{
		GlobalNewRiskKill: "*", AccountFreeze: "acct", AgentPause: "agent", StrategyVersionDisable: "sv", VenueDisable: "jupiter",
		InstrumentCloseOnly: "SOL-USDC", InstrumentHalt: "SOL-USDC", ChainDisableNewActions: "solana", ProviderDisableNewActions: "helius",
		FundingDisable: "*", WithdrawalsDisable: "*", ModelDisable: "*",
	}
	for mask := 0; mask < 1<<len(kinds); mask++ {
		var sws []Switch
		for i, k := range kinds {
			if mask&(1<<i) != 0 {
				sws = append(sws, active(k, matching[k]))
			}
		}
		for _, class := range []ActionClass{Observe, Settle, Reconcile, LedgerPost, Cancel} {
			a := trade(class)
			a.Funding = true
			for _, p := range []Policy{{}, {AccountFreezeBlocksRiskReduction: true}} {
				if _, blocked := Blocking(sws, a, p); blocked {
					t.Fatalf("mask %012b blocks %s", mask, class)
				}
			}
		}
		// With everything active, NEW_RISK is blocked; that is the point.
		if mask == 1<<len(kinds)-1 {
			_, blocked := Blocking(sws, trade(NewRisk), Policy{})
			assert.True(t, blocked)
		}
	}
}

// TestProp_InactiveNeverBlocks: an inactive row never blocks any class.
func TestProp_InactiveNeverBlocks(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sws := genSwitches(t)
		for i := range sws {
			sws[i].Active = false
		}
		a := genAction(t, AllActionClasses())
		if _, blocked := Blocking(sws, a, genPolicy(t)); blocked {
			t.Fatalf("inactive switch blocked %+v", a)
		}
	})
}

// TestProp_GlobalKillNeverStopsRiskReduction: PART 52's headline example.
func TestProp_GlobalKillNeverStopsRiskReduction(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := genAction(t, []ActionClass{ReduceRisk})
		sws := []Switch{
			active(GlobalNewRiskKill, "*"), active(FundingDisable, "*"), active(WithdrawalsDisable, "*"),
			active(VenueDisable, a.Venue), active(AgentPause, a.AgentID), active(AccountFreeze, a.AccountID),
			active(StrategyVersionDisable, a.StrategyVersionID), active(InstrumentCloseOnly, a.InstrumentID), active(ModelDisable, "*"),
		}
		if _, blocked := Blocking(sws, a, Policy{}); blocked {
			t.Fatalf("risk reduction blocked: %+v", a)
		}
	})
}

// --- controller authorization runs before any query ------------------------

func TestController_AgentAndServiceRejectedBeforeAnyQuery(t *testing.T) {
	c, _, audit, ver := newTestController(t)
	agent := security.AgentPrincipal("agent-1", "acct-1")
	svc := security.Principal{SubjectID: "svc", ActorType: security.ActorService, Roles: []security.Role{security.RoleAdmin}, AuthTime: t0, AMR: []string{"mfa"}}
	rogue := security.Principal{SubjectID: "agent-2", ActorType: security.ActorAgent, Roles: []security.Role{security.RoleOperations}, AccountIDs: []string{"a"}}
	apID := "0199260c-0000-7000-8000-000000000001"
	for name, p := range map[string]security.Principal{"agent": agent, "service": svc, "agent-with-roles": rogue} {
		// tx is nil: a query would panic.
		_, err := c.Activate(ctxWith(p), nil, GlobalNewRiskKill, "*", "emergency")
		assert.Equalf(t, errs.CodeForbidden, errs.CodeOf(err), "%s activate", name)
		_, err = c.Release(ctxWith(p), nil, AccountFreeze, "acct", "done", &apID)
		assert.Equalf(t, errs.CodeForbidden, errs.CodeOf(err), "%s release", name)
	}
	_, err := c.Activate(context.Background(), nil, GlobalNewRiskKill, "*", "emergency")
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
	_, err = c.Release(context.Background(), nil, GlobalNewRiskKill, "*", "done", &apID)
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
	assert.Empty(t, audit.events)
	assert.Zero(t, ver.calls)
}

func TestController_PermissionsBeforeAnyQuery(t *testing.T) {
	c, clk, _, ver := newTestController(t)
	// Customers and read-only support cannot activate; OPERATIONS, RISK,
	// SECURITY and ADMIN can (the query then happens, so only the negative
	// cases run with a nil tx).
	customer := security.Principal{SubjectID: "cust", ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{"a"}}
	_, err := c.Activate(ctxWith(customer), nil, AccountFreeze, "a", "freeze me")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	_, err = c.Activate(ctxWith(operator("ro", security.RoleSupportReadOnly)), nil, GlobalNewRiskKill, "*", "x")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	// Validation still precedes the query.
	_, err = c.Activate(ctxWith(operator("ops", security.RoleOperations)), nil, AccountFreeze, "*", "x")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "id-scoped kind with global scope")
	_, err = c.Activate(ctxWith(operator("ops", security.RoleOperations)), nil, GlobalNewRiskKill, "*", "   ")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "reason required")
	_, err = c.Activate(ctxWith(operator("ops", security.RoleOperations)), nil, "BOGUS", "*", "x")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// Release: kill:release is dual-control, so OPERATIONS/ADMIN lack it.
	for _, r := range []security.Role{security.RoleOperations, security.RoleAdmin, security.RoleSecurity, security.RoleRisk} {
		_, err = c.Release(ctxWith(operator("op", r)), nil, AccountFreeze, "a", "done", nil)
		assert.Equalf(t, errs.CodeForbidden, errs.CodeOf(err), "%s", r)
	}
	// Break-glass without a recent strong authentication: STEP_UP_REQUIRED.
	stale := breakGlass("bg")
	stale.AuthTime = t0.Add(-StepUpMaxAge - time.Second)
	_, err = c.Release(ctxWith(stale), nil, AccountFreeze, "a", "done", nil)
	assert.Equal(t, errs.CodeStepUpRequired, errs.CodeOf(err))
	weak := breakGlass("bg")
	weak.AMR = []string{"pwd"}
	_, err = c.Release(ctxWith(weak), nil, AccountFreeze, "a", "done", nil)
	assert.Equal(t, errs.CodeStepUpRequired, errs.CodeOf(err))
	// SEVERE without an approval is refused before any query.
	for _, k := range []Kind{GlobalNewRiskKill, ChainDisableNewActions, ProviderDisableNewActions, FundingDisable, WithdrawalsDisable} {
		scope := "*"
		if k.scopeRule() == scopeIDOnly {
			scope = "x"
		}
		_, err = c.Release(ctxWith(breakGlass("bg")), nil, k, scope, "done", nil)
		assert.Equalf(t, errs.CodeForbidden, errs.CodeOf(err), "%s", k)
		empty := "  "
		_, err = c.Release(ctxWith(breakGlass("bg")), nil, k, scope, "done", &empty)
		assert.Equalf(t, errs.CodeForbidden, errs.CodeOf(err), "%s blank approval", k)
	}
	assert.Zero(t, ver.calls)
	// A malformed approval id never reaches the verifier.
	bad := "not-a-uuid"
	_, err = c.Release(ctxWith(breakGlass("bg")), nil, GlobalNewRiskKill, "*", "done", &bad)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Zero(t, ver.calls)
	// Expired break-glass elevation loses kill:release.
	clk.Advance(2 * time.Hour)
	_, err = c.Release(ctxWith(breakGlass("bg")), nil, AccountFreeze, "a", "done", nil)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}

func TestController_ApprovalVerificationRules(t *testing.T) {
	c, _, _, ver := newTestController(t)
	const apID = "0199260c-0000-7000-8000-000000000001"
	good := Approval{ID: apID, Kind: ApprovalKindRelease, TargetID: ReleaseTargetID(GlobalNewRiskKill, "*"), ProposedBy: "alice", ApprovedBy: "bob", ApprovedAt: t0, ExpiresAt: t0.Add(time.Hour)}
	cases := map[string]Approval{
		"wrong kind":     func() Approval { a := good; a.Kind = "LEDGER_CORRECTION"; return a }(),
		"wrong target":   func() Approval { a := good; a.TargetID = ReleaseTargetID(FundingDisable, "*"); return a }(),
		"self approved":  func() Approval { a := good; a.ApprovedBy = "alice"; return a }(),
		"no approver":    func() Approval { a := good; a.ApprovedBy = ""; return a }(),
		"no proposer":    func() Approval { a := good; a.ProposedBy = ""; return a }(),
		"expired":        func() Approval { a := good; a.ExpiresAt = t0; return a }(),
		"id mismatch":    func() Approval { a := good; a.ID = "0199260c-0000-7000-8000-000000000002"; return a }(),
		"not on record":  {},
		"verifier error": {},
	}
	for name, ap := range cases {
		t.Run(name, func(t *testing.T) {
			ver.approvals = map[string]Approval{}
			ver.err = nil
			switch name {
			case "not on record":
			case "verifier error":
				ver.err = errors.New("db down")
			default:
				ver.approvals[apID] = ap
			}
			err := c.verifyApproval(context.Background(), nil, apID, GlobalNewRiskKill, "*", t0)
			require.Error(t, err)
			code := errs.CodeOf(err)
			if name == "not on record" {
				assert.Equal(t, errs.CodeNotFound, code, "verifier's own coded error passes through")
			} else {
				assert.Equal(t, errs.CodeForbidden, code)
			}
		})
	}
	ver.err = nil
	ver.approvals = map[string]Approval{apID: good}
	assert.NoError(t, c.verifyApproval(context.Background(), nil, apID, GlobalNewRiskKill, "*", t0))
	// A verifier that mis-reports kind/target for an approval it holds is
	// caught by the re-check even though it returned no error.
	ver.approvals[apID] = func() Approval { a := good; a.TargetID = "GLOBAL_NEW_RISK_KILL:*"; return a }()
	assert.NoError(t, c.verifyApproval(context.Background(), nil, apID, GlobalNewRiskKill, "*", t0))
	assert.Equal(t, "GLOBAL_NEW_RISK_KILL:*", ReleaseTargetID(GlobalNewRiskKill, "*"))
	assert.Equal(t, "ACCOUNT_FREEZE:acct", ReleaseTargetID(AccountFreeze, "acct"))
}

func TestConstructors_Validate(t *testing.T) {
	clk := clock.NewFake(t0)
	_, err := NewController(nil, &fakeAudit{}, &fakeVerifier{})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = NewController(clk, nil, &fakeVerifier{})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = NewController(clk, &fakeAudit{}, nil)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Equal(t, Policy{AccountFreezeBlocksRiskReduction: true}, NewChecker(Policy{AccountFreezeBlocksRiskReduction: true}).Policy())

	// Cache TTL is capped at one second.
	inner := NewChecker(Policy{})
	q := struct{ db.Querier }{}
	_, err = NewCachedChecker(inner, q, clk, 2*time.Second)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = NewCachedChecker(inner, q, clk, 0)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = NewCachedChecker(nil, q, clk, time.Second)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	cc, err := NewCachedChecker(inner, q, clk, MaxCacheTTL)
	require.NoError(t, err)
	// Never-blocked classes do not consult the (here unusable) querier.
	for _, class := range []ActionClass{Observe, Settle, Reconcile, LedgerPost, Cancel} {
		assert.NoError(t, cc.PreCheck(context.Background(), Action{Class: class}))
	}
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(cc.PreCheck(context.Background(), Action{Class: "X"})))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(inner.Check(context.Background(), nil, Action{Class: "X"})))
}

// TestParseReleaseTargetID: an executor releases the switch the stored
// approval names, so the parse has to be the exact inverse of the format the
// proposal was written in — and has to refuse anything it does not
// understand rather than fall back to a global scope.
func TestParseReleaseTargetID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind  Kind
		scope string
	}{
		{GlobalNewRiskKill, "*"},
		{FundingDisable, "*"},
		{WithdrawalsDisable, "*"},
		{ChainDisableNewActions, "solana"},
		{AccountFreeze, "0193b2e0-0000-7000-8000-000000000001"},
		{ProviderDisableNewActions, "stripe"},
		{ModelDisable, "*"},
		{VenueDisable, "jupiter:v6"}, // a scope may itself contain a colon
	} {
		kind, scope, err := ParseReleaseTargetID(ReleaseTargetID(tc.kind, tc.scope))
		require.NoError(t, err, "%s/%s", tc.kind, tc.scope)
		assert.Equal(t, tc.kind, kind)
		assert.Equal(t, tc.scope, scope)
	}

	for name, target := range map[string]string{
		"no separator":            "GLOBAL_NEW_RISK_KILL",
		"unknown kind":            "TOTALLY_MADE_UP:*",
		"empty":                   "",
		"scope on a global kind":  ReleaseTargetID(FundingDisable, "some-scope"),
		"global on a scoped kind": ReleaseTargetID(ChainDisableNewActions, "*"),
		"whitespace in scope":     "CHAIN_DISABLE_NEW_ACTIONS:so lana",
		"lower-cased kind":        "chain_disable_new_actions:solana",
	} {
		_, _, err := ParseReleaseTargetID(target)
		require.Error(t, err, name)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), name)
	}
}
