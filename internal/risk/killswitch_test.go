package risk

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestKillSwitchMatrix checks POLICY_AUTHORITY §2 for every switch kind and
// both action classes, under the default policy flags and under each flag
// flipped. NEW_RISK is blocked by every matching switch; REDUCE_RISK only by
// the kinds the table names (plus the two policy-controlled ones).
func TestKillSwitchMatrix(t *testing.T) {
	base := baseInput(t)
	it := base.Intent
	agentID := "019917c0-0000-7000-8000-000000000a01"
	scopes := map[string]string{
		KillGlobalNewRisk:          KillScopeAll,
		KillAccountFreeze:          it.AccountID,
		KillAgentPause:             agentID,
		KillStrategyVersionDisable: "019917c0-0000-7000-8000-000000000501",
		KillVenueDisable:           it.Venue,
		KillInstrumentCloseOnly:    it.InstrumentID,
		KillInstrumentHalt:         it.InstrumentID,
		KillChainDisable:           it.Chain,
		KillProviderDisable:        it.Provider,
		KillFundingDisable:         KillScopeAll,
		KillWithdrawalsDisable:     KillScopeAll,
		KillModelDisable:           "model-x",
		"UNKNOWN_FUTURE_KIND":      KillScopeAll,
	}
	// blocksReduce[kind] under (allowDuringKill=true, blockOnFreeze=false), the fixture defaults.
	blocksReduceDefault := map[string]bool{
		KillInstrumentHalt: true, KillChainDisable: true, KillProviderDisable: true, "UNKNOWN_FUTURE_KIND": true,
	}
	notTrade := map[string]bool{KillFundingDisable: true, KillWithdrawalsDisable: true}

	policies := map[string]Policy{
		"default":      loadPolicyFixture(t, "global_test"),
		"strict_kill":  loadPolicyFixture(t, "strict_kill"),
		"block_freeze": loadPolicyFixture(t, "block_freeze"),
	}
	for policyName, p := range policies {
		for kind, scope := range scopes {
			for _, class := range []ActionClass{ClassNewRisk, ClassReduceRisk} {
				t.Run(policyName+"/"+kind+"/"+string(class), func(t *testing.T) {
					in := base
					in.Intent.AgentID = agentID
					in.Intent.StrategyVersionID = scopes[KillStrategyVersionDisable]
					in.Intent.ModelID = "model-x"
					if class == ClassReduceRisk {
						in.Intent.Action = ActionReduceNotional
					}
					in.KillSwitches = []KillSwitch{{Kind: kind, Scope: scope}}
					d := Evaluate(p, in)
					require.Equal(t, class, d.ActionClass)

					want := !notTrade[kind]
					if class == ClassReduceRisk {
						want = blocksReduceDefault[kind]
						switch {
						case kind == KillGlobalNewRisk && policyName == "strict_kill":
							want = true
						case kind == KillAccountFreeze && policyName == "block_freeze":
							want = true
						}
					}
					got := containsCode(d.ReasonCodes, ReasonKillSwitch)
					assert.Equal(t, want, got, "kind=%s class=%s policy=%s codes=%v", kind, class, policyName, d.ReasonCodes)
					if want {
						assert.Equal(t, []KillSwitch{{Kind: kind, Scope: scope}}, d.MatchedKillSwitches)
						assert.Equal(t, Reject, d.Verdict)
					} else {
						assert.Empty(t, d.MatchedKillSwitches)
						assert.Equal(t, Allow, d.Verdict, "only the kill switch differs from the ALLOW fixture")
					}
				})
			}
		}
	}
}

// TestKillSwitch_ScopeMatching: scoped switches only match their own scope;
// "*" matches every value; agent/strategy/model switches ignore manual intents.
func TestKillSwitch_ScopeMatching(t *testing.T) {
	p := loadPolicyFixture(t, "global_test")
	base := baseInput(t)
	cases := []struct {
		name    string
		mutate  func(in *Input)
		switch_ KillSwitch
		block   bool
	}{
		{"account freeze other account", nil, KillSwitch{KillAccountFreeze, "019917c0-0000-7000-8000-0000000000bb"}, false},
		{"account freeze wildcard", nil, KillSwitch{KillAccountFreeze, KillScopeAll}, true},
		{"venue other", nil, KillSwitch{KillVenueDisable, "RAYDIUM"}, false},
		{"venue wildcard", nil, KillSwitch{KillVenueDisable, KillScopeAll}, true},
		{"instrument other", nil, KillSwitch{KillInstrumentHalt, "019917c0-0000-7000-8000-000000000999"}, false},
		{"chain other", nil, KillSwitch{KillChainDisable, "ethereum"}, false},
		{"provider via quote", func(in *Input) { in.Intent.Provider = "HELIUS" }, KillSwitch{KillProviderDisable, "JUPITER_API"}, true},
		{"provider other", nil, KillSwitch{KillProviderDisable, "OTHER"}, false},
		{"agent pause manual intent wildcard", nil, KillSwitch{KillAgentPause, KillScopeAll}, false},
		{"agent pause other agent", func(in *Input) { in.Intent.AgentID = "a1" }, KillSwitch{KillAgentPause, "a2"}, false},
		{"agent pause this agent", func(in *Input) { in.Intent.AgentID = "a1" }, KillSwitch{KillAgentPause, "a1"}, true},
		{"strategy manual intent", nil, KillSwitch{KillStrategyVersionDisable, KillScopeAll}, false},
		{"model manual intent", nil, KillSwitch{KillModelDisable, KillScopeAll}, false},
		{"model this model", func(in *Input) { in.Intent.ModelID = "m1" }, KillSwitch{KillModelDisable, "m1"}, true},
		{"empty scope means wildcard", nil, KillSwitch{KillGlobalNewRisk, ""}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := base
			if c.mutate != nil {
				c.mutate(&in)
			}
			in.KillSwitches = []KillSwitch{c.switch_}
			d := Evaluate(p, in)
			assert.Equal(t, c.block, containsCode(d.ReasonCodes, ReasonKillSwitch), "codes=%v", d.ReasonCodes)
		})
	}
}

// TestKillSwitch_OrderAndDuplicatesIrrelevant: matched switches are reported
// sorted and de-duplicated whatever the input order.
func TestKillSwitch_OrderAndDuplicatesIrrelevant(t *testing.T) {
	p := loadPolicyFixture(t, "global_test")
	in := baseInput(t)
	in.KillSwitches = []KillSwitch{
		{KillVenueDisable, "JUPITER"}, {KillGlobalNewRisk, "*"}, {KillVenueDisable, "JUPITER"}, {KillAccountFreeze, "*"},
	}
	a := Evaluate(p, in)
	in.KillSwitches = []KillSwitch{
		{KillAccountFreeze, "*"}, {KillVenueDisable, "JUPITER"}, {KillGlobalNewRisk, ""},
	}
	b := Evaluate(p, in)
	require.Equal(t, a.Hash, b.Hash)
	assert.Equal(t, []KillSwitch{{KillAccountFreeze, "*"}, {KillGlobalNewRisk, "*"}, {KillVenueDisable, "JUPITER"}}, a.MatchedKillSwitches)
}

// TestKillSwitch_ContinuousStageIgnoresSwitches: kill switches stop new
// actions; a CONTINUOUS evaluation of existing exposure is not an action.
func TestKillSwitch_ContinuousStageIgnoresSwitches(t *testing.T) {
	p := loadPolicyFixture(t, "global_test")
	in := baseInput(t)
	in.Stage = StageContinuous
	in.KillSwitches = []KillSwitch{{KillGlobalNewRisk, "*"}}
	d := Evaluate(p, in)
	assert.Equal(t, Allow, d.Verdict, d.ReasonCodes)
}
