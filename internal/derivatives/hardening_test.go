package derivatives

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// TestMarkFreshnessIsOneInclusiveWindow: a mark is fresh from the moment it
// was observed until exactly the oracle max age later, and at no other time --
// the one test a close's price, a run's mark and the settlement rule share.
func TestMarkFreshnessIsOneInclusiveWindow(t *testing.T) {
	s := perpSpec(t)
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for age, fresh := range map[time.Duration]bool{
		0: true, time.Second: true, s.OracleMaxAge: true, s.OracleMaxAge + time.Nanosecond: false, -time.Nanosecond: false,
	} {
		assert.Equal(t, fresh, markFresh(s, Mark{ObservedAt: at.Add(-age)}, at), "age %s", age)
	}
}

// TestAMarkPricesOnlyOnTheCurrentTerms: a mark at another price scale, or off
// the tick grid, prices nothing.
func TestAMarkPricesOnlyOnTheCurrentTerms(t *testing.T) {
	s := perpSpec(t)
	assert.True(t, markPriceable(s, Mark{MarkMantissa: q(6_500_000), MarkScale: 2}))
	assert.False(t, markPriceable(s, Mark{MarkMantissa: q(6_500_000), MarkScale: 3}))
	assert.False(t, markPriceable(s, Mark{MarkMantissa: q(6_500_010), MarkScale: 2}))
}

// TestProp_AnOverrideBoundIsExactOnTheTickGrid: for any on-grid reference and
// bound, a price on the grid is within the bound exactly when it lies between
// the bounds the preview states -- the same arithmetic as the CHECK
// derivative_settlement_overrides_bounded.
func TestProp_AnOverrideBoundIsExactOnTheTickGrid(t *testing.T) {
	s := futureSpec(t)
	rapid.Check(t, func(t *rapid.T) {
		ref := q(rapid.Int64Range(0, 1<<40).Draw(t, "reference ticks")).Mul(s.Tick)
		bps := money.BPS(rapid.Int64Range(1, 10_000).Draw(t, "bps"))
		price := q(rapid.Int64Range(0, 1<<41).Draw(t, "price ticks")).Mul(s.Tick)
		lo, hi, err := overrideBounds(s, ref, bps)
		if err != nil {
			t.Fatal(err)
		}
		if !withinBound(lo, ref, bps) || !withinBound(hi, ref, bps) {
			t.Fatalf("the bounds %s..%s of %s at %d bps are not within it", lo, hi, ref, bps)
		}
		between := price.Cmp(lo) >= 0 && price.Cmp(hi) <= 0
		if got := withinBound(price, ref, bps); got != between {
			t.Fatalf("price %s, reference %s, %d bps: within=%t but between %s and %s=%t", price, ref, bps, got, lo, hi, between)
		}
	})
}

func TestAnOverrideDeviationIsRoundedUp(t *testing.T) {
	assert.Equal(t, money.BPS(0), deviationBPS(q(6_600_000), q(6_600_000)))
	assert.Equal(t, money.BPS(152), deviationBPS(q(6_700_000), q(6_600_000)), "151.5 bps reads as 152")
	assert.Equal(t, money.BPS(152), deviationBPS(q(6_500_000), q(6_600_000)))
	assert.Equal(t, money.BPS(10_000), deviationBPS(q(0), q(6_600_000)))
	assert.Equal(t, maxDivergenceBPS, deviationBPS(q(1), q(0)))
}

// TestACallerConfirmsTheSettlementPriceNeverChoosesIt: an unset confirmation
// confirms nothing; a set one must be exactly the price and source the rule
// gave.
func TestACallerConfirmsTheSettlementPriceNeverChoosesIt(t *testing.T) {
	price := q(6_500_000)
	require.NoError(t, confirmPrice(nil, "", price, "mark:a"))
	require.NoError(t, confirmPrice(&price, "mark:a", price, "mark:a"))
	other := q(9_000_000)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(confirmPrice(&other, "", price, "mark:a")))
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(confirmPrice(nil, "anything", price, "mark:a")))
}

// TestTheSettlementRuleIsStatedOnce: the statement archived with every
// settlement names the rule's every condition.
func TestTheSettlementRuleIsStatedOnce(t *testing.T) {
	for _, part := range []string{"EXPIRY_MARK v1", "observed at or before expiry", "recorded at or before expiry",
		"oracle max age", "half-even", "approved by a second"} {
		assert.Contains(t, SettlementRuleStatement, part)
	}
	assert.Equal(t, []SettlementPriceRule{RuleExpiryMark, RuleOperatorOverride}, AllSettlementPriceRules())
	assert.Equal(t, []OverrideDecision{DecisionApproved, DecisionRejected}, AllOverrideDecisions())
}
