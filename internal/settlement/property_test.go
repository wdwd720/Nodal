package settlement

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

// TestProp_ConstraintsNeverLooser: for any intent constraints and risk
// resulting constraints, the plan's hard constraints are never looser than
// either (SETTLEMENT_COMPILER §8).
func TestProp_ConstraintsNeverLooser(t *testing.T) {
	t.Parallel()
	planner := NewPlanner(DefaultOptions())
	rapid.Check(t, func(rt *rapid.T) {
		in := goldenBase()
		bps := func(name string) money.BPS { return money.BPS(rapid.Int64Range(0, 2000).Draw(rt, name)) }
		in.Intent.Constraints.MaxSlippageBPS = bps("intent_slippage")
		in.Intent.Constraints.MaxFeeBPS = bps("intent_fee")
		in.Intent.Constraints.MaxPriceImpactBPS = bps("intent_impact")
		in.Intent.Constraints.QuoteFreshness = time.Duration(rapid.Int64Range(0, 10_000).Draw(rt, "intent_freshness")) * time.Millisecond
		in.Risk.Constraints.MaxSlippageBPS = bps("risk_slippage")
		in.Risk.Constraints.MaxFeeBPS = bps("risk_fee")
		in.Risk.Constraints.MaxPriceImpactBPS = bps("risk_impact")
		in.Risk.Constraints.MaxQuoteAgeMS = rapid.Int64Range(0, 10_000).Draw(rt, "risk_quote_age")
		in.Risk.Constraints.MaxNotionalUSD = money.USDFromMinor(rapid.Int64Range(0, 200_000).Draw(rt, "risk_max_notional"))
		notional := money.USDFromMinor(rapid.Int64Range(100, 100_000).Draw(rt, "notional"))
		in.Intent.NotionalUSD = &notional
		in.Risk.EffectiveNotionalUSD = notional
		in.Intent.Deadline = in.Now.Add(time.Duration(rapid.Int64Range(0, 3600).Draw(rt, "deadline_s")) * time.Second)
		if rapid.Bool().Draw(rt, "sell") {
			in.Intent.Action = ActionReduceNotional
			in.Risk.ActionClass = risk.ClassReduceRisk
		}

		plan, err := planner.Plan(in)
		if err != nil {
			require.True(rt, errs.HasCode(err, errs.CodeNoValidPlan) || errs.HasCode(err, errs.CodeValidationFailed), "%v", err)
			return
		}
		hc := plan.HardConstraints
		check := func(name string, got, intent, riskBound money.BPS) {
			if intent > 0 {
				require.LessOrEqual(rt, got, intent, "%s looser than the intent", name)
			}
			if riskBound > 0 {
				require.LessOrEqual(rt, got, riskBound, "%s looser than the risk kernel", name)
			}
			if intent == 0 && riskBound == 0 {
				require.Equal(rt, money.BPS(0), got, "%s bounded by nobody", name)
			}
		}
		check("slippage", hc.MaxSlippageBPS, in.Intent.Constraints.MaxSlippageBPS, in.Risk.Constraints.MaxSlippageBPS)
		check("fee", hc.MaxFeeBPS, in.Intent.Constraints.MaxFeeBPS, in.Risk.Constraints.MaxFeeBPS)
		check("impact", hc.MaxPriceImpactBPS, in.Intent.Constraints.MaxPriceImpactBPS, in.Risk.Constraints.MaxPriceImpactBPS)
		if in.Intent.Constraints.QuoteFreshness > 0 {
			require.LessOrEqual(rt, hc.QuoteMaxAgeMS, in.Intent.Constraints.QuoteFreshness.Milliseconds())
		}
		if in.Risk.Constraints.MaxQuoteAgeMS > 0 {
			require.LessOrEqual(rt, hc.QuoteMaxAgeMS, in.Risk.Constraints.MaxQuoteAgeMS)
		}
		require.Greater(rt, hc.QuoteMaxAgeMS, int64(0), "quote age is always bounded")
		if in.Risk.Constraints.MaxNotionalUSD.IsPositive() {
			require.LessOrEqual(rt, hc.MaxNotionalUSD.Minor(), in.Risk.Constraints.MaxNotionalUSD.Minor())
		}
		require.LessOrEqual(rt, hc.MaxNotionalUSD.Minor(), hc.NotionalUSD.Minor(), "max notional never exceeds the trade")
		require.LessOrEqual(rt, hc.NotionalUSD.Minor(), notional.Minor(), "a plan never trades more than asked")
		require.False(rt, hc.Deadline.After(in.Intent.Deadline), "deadline never later than the intent's")
		require.True(rt, hc.MaxInputQuantity.IsPositive())
		if hc.Side == execution.SideSell {
			require.LessOrEqual(rt, hc.MaxInputQuantity.Cmp(openHolding(in, in.Assets[*in.Instrument.BaseAssetID].ID)), 0, "never sells more than held")
		}
		for _, s := range plan.Steps {
			require.False(rt, s.Type.Reserved())
		}
		ok, err := VerifyHash(plan)
		require.NoError(rt, err)
		require.True(rt, ok)
	})
}

// TestProp_SizingRoundTrip: the sized settlement quantity of an acquire at
// face value equals the notional at the asset's precision.
func TestProp_SizingFaceValue(t *testing.T) {
	t.Parallel()
	planner := NewPlanner(DefaultOptions())
	rapid.Check(t, func(rt *rapid.T) {
		in := goldenBase()
		minor := rapid.Int64Range(100, 100_000).Draw(rt, "notional")
		notional := money.USDFromMinor(minor)
		in.Intent.NotionalUSD = &notional
		in.Risk.EffectiveNotionalUSD = notional
		plan, err := planner.Plan(in)
		require.NoError(rt, err)
		want := money.QuantityFromInt64(minor * 10_000) // cents → micro-USDC
		require.True(rt, plan.HardConstraints.MaxInputQuantity.Equal(want), "%s != %s", plan.HardConstraints.MaxInputQuantity, want)
		require.Equal(rt, notional.String(), plan.HardConstraints.NotionalUSD.String())
	})
}
