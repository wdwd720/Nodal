package delegation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/valuation"
)

// classAsset is a SIMULATED asset of instrument class c ("" for one with no
// class: an UNSUPPORTED asset that is not a USD-pegged stable unit).
func classAsset(c InstrumentClass) assets.Asset {
	a := assets.Asset{Kind: assets.KindSimulated, RiskClass: assets.RiskMajor}
	switch c {
	case ClassStableValue:
		a.IsStablecoin, a.PegCurrency, a.RiskClass = true, valuation.USDPeg, assets.RiskSettlement
	case ClassSpeculativeToken:
		a.RiskClass = assets.RiskSpeculative
	case "":
		a.RiskClass = assets.RiskUnsupported
	}
	return a
}

// TestSwapActionIsTheDirectionOfValueNeverTheLabel: only paying a token
// (SPOT_TOKEN or SPECULATIVE_TOKEN) for a USD-pegged stable unit reduces
// exposure. Buying a token, swapping one token for another or one stable
// unit for another, and anything touching an asset with no class acquire it.
func TestSwapActionIsTheDirectionOfValueNeverTheLabel(t *testing.T) {
	classes := []InstrumentClass{ClassStableValue, ClassSpotToken, ClassSpeculativeToken, ""}
	reduces := map[[2]InstrumentClass]bool{
		{ClassSpotToken, ClassStableValue}:        true,
		{ClassSpeculativeToken, ClassStableValue}: true,
	}
	for _, pay := range classes {
		for _, receive := range classes {
			p, r := classAsset(pay), classAsset(receive)
			got, ok := ClassifyAsset(p)
			require.Equal(t, pay != "", ok)
			require.Equal(t, pay, got)
			want := ActionAcquireExposure
			if reduces[[2]InstrumentClass{pay, receive}] {
				want = ActionReduceExposure
			}
			assert.Equal(t, want, SwapAction(p, r), "pay %q, receive %q", pay, receive)
		}
	}
	// Not SIMULATED: no class, never a reduction, whatever it pegs.
	pegged := classAsset(ClassStableValue)
	pegged.Kind = assets.KindSPLToken
	assert.Equal(t, ActionAcquireExposure, SwapAction(classAsset(ClassSpotToken), pegged))
}

// TestProp_AReductionReceivesOnlyTheStableUnitAndReversesToAnAcquisition:
// for any two assets, a swap SwapAction calls a reduction receives a
// USD-pegged stable unit and pays a classified token -- so no relabelling can
// make a purchase of exposure a reduction -- and the same pair the other way
// round is an acquisition.
func TestProp_AReductionReceivesOnlyTheStableUnitAndReversesToAnAcquisition(t *testing.T) {
	kinds := []assets.Kind{assets.KindSimulated, assets.KindSimulated, assets.KindSPLToken, assets.KindCredit}
	riskClasses := []assets.RiskClass{assets.RiskSettlement, assets.RiskMajor, assets.RiskStandard, assets.RiskSpeculative,
		assets.RiskUnsupported}
	pegs := []string{"", valuation.USDPeg, "EUR"}
	draw := func(t *rapid.T, label string) assets.Asset {
		return assets.Asset{
			Kind:         rapid.SampledFrom(kinds).Draw(t, label+"-kind"),
			IsStablecoin: rapid.Bool().Draw(t, label+"-stable"),
			PegCurrency:  rapid.SampledFrom(pegs).Draw(t, label+"-peg"),
			RiskClass:    rapid.SampledFrom(riskClasses).Draw(t, label+"-risk"),
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		pay, receive := draw(t, "pay"), draw(t, "receive")
		action := SwapAction(pay, receive)
		if !action.IsSwap() {
			t.Fatalf("%q is not a swap action", action)
		}
		if action != ActionReduceExposure {
			return
		}
		r, okR := ClassifyAsset(receive)
		p, okP := ClassifyAsset(pay)
		if !okR || r != ClassStableValue || !okP || (p != ClassSpotToken && p != ClassSpeculativeToken) {
			t.Fatalf("a reduction pays %q (%v) for %q (%v)", p, okP, r, okR)
		}
		if SwapAction(receive, pay) != ActionAcquireExposure {
			t.Fatalf("the reverse of a reduction is not an acquisition")
		}
	})
}

// TestEveryKillSwitchCheckOfAnAgentNamesItsStrategyVersion: the action an
// agent's delegated activity is checked as names the account, the agent and
// the strategy version it runs, so the kill-switch matrix stops it on a
// STRATEGY_VERSION_DISABLE of that version -- and only that version -- as it
// does on an AGENT_PAUSE.
func TestEveryKillSwitchCheckOfAnAgentNamesItsStrategyVersion(t *testing.T) {
	account := accounts.AccountID(id.New[id.Any]())
	agent := agentRow{ID: id.New[id.Any]().String(), AccountID: account, StrategyID: id.New[id.Any]().String(),
		StrategyVersionID: id.New[id.Any]().String(), State: "VALIDATED"}
	on := func(kind killswitch.Kind, scope string) []killswitch.Switch {
		return []killswitch.Switch{{Kind: kind, ScopeID: scope, Active: true}}
	}
	for _, capability := range []gates.Capability{gates.AgentSigning, gates.AgentWithdrawals} {
		a := agent.killAction(killswitch.NewRisk, account, capability)
		assert.Equal(t, agent.StrategyVersionID, a.StrategyVersionID)
		assert.Equal(t, agent.ID, a.AgentID)
		assert.Equal(t, account.String(), a.AccountID)
		assert.Equal(t, string(capability), a.Capability)
		require.NoError(t, a.Validate())

		_, blocked := killswitch.Blocking(on(killswitch.StrategyVersionDisable, agent.StrategyVersionID), a, killswitch.Policy{})
		assert.True(t, blocked, "%s: the agent's own version is disabled", capability)
		_, blocked = killswitch.Blocking(on(killswitch.StrategyVersionDisable, id.New[id.Any]().String()), a, killswitch.Policy{})
		assert.False(t, blocked, "%s: another version's switch", capability)
		_, blocked = killswitch.Blocking(on(killswitch.AgentPause, agent.ID), a, killswitch.Policy{})
		assert.True(t, blocked, "%s: the agent is paused", capability)
	}
	// A withdrawal's own check names them too; an account freeze stops it.
	w := agent.killAction(killswitch.Withdraw, account, gates.AgentWithdrawals)
	assert.Equal(t, agent.StrategyVersionID, w.StrategyVersionID)
	_, blocked := killswitch.Blocking(on(killswitch.AccountFreeze, account.String()), w, killswitch.Policy{})
	assert.True(t, blocked)
	// An agent with no version (a DRAFT) is matched by no version's switch.
	draft := agentRow{ID: agent.ID, AccountID: account}
	_, blocked = killswitch.Blocking(on(killswitch.StrategyVersionDisable, agent.StrategyVersionID),
		draft.killAction(killswitch.NewRisk, account, gates.AgentSigning), killswitch.Policy{})
	assert.False(t, blocked)
}

// TestExecutableSubjectsNameTheirOwnID: a selected subject reports the id of
// its own kind.
func TestExecutableSubjectsNameTheirOwnID(t *testing.T) {
	sid, wid := id.New[swapKind](), id.New[withdrawalKind]()
	assert.Equal(t, sid.String(), ExecutableSubject{Kind: SubjectSwap, SwapID: sid}.ID())
	assert.Equal(t, wid.String(), ExecutableSubject{Kind: SubjectWithdrawal, WithdrawalID: wid}.ID())
	assert.Equal(t, 10, Params{}.withDefaults().ExecutablePerAccount, "a run takes at most ten of one account's subjects by default")
	assert.Equal(t, 50, selectionLimit(0, 50))
	assert.Equal(t, 50, selectionLimit(maxSelection+1, 50))
	assert.Equal(t, maxSelection, selectionLimit(maxSelection, 50))
}
