package nativemarket

import (
	"math/big"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// The market model, stated once (gola.md PART XIV requires exactly one model,
// documented).
//
// # Constant product with a virtual credit reserve
//
// A market holds two reserves: Credits and units of the native asset. The
// invariant is
//
//	(V + R) * Y  >=  K,   where K = V * Y0
//
//	V  virtual credit reserve, fixed at activation, never held by anyone
//	R  real Credit reserve, the Credits users have actually paid in
//	Y  asset units still in the pool
//	Y0 asset units the pool started with
//
// The virtual reserve is what makes launch possible without a liquidity
// provider: at activation R = 0 and the pool still prices, because the curve
// behaves as though V Credits were present. Nobody can withdraw V, because
// every payout path draws on R.
//
// # Why this model and not the others
//
// PART XIV asks for one production-quality model, chosen on stated criteria.
//
//   - Deterministic pricing: price is a pure function of two integers. No
//     oracle, no clock, no external state.
//   - Bounded computation: one multiplication and one division per trade,
//     regardless of size or history. An order book's matching loop is not
//     bounded by anything the market controls.
//   - Simple accounting: every trade is one debit and one credit per asset.
//   - No hidden counterparty: the pool is the counterparty and its reserves
//     are public. An order book needs resting orders from someone.
//   - Resistant to rounding exploits: every division rounds in the pool's
//     favour, so K is non-decreasing and no sequence of trades can extract
//     value that was not paid in. This is proved by construction below and
//     tested by a fuzz target over random trade sequences.
//   - Strong concurrency: market state is a single row with a version, so a
//     trade is one compare-and-set.
//
// A pure bonding curve (price linear in supply) was the alternative. It is
// equally deterministic but needs an integer square root to answer "how much
// can I buy with 100 Credits", which is the question the product actually
// asks; constant product answers it with one division.
//
// # The two properties that matter, and why they hold
//
// K never decreases. Both trade directions compute the new opposite reserve
// with a CEILING division, so the pool retains at least the exact-arithmetic
// amount and usually a sub-unit more. Rounding can only ever add to K.
//
// Credits paid out never exceed Credits paid in. On a sell, the new effective
// reserve is x' = ceil(K / y') where y' = y + dy. Users can only sell units
// they hold, and in aggregate they hold Y0 - Y, so y' <= Y0. Therefore
// x' >= K / Y0 = V, and the Credits released, x - x' = (V + R) - x', are at
// most R. The real reserve cannot go negative, whatever sequence of trades
// occurs. TestProp_RealReserveNeverGoesNegative exercises this against random
// sequences; the arithmetic above is why it is not merely likely.

// Curve is the immutable half of a market's economics, fixed at activation.
//
// Nothing here can change once a market is ACTIVE: PART XIII forbids a creator
// altering economics after buyers enter, and migration 00712 enforces it with
// an immutability trigger rather than trusting this comment.
type Curve struct {
	// VirtualCreditReserve is V.
	VirtualCreditReserve money.Quantity
	// InitialAssetReserve is Y0, the units the pool began with. Creator
	// allocation is carved out before this, so Y0 is what the curve sells.
	InitialAssetReserve money.Quantity
}

// Validate checks that the curve can price at all.
func (c Curve) Validate() error {
	if c.VirtualCreditReserve.Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed, "market curve: virtual credit reserve must be positive")
	}
	if c.InitialAssetReserve.Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed, "market curve: initial asset reserve must be positive")
	}
	return nil
}

// K is the constant product the pool must always hold at least.
func (c Curve) K() *big.Int {
	return new(big.Int).Mul(c.VirtualCreditReserve.BigInt(), c.InitialAssetReserve.BigInt())
}

// State is the mutable half: what the pool holds right now.
type State struct {
	// RealCreditReserve is R.
	RealCreditReserve money.Quantity
	// AssetReserve is Y.
	AssetReserve money.Quantity
	// Version increments on every trade and is what a quote is pinned to.
	Version int64
}

// Effective returns x = V + R, the reserve the curve prices against.
func (s State) Effective(c Curve) money.Quantity {
	return c.VirtualCreditReserve.Add(s.RealCreditReserve)
}

// Validate checks the state is internally coherent.
func (s State) Validate(c Curve) error {
	if s.RealCreditReserve.IsNegative() {
		return errs.New(errs.CodeValidationFailed, "market state: real credit reserve cannot be negative")
	}
	if s.AssetReserve.IsNegative() {
		return errs.New(errs.CodeValidationFailed, "market state: asset reserve cannot be negative")
	}
	if s.AssetReserve.Cmp(c.InitialAssetReserve) > 0 {
		return errs.New(errs.CodeValidationFailed,
			"market state: the pool holds more asset units than were ever minted into it")
	}
	return nil
}

// HoldsInvariant reports whether (V+R)*Y >= K. It is checked after every
// computed trade and again after every persisted one.
func (s State) HoldsInvariant(c Curve) bool {
	product := new(big.Int).Mul(s.Effective(c).BigInt(), s.AssetReserve.BigInt())
	return product.Cmp(c.K()) >= 0
}

// Fees is the fee schedule of one market, fixed at activation.
type Fees struct {
	// PlatformBPS is taken by the platform on the Credit side of every trade.
	PlatformBPS money.BPS
	// CreatorBPS is taken by the market's creator. It is recorded with origin
	// MARKET_CREATOR_EARNING, which is deliberately distinct from ordinary
	// creator revenue because its source is speculative trading (PART LXXX).
	CreatorBPS money.BPS
}

// MaxTotalFeeBPS caps the combined fee at 10%. A market that could charge
// more would be a mechanism for extracting a holder's position rather than a
// market, and a creator cannot raise it after launch in any case.
const MaxTotalFeeBPS money.BPS = 1_000

// Validate checks the fee schedule.
func (f Fees) Validate() error {
	if f.PlatformBPS < 0 || f.CreatorBPS < 0 {
		return errs.New(errs.CodeValidationFailed, "market fees cannot be negative")
	}
	if f.Total() > MaxTotalFeeBPS {
		return errs.Newf(errs.CodeValidationFailed,
			"market fees total %s, above the %s maximum", f.Total(), MaxTotalFeeBPS)
	}
	return nil
}

// Total is the combined fee.
func (f Fees) Total() money.BPS { return f.PlatformBPS + f.CreatorBPS }

// Side is the direction of a trade.
type Side string

// Trade sides.
const (
	Buy  Side = "BUY"
	Sell Side = "SELL"
)

var allSides = []Side{Buy, Sell}

// AllSides returns every declared side in declaration order (a copy).
//
// It exists so the SQL CHECK on a side column can be compared against the list
// that DECLARES the values rather than against a literal repeated in a test.
// test/integration/enums does that comparison.
func AllSides() []Side { return append([]Side(nil), allSides...) }

// Valid reports whether s is a declared side.
func (s Side) Valid() bool { return s == Buy || s == Sell }

// Fill is the exact, fully specified result of one trade against the curve.
//
// Every quantity is an exact integer in base units. There is no field here a
// caller has to recompute, because a caller that recomputes will eventually
// recompute differently.
type Fill struct {
	Side Side

	// CreditsIn is what the user pays on a BUY, including fees.
	CreditsIn money.Quantity
	// CreditsOut is what the user receives on a SELL, after fees.
	CreditsOut money.Quantity
	// AssetsIn is what the user delivers on a SELL.
	AssetsIn money.Quantity
	// AssetsOut is what the user receives on a BUY.
	AssetsOut money.Quantity

	// CreditsToPool is what actually reaches (BUY) or leaves (SELL) the real
	// reserve. On a BUY this is CreditsIn minus fees; on a SELL it is the
	// gross release, of which the user keeps CreditsOut.
	CreditsToPool money.Quantity

	PlatformFee money.Quantity
	CreatorFee  money.Quantity

	// StateBefore and StateAfter bracket the trade. StateAfter.Version is not
	// set here; the persistence layer assigns it.
	StateBefore State
	StateAfter  State

	// SpotBefore and SpotAfter are credit base units per asset base unit,
	// scaled by PriceScale. They are for display and slippage only; no
	// economic quantity is derived from them.
	SpotBefore money.Quantity
	SpotAfter  money.Quantity
	// EffectivePrice is the price the trade actually achieved, same scaling.
	EffectivePrice money.Quantity
}

// PriceScale is the number of decimal places prices carry. It is large
// because a freshly launched market prices units far below one Credit, and a
// coarse price would render as zero.
const PriceScale = 18

var priceScaleFactor = new(big.Int).Exp(big.NewInt(10), big.NewInt(PriceScale), nil)

// QuoteBuy computes the exact result of spending creditsIn on a market.
//
// creditsIn is the gross amount the user pays. Fees come out of it first, and
// the remainder enters the pool.
func QuoteBuy(c Curve, s State, creditsIn money.Quantity, f Fees) (Fill, error) {
	if err := preflight(c, s, f); err != nil {
		return Fill{}, err
	}
	if creditsIn.Sign() <= 0 {
		return Fill{}, errs.New(errs.CodeValidationFailed, "buy amount must be positive")
	}
	platformFee, creatorFee := splitFee(creditsIn, f)
	toPool := creditsIn.Sub(platformFee).Sub(creatorFee)
	if toPool.Sign() <= 0 {
		// Only reachable when the trade is so small that it is entirely fee.
		return Fill{}, errs.Newf(errs.CodeValidationFailed,
			"buy of %s is smaller than its own fees; nothing would reach the market", creditsIn)
	}

	k := c.K()
	x := s.Effective(c).BigInt()
	y := s.AssetReserve.BigInt()

	xNew := new(big.Int).Add(x, toPool.BigInt())
	// Ceiling: the pool keeps at least K, never less.
	yNew := ceilDiv(k, xNew)
	if yNew.Cmp(y) > 0 {
		// Impossible while K is held; treated as a hard error rather than
		// clamped, because clamping would hide a broken invariant.
		return Fill{}, errs.New(errs.CodeInternal,
			"market curve produced a larger asset reserve on a buy; the invariant is broken")
	}
	assetsOut := money.QuantityFromBigInt(new(big.Int).Sub(y, yNew))
	if assetsOut.Sign() <= 0 {
		return Fill{}, errs.Newf(errs.CodeValidationFailed,
			"buy of %s Credits rounds to zero units at this price; increase the amount", creditsIn)
	}

	after := State{
		RealCreditReserve: s.RealCreditReserve.Add(toPool),
		AssetReserve:      money.QuantityFromBigInt(yNew),
	}
	fill := Fill{
		Side:          Buy,
		CreditsIn:     creditsIn,
		AssetsOut:     assetsOut,
		CreditsToPool: toPool,
		PlatformFee:   platformFee,
		CreatorFee:    creatorFee,
		StateBefore:   s,
		StateAfter:    after,
		SpotBefore:    spot(c, s),
		SpotAfter:     spot(c, after),
	}
	fill.EffectivePrice = ratioScaled(creditsIn.BigInt(), assetsOut.BigInt())
	return fill, checkFill(c, fill)
}

// QuoteSell computes the exact result of selling assetsIn units back.
func QuoteSell(c Curve, s State, assetsIn money.Quantity, f Fees) (Fill, error) {
	if err := preflight(c, s, f); err != nil {
		return Fill{}, err
	}
	if assetsIn.Sign() <= 0 {
		return Fill{}, errs.New(errs.CodeValidationFailed, "sell amount must be positive")
	}
	k := c.K()
	x := s.Effective(c).BigInt()
	y := s.AssetReserve.BigInt()

	yNew := new(big.Int).Add(y, assetsIn.BigInt())
	if yNew.Cmp(c.InitialAssetReserve.BigInt()) > 0 {
		// More units are being returned than the curve ever sold. In a correct
		// system this cannot happen, because holders in aggregate hold Y0 - Y;
		// refusing rather than pricing it is what stops a supply bug from
		// draining the reserve.
		return Fill{}, errs.New(errs.CodeValidationFailed,
			"selling these units would put more into the pool than the curve ever sold")
	}
	// Ceiling: the pool keeps at least K, so it releases at most the exact
	// amount and usually a sub-unit less.
	xNew := ceilDiv(k, yNew)
	if xNew.Cmp(x) > 0 {
		return Fill{}, errs.New(errs.CodeInternal,
			"market curve produced a larger credit reserve on a sell; the invariant is broken")
	}
	gross := money.QuantityFromBigInt(new(big.Int).Sub(x, xNew))
	if gross.Sign() <= 0 {
		return Fill{}, errs.Newf(errs.CodeValidationFailed,
			"selling %s units releases no Credits at this price; increase the amount", assetsIn)
	}
	// The proof in the file header: gross can never exceed the real reserve.
	// Asserting it costs one comparison and turns a silent drain into a loud
	// refusal if the arithmetic above is ever changed carelessly.
	if gross.Cmp(s.RealCreditReserve) > 0 {
		return Fill{}, errs.New(errs.CodeInternal,
			"market curve would release more Credits than the pool holds; the invariant is broken")
	}

	platformFee, creatorFee := splitFee(gross, f)
	net := gross.Sub(platformFee).Sub(creatorFee)
	if net.Sign() < 0 {
		return Fill{}, errs.New(errs.CodeInternal, "market fees exceeded the gross proceeds")
	}

	after := State{
		RealCreditReserve: s.RealCreditReserve.Sub(gross),
		AssetReserve:      money.QuantityFromBigInt(yNew),
	}
	fill := Fill{
		Side:          Sell,
		AssetsIn:      assetsIn,
		CreditsOut:    net,
		CreditsToPool: gross,
		PlatformFee:   platformFee,
		CreatorFee:    creatorFee,
		StateBefore:   s,
		StateAfter:    after,
		SpotBefore:    spot(c, s),
		SpotAfter:     spot(c, after),
	}
	fill.EffectivePrice = ratioScaled(net.BigInt(), assetsIn.BigInt())
	return fill, checkFill(c, fill)
}

func preflight(c Curve, s State, f Fees) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := s.Validate(c); err != nil {
		return err
	}
	if err := f.Validate(); err != nil {
		return err
	}
	if !s.HoldsInvariant(c) {
		return errs.New(errs.CodeInternal,
			"market state does not hold the constant-product invariant; refusing to trade against it")
	}
	return nil
}

// splitFee divides a gross amount into platform and creator fees.
//
// Both round DOWN, so the fees taken never exceed their exact share and the
// remainder — which is what reaches the pool on a buy, or the user on a sell —
// is never short. Rounding a fee up would let a long sequence of tiny trades
// extract sub-units from users, which is precisely the rounding exploit PART
// LXXVIII asks about.
func splitFee(gross money.Quantity, f Fees) (platform, creator money.Quantity) {
	platform = gross.MulBPS(f.PlatformBPS, money.RoundDown)
	creator = gross.MulBPS(f.CreatorBPS, money.RoundDown)
	return platform, creator
}

// checkFill re-derives the safety properties from the produced fill rather
// than trusting the code path that built it.
func checkFill(c Curve, fill Fill) error {
	if err := fill.StateAfter.Validate(c); err != nil {
		return err
	}
	if !fill.StateAfter.HoldsInvariant(c) {
		return errs.New(errs.CodeInternal,
			"trade would leave the market below its constant-product invariant")
	}
	// Conservation on the Credit side, stated as an equation rather than as a
	// comment: what the user pays equals what the pool takes plus the fees.
	switch fill.Side {
	case Buy:
		sum := fill.CreditsToPool.Add(fill.PlatformFee).Add(fill.CreatorFee)
		if sum.Cmp(fill.CreditsIn) != 0 {
			return errs.Newf(errs.CodeInternal,
				"buy does not conserve Credits: paid %s, accounted %s", fill.CreditsIn, sum)
		}
	case Sell:
		sum := fill.CreditsOut.Add(fill.PlatformFee).Add(fill.CreatorFee)
		if sum.Cmp(fill.CreditsToPool) != 0 {
			return errs.Newf(errs.CodeInternal,
				"sell does not conserve Credits: released %s, accounted %s", fill.CreditsToPool, sum)
		}
	}
	return nil
}

// ceilDiv returns ceil(a/b) for a >= 0, b > 0.
func ceilDiv(a, b *big.Int) *big.Int {
	if b.Sign() <= 0 {
		// Unreachable: every caller divides by a reserve the validators have
		// already proved positive.
		panic("nativemarket: division by a non-positive reserve")
	}
	q, r := new(big.Int).QuoRem(a, b, new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

// SpotPrice is the marginal price: credit base units per asset base unit,
// scaled by 10^PriceScale. It is display-only, and it is exported because the
// API surface has to show a price without recomputing one of its own.
func SpotPrice(c Curve, s State) money.Quantity { return spot(c, s) }

// spot is SpotPrice's internal name, used throughout the curve.
func spot(c Curve, s State) money.Quantity {
	return ratioScaled(s.Effective(c).BigInt(), s.AssetReserve.BigInt())
}

func ratioScaled(num, den *big.Int) money.Quantity {
	if den.Sign() == 0 {
		return money.Quantity{}
	}
	scaled := new(big.Int).Mul(num, priceScaleFactor)
	return money.QuantityFromBigInt(scaled.Quo(scaled, den))
}

// SlippageBPS is how far the effective price moved from the pre-trade spot,
// in basis points. It is reported on every quote so a user can see the cost
// of their own size rather than discovering it in the fill.
func (f Fill) SlippageBPS() money.BPS {
	if f.SpotBefore.Sign() == 0 {
		return 0
	}
	diff := new(big.Int).Sub(f.EffectivePrice.BigInt(), f.SpotBefore.BigInt())
	diff.Abs(diff)
	diff.Mul(diff, big.NewInt(int64(money.OneHundredPercent)))
	diff.Quo(diff, f.SpotBefore.BigInt())
	if !diff.IsInt64() {
		return money.OneHundredPercent
	}
	return money.BPS(diff.Int64())
}
