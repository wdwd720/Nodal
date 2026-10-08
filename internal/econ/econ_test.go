package econ

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

func validIntent() Intent {
	q := money.QuantityFromInt64(1_000_000)
	return Intent{
		AccountID:      accounts.AccountID(NewIntentID()),
		Kind:           KindTransferValue,
		Mode:           gates.ModeSandbox,
		Capability:     gates.P2PTransfers,
		ActorType:      security.ActorUser,
		ActorID:        "user-1",
		Quantity:       &q,
		Terms:          json.RawMessage(`{"recipient":"acct-b","memo":"rent"}`),
		IdempotencyKey: "p2p-1",
	}
}

func TestIntent_ValidateRefusesLiveAndEveryMalformedShape(t *testing.T) {
	require.NoError(t, validIntent().Validate())

	cases := map[string]func(*Intent){
		"live mode":                  func(i *Intent) { i.Mode = gates.ModeLive },
		"unknown mode":               func(i *Intent) { i.Mode = "PRODUCTION" },
		"unknown kind":               func(i *Intent) { i.Kind = "PRINT_MONEY" },
		"not an advanced capability": func(i *Intent) { i.Capability = gates.LiveFunding },
		"agent without agent id":     func(i *Intent) { i.ActorType = security.ActorAgent },
		"agent id on a human":        func(i *Intent) { i.AgentID = "0193b2e0-0000-7000-8000-000000000001" },
		"zero quantity":              func(i *Intent) { z := money.QuantityFromInt64(0); i.Quantity = &z },
		"negative notional":          func(i *Intent) { u := money.USDFromMinor(-1); i.NotionalUSD = &u },
		"empty idempotency key":      func(i *Intent) { i.IdempotencyKey = "" },
		"padded idempotency key":     func(i *Intent) { i.IdempotencyKey = " k " },
		"terms not an object":        func(i *Intent) { i.Terms = json.RawMessage(`[1,2]`) },
		"bad instrument id":          func(i *Intent) { i.InstrumentID = "SOL-USDC" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := validIntent()
			mutate(&in)
			err := in.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

func TestIntent_ContentHashIgnoresTermKeyOrderAndWhitespace(t *testing.T) {
	a := validIntent()
	b := a
	b.Terms = json.RawMessage(`{ "memo" : "rent",   "recipient":"acct-b" }`)
	ha, err := a.ComputeContentHash()
	require.NoError(t, err)
	hb, err := b.ComputeContentHash()
	require.NoError(t, err)
	assert.Equal(t, ha, hb)

	c := a
	c.Terms = json.RawMessage(`{"recipient":"acct-c","memo":"rent"}`)
	hc, err := c.ComputeContentHash()
	require.NoError(t, err)
	assert.NotEqual(t, ha, hc, "a different recipient is a different intent")

	// A float in the terms cannot be hashed canonically and is refused.
	d := a
	d.Terms = json.RawMessage(`{"amount":1.5}`)
	_, err = d.ComputeContentHash()
	require.Error(t, err)
}

func TestTransitions_IntentAndEffectTables(t *testing.T) {
	for _, s := range AllIntentStatuses() {
		assert.False(t, CanTransitionIntent(s, s), "%s self transition", s)
		if s.Terminal() {
			for _, to := range AllIntentStatuses() {
				assert.False(t, CanTransitionIntent(s, to), "terminal %s -> %s", s, to)
			}
		}
	}
	assert.False(t, CanTransitionIntent(StatusExecuting, StatusRejected), "an executing intent is not rejected; it fails or becomes unknown")
	assert.False(t, CanTransitionIntent(StatusUnknown, StatusCompleted), "an unknown outcome is reconciled, not declared complete")
	assert.True(t, CanTransitionIntent(StatusUnknown, StatusReconciled))

	for _, s := range AllEffectStatuses() {
		assert.False(t, CanTransitionEffect(s, s))
	}
	assert.False(t, CanTransitionEffect(EffectFailed, EffectApplied), "a failed effect never becomes applied")
	assert.False(t, CanTransitionEffect(EffectCompensated, EffectApplied))
	assert.False(t, CanTransitionEffect(EffectApplied, EffectFailed), "an applied effect is compensated, never failed")
	assert.False(t, CanTransitionEffect(EffectUnknown, EffectCompensated))
}

func TestAdvancedSpecs_CoverEveryAdvancedCapabilityOnceAndNoneIsLive(t *testing.T) {
	seen := map[gates.Capability]bool{}
	for _, s := range gates.AdvancedSpecs() {
		assert.False(t, seen[s.Capability], "%s listed twice", s.Capability)
		seen[s.Capability] = true
		assert.True(t, gates.IsHighRisk(s.Capability), "%s must be high risk", s.Capability)
		assert.False(t, s.LiveImplemented, "%s claims a live implementation", s.Capability)
		assert.True(t, s.MaxLiveCapital.IsZero(), "%s would put capital at risk", s.Capability)
		assert.NotEmpty(t, s.SimulatedModes)
		for _, m := range s.SimulatedModes {
			assert.True(t, m.Simulated(), "%s lists non-simulated mode %s", s.Capability, m)
		}
		assert.GreaterOrEqual(t, s.MinApprovers, 2)
		assert.Contains(t, s.LiveEvidence, gates.EvidenceLegalReview)
	}
	assert.Len(t, seen, 16)
	assert.ElementsMatch(t, AdvancedCapabilities(), func() []gates.Capability {
		out := []gates.Capability{}
		for c := range seen {
			out = append(out, c)
		}
		return out
	}())
}

func TestDecideMode_Table(t *testing.T) {
	type tc struct {
		env    string
		mode   gates.Mode
		gate   bool
		killed bool
		allow  bool
		reason string
	}
	for _, c := range []tc{
		{"LOCAL", gates.ModePaper, false, false, true, ""},
		{"TEST", gates.ModeSandbox, false, false, true, ""},
		{"DEV", gates.ModePaper, false, false, true, ""},
		{"STAGING", gates.ModePaper, false, false, false, gates.ReasonGateNotActive},
		{"PROD", gates.ModePaper, false, false, false, gates.ReasonGateNotActive},
		{"PROD", gates.ModePaper, true, false, true, ""},
		{"LOCAL", gates.ModeLive, true, false, false, gates.ReasonLiveNotImplemented},
		{"PROD", gates.ModeLive, true, false, false, gates.ReasonLiveNotImplemented},
		{"LOCAL", gates.ModeDevnet, false, false, false, gates.ReasonModeNotSupported}, // MARGIN has no devnet mode
		{"LOCAL", gates.ModePaper, false, true, false, gates.ReasonCapabilityKillSwitched},
		{"MOON", gates.ModePaper, false, false, false, gates.ReasonEnvironmentUnknown},
		{"LOCAL", "PRETEND", false, false, false, gates.ReasonModeUnknown},
	} {
		d := gates.DecideMode(gates.ModeRequest{Capability: gates.Margin, Environment: c.env, Mode: c.mode, GateActive: c.gate, KillSwitched: c.killed})
		assert.Equal(t, c.allow, d.Allowed, "%+v reasons=%v", c, d.Reasons)
		if c.reason != "" {
			assert.Contains(t, d.Reasons, c.reason, "%+v", c)
		}
		assert.Equal(t, d.Allowed, len(d.Reasons) == 0, "allowed and reasons can never disagree")
	}
	d := gates.DecideMode(gates.ModeRequest{Capability: gates.LiveFunding, Environment: "LOCAL", Mode: gates.ModePaper})
	assert.False(t, d.Allowed)
	assert.Equal(t, []string{gates.ReasonNotAdvancedCapability}, d.Reasons)
}

// TestProp_LiveIsNeverAllowed: for every advanced capability, environment,
// gate state and kill-switch state, LIVE is refused. This is the property the
// whole sandbox-first ADR rests on.
func TestProp_LiveIsNeverAllowed(t *testing.T) {
	envs := []string{"LOCAL", "TEST", "DEV", "STAGING", "PROD"}
	rapid.Check(t, func(rt *rapid.T) {
		spec := rapid.SampledFrom(gates.AdvancedSpecs()).Draw(rt, "spec")
		d := gates.DecideMode(gates.ModeRequest{
			Capability:   spec.Capability,
			Environment:  rapid.SampledFrom(envs).Draw(rt, "env"),
			Mode:         gates.ModeLive,
			GateActive:   rapid.Bool().Draw(rt, "gate"),
			KillSwitched: rapid.Bool().Draw(rt, "killed"),
		})
		if d.Allowed {
			rt.Fatalf("LIVE allowed for %s", spec.Capability)
		}
		found := false
		for _, r := range d.Reasons {
			if r == gates.ReasonLiveNotImplemented {
				found = true
			}
		}
		if !found {
			rt.Fatalf("LIVE refused for %s without saying it is not implemented: %v", spec.Capability, d.Reasons)
		}
	})
}

// --- Authorizer with fake readers -------------------------------------------

type fakeGates struct {
	env    string
	active map[gates.Capability]bool
}

func (f fakeGates) IsActive(_ context.Context, _ db.Querier, c gates.Capability) (gates.Verdict, error) {
	return gates.Verdict{Active: f.active[c]}, nil
}
func (f fakeGates) Environment() string { return f.env }

type fakeKills struct{ snap killswitch.Snapshot }

func (f fakeKills) Snapshot(context.Context, db.Querier) (killswitch.Snapshot, error) {
	return f.snap, nil
}

func TestAuthorizer_KillSwitchAndGate(t *testing.T) {
	ctx := context.Background()
	open := NewAuthorizer(fakeGates{env: "LOCAL"}, fakeKills{})
	require.NoError(t, open.Require(ctx, nil, gates.P2PTransfers, gates.ModeSandbox))

	err := open.Require(ctx, nil, gates.P2PTransfers, gates.ModeLive)
	require.Error(t, err)
	assert.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))

	killed := NewAuthorizer(fakeGates{env: "LOCAL"}, fakeKills{snap: killswitch.Snapshot{Switches: []killswitch.Switch{
		{Kind: killswitch.CapabilityDisable, ScopeID: string(gates.P2PTransfers), Active: true},
	}}})
	err = killed.Require(ctx, nil, gates.P2PTransfers, gates.ModeSandbox)
	require.Error(t, err)
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err))
	// A capability switch stops only its own capability.
	require.NoError(t, killed.Require(ctx, nil, gates.Margin, gates.ModePaper))

	global := NewAuthorizer(fakeGates{env: "LOCAL"}, fakeKills{snap: killswitch.Snapshot{Switches: []killswitch.Switch{
		{Kind: killswitch.GlobalNewRiskKill, ScopeID: "*", Active: true},
	}}})
	require.Error(t, global.Require(ctx, nil, gates.Margin, gates.ModePaper), "the global kill stops every advanced capability")

	prod := NewAuthorizer(fakeGates{env: "PROD", active: map[gates.Capability]bool{gates.Margin: true}}, fakeKills{})
	require.NoError(t, prod.Require(ctx, nil, gates.Margin, gates.ModePaper))
	require.Error(t, prod.Require(ctx, nil, gates.Derivatives, gates.ModePaper), "in PROD even paper needs the gate")
}

func TestEffect_KindPattern(t *testing.T) {
	for _, ok := range []string{"P2P_TRANSFER", "MINT", "OPTION_SETTLEMENT_2"} {
		assert.True(t, effectKindPattern(ok), ok)
	}
	for _, bad := range []string{"", "x", "p2p", "1ABC", "A-B", "A B"} {
		assert.False(t, effectKindPattern(bad), bad)
	}
}

func TestRequestHash_IsDeterministicAndRefusesFloats(t *testing.T) {
	type req struct {
		Amount money.Quantity `json:"amount"`
		To     string         `json:"to"`
	}
	a, err := RequestHash(req{money.QuantityFromInt64(5), "b"})
	require.NoError(t, err)
	b, err := RequestHash(map[string]any{"to": "b", "amount": "5"})
	require.NoError(t, err)
	assert.Equal(t, a, b, "the hash depends on content, not on the Go type that carried it")
	_, err = RequestHash(map[string]any{"amount": 1.5})
	require.Error(t, err)
}
