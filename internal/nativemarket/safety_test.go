package nativemarket

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func bpsPtr(v money.BPS) *money.BPS { return &v }

// TestConservativeSafetyPolicy_IsComplete: the compiled-in policy is a real
// policy. A deployment that has recorded nothing runs it, so an incomplete one
// would mean "no limits" dressed as "the safe default".
func TestConservativeSafetyPolicy_IsComplete(t *testing.T) {
	t.Parallel()
	p := ConservativeSafetyPolicy()
	assert.Empty(t, p.Missing(), "the conservative policy must set every limit")
	require.NoError(t, p.Validate())
	assert.Equal(t, ConservativeSafetyVersion, p.Version)
	assert.False(t, p.BreakerEnabled(),
		"the compiled-in policy leaves the breaker disarmed on purpose; see ConservativeSafetyPolicy")
	assert.Len(t, p.Hash(), 64)
}

// TestSafetyPolicy_MissingLimitIsRefused: an absent limit is not a permissive
// one. Every one of them, one at a time.
func TestSafetyPolicy_MissingLimitIsRefused(t *testing.T) {
	t.Parallel()
	clear := map[string]func(*SafetyPolicy){
		"max_price_impact_bps":           func(p *SafetyPolicy) { p.MaxPriceImpactBPS = nil },
		"max_slippage_bps":               func(p *SafetyPolicy) { p.MaxSlippageBPS = nil },
		"circuit_breaker_move_bps":       func(p *SafetyPolicy) { p.CircuitBreakerMoveBPS = nil },
		"circuit_breaker_window_seconds": func(p *SafetyPolicy) { p.CircuitBreakerWindowSeconds = nil },
		"min_opening_liquidity_credits":  func(p *SafetyPolicy) { p.MinOpeningLiquidityCredits = nil },
		"creator_may_buy_own_asset":      func(p *SafetyPolicy) { p.CreatorMayBuyOwnAsset = nil },
	}
	for name, drop := range clear {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := ConservativeSafetyPolicy()
			drop(&p)
			require.Equal(t, []string{name}, p.Missing())
			err := p.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// TestSafetyPolicy_HashChangesWithEveryLimit: a policy hash that did not move
// when a limit moved would let two different documents claim the same identity,
// which is the whole point of recording one.
func TestSafetyPolicy_HashChangesWithEveryLimit(t *testing.T) {
	t.Parallel()
	base := ConservativeSafetyPolicy()
	baseHash := base.Hash()

	mutate := []func(*SafetyPolicy){
		func(p *SafetyPolicy) { p.MaxPriceImpactBPS = bpsPtr(1) },
		func(p *SafetyPolicy) { p.MaxSlippageBPS = bpsPtr(1) },
		func(p *SafetyPolicy) { p.CircuitBreakerMoveBPS = bpsPtr(7_777) },
		func(p *SafetyPolicy) { v := 1; p.CircuitBreakerWindowSeconds = &v },
		func(p *SafetyPolicy) { v := money.QuantityFromInt64(1); p.MinOpeningLiquidityCredits = &v },
		func(p *SafetyPolicy) { v := false; p.CreatorMayBuyOwnAsset = &v },
	}
	seen := map[string]bool{baseHash: true}
	for i, f := range mutate {
		p := ConservativeSafetyPolicy()
		f(&p)
		h := p.Hash()
		assert.NotEqual(t, baseHash, h, "mutation %d did not change the hash", i)
		assert.False(t, seen[h], "mutation %d collides with an earlier document", i)
		seen[h] = true
	}

	// The version is NOT part of the hash: two deployments recording the same
	// limits under different version names have recorded the same document.
	renamed := ConservativeSafetyPolicy()
	renamed.Version = "something-else"
	assert.Equal(t, baseHash, renamed.Hash())
}

// TestParseSafetyPolicy_RejectsWhatItShould.
func TestParseSafetyPolicy_RejectsWhatItShould(t *testing.T) {
	t.Parallel()
	canonical, err := ConservativeSafetyPolicy().CanonicalJSON()
	require.NoError(t, err)

	round, err := ParseSafetyPolicy(canonical)
	require.NoError(t, err, "the policy this code writes must be the policy this code reads")
	assert.Equal(t, ConservativeSafetyPolicy().Hash(), round.Hash())

	for name, body := range map[string]string{
		"not an object":   `[]`,
		"empty":           `{}`,
		"unknown field":   string(canonical[:len(canonical)-1]) + `,"max_wishes":3}`,
		"trailing data":   string(canonical) + `{}`,
		"bps out of band": `{"max_price_impact_bps":99999,"max_slippage_bps":1,"circuit_breaker_move_bps":1,"circuit_breaker_window_seconds":1,"min_opening_liquidity_credits":"0","creator_may_buy_own_asset":true}`,
		"zero window":     `{"max_price_impact_bps":1,"max_slippage_bps":1,"circuit_breaker_move_bps":1,"circuit_breaker_window_seconds":0,"min_opening_liquidity_credits":"0","creator_may_buy_own_asset":true}`,
		"numeric credits": `{"max_price_impact_bps":1,"max_slippage_bps":1,"circuit_breaker_move_bps":1,"circuit_breaker_window_seconds":1,"min_opening_liquidity_credits":0,"creator_may_buy_own_asset":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseSafetyPolicy(json.RawMessage(body))
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// TestPriceImpactBPS_IsTheMarketsMoveNotTheCallersCost, measured on the
// RESERVES rather than on two rendered prices.
//
// The marginal price of this curve is K/Y^2, so the move a fill makes is
// (Y/Y')^2 - 1 and the impact is |Y^2 - Y'^2| / Y'^2 in exact integers. Two
// prices at PriceScale are a rendering of the same fact and can lose all of it:
// on a market whose price truncates near zero they render identically and the
// difference is nothing, which is how a 12,500 basis point order passed a 9,000
// basis point ceiling (F-193).
func TestPriceImpactBPS_IsTheMarketsMoveNotTheCallersCost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		before int64
		after  int64
		want   money.BPS
	}{
		{"unmoved", 1_000, 1_000, 0},
		// A buy takes a tenth of the units: (1000/900)^2 - 1 = 23.4567%.
		{"a buy takes a tenth of the pool", 1_000, 900, 2_345},
		// A buy takes half of them, so the price quadruples.
		{"a buy takes half the pool", 1_000, 500, 30_000},
		// A sell doubles them, so the price falls to a quarter of itself. The
		// limit is on the SIZE of the move, so this is a magnitude.
		{"a sell doubles the pool", 1_000, 2_000, 7_500},
		// (1000/999)^2 - 1 = 0.2003%, and the basis points truncate.
		{"rounds down", 1_000, 999, 20},
		// Neither of these is a fill this package produces, and neither is
		// within limits: an impact that cannot be measured is refused.
		{"a fill that carries no reserves", 0, 0, unmeasurableBPS},
		{"a fill with no pool before it", 0, 5_000, unmeasurableBPS},
		{"a fill that emptied the pool", 5_000, 0, unmeasurableBPS},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := PriceImpactBPS(Fill{
				StateBefore: State{AssetReserve: money.QuantityFromInt64(c.before)},
				StateAfter:  State{AssetReserve: money.QuantityFromInt64(c.after)},
			})
			assert.Equal(t, c.want, got)
		})
	}
}

// TestTheSafetyReadersFailClosedOnAPriceTheyCannotMeasure.
//
// Three ratios divide by a price: the impact ceiling, the slippage ceiling and
// the circuit breaker. Each used to answer ZERO when its reference was zero,
// which reads as "this order is within limits" and "this market has not moved"
// -- on precisely the market where neither was known (F-193). Each now
// saturates, which refuses.
func TestTheSafetyReadersFailClosedOnAPriceTheyCannotMeasure(t *testing.T) {
	t.Parallel()
	tiny := money.QuantityFromInt64(MinSpotUnits - 1)
	ok := money.QuantityFromInt64(MinSpotUnits)

	assert.Equal(t, unmeasurableBPS, Fill{SpotBefore: money.Quantity{}}.SlippageBPS())
	assert.Equal(t, unmeasurableBPS, Fill{SpotBefore: tiny, EffectivePrice: tiny}.SlippageBPS())
	assert.Equal(t, money.BPS(0), Fill{SpotBefore: ok, EffectivePrice: ok}.SlippageBPS(),
		"a price at the floor is measurable, and an order that fills at it has no slippage")

	assert.Equal(t, unmeasurableBPS, moveBPS(money.Quantity{}, money.QuantityFromInt64(5_000_000)))
	assert.Equal(t, unmeasurableBPS, moveBPS(tiny, tiny))
	assert.Equal(t, money.BPS(0), moveBPS(ok, ok))
}

// TestChangeBPS_KeepsItsSign: a market that fell 20% and one that rose 20% are
// not the same market, so the list's sort key and the response's figure are
// signed where the safety limit's is not.
func TestChangeBPS_KeepsItsSign(t *testing.T) {
	t.Parallel()
	up := changeBPS(money.QuantityFromInt64(1_000), money.QuantityFromInt64(1_200))
	down := changeBPS(money.QuantityFromInt64(1_000), money.QuantityFromInt64(800))
	assert.Equal(t, money.BPS(2_000), up)
	assert.Equal(t, money.BPS(-2_000), down)
	assert.Equal(t, money.BPS(0), changeBPS(money.Quantity{}, money.QuantityFromInt64(5)))

	// moveBPS is the same measurement without the sign, on prices a market may
	// actually have: a reference below MinSpotUnits saturates rather than
	// reporting a move, which changeBPS does not do because it is a figure on a
	// screen and not a control (TestTheSafetyReadersFailClosed...).
	assert.Equal(t, money.BPS(2_000),
		moveBPS(money.QuantityFromInt64(1_000_000), money.QuantityFromInt64(800_000)))
}

// TestCheckSafety_EachLimitAtItsBoundary: at the limit is permitted, one basis
// point past it is refused. An off-by-one here is a control that is not the
// control anybody wrote down.
func TestCheckSafety_EachLimitAtItsBoundary(t *testing.T) {
	t.Parallel()
	svc := &Service{}
	m := Market{ID: NewMarketID(), AssetID: newTestAssetID()}
	creator := newTestAccountID()
	trader := newTestAccountID()

	policy := ConservativeSafetyPolicy()
	policy.MaxPriceImpactBPS = bpsPtr(1_000)
	policy.MaxSlippageBPS = bpsPtr(10_000) // out of the way for the impact cases

	// One fill, read by two policies. The impact a fill makes is fixed by its
	// reserves and the interesting mistake is in the COMPARISON, so the fill
	// stays still and the limit moves across it by one basis point.
	//
	// Every price here is well above MinSpotUnits, because a price below it is
	// refused before a limit is ever consulted (TestCheckOpeningLiquidity).
	const onScreen = 10_000_000_000
	at := func(before, after int64) Fill {
		return Fill{
			Side:        Buy,
			StateBefore: State{AssetReserve: money.QuantityFromInt64(before)},
			StateAfter:  State{AssetReserve: money.QuantityFromInt64(after)},
			SpotBefore:  money.QuantityFromInt64(onScreen),
			SpotAfter:   money.QuantityFromInt64(onScreen),
			// Equal to the price on screen, so slippage is zero and only the
			// impact limit is under test.
			EffectivePrice: money.QuantityFromInt64(onScreen),
		}
	}
	r := ExecuteRequest{AccountID: trader, Side: Buy}
	moved := at(1_000, 900)
	require.Equal(t, money.BPS(2_345), PriceImpactBPS(moved))

	policy.MaxPriceImpactBPS = bpsPtr(2_345)
	require.NoError(t, svc.checkSafety(policy, m, r, moved, creator), "exactly at the limit is permitted")

	policy.MaxPriceImpactBPS = bpsPtr(2_344)
	err := svc.checkSafety(policy, m, r, moved, creator)
	require.Error(t, err)
	assert.Equal(t, errs.CodeVenueLiquidityInsufficient, errs.CodeOf(err))

	// Slippage, with impact out of the way: the reserves do not move, so the
	// only thing this fill can be refused for is where it filled.
	policy.MaxPriceImpactBPS = bpsPtr(10_000)
	policy.MaxSlippageBPS = bpsPtr(500)
	slip := func(effective int64) Fill {
		f := at(1_000, 1_000)
		f.EffectivePrice = money.QuantityFromInt64(effective)
		return f
	}
	require.NoError(t, svc.checkSafety(policy, m, r, slip(10_500_000_000), creator))
	err = svc.checkSafety(policy, m, r, slip(10_501_000_000), creator)
	require.Error(t, err)
	assert.Equal(t, errs.CodeVenueLiquidityInsufficient, errs.CodeOf(err))

	// A SELL is never refused by either limit. Both numbers describe the cost
	// of the caller's own size, and refusing an exit traps a holder.
	sell := ExecuteRequest{AccountID: trader, Side: Sell}
	sellFill := slip(99_999_000_000)
	sellFill.StateAfter = State{AssetReserve: money.QuantityFromInt64(4_000)}
	sellFill.Side = Sell
	require.NoError(t, svc.checkSafety(policy, m, sell, sellFill, creator),
		"a sell is never refused for its own price impact")
}

// TestCheckSafety_CreatorSelfBuyIsAPolicyChoice: permitted by the conservative
// policy (surveillance reports it instead), refused when a deployment records a
// policy that says so.
func TestCheckSafety_CreatorSelfBuyIsAPolicyChoice(t *testing.T) {
	t.Parallel()
	svc := &Service{}
	creator := newTestAccountID()
	m := Market{ID: NewMarketID(), AssetID: newTestAssetID()}
	fill := Fill{
		Side:        Buy,
		StateBefore: State{AssetReserve: money.QuantityFromInt64(1_000)},
		StateAfter:  State{AssetReserve: money.QuantityFromInt64(1_000)},
		SpotBefore:  money.QuantityFromInt64(1_000_000_000),
		SpotAfter:   money.QuantityFromInt64(1_000_000_000),
		// Nothing moved and nothing slipped, so the only limit that can refuse
		// this fill is the one under test.
		EffectivePrice: money.QuantityFromInt64(1_000_000_000),
	}
	buy := ExecuteRequest{AccountID: creator, Side: Buy}
	sell := ExecuteRequest{AccountID: creator, Side: Sell}

	permissive := ConservativeSafetyPolicy()
	require.NoError(t, svc.checkSafety(permissive, m, buy, fill, creator),
		"the conservative policy exposes creator self-dealing rather than preventing it")

	strict := ConservativeSafetyPolicy()
	no := false
	strict.CreatorMayBuyOwnAsset = &no
	err := svc.checkSafety(strict, m, buy, fill, creator)
	require.Error(t, err)
	assert.Equal(t, errs.CodeAssetRestricted, errs.CodeOf(err))

	// Even under the strict policy the creator may always LEAVE.
	sellFill := fill
	sellFill.Side = Sell
	require.NoError(t, svc.checkSafety(strict, m, sell, sellFill, creator),
		"a creator's own allocation must never be trapped")

	// And somebody who is not the creator is unaffected.
	other := ExecuteRequest{AccountID: newTestAccountID(), Side: Buy}
	require.NoError(t, svc.checkSafety(strict, m, other, fill, creator))
}

// TestCheckOpeningLiquidity_AtTheBoundary.
func TestCheckOpeningLiquidity_AtTheBoundary(t *testing.T) {
	t.Parallel()
	p := ConservativeSafetyPolicy()
	floor := *p.MinOpeningLiquidityCredits

	req := func(v money.Quantity) CreateRequest { return CreateRequest{VirtualCreditReserve: v} }
	require.NoError(t, checkOpeningLiquidity(p, req(floor)), "exactly the floor is permitted")
	require.NoError(t, checkOpeningLiquidity(p, req(floor.Add(money.QuantityFromInt64(1)))))

	err := checkOpeningLiquidity(p, req(floor.Sub(money.QuantityFromInt64(1))))
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// TestCheckOpeningLiquidity_RefusesAPriceTooSmallToMeasure.
//
// The depth floor bounds the reserve and says nothing about the supply it is
// spread over, so a market clearing it by a wide margin could still open at a
// marginal price of zero and stay there for its whole life (F-193). The second
// bound is on the price the depth produces, and it is compiled in rather than
// policy, because a deployment may decide how deep a market must be and may not
// decide that a price of zero is measurable.
func TestCheckOpeningLiquidity_RefusesAPriceTooSmallToMeasure(t *testing.T) {
	t.Parallel()
	p := ConservativeSafetyPolicy()
	v := *p.MinOpeningLiquidityCredits

	// At the reserve floor, 10^21 units price at V*10^18/Y0 = 10^6 exactly,
	// which is MinSpotUnits: a thousand tokens of an eighteen-decimal asset.
	y0 := qs("1000000000000000000000")
	require.NoError(t, checkOpeningLiquidity(p, CreateRequest{VirtualCreditReserve: v, PoolSupply: y0}),
		"exactly the smallest measurable price is permitted")

	// One more unit of supply and the price falls below it.
	tooMany := y0.Add(money.QuantityFromInt64(1))
	err := checkOpeningLiquidity(p, CreateRequest{VirtualCreditReserve: v, PoolSupply: tooMany})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// The market the audit opened: ten billion units of an eighteen-decimal
	// asset on the reserve floor, whose price truncated to zero.
	err = checkOpeningLiquidity(p, CreateRequest{
		VirtualCreditReserve: v, PoolSupply: qs("10000000000000000000000000000"),
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// And the ordinary six-decimal launch is nowhere near it: a billion tokens
	// at six decimals on the same reserve opens at 10^12.
	require.NoError(t, checkOpeningLiquidity(p, CreateRequest{
		VirtualCreditReserve: v, PoolSupply: qs("1000000000000000"),
	}))
}

// TestBreakerPauseStatus_LetsHoldersOut: the breaker pauses to CLOSE_ONLY, not
// HALTED, and the market's own transition table permits the move from both
// states a live market can be in.
func TestBreakerPauseStatus_LetsHoldersOut(t *testing.T) {
	t.Parallel()
	assert.Equal(t, StatusCloseOnly, BreakerPauseStatus)
	assert.True(t, BreakerPauseStatus.Accepts(Sell), "a paused market must still let holders leave")
	assert.False(t, BreakerPauseStatus.Accepts(Buy), "a paused market must stop new exposure")
	assert.True(t, CanTransition(StatusActive, BreakerPauseStatus))
	assert.True(t, CanTransition(BreakerPauseStatus, StatusActive),
		"resuming is an operator decision the table must permit")
}

// newTestAccountID and newTestAssetID mint ids of the right TYPE. The values
// never reach a database in this file; what matters is that two of them differ
// and that the creator check compares the same type it is given.
func newTestAccountID() accounts.AccountID {
	id, err := accounts.ParseAccountID(NewMarketID().String())
	if err != nil {
		panic(err)
	}
	return id
}

func newTestAssetID() assets.AssetID {
	id, err := assets.ParseAssetID(NewMarketID().String())
	if err != nil {
		panic(err)
	}
	return id
}
