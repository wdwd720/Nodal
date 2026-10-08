package risk

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/money"
)

var advNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func advUSD(minor int64) money.USD { return money.USDFromMinor(minor) }

// openPosition is a well-formed, allowable risk-increasing intent with every
// section a careful caller supplies.
func openPosition() AdvancedInput {
	return AdvancedInput{
		Stage: StagePreTrade,
		Intent: AdvancedIntent{
			ID: "0193b2e0-0000-7000-8000-000000000001", Kind: "OPEN_POSITION",
			AccountID: "0193b2e0-0000-7000-8000-0000000000a1", ActorType: "USER",
			Capability: "DERIVATIVES", Mode: "PAPER", InstrumentID: "0193b2e0-0000-7000-8000-0000000000c1",
			InstrumentKind: "PERPETUAL", NotionalUSD: advUSD(1_000_00), RiskIncreasing: true,
		},
		Identity:   &IdentitySnapshot{Verified: true},
		Account:    &AdvancedAccountSnapshot{Status: "ACTIVE"},
		Instrument: &InstrumentSnapshot{Status: "ACTIVE", SessionOpen: true},
		Margin: &MarginSnapshot{
			EquityAfter: advUSD(10_000_00), InitialRequiredAfter: advUSD(1_000_00),
			MaintenanceRequiredAfter: advUSD(500_00), LeverageAfterBPS: 10_000,
		},
		Oracle: &OracleSnapshot{Available: true, ObservedAt: advNow.Add(-time.Second)},
		Now:    advNow,
	}
}

func TestAdvanced_EveryCategoryHasExactlyOneCheck(t *testing.T) {
	seen := map[AdvancedCategory]int{}
	for _, c := range advancedChecks {
		seen[c.category]++
	}
	for _, c := range AllAdvancedCategories() {
		assert.Equal(t, 1, seen[c], "category %s", c)
	}
	assert.Len(t, seen, len(AllAdvancedCategories()))
	assert.Len(t, AllAdvancedCategories(), 21)
}

func TestAdvanced_AllowsAWellFormedIntent(t *testing.T) {
	d := EvaluateAdvanced(DefaultAdvancedPolicy(), openPosition())
	require.Equal(t, Allow, d.Verdict, "reasons=%v missing=%v", d.ReasonCodes, d.MissingSections)
	assert.Empty(t, d.ReasonCodes)
	assert.Equal(t, d.Hash, d.ComputeHash())
	assert.Len(t, d.Checks, 21, "every category reports, applied or not")
	assert.NoError(t, d.RejectionError())
}

func TestAdvanced_MissingRequiredSectionsFailClosed(t *testing.T) {
	in := openPosition()
	in.Oracle = nil
	in.Identity = nil
	d := EvaluateAdvanced(DefaultAdvancedPolicy(), in)
	require.Equal(t, Reject, d.Verdict)
	assert.Contains(t, d.ReasonCodes, ReasonInputInvalid)
	assert.ElementsMatch(t, []string{SecIdentity, SecOracle}, d.MissingSections)

	// A transfer needs the destination and velocity sections.
	tr := AdvancedInput{Stage: StagePreTrade, Now: advNow,
		Intent:   AdvancedIntent{Kind: "TRANSFER_VALUE", AccountID: "a", ActorType: "USER", NotionalUSD: advUSD(100)},
		Identity: &IdentitySnapshot{Verified: true}, Account: &AdvancedAccountSnapshot{Status: "ACTIVE"},
		Balance: &BalanceSnapshot{Available: advUSD(1000), Required: advUSD(100)},
	}
	d = EvaluateAdvanced(DefaultAdvancedPolicy(), tr)
	require.Equal(t, Reject, d.Verdict)
	assert.ElementsMatch(t, []string{SecDestination, SecVelocity}, d.MissingSections)

	// An agent intent needs its authority.
	ag := openPosition()
	ag.Intent.ActorType = "AGENT"
	ag.Intent.AgentID = "0193b2e0-0000-7000-8000-0000000000e1"
	d = EvaluateAdvanced(DefaultAdvancedPolicy(), ag)
	require.Equal(t, Reject, d.Verdict)
	assert.Contains(t, d.MissingSections, SecAuthority)
}

// TestAdvanced_StaleMarkBlocksNewRiskButNotReduction is goal §6 "stale marks
// block new leverage" and §7 "stale oracle prevents risk-increasing derivative
// order", with the deliberate other half: closing on a stale mark stays open.
func TestAdvanced_StaleMarkBlocksNewRiskButNotReduction(t *testing.T) {
	stale := openPosition()
	stale.Oracle.ObservedAt = advNow.Add(-time.Hour)
	d := EvaluateAdvanced(DefaultAdvancedPolicy(), stale)
	require.Equal(t, Reject, d.Verdict)
	assert.Contains(t, d.ReasonCodes, ReasonOracleStale)

	unavailable := openPosition()
	unavailable.Oracle.Available = false
	d = EvaluateAdvanced(DefaultAdvancedPolicy(), unavailable)
	assert.Contains(t, d.ReasonCodes, ReasonOracleUnavailable)

	future := openPosition()
	future.Oracle.ObservedAt = advNow.Add(time.Minute)
	d = EvaluateAdvanced(DefaultAdvancedPolicy(), future)
	assert.Contains(t, d.ReasonCodes, ReasonOracleStale, "a price from the future is not fresh")

	closing := openPosition()
	closing.Intent.Kind = "CLOSE_POSITION"
	closing.Intent.RiskIncreasing = false
	closing.Oracle.ObservedAt = advNow.Add(-time.Hour)
	closing.Oracle.Available = false
	d = EvaluateAdvanced(DefaultAdvancedPolicy(), closing)
	assert.Equal(t, Allow, d.Verdict, "reasons=%v", d.ReasonCodes)
}

func TestAdvanced_MarginAndLeverage(t *testing.T) {
	under := openPosition()
	under.Margin.EquityAfter = advUSD(900_00)
	d := EvaluateAdvanced(DefaultAdvancedPolicy(), under)
	assert.Contains(t, d.ReasonCodes, ReasonMarginInitial)

	levered := openPosition()
	levered.Margin.LeverageAfterBPS = 60_000
	d = EvaluateAdvanced(DefaultAdvancedPolicy(), levered)
	assert.Contains(t, d.ReasonCodes, ReasonLeverageCap)

	// A withdrawal that would leave equity below maintenance is refused,
	// whatever else is true (goal §6).
	w := AdvancedInput{Stage: StagePreTrade, Now: advNow,
		Intent:   AdvancedIntent{Kind: "WITHDRAW", AccountID: "a", ActorType: "USER", NotionalUSD: advUSD(100_00)},
		Identity: &IdentitySnapshot{Verified: true}, Account: &AdvancedAccountSnapshot{Status: "ACTIVE"},
		Balance:  &BalanceSnapshot{Available: advUSD(1_000_00), Required: advUSD(100_00)},
		Velocity: &VelocitySnapshot{}, Destination: &DestinationSnapshot{Allowlisted: true, AddedAt: advNow.Add(-24 * time.Hour)},
		Chain: &ChainSnapshot{Healthy: true}, Settlement: &SettlementSnapshot{},
		Margin: &MarginSnapshot{EquityAfter: advUSD(400_00), MaintenanceRequiredAfter: advUSD(500_00), InitialRequiredAfter: advUSD(800_00)},
	}
	d = EvaluateAdvanced(DefaultAdvancedPolicy(), w)
	require.Equal(t, Reject, d.Verdict)
	assert.Contains(t, d.ReasonCodes, ReasonWithdrawBreachesMargin)
	w.Margin.EquityAfter = advUSD(600_00)
	d = EvaluateAdvanced(DefaultAdvancedPolicy(), w)
	assert.Equal(t, Allow, d.Verdict, "reasons=%v", d.ReasonCodes)
}

func TestAdvanced_AuthorityAndSandbox(t *testing.T) {
	ag := openPosition()
	ag.Intent.ActorType = "AGENT"
	ag.Intent.AgentID = "0193b2e0-0000-7000-8000-0000000000e1"
	ag.Authority = &AuthoritySnapshot{Present: true, WithinLimits: true, InScope: true}
	require.Equal(t, Allow, EvaluateAdvanced(DefaultAdvancedPolicy(), ag).Verdict)

	for name, mut := range map[string]func(*AuthoritySnapshot){
		ReasonAuthorityExpired: func(a *AuthoritySnapshot) { a.Expired = true },
		ReasonAuthorityRevoked: func(a *AuthoritySnapshot) { a.Revoked = true },
		ReasonAuthorityLimit:   func(a *AuthoritySnapshot) { a.WithinLimits = false },
		ReasonAuthorityScope:   func(a *AuthoritySnapshot) { a.InScope = false },
		ReasonAuthorityMissing: func(a *AuthoritySnapshot) { a.Present = false },
	} {
		in := ag
		a := *ag.Authority
		mut(&a)
		in.Authority = &a
		d := EvaluateAdvanced(DefaultAdvancedPolicy(), in)
		assert.Contains(t, d.ReasonCodes, name)
	}

	sb := openPosition()
	sb.Intent.SandboxOriginated = true
	sb.Sandbox = &SandboxSnapshot{ArtifactVerified: false, Violations: 1}
	d := EvaluateAdvanced(DefaultAdvancedPolicy(), sb)
	assert.Contains(t, d.ReasonCodes, ReasonSandboxArtifactUnverified)
	assert.Contains(t, d.ReasonCodes, ReasonSandboxViolation)
}

func TestAdvanced_KillSwitchesAndBreakers(t *testing.T) {
	in := openPosition()
	in.KillSwitches = []KillSwitch{{Kind: KillCapabilityDisable, Scope: "DERIVATIVES"}}
	assert.Contains(t, EvaluateAdvanced(DefaultAdvancedPolicy(), in).ReasonCodes, ReasonKillSwitch)

	in.KillSwitches = []KillSwitch{{Kind: KillCapabilityDisable, Scope: "MARGIN"}}
	assert.Equal(t, Allow, EvaluateAdvanced(DefaultAdvancedPolicy(), in).Verdict, "another capability's switch")

	reduce := openPosition()
	reduce.Intent.Kind = "CLOSE_POSITION"
	reduce.Intent.RiskIncreasing = false
	reduce.KillSwitches = []KillSwitch{{Kind: KillCapabilityDisable, Scope: "DERIVATIVES"}, {Kind: KillGlobalNewRisk, Scope: KillScopeAll}}
	assert.Equal(t, Allow, EvaluateAdvanced(DefaultAdvancedPolicy(), reduce).Verdict, "closing is never blocked by a new-risk switch")
	reduce.KillSwitches = []KillSwitch{{Kind: KillInstrumentHalt, Scope: reduce.Intent.InstrumentID}}
	assert.Equal(t, Reject, EvaluateAdvanced(DefaultAdvancedPolicy(), reduce).Verdict, "a halt stops everything on the instrument")

	cb := openPosition()
	cb.CircuitBreakers = []string{"PEG_DEVIATION"}
	assert.Contains(t, EvaluateAdvanced(DefaultAdvancedPolicy(), cb).ReasonCodes, ReasonCircuitBreaker)
}

func TestAdvanced_VelocityAndDestination(t *testing.T) {
	p := DefaultAdvancedPolicy()
	p.MinDestinationAgeMS = int64(time.Hour / time.Millisecond)
	tr := AdvancedInput{Stage: StagePreTrade, Now: advNow,
		Intent:   AdvancedIntent{Kind: "TRANSFER_VALUE", AccountID: "a", ActorType: "USER", NotionalUSD: advUSD(100)},
		Identity: &IdentitySnapshot{Verified: true}, Account: &AdvancedAccountSnapshot{Status: "ACTIVE"},
		Balance:     &BalanceSnapshot{Available: advUSD(1000), Required: advUSD(100)},
		Velocity:    &VelocitySnapshot{CountInWindow: p.VelocityMaxCount},
		Destination: &DestinationSnapshot{Allowlisted: true, AddedAt: advNow.Add(-time.Minute)},
	}
	d := EvaluateAdvanced(p, tr)
	assert.Contains(t, d.ReasonCodes, ReasonVelocity)
	assert.Contains(t, d.ReasonCodes, ReasonDestinationTooNew)
	tr.Destination.Allowlisted = false
	assert.Contains(t, EvaluateAdvanced(p, tr).ReasonCodes, ReasonDestinationNotAllowed)
}

// TestProp_AdvancedDeterminism is goal §31 "Deterministic risk": same input
// snapshot + policy version => same decision, regardless of the order in which
// unordered inputs were assembled.
func TestProp_AdvancedDeterminism(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := genAdvancedInput(rt)
		d1 := EvaluateAdvanced(DefaultAdvancedPolicy(), in)
		shuffled := in
		shuffled.KillSwitches = reverseKS(in.KillSwitches)
		shuffled.CircuitBreakers = reverseStrings(in.CircuitBreakers)
		d2 := EvaluateAdvanced(DefaultAdvancedPolicy(), shuffled)
		if d1.Hash != d2.Hash || d1.InputHash != d2.InputHash {
			rt.Fatalf("decision depends on input order")
		}
		if (d1.Verdict == Allow) != (len(d1.ReasonCodes) == 0) {
			rt.Fatalf("verdict %s with reasons %v", d1.Verdict, d1.ReasonCodes)
		}
		if d1.Hash != d1.ComputeHash() {
			rt.Fatalf("decision hash does not verify")
		}
		known := map[string]bool{}
		for _, c := range AdvancedReasonCodes() {
			known[c] = true
		}
		for _, c := range d1.ReasonCodes {
			if !known[c] {
				rt.Fatalf("undeclared reason code %s", c)
			}
		}
	})
}

// TestProp_StaleOrMissingOracleNeverAllowsNewRisk.
func TestProp_StaleOrMissingOracleNeverAllowsNewRisk(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := genAdvancedInput(rt)
		in.Intent.Kind = rapid.SampledFrom([]string{"OPEN_POSITION", "ACQUIRE_EXPOSURE", "BORROW", "CREATE_PREDICTION_POSITION"}).Draw(rt, "kind")
		in.Intent.RiskIncreasing = true
		switch rapid.IntRange(0, 2).Draw(rt, "oracle") {
		case 0:
			in.Oracle = nil
		case 1:
			in.Oracle = &OracleSnapshot{Available: false, ObservedAt: advNow}
		default:
			age := time.Duration(rapid.Int64Range(DefaultAdvancedPolicy().MaxOracleAgeMS+1, 1e9).Draw(rt, "age_ms")) * time.Millisecond
			in.Oracle = &OracleSnapshot{Available: true, ObservedAt: advNow.Add(-age)}
		}
		if EvaluateAdvanced(DefaultAdvancedPolicy(), in).Verdict == Allow {
			rt.Fatalf("new risk allowed without a fresh mark")
		}
	})
}

// TestProp_RiskIncreasingNeverLeavesEquityBelowMaintenance is goal §31
// "Margin: risk-increasing action cannot reduce required margin below
// maintenance" stated over the kernel's inputs.
func TestProp_RiskIncreasingNeverLeavesEquityBelowMaintenance(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := openPosition()
		maint := rapid.Int64Range(0, 1e9).Draw(rt, "maint")
		initial := maint + rapid.Int64Range(0, 1e9).Draw(rt, "im_extra")
		equity := rapid.Int64Range(-1e9, 3e9).Draw(rt, "equity")
		in.Margin = &MarginSnapshot{EquityAfter: advUSD(equity), InitialRequiredAfter: advUSD(initial),
			MaintenanceRequiredAfter: advUSD(maint), LeverageAfterBPS: money.BPS(rapid.Int64Range(0, 100_000).Draw(rt, "lev"))}
		d := EvaluateAdvanced(DefaultAdvancedPolicy(), in)
		if d.Verdict == Allow && equity < maint {
			rt.Fatalf("allowed with equity %d below maintenance %d", equity, maint)
		}
		if d.Verdict == Allow && equity < initial {
			rt.Fatalf("allowed with equity %d below initial %d", equity, initial)
		}
	})
}

func genAdvancedInput(rt *rapid.T) AdvancedInput {
	in := openPosition()
	in.Intent.NotionalUSD = advUSD(rapid.Int64Range(0, 1e12).Draw(rt, "notional"))
	in.Intent.RiskIncreasing = rapid.Bool().Draw(rt, "increasing")
	if rapid.Bool().Draw(rt, "drop_identity") {
		in.Identity = nil
	}
	n := rapid.IntRange(0, 4).Draw(rt, "kills")
	kinds := []string{KillGlobalNewRisk, KillCapabilityDisable, KillAccountFreeze, KillInstrumentHalt, KillAgentPause}
	for i := 0; i < n; i++ {
		in.KillSwitches = append(in.KillSwitches, KillSwitch{
			Kind:  rapid.SampledFrom(kinds).Draw(rt, "kind"),
			Scope: rapid.SampledFrom([]string{KillScopeAll, "DERIVATIVES", in.Intent.AccountID, "x"}).Draw(rt, "scope"),
		})
	}
	m := rapid.IntRange(0, 3).Draw(rt, "breakers")
	for i := 0; i < m; i++ {
		in.CircuitBreakers = append(in.CircuitBreakers, rapid.SampledFrom([]string{"PEG", "RESERVE", "LIQUIDATIONS"}).Draw(rt, "breaker"))
	}
	return in
}

func reverseKS(in []KillSwitch) []KillSwitch {
	out := make([]KillSwitch, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

func reverseStrings(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}
