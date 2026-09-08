package risk

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
)

// The two limits PART XXXII names for the Nodal-native economy.
//
// Neither has a USD term. A Credit has no approved external value, so a USD
// limit on a native position would need an exchange rate nobody set (PART LIV),
// and both limits are ratios instead: units held against every unit that
// exists, and Credits committed to one creator against every Credit this
// account has committed or could still commit.

func bpsPtr(v money.BPS) *money.BPS { return &v }

func TestNativeConcentration_HoldingTooMuchOfOneMarketIsRefused(t *testing.T) {
	t.Parallel()
	e := evaluator{
		p:  Policy{MaxNativeMarketConcentrationBPS: bpsPtr(2000)}, // 20%
		rs: reasonSet{},
		in: Input{NativeMarket: &NativeMarketSnapshot{
			HoldingAfter: money.QuantityFromInt64(2_001),
			TotalSupply:  money.QuantityFromInt64(10_000),
		}},
	}
	e.nativeMarketChecks()
	assert.Contains(t, e.rs.sorted(), ReasonNativeMarketConcentration,
		"20.01% of an asset's total supply is over a 20% limit")

	// Exactly at the limit is allowed. A limit is a ceiling, not a fence one
	// unit short of it, and an off-by-one here would refuse the holder who did
	// exactly what the policy permits.
	atLimit := evaluator{
		p:  Policy{MaxNativeMarketConcentrationBPS: bpsPtr(2000)},
		rs: reasonSet{},
		in: Input{NativeMarket: &NativeMarketSnapshot{
			HoldingAfter: money.QuantityFromInt64(2_000),
			TotalSupply:  money.QuantityFromInt64(10_000),
		}},
	}
	atLimit.nativeMarketChecks()
	assert.Empty(t, atLimit.rs.sorted(), "exactly 20% of 10,000 is not over 20%")
}

func TestNativeConcentration_TooMuchOfOneCreatorIsRefused(t *testing.T) {
	t.Parallel()
	e := evaluator{
		p:  Policy{MaxCreatorConcentrationBPS: bpsPtr(3000)}, // 30%
		rs: reasonSet{},
		in: Input{NativeMarket: &NativeMarketSnapshot{
			SpendOnThisCreatorAfter: money.QuantityFromInt64(4_000),
			CreditBaseAfter:         money.QuantityFromInt64(10_000),
		}},
	}
	e.nativeMarketChecks()
	assert.Contains(t, e.rs.sorted(), ReasonCreatorConcentration)

	ok := evaluator{
		p:  Policy{MaxCreatorConcentrationBPS: bpsPtr(3000)},
		rs: reasonSet{},
		in: Input{NativeMarket: &NativeMarketSnapshot{
			SpendOnThisCreatorAfter: money.QuantityFromInt64(3_000),
			CreditBaseAfter:         money.QuantityFromInt64(10_000),
		}},
	}
	ok.nativeMarketChecks()
	assert.Empty(t, ok.rs.sorted())
}

// TestNativeConcentration_TheFirstBuyerOfANewMarketIsNotConcentrated: a zero
// denominator is not a violation. Reporting one would refuse the first buyer of
// every market ever created, which is a limit that stops the thing it is meant
// to permit.
func TestNativeConcentration_TheFirstBuyerOfANewMarketIsNotConcentrated(t *testing.T) {
	t.Parallel()
	for name, snap := range map[string]*NativeMarketSnapshot{
		"nothing outstanding": {
			HoldingAfter: money.QuantityFromInt64(0), TotalSupply: money.QuantityFromInt64(0),
			SpendOnThisCreatorAfter: money.QuantityFromInt64(0), CreditBaseAfter: money.QuantityFromInt64(0),
		},
		"holds nothing of a live market": {
			HoldingAfter: money.QuantityFromInt64(0), TotalSupply: money.QuantityFromInt64(1_000),
			SpendOnThisCreatorAfter: money.QuantityFromInt64(0), CreditBaseAfter: money.QuantityFromInt64(500),
		},
	} {
		e := evaluator{
			p: Policy{
				MaxNativeMarketConcentrationBPS: bpsPtr(1),
				MaxCreatorConcentrationBPS:      bpsPtr(1),
			},
			rs: reasonSet{},
			in: Input{NativeMarket: snap},
		}
		e.nativeMarketChecks()
		assert.Empty(t, e.rs.sorted(), name)
	}
}

// TestNativeConcentration_AnIntentThatIsNotANativeTradeIsUnaffected: the
// snapshot is absent for every other kind of intent, and its absence must not
// be read as a violation or as a pass that hides one.
func TestNativeConcentration_AnIntentThatIsNotANativeTradeIsUnaffected(t *testing.T) {
	t.Parallel()
	e := evaluator{
		p: Policy{
			MaxNativeMarketConcentrationBPS: bpsPtr(1),
			MaxCreatorConcentrationBPS:      bpsPtr(1),
		},
		rs: reasonSet{},
		in: Input{NativeMarket: nil},
	}
	e.nativeMarketChecks()
	assert.Empty(t, e.rs.sorted())
}

// TestNativeConcentration_TheComparisonIsExact: the share is compared as
// part*10000 > whole*limit, so there is no division and no rounding step to
// argue about. This checks a ratio that no float could represent exactly.
func TestNativeConcentration_TheComparisonIsExact(t *testing.T) {
	t.Parallel()
	// 1/3 of the supply against a 3333 bps limit: 33.33% is over 33.33% only
	// if the arithmetic rounds up, and it must not.
	assert.False(t, shareExceeds(
		money.QuantityFromInt64(3_333), money.QuantityFromInt64(10_000), 3333,
	))
	assert.True(t, shareExceeds(
		money.QuantityFromInt64(3_334), money.QuantityFromInt64(10_000), 3333,
	))

	// Quantities far beyond int64, which is what a native asset's supply
	// actually looks like: 1e15 base units is a routine max supply.
	huge, err := money.ParseQuantity("1000000000000000000000000000")
	require.NoError(t, err)
	half, err := money.ParseQuantity("500000000000000000000000000")
	require.NoError(t, err)
	assert.False(t, shareExceeds(half, huge, 5000), "exactly half is not over 50%")
	assert.True(t, shareExceeds(half, huge, 4999))
}

// TestEvaluateNativeTrade_FailsClosedOnAnIncompletePolicy: a deployment that
// has not set these limits has not decided that any amount is fine.
//
// The missing names go in the evaluator version because Decision has nowhere
// else to put them, and a refusal that does not say WHICH limit is absent
// sends somebody to read the whole policy.
func TestEvaluateNativeTrade_FailsClosedOnAnIncompletePolicy(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	snap := NativeMarketSnapshot{
		HoldingAfter: money.QuantityFromInt64(1), TotalSupply: money.QuantityFromInt64(1_000_000),
		SpendOnThisCreatorAfter: money.QuantityFromInt64(1), CreditBaseAfter: money.QuantityFromInt64(1_000_000),
	}
	in := NativeTradeInput("", snap, now)

	for name, p := range map[string]Policy{
		"nothing set":            {},
		"only the market limit":  {MaxNativeMarketConcentrationBPS: bpsPtr(2000)},
		"only the creator limit": {MaxCreatorConcentrationBPS: bpsPtr(3000)},
	} {
		d := EvaluateNativeTrade(p, in)
		assert.Equal(t, Reject, d.Verdict, name)
		assert.Equal(t, []string{ReasonPolicyMissing}, d.ReasonCodes, name)
		assert.Contains(t, d.EvaluatorVersion, "missing:", name)
	}

	// Both set, and a position well inside them: allowed, and it says so with
	// a hash somebody can check.
	complete := Policy{
		Version:                         "v1",
		MaxNativeMarketConcentrationBPS: bpsPtr(2000),
		MaxCreatorConcentrationBPS:      bpsPtr(3000),
	}
	d := EvaluateNativeTrade(complete, in)
	assert.Equal(t, Allow, d.Verdict)
	assert.Empty(t, d.ReasonCodes)
	assert.Equal(t, StagePreTrade, d.Stage)
	assert.Equal(t, "v1", d.PolicyVersion)
	assert.Len(t, d.InputHash, 64, "the decision must name the input it was computed from")
	assert.Equal(t, d.ComputeHash(), d.Hash, "the determinism witness must verify")
}

// TestEvaluateNativeTrade_RefusesAnInputItCannotEvaluate: this entry point can
// answer exactly one question. Asked anything else it must refuse rather than
// answer ALLOW, which is what a nil snapshot or a zero clock would otherwise
// get: every check skipped, no reasons, verdict ALLOW.
func TestEvaluateNativeTrade_RefusesAnInputItCannotEvaluate(t *testing.T) {
	t.Parallel()
	p := Policy{
		MaxNativeMarketConcentrationBPS: bpsPtr(2000),
		MaxCreatorConcentrationBPS:      bpsPtr(3000),
	}
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	noSnapshot := EvaluateNativeTrade(p, Input{Stage: StagePreTrade, Now: now})
	assert.Equal(t, Reject, noSnapshot.Verdict)
	assert.Equal(t, []string{ReasonInputInvalid}, noSnapshot.ReasonCodes)

	noClock := EvaluateNativeTrade(p, NativeTradeInput("", NativeMarketSnapshot{}, time.Time{}))
	assert.Equal(t, Reject, noClock.Verdict)
	assert.Equal(t, []string{ReasonInputInvalid}, noClock.ReasonCodes)
}
