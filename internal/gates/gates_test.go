package gates

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func ptr(t time.Time) *time.Time { return &t }

func operator(sub string, roles ...security.Role) security.Principal {
	return security.Principal{SubjectID: sub, ActorType: security.ActorOperator, Roles: roles, AuthTime: t0, AMR: []string{"mfa"}}
}

// breakGlass is an operator holding a live BREAK_GLASS elevation (the only
// way to hold gate:approve) with a fresh strong authentication.
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

func newTestAdmin(t *testing.T) (*Admin, *clock.Fake, *fakeAudit) {
	t.Helper()
	clk := clock.NewFake(t0)
	audit := &fakeAudit{}
	a, err := NewAdmin("TEST", clk, audit)
	require.NoError(t, err)
	return a, clk, audit
}

// validActiveGate is a high-risk gate satisfying every activation condition
// at t0: proposed by "prop", approved by "appr", activated by "act".
func validActiveGate() Gate {
	return Gate{
		ID: NewGateID(), Capability: LiveFunding, Environment: "TEST", State: StateActive, ApprovalVersion: 1,
		LegalReviewRef: "legal-1", ProviderContractRef: "contract-1", RiskApprovalRef: "risk-1", SecurityApprovalRef: "sec-1",
		ProposedBy: "prop",
		Approvers: []Approver{
			{UserID: "prop", Step: StepPropose, At: t0.Add(-3 * time.Hour)},
			{UserID: "appr", Step: StepApprove, At: t0.Add(-2 * time.Hour)},
			{UserID: "act", Step: StepActivate, At: t0.Add(-time.Hour)},
		},
		EvidenceHashes: []string{"ab" + repeat("0", 62)},
		EffectiveAt:    ptr(t0.Add(-time.Hour)),
		ExpiresAt:      ptr(t0.Add(24 * time.Hour)),
		Version:        3,
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

// --- transition table -------------------------------------------------------

func TestCanTransition_EveryPair(t *testing.T) {
	allowed := map[GateState]map[GateState]bool{
		StateDisabled:        {StatePendingApproval: true, StateRevoked: true},
		StatePendingApproval: {StateApproved: true, StateRevoked: true},
		StateApproved:        {StateActive: true, StateExpired: true, StateRevoked: true},
		StateActive:          {StateSuspended: true, StateExpired: true, StateRevoked: true},
		StateSuspended:       {StateApproved: true, StateRevoked: true},
		StateRevoked:         {StatePendingApproval: true},
		StateExpired:         {StatePendingApproval: true, StateRevoked: true},
	}
	states := AllStates()
	require.Len(t, states, 7)
	pairs := 0
	for _, from := range states {
		for _, to := range states {
			pairs++
			want := allowed[from][to]
			assert.Equalf(t, want, CanTransition(from, to), "%s -> %s", from, to)
		}
	}
	assert.Equal(t, 49, pairs)
	// Invariants worth stating explicitly.
	for _, s := range states {
		assert.Falsef(t, CanTransition(s, s), "%s -> %s self transition", s, s)
		assert.Falsef(t, CanTransition(s, StateDisabled), "%s -> DISABLED: DISABLED is only ever the bootstrap default", s)
		if s != StateRevoked {
			assert.Truef(t, CanTransition(s, StateRevoked), "%s -> REVOKED must be allowed (any -> REVOKED)", s)
		}
	}
	assert.False(t, CanTransition(StateSuspended, StateActive), "SUSPENDED must go through APPROVED and a distinct activator")
	assert.False(t, CanTransition(StateDisabled, StateActive), "no shortcut to ACTIVE")
	assert.False(t, CanTransition(StatePendingApproval, StateActive), "single approval must not activate")
	assert.False(t, CanTransition("", StatePendingApproval))
}

func TestCapabilities_And_HighRisk(t *testing.T) {
	caps := AllCapabilities()
	require.Len(t, caps, 10)
	high := map[Capability]bool{
		LiveFunding: true, LiveManualTrading: true, LiveAgentTrading: true, Withdrawals: true,
		Securities: true, CEXTrading: true, CrossChain: true, PredictionMarkets: true,
		SocialDataPersistence: false, Marketplace: false,
	}
	for _, c := range caps {
		assert.True(t, c.Valid())
		assert.Equalf(t, high[c], IsHighRisk(c), "%s", c)
	}
	assert.False(t, Capability("LIVE_EVERYTHING").Valid())
	assert.False(t, IsHighRisk("LIVE_EVERYTHING"))
	assert.False(t, GateState("ON").Valid())
}

// --- the five activation conditions -----------------------------------------

func TestEvaluate_ValidGateIsActive(t *testing.T) {
	g := validActiveGate()
	v := Evaluate(&g, true, t0)
	assert.True(t, v.Active)
	assert.Empty(t, v.Reason)
	assert.Equal(t, StateActive, v.State)
	assert.Equal(t, 1, v.ApprovalVersion)
	assert.Equal(t, g.EvidenceHashes, v.EvidenceHashes)
	// Boundaries: effective_at <= now < expires_at.
	g.EffectiveAt = ptr(t0)
	assert.True(t, Evaluate(&g, true, t0).Active, "effective_at == now is effective")
	g.ExpiresAt = ptr(t0)
	assert.Equal(t, ReasonExpired, Evaluate(&g, true, t0).Reason, "expires_at == now is expired")
}

func TestEvaluate_EachConditionFailsWithDistinctReason(t *testing.T) {
	cases := []struct {
		name    string
		config  bool
		mutate  func(g *Gate) *Gate
		reason  string
		cond    int
		wantNil bool
	}{
		{"1 config disabled", false, func(g *Gate) *Gate { return g }, ReasonConfigDisabled, 1, false},
		{"2 no gate row", true, func(g *Gate) *Gate { return nil }, ReasonNoGateRow, 2, true},
		{"2 state APPROVED", true, func(g *Gate) *Gate { g.State = StateApproved; return g }, ReasonStateNotActive, 2, false},
		{"2 state SUSPENDED", true, func(g *Gate) *Gate { g.State = StateSuspended; return g }, ReasonStateNotActive, 2, false},
		{"3 revoked", true, func(g *Gate) *Gate { g.RevokedAt = ptr(t0.Add(-time.Minute)); return g }, ReasonRevoked, 3, false},
		{"3 no effective_at", true, func(g *Gate) *Gate { g.EffectiveAt = nil; return g }, ReasonNoEffectiveAt, 3, false},
		{"3 not yet effective", true, func(g *Gate) *Gate { g.EffectiveAt = ptr(t0.Add(time.Second)); return g }, ReasonNotYetEffective, 3, false},
		{"3 expired", true, func(g *Gate) *Gate { g.ExpiresAt = ptr(t0.Add(-time.Second)); return g }, ReasonExpired, 3, false},
		{"4 missing legal review", true, func(g *Gate) *Gate { g.LegalReviewRef = ""; return g }, ReasonEvidenceMissing, 4, false},
		{"4 missing provider contract", true, func(g *Gate) *Gate { g.ProviderContractRef = "  "; return g }, ReasonEvidenceMissing, 4, false},
		{"4 missing risk approval", true, func(g *Gate) *Gate { g.RiskApprovalRef = ""; return g }, ReasonEvidenceMissing, 4, false},
		{"4 missing security approval", true, func(g *Gate) *Gate { g.SecurityApprovalRef = ""; return g }, ReasonEvidenceMissing, 4, false},
		{"5 no proposer", true, func(g *Gate) *Gate {
			g.ProposedBy = ""
			g.Approvers = g.Approvers[1:]
			return g
		}, ReasonNoProposer, 5, false},
		{"5 proposer approved", true, func(g *Gate) *Gate { g.Approvers[1].UserID = "prop"; return g }, ReasonProposerApproved, 5, false},
		{"5 proposer activated", true, func(g *Gate) *Gate { g.Approvers[2].UserID = "prop"; return g }, ReasonProposerApproved, 5, false},
		{"5 one approver twice", true, func(g *Gate) *Gate { g.Approvers[2].UserID = "appr"; return g }, ReasonInsufficientApprovers, 5, false},
		{"5 no approvers", true, func(g *Gate) *Gate { g.Approvers = g.Approvers[:1]; return g }, ReasonInsufficientApprovers, 5, false},
	}
	seen := map[string]int{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := validActiveGate()
			gp := tc.mutate(&g)
			if tc.wantNil {
				require.Nil(t, gp)
			}
			v := Evaluate(gp, tc.config, t0)
			assert.False(t, v.Active)
			assert.Equal(t, tc.reason, v.Reason)
			if prev, ok := seen[tc.reason]; ok {
				assert.Equalf(t, prev, tc.cond, "reason %q is shared across conditions %d and %d", tc.reason, prev, tc.cond)
			}
			seen[tc.reason] = tc.cond
		})
	}
	// Every condition has at least one reason and no reason is empty.
	conds := map[int]bool{}
	for r, c := range seen {
		assert.NotEmpty(t, r)
		conds[c] = true
	}
	for c := 1; c <= 5; c++ {
		assert.Truef(t, conds[c], "condition %d has no reason", c)
	}
}

func TestEvaluate_LowRiskDoesNotRequireEvidenceRefs(t *testing.T) {
	g := validActiveGate()
	g.Capability = Marketplace
	g.LegalReviewRef, g.ProviderContractRef, g.RiskApprovalRef, g.SecurityApprovalRef = "", "", "", ""
	v := Evaluate(&g, true, t0)
	assert.True(t, v.Active, v.Reason)
	// ... but still needs dual authorization.
	g.Approvers = g.Approvers[:2]
	assert.Equal(t, ReasonInsufficientApprovers, Evaluate(&g, true, t0).Reason)
}

func TestEvaluate_ConfigAloneNeverActivates(t *testing.T) {
	// A fresh (bootstrapped) row with configuration enabled is still off.
	g := Gate{Capability: LiveFunding, Environment: "PROD", State: StateDisabled}
	v := Evaluate(&g, true, t0)
	assert.False(t, v.Active)
	assert.Equal(t, ReasonStateNotActive, v.Reason)
	assert.Equal(t, ReasonNoGateRow, Evaluate(nil, true, t0).Reason)
}

// --- checker --------------------------------------------------------------

func TestChecker_ConfigDisabledSkipsQuery(t *testing.T) {
	c, err := NewChecker("PROD", func(Capability) bool { return false }, clock.NewFake(t0))
	require.NoError(t, err)
	// q is nil: touching it would panic, so a passing test proves no query.
	v, err := c.IsActive(context.Background(), nil, LiveFunding)
	require.NoError(t, err)
	assert.False(t, v.Active)
	assert.Equal(t, ReasonConfigDisabled, v.Reason)

	err = c.RequireActive(context.Background(), nil, LiveFunding)
	require.Error(t, err)
	assert.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))
	e, _ := errs.As(err)
	assert.Equal(t, ReasonConfigDisabled, e.Fields["reason"])
	assert.Equal(t, "LIVE_FUNDING", e.Fields["capability"])

	v, err = c.IsActive(context.Background(), nil, "NOT_A_CAPABILITY")
	require.NoError(t, err)
	assert.Equal(t, ReasonNoGateRow, v.Reason)
}

func TestChecker_NilEnabledFailsClosed(t *testing.T) {
	c, err := NewChecker("TEST", nil, clock.NewFake(t0))
	require.NoError(t, err)
	v, err := c.IsActive(context.Background(), nil, Marketplace)
	require.NoError(t, err)
	assert.Equal(t, ReasonConfigDisabled, v.Reason)
}

func TestConstructors_Validate(t *testing.T) {
	_, err := NewChecker("PRODUCTION", nil, clock.NewFake(t0))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = NewChecker("PROD", nil, nil)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = NewAdmin("PROD", clock.NewFake(t0), nil)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = NewAdmin("nope", clock.NewFake(t0), &fakeAudit{})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = NewAdmin("PROD", nil, &fakeAudit{})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// --- admin authorization runs before any query ------------------------------

type adminOp struct {
	name string
	call func(ctx context.Context, a *Admin) error
}

// allOps exercises every Admin method with a nil transaction: any query
// would dereference it and panic, so a clean error proves the rejection
// happened first.
func allOps() []adminOp {
	prop := Proposal{Reason: "test proposal", LegalReviewRef: "l", ProviderContractRef: "p", RiskApprovalRef: "r", SecurityApprovalRef: "s"}
	return []adminOp{
		{"Propose", func(ctx context.Context, a *Admin) error {
			_, err := a.Propose(ctx, nil, LiveFunding, prop)
			return err
		}},
		{"Approve", func(ctx context.Context, a *Admin) error {
			_, err := a.Approve(ctx, nil, LiveFunding, "ok")
			return err
		}},
		{"Activate", func(ctx context.Context, a *Admin) error {
			_, err := a.Activate(ctx, nil, LiveFunding, "ok")
			return err
		}},
		{"Suspend", func(ctx context.Context, a *Admin) error {
			_, err := a.Suspend(ctx, nil, LiveFunding, "incident")
			return err
		}},
		{"Resume", func(ctx context.Context, a *Admin) error {
			_, err := a.Resume(ctx, nil, LiveFunding, "resolved")
			return err
		}},
		{"Revoke", func(ctx context.Context, a *Admin) error {
			_, err := a.Revoke(ctx, nil, LiveFunding, "withdrawn")
			return err
		}},
		{"ExpireDue", func(ctx context.Context, a *Admin) error { _, err := a.ExpireDue(ctx, nil, t0); return err }},
	}
}

func TestAdmin_AgentRejectedBeforeAnyQuery(t *testing.T) {
	a, _, audit := newTestAdmin(t)
	agent := security.AgentPrincipal("agent-1", "acct-1")
	for _, op := range allOps() {
		t.Run(op.name, func(t *testing.T) {
			err := op.call(ctxWith(agent), a)
			require.Error(t, err)
			assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
		})
	}
	// Even an agent that somehow carries roles is refused (invalid principal).
	rogue := security.Principal{SubjectID: "agent-2", ActorType: security.ActorAgent, Roles: []security.Role{security.RoleAdmin}, AccountIDs: []string{"a"}}
	for _, op := range allOps() {
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(op.call(ctxWith(rogue), a)), op.name)
	}
	_, err := Bootstrap(ctxWith(agent), nil, "TEST")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	assert.Empty(t, audit.events, "nothing may be audited for a rejected agent")
}

func TestAdmin_ServiceAndAnonymousRejectedBeforeAnyQuery(t *testing.T) {
	a, _, _ := newTestAdmin(t)
	svc := security.Principal{SubjectID: "svc-1", ActorType: security.ActorService, Roles: []security.Role{security.RoleAdmin, security.RoleBreakGlass}, AuthTime: t0, AMR: []string{"mfa"}}
	until := t0.Add(time.Hour)
	svc.BreakGlassUntil = &until
	for _, op := range allOps() {
		if op.name == "ExpireDue" {
			// ExpireDue without a principal is the system path; with a
			// SERVICE principal it is refused like the others.
			assert.Equal(t, errs.CodeForbidden, errs.CodeOf(op.call(ctxWith(svc), a)), op.name)
			continue
		}
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(op.call(ctxWith(svc), a)), "service "+op.name)
		assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(op.call(context.Background(), a)), "anonymous "+op.name)
	}
}

func TestAdmin_PermissionAndStepUpBeforeAnyQuery(t *testing.T) {
	a, clk, _ := newTestAdmin(t)
	// SUPPORT_READ_ONLY has no gate permissions at all.
	ro := ctxWith(operator("ro", security.RoleSupportReadOnly))
	for _, op := range allOps() {
		if op.name == "ExpireDue" {
			continue
		}
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(op.call(ro, a)), op.name)
	}
	// ADMIN may propose and suspend but never holds the approve side.
	admin := ctxWith(operator("admin", security.RoleAdmin))
	for _, name := range []string{"Approve", "Activate", "Resume", "Revoke"} {
		for _, op := range allOps() {
			if op.name == name {
				assert.Equal(t, errs.CodeForbidden, errs.CodeOf(op.call(admin, a)), name)
			}
		}
	}
	// RISK may propose; a customer may not.
	customer := security.Principal{SubjectID: "cust", ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{"acct"}}
	_, err := a.Propose(ctxWith(customer), nil, Marketplace, Proposal{Reason: "please"})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// Break-glass holder whose authentication is too old: STEP_UP_REQUIRED
	// before any query for Approve/Activate/Resume.
	stale := breakGlass("bg")
	stale.AuthTime = t0.Add(-StepUpMaxAge - time.Second)
	for _, name := range []string{"Approve", "Activate", "Resume"} {
		for _, op := range allOps() {
			if op.name == name {
				assert.Equal(t, errs.CodeStepUpRequired, errs.CodeOf(op.call(ctxWith(stale), a)), name)
			}
		}
	}
	// Password-only authentication is not a step-up either.
	weak := breakGlass("bg")
	weak.AMR = []string{"pwd"}
	_, err = a.Approve(ctxWith(weak), nil, LiveFunding, "ok")
	assert.Equal(t, errs.CodeStepUpRequired, errs.CodeOf(err))

	// Expired break-glass elevation loses gate:approve entirely.
	clk.Advance(2 * time.Hour)
	_, err = a.Revoke(ctxWith(breakGlass("bg")), nil, LiveFunding, "late")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}

func TestAdmin_ValidationBeforeAnyQuery(t *testing.T) {
	a, _, _ := newTestAdmin(t)
	risk := ctxWith(operator("risk", security.RoleRisk))
	_, err := a.Propose(risk, nil, "NOPE", Proposal{Reason: "x"})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = a.Propose(risk, nil, LiveFunding, Proposal{Reason: "missing evidence"})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	bg := ctxWith(breakGlass("bg"))
	_, err = a.Approve(bg, nil, LiveFunding, "   ")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	ops := ctxWith(operator("ops", security.RoleOperations))
	_, err = a.Suspend(ops, nil, LiveFunding, "")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// --- dual-control rules -------------------------------------------------------

func TestRules_SamePrincipalCannotApproveAndActivate(t *testing.T) {
	g := Gate{Capability: LiveFunding, State: StateApproved, ProposedBy: "prop", Approvers: []Approver{
		{UserID: "prop", Step: StepPropose},
		{UserID: "appr", Step: StepApprove},
	}}
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(activateRule(&g, "appr")), "approver activating")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(activateRule(&g, "prop")), "proposer activating")
	assert.NoError(t, activateRule(&g, "third"))

	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(approveRule(&g, "prop")), "proposer approving")
	assert.NoError(t, approveRule(&g, "appr"))

	// Resume flow: after [prop, appr, act, resumer] the activator must differ
	// from the resumer (the principal that put it back into APPROVED).
	g.Approvers = append(g.Approvers, Approver{UserID: "act", Step: StepActivate}, Approver{UserID: "resumer", Step: StepResume})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(activateRule(&g, "resumer")))
	assert.NoError(t, activateRule(&g, "act"))
	assert.NoError(t, activateRule(&g, "appr"))

	// No proposer or no approver: dual control impossible.
	none := Gate{Capability: LiveFunding, State: StateApproved}
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(approveRule(&none, "x")))
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(activateRule(&none, "x")))
	onlyProp := Gate{Capability: LiveFunding, ProposedBy: "prop", Approvers: []Approver{{UserID: "prop", Step: StepPropose}}}
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(activateRule(&onlyProp, "x")))
}

func TestRules_ApplyToLowRiskToo(t *testing.T) {
	g := Gate{Capability: Marketplace, State: StateApproved, ProposedBy: "prop", Approvers: []Approver{
		{UserID: "prop", Step: StepPropose}, {UserID: "appr", Step: StepApprove},
	}}
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(activateRule(&g, "appr")))
}

// --- proposal validation and evidence digest ---------------------------------

func TestProposal_Normalize(t *testing.T) {
	h1 := "AA" + repeat("0", 62)
	h2 := "bb" + repeat("1", 62)
	p := Proposal{
		LegalReviewRef: " legal ", ProviderContractRef: "c", RiskApprovalRef: "r", SecurityApprovalRef: "s",
		EvidenceHashes: []string{h2, h1, " " + h2}, Reason: "  go live ", ExpiresAt: t0.Add(time.Hour),
	}
	n, err := p.normalize(LiveFunding, t0)
	require.NoError(t, err)
	assert.Equal(t, "legal", n.LegalReviewRef)
	assert.Equal(t, "go live", n.Reason)
	assert.Equal(t, []string{"aa" + repeat("0", 62), h2}, n.EvidenceHashes, "lowercased, de-duplicated, sorted")

	cases := map[string]Proposal{
		"no reason":            {LegalReviewRef: "l", ProviderContractRef: "c", RiskApprovalRef: "r", SecurityApprovalRef: "s"},
		"missing ref":          {LegalReviewRef: "l", ProviderContractRef: "c", RiskApprovalRef: "r", Reason: "x"},
		"blank ref":            {LegalReviewRef: "l", ProviderContractRef: "c", RiskApprovalRef: "r", SecurityApprovalRef: "  ", Reason: "x"},
		"bad hash":             {LegalReviewRef: "l", ProviderContractRef: "c", RiskApprovalRef: "r", SecurityApprovalRef: "s", Reason: "x", EvidenceHashes: []string{"deadbeef"}},
		"expires in past":      {LegalReviewRef: "l", ProviderContractRef: "c", RiskApprovalRef: "r", SecurityApprovalRef: "s", Reason: "x", ExpiresAt: t0},
		"expires before start": {LegalReviewRef: "l", ProviderContractRef: "c", RiskApprovalRef: "r", SecurityApprovalRef: "s", Reason: "x", EffectiveAt: t0.Add(2 * time.Hour), ExpiresAt: t0.Add(time.Hour)},
	}
	for name, p := range cases {
		_, err := p.normalize(LiveFunding, t0)
		assert.Equalf(t, errs.CodeValidationFailed, errs.CodeOf(err), "%s: %v", name, err)
	}
	// Low-risk capabilities need a reason but not the four references.
	_, err = Proposal{Reason: "marketplace beta"}.normalize(Marketplace, t0)
	assert.NoError(t, err)
}

func TestEvidenceDigest_DeterministicAndOrderIndependent(t *testing.T) {
	a := Gate{LegalReviewRef: "l", EvidenceHashes: []string{"b", "a"}}
	b := Gate{LegalReviewRef: "l", EvidenceHashes: []string{"a", "b"}}
	assert.Equal(t, a.EvidenceDigestHex(), b.EvidenceDigestHex())
	c := Gate{LegalReviewRef: "l2", EvidenceHashes: []string{"a", "b"}}
	assert.NotEqual(t, a.EvidenceDigestHex(), c.EvidenceDigestHex())
	assert.Len(t, a.EvidenceDigestHex(), 64)
	assert.Equal(t, Gate{}.EvidenceDigestHex(), Gate{EvidenceHashes: []string{}}.EvidenceDigestHex(), "nil and empty hash lists are the same evidence")
}

func TestApprover_JSONShape(t *testing.T) {
	b, err := json.Marshal(Approver{UserID: "u", Role: "BREAK_GLASS", At: t0, Step: StepApprove, EvidenceHash: "ff"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"user_id":"u","role":"BREAK_GLASS","at":"2026-09-05T12:00:00Z","step":"APPROVE","evidence_hash":"ff"}`, string(b))
}

func TestDistinctApprovers_ExcludesProposer(t *testing.T) {
	g := validActiveGate()
	assert.Equal(t, []string{"act", "appr"}, g.DistinctApprovers())
	last, ok := g.lastApprover()
	require.True(t, ok)
	assert.Equal(t, "act", last.UserID)
	_, ok = Gate{Approvers: []Approver{{UserID: "p", Step: StepPropose}}}.lastApprover()
	assert.False(t, ok)
}
