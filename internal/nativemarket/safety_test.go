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

// TestPriceImpactBPS_IsTheMarketsMoveNotTheCallersCost.
func TestPriceImpactBPS_IsTheMarketsMoveNotTheCallersCost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		before int64
		after  int64
		want   money.BPS
	}{
		{"unmoved", 1_000, 1_000, 0},
		{"up ten percent", 1_000, 1_100, 1_000},
		{"down ten percent", 1_000, 900, 1_000},
		{"doubled", 1_000, 2_000, 10_000},
		{"rounds down", 1_000, 1_000 + 1, 10},
		{"no pre-trade price", 0, 5_000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := PriceImpactBPS(Fill{
				SpotBefore: money.QuantityFromInt64(c.before),
				SpotAfter:  money.QuantityFromInt64(c.after),
			})
			assert.Equal(t, c.want, got)
		})
	}
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

	// moveBPS is the same measurement without the sign.
	assert.Equal(t, money.BPS(2_000), moveBPS(money.QuantityFromInt64(1_000), money.QuantityFromInt64(800)))
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

	// A fill that moves the spot by exactly 1000 bps, and one that moves it by
	// 1001. EffectivePrice equals SpotBefore so slippage is zero either way.
	at := func(after int64) Fill {
		return Fill{
			Side: Buy, SpotBefore: money.QuantityFromInt64(10_000),
			SpotAfter: money.QuantityFromInt64(after), EffectivePrice: money.QuantityFromInt64(10_000),
		}
	}
	r := ExecuteRequest{AccountID: trader, Side: Buy}
	require.NoError(t, svc.checkSafety(policy, m, r, at(11_000), creator), "exactly at the limit is permitted")

	err := svc.checkSafety(policy, m, r, at(11_001), creator)
	require.Error(t, err)
	assert.Equal(t, errs.CodeVenueLiquidityInsufficient, errs.CodeOf(err))

	// Slippage, with impact out of the way.
	policy.MaxPriceImpactBPS = bpsPtr(10_000)
	policy.MaxSlippageBPS = bpsPtr(500)
	slip := func(effective int64) Fill {
		return Fill{
			Side: Buy, SpotBefore: money.QuantityFromInt64(10_000),
			SpotAfter: money.QuantityFromInt64(10_000), EffectivePrice: money.QuantityFromInt64(effective),
		}
	}
	require.NoError(t, svc.checkSafety(policy, m, r, slip(10_500), creator))
	err = svc.checkSafety(policy, m, r, slip(10_501), creator)
	require.Error(t, err)
	assert.Equal(t, errs.CodeVenueLiquidityInsufficient, errs.CodeOf(err))

	// A SELL is never refused by either limit. Both numbers describe the cost
	// of the caller's own size, and refusing an exit traps a holder.
	sell := ExecuteRequest{AccountID: trader, Side: Sell}
	sellFill := slip(99_999)
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
		Side: Buy, SpotBefore: money.QuantityFromInt64(1_000),
		SpotAfter: money.QuantityFromInt64(1_000), EffectivePrice: money.QuantityFromInt64(1_000),
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
