package fees

import (
	"encoding/json"
	"testing"

	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func q64(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func bpsIn(t *rapid.T, label string, lo, hi int64) money.BPS {
	return money.BPS(rapid.Int64Range(lo, hi).Draw(t, label))
}

// genSchedule draws a valid schedule with the default (customer-favourable)
// rounding: every component, a network estimate in the quote asset or
// another one, optional minimum and caps, and a ladder of up to three tiers.
func genSchedule(t *rapid.T) Schedule {
	s := Schedule{
		Version:                 "prop-v1",
		QuoteAsset:              quoteAsset,
		QuoteDecimals:           uint8(rapid.IntRange(0, 18).Draw(t, "quote_decimals")),
		SpreadBPS:               bpsIn(t, "spread", 0, 1_000),
		FixedFee:                q64(rapid.Int64Range(0, 10_000_000).Draw(t, "fixed")),
		PercentageFeeBPS:        bpsIn(t, "percentage", 0, 500),
		VenuePassThroughBPS:     bpsIn(t, "venue", 0, 100),
		SlippageReserveBPS:      bpsIn(t, "reserve", 0, 500),
		SubscriptionDiscountBPS: bpsIn(t, "discount", 0, 10_000),
		StrategyFeeBPS:          bpsIn(t, "strategy", 0, 300),
		Rounding:                DefaultRounding(),
	}
	switch rapid.IntRange(0, 2).Draw(t, "network") {
	case 1:
		s.NetworkFee = NetworkFeeEstimate{Asset: quoteAsset, Amount: q64(rapid.Int64Range(0, 1_000_000).Draw(t, "network_amount"))}
	case 2:
		s.NetworkFee = NetworkFeeEstimate{Asset: solAsset, Amount: q64(rapid.Int64Range(0, 1_000_000).Draw(t, "network_amount"))}
	}
	if rapid.Bool().Draw(t, "has_min") {
		s.MinFee = q64(rapid.Int64Range(1, 1_000_000).Draw(t, "min"))
	}
	if rapid.Bool().Draw(t, "has_cap_bps") {
		s.MaxTotalFeeBPS = bpsIn(t, "cap_bps", 1, 1_000)
	}
	if rapid.Bool().Draw(t, "has_cap_abs") {
		s.MaxTotalFee = s.MinFee.Add(q64(rapid.Int64Range(1, 100_000_000).Draw(t, "cap_abs")))
	}
	volume := int64(0)
	for range rapid.IntRange(0, 3).Draw(t, "tiers") {
		volume += rapid.Int64Range(1, 1_000_000_000).Draw(t, "tier_step")
		tier := VolumeTier{MinTrailingVolume: money.USDFromMinor(volume)}
		switch rapid.IntRange(0, 2).Draw(t, "tier_kind") {
		case 0:
			tier.OverridesPercentageFee, tier.PercentageFeeBPS = true, bpsIn(t, "tier_pct", 0, 500)
		case 1:
			tier.OverridesSpread, tier.SpreadBPS = true, bpsIn(t, "tier_spread", 0, 1_000)
		default:
			tier.OverridesPercentageFee, tier.PercentageFeeBPS = true, bpsIn(t, "tier_pct", 0, 500)
			tier.OverridesSpread, tier.SpreadBPS = true, bpsIn(t, "tier_spread", 0, 1_000)
		}
		s.Tiers = append(s.Tiers, tier)
	}
	return s
}

// genInput draws an order for s: quantities and price mantissas well beyond
// the int64 range, and any decimals up to 18.
func genInput(t *rapid.T, s Schedule) QuoteInput {
	qty, err := money.ParseQuantity(rapid.StringMatching(`[1-9][0-9]{0,24}`).Draw(t, "qty"))
	if err != nil {
		t.Fatal(err)
	}
	mantissa, err := money.ParseQuantity(rapid.StringMatching(`[1-9][0-9]{0,15}`).Draw(t, "mantissa"))
	if err != nil {
		t.Fatal(err)
	}
	return QuoteInput{
		Side:          rapid.SampledFrom([]Side{SideBuy, SideSell}).Draw(t, "side"),
		BaseQty:       qty,
		BaseDecimals:  uint8(rapid.IntRange(0, 18).Draw(t, "base_decimals")),
		QuoteDecimals: s.QuoteDecimals,
		ReferencePrice: money.Price{
			Mantissa: mantissa, Scale: rapid.Int32Range(0, 18).Draw(t, "scale"),
			QuoteAsset: quoteAsset.String(), Source: "prop", At: refTime,
		},
		TrailingVolume:     money.USDFromMinor(rapid.Int64Range(0, 4_000_000_000).Draw(t, "volume")),
		HasSubscription:    rapid.Bool().Draw(t, "subscription"),
		StrategyOriginated: rapid.Bool().Draw(t, "strategy_order"),
	}
}

// quoteOrSkip prices in; a sell too small to cover its charges is the one
// legitimate refusal and reports false.
func quoteOrSkip(t *rapid.T, s Schedule, in QuoteInput) (Quote, bool) {
	quote, err := s.Quote(in)
	if err != nil {
		if in.Side == SideSell && errs.CodeOf(err) == errs.CodeValidationFailed && containsKey(fieldKeys(err), "base_qty") {
			return Quote{}, false
		}
		t.Fatalf("quote refused: %v %v", err, fieldKeys(err))
	}
	return quote, true
}

func containsKey(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

func add(a, b rational) rational {
	return rational{num: a.num.Mul(b.den).Add(b.num.Mul(a.den)), den: a.den.Mul(b.den)}
}

func sub(a, b rational) rational {
	return rational{num: a.num.Mul(b.den).Sub(b.num.Mul(a.den)), den: a.den.Mul(b.den)}
}

func whole(v money.Quantity) rational { return rational{num: v, den: q64(1)} }

// TestProp_QuoteConservesEveryUnit: the totals are exactly the itemized
// components, nothing is negative, the revenue split books exactly the
// charges, and the auditor accepts every quote.
func TestProp_QuoteConservesEveryUnit(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := genSchedule(t)
		in := genInput(t, s)
		quote, ok := quoteOrSkip(t, s, in)
		if !ok {
			return
		}
		f := quote.Fees
		for _, v := range []money.Quantity{
			quote.NotionalAtReference, quote.NotionalAtExecution, quote.Spread.Amount, f.Total, f.MinimumTopUp,
			quote.SlippageReserve.Amount, quote.TotalDebit, quote.TotalCredit, quote.WorstCaseTotal, quote.Network.Amount,
		} {
			if v.IsNegative() {
				t.Fatalf("negative component %s", v)
			}
		}
		sum := f.MinimumTopUp
		for _, l := range f.Lines() {
			if l.Computed.IsNegative() || l.Charged.IsNegative() || l.CapReduction.IsNegative() {
				t.Fatalf("negative fee line %+v", l)
			}
			sum = sum.Add(l.Charged)
		}
		if !sum.Equal(f.Total) {
			t.Fatalf("fee lines %s != fee total %s", sum, f.Total)
		}
		charges := quote.Spread.Amount.Add(f.Total)
		if quote.Network.Asset == quoteAsset {
			charges = charges.Add(quote.Network.Amount)
		}
		switch in.Side {
		case SideBuy:
			if !quote.TotalDebit.Equal(quote.NotionalAtReference.Add(charges)) || !quote.TotalCredit.IsZero() {
				t.Fatalf("BUY conservation: debit %s, notional %s, charges %s", quote.TotalDebit, quote.NotionalAtReference, charges)
			}
		case SideSell:
			if !quote.TotalCredit.Equal(quote.NotionalAtReference.Sub(charges)) || !quote.TotalDebit.IsZero() {
				t.Fatalf("SELL conservation: credit %s, notional %s, charges %s", quote.TotalCredit, quote.NotionalAtReference, charges)
			}
		}
		if !quote.RevenueSplit().QuoteAssetTotal().Equal(charges) {
			t.Fatalf("revenue split %s != charges %s", quote.RevenueSplit().QuoteAssetTotal(), charges)
		}
		if err := VerifyQuote(s, in, quote); err != nil {
			t.Fatalf("auditor rejects an engine quote: %v %v", err, fieldKeys(err))
		}
	})
}

// TestProp_DefaultRoundingNeverChargesMoreThanExact: under the default
// policy every rounded charge is at most its exact rational value (and
// within one unit of it), a SELL's notional is at least its exact value, and
// -- unless the minimum fee raised the fees -- the whole debit is at most,
// and the whole credit at least, the exact amount.
func TestProp_DefaultRoundingNeverChargesMoreThanExact(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := genSchedule(t)
		in := genInput(t, s)
		quote, ok := quoteOrSkip(t, s, in)
		if !ok {
			return
		}
		exact := referenceNotional(in)
		tier := s.resolveTier(in.TrailingVolume)
		spread := exact.mulBPS(tier.SpreadBPS)
		pct := exact.mulBPS(tier.PercentageFeeBPS).mulBPS(money.OneHundredPercent - appliedDiscount(s, in))
		venue := exact.mulBPS(s.VenuePassThroughBPS)
		strategy := exact.mulBPS(appliedStrategyRate(s, in))
		for _, c := range []struct {
			name  string
			got   money.Quantity
			exact rational
		}{
			{"spread", quote.Spread.Amount, spread},
			{"percentage", quote.Fees.Percentage.Computed, pct},
			{"venue", quote.Fees.VenuePassThrough.Computed, venue},
			{"strategy", quote.Fees.Strategy.Computed, strategy},
		} {
			if c.exact.lessThan(c.got) || !c.exact.withinOneUnit(c.got) {
				t.Fatalf("%s %s is not the exact amount rounded down", c.name, c.got)
			}
		}
		if in.Side == SideBuy && exact.lessThan(quote.NotionalAtReference) {
			t.Fatalf("BUY notional %s above the exact notional", quote.NotionalAtReference)
		}
		if in.Side == SideSell && exact.greaterThan(quote.NotionalAtReference) {
			t.Fatalf("SELL notional %s below the exact notional", quote.NotionalAtReference)
		}
		// The execution notional is within two units of quantity × execution
		// price, and never on the customer's wrong side of it.
		ep := quote.ExecutionPrice
		execExact := rational{
			num: in.BaseQty.Mul(ep.Mantissa).Mul(pow10(int(in.QuoteDecimals))),
			den: pow10(int(in.BaseDecimals) + int(ep.Scale)),
		}
		two := q64(2)
		switch in.Side {
		case SideBuy:
			if execExact.lessThan(quote.NotionalAtExecution) || !execExact.lessThan(quote.NotionalAtExecution.Add(two)) {
				t.Fatalf("BUY execution notional %s not within (exact − 2, exact]", quote.NotionalAtExecution)
			}
		case SideSell:
			if execExact.greaterThan(quote.NotionalAtExecution) || !execExact.greaterThan(quote.NotionalAtExecution.Sub(two)) {
				t.Fatalf("SELL execution notional %s not within [exact, exact + 2)", quote.NotionalAtExecution)
			}
		}
		if quote.Fees.Bounds.MinimumApplied {
			return
		}
		fixedAndNetwork := s.FixedFee
		if s.NetworkFee.Asset == quoteAsset {
			fixedAndNetwork = fixedAndNetwork.Add(s.NetworkFee.Amount)
		}
		charges := add(add(add(add(spread, pct), venue), strategy), whole(fixedAndNetwork))
		switch in.Side {
		case SideBuy:
			if add(exact, charges).lessThan(quote.TotalDebit) {
				t.Fatalf("debit %s exceeds the exact debit", quote.TotalDebit)
			}
		case SideSell:
			if sub(exact, charges).greaterThan(quote.TotalCredit) {
				t.Fatalf("credit %s is below the exact credit", quote.TotalCredit)
			}
		}
	})
}

// TestProp_FeesNeverExceedTheirCaps: the explicit fees never exceed an
// enabled cap (and the bps cap never exceeds its exact value), and they
// reach the minimum unless a cap is lower.
func TestProp_FeesNeverExceedTheirCaps(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := genSchedule(t)
		in := genInput(t, s)
		quote, ok := quoteOrSkip(t, s, in)
		if !ok {
			return
		}
		f := quote.Fees
		floor := s.MinFee
		if s.MaxTotalFeeBPS > 0 {
			if f.Total.Cmp(f.Bounds.MaxBPSAmount) > 0 {
				t.Fatalf("fees %s above the bps cap %s", f.Total, f.Bounds.MaxBPSAmount)
			}
			if referenceNotional(in).mulBPS(s.MaxTotalFeeBPS).lessThan(f.Bounds.MaxBPSAmount) {
				t.Fatalf("bps cap %s above its exact value", f.Bounds.MaxBPSAmount)
			}
			floor = floor.Min(f.Bounds.MaxBPSAmount)
		}
		if s.MaxTotalFee.IsPositive() {
			if f.Total.Cmp(s.MaxTotalFee) > 0 {
				t.Fatalf("fees %s above the absolute cap %s", f.Total, s.MaxTotalFee)
			}
			floor = floor.Min(s.MaxTotalFee)
		}
		if f.Total.Cmp(floor) < 0 {
			t.Fatalf("fees %s below the minimum %s that no cap overrides", f.Total, floor)
		}
		for _, l := range f.Lines() {
			if l.Charged.Cmp(l.Computed) > 0 {
				t.Fatalf("a bound raised a line instead of adding a top-up: %+v", l)
			}
		}
	})
}

// TestProp_ZeroScheduleIsTheIdentity: no spread and no fees means the
// execution price is the reference price and the total is the notional.
func TestProp_ZeroScheduleIsTheIdentity(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := ZeroSchedule(quoteAsset, uint8(rapid.IntRange(0, 18).Draw(t, "quote_decimals")))
		in := genInput(t, s)
		quote, ok := quoteOrSkip(t, s, in)
		if !ok {
			t.Fatal("a zero schedule never refuses a sell")
		}
		if !quote.ExecutionPrice.Mantissa.Equal(in.ReferencePrice.Mantissa) || quote.ExecutionPrice.Scale != in.ReferencePrice.Scale {
			t.Fatalf("execution price %s != reference %s", quote.ExecutionPrice, in.ReferencePrice)
		}
		if !quote.Total().Equal(quote.NotionalAtReference) || !quote.NotionalAtExecution.Equal(quote.NotionalAtReference) {
			t.Fatalf("total %s != notional %s", quote.Total(), quote.NotionalAtReference)
		}
		if !quote.WorstCaseTotal.Equal(quote.Total()) {
			t.Fatalf("worst case %s != total %s", quote.WorstCaseTotal, quote.Total())
		}
	})
}

// TestProp_SubscriptionDiscountOnlyLowersThePercentageFee.
func TestProp_SubscriptionDiscountOnlyLowersThePercentageFee(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := genSchedule(t)
		in := genInput(t, s)
		in.Side = SideBuy
		in.HasSubscription = false
		plain, _ := quoteOrSkip(t, s, in)
		in.HasSubscription = true
		sub, _ := quoteOrSkip(t, s, in)
		p, d := plain.Fees.Percentage.Computed, sub.Fees.Percentage.Computed
		if d.IsNegative() || d.Cmp(p) > 0 {
			t.Fatalf("discounted fee %s outside [0, %s]", d, p)
		}
		if !plain.Spread.Amount.Equal(sub.Spread.Amount) ||
			!plain.Fees.Fixed.Computed.Equal(sub.Fees.Fixed.Computed) ||
			!plain.Fees.VenuePassThrough.Computed.Equal(sub.Fees.VenuePassThrough.Computed) ||
			!plain.Fees.Strategy.Computed.Equal(sub.Fees.Strategy.Computed) {
			t.Fatal("the subscription discount changed something other than the percentage fee")
		}
	})
}

// TestProp_TierSelectionMatchesAnOracle: the selected tier is the highest
// one whose threshold the volume reaches.
func TestProp_TierSelectionMatchesAnOracle(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := genSchedule(t)
		volume := money.USDFromMinor(rapid.Int64Range(0, 4_000_000_000).Draw(t, "volume"))
		if len(s.Tiers) > 0 && rapid.Bool().Draw(t, "on_a_boundary") {
			b := s.Tiers[rapid.IntRange(0, len(s.Tiers)-1).Draw(t, "boundary")].MinTrailingVolume
			volume = money.USDFromMinor(b.Minor() + rapid.Int64Range(-1, 1).Draw(t, "offset"))
		}
		want := -1
		for i := len(s.Tiers) - 1; i >= 0; i-- {
			if volume.Cmp(s.Tiers[i].MinTrailingVolume) >= 0 {
				want = i
				break
			}
		}
		got, err := s.TierFor(volume)
		if err != nil {
			t.Fatal(err)
		}
		if got.Index != want {
			t.Fatalf("volume %s selected tier %d, want %d", volume, got.Index, want)
		}
	})
}

// TestProp_QuoteIsDeterministicAndItsRecordVerifies.
func TestProp_QuoteIsDeterministicAndItsRecordVerifies(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := genSchedule(t)
		in := genInput(t, s)
		a, ok := quoteOrSkip(t, s, in)
		if !ok {
			return
		}
		b, _ := quoteOrSkip(t, s, in)
		ha, err := a.Disclosure().Hash()
		if err != nil {
			t.Fatal(err)
		}
		hb, err := b.Disclosure().Hash()
		if err != nil {
			t.Fatal(err)
		}
		if ha != hb {
			t.Fatal("the same schedule and input produced different records")
		}
		raw, err := json.Marshal(a.Disclosure())
		if err != nil {
			t.Fatal(err)
		}
		var stored Disclosure
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		if err := VerifyDisclosure(s, stored); err != nil {
			t.Fatalf("stored record does not verify: %v %v", err, fieldKeys(err))
		}
	})
}

// TestProp_AnyOneUnitTamperIsDetected: adding one base unit to any amount of
// a quote makes the auditor reject it.
func TestProp_AnyOneUnitTamperIsDetected(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := genSchedule(t)
		in := genInput(t, s)
		quote, ok := quoteOrSkip(t, s, in)
		if !ok {
			return
		}
		targets := []*money.Quantity{
			&quote.NotionalAtReference, &quote.NotionalAtExecution, &quote.Spread.Amount,
			&quote.Fees.Fixed.Charged, &quote.Fees.Percentage.Computed, &quote.Fees.VenuePassThrough.Charged,
			&quote.Fees.Strategy.Computed, &quote.Fees.MinimumTopUp, &quote.Fees.ComputedTotal, &quote.Fees.Total,
			&quote.Network.Amount, &quote.SlippageReserve.Amount, &quote.TotalDebit, &quote.TotalCredit,
			&quote.WorstCaseTotal, &quote.Fees.Bounds.MaxBPSAmount, &quote.ExecutionPrice.Mantissa, &quote.EffectivePrice.Mantissa,
		}
		i := rapid.IntRange(0, len(targets)-1).Draw(t, "target")
		*targets[i] = targets[i].Add(q64(1))
		if VerifyQuote(s, in, quote) == nil {
			t.Fatalf("a one-unit change to amount %d went unnoticed", i)
		}
	})
}

// TestProp_PlatformFavourableModesAreAlwaysDisclosed: whatever modes a
// schedule adopts, the quote verifies and its record flags exactly the
// components whose mode can favour the platform.
func TestProp_PlatformFavourableModesAreAlwaysDisclosed(t *testing.T) {
	modes := []money.RoundingMode{money.RoundDown, money.RoundUp, money.RoundHalfEven, money.RoundHalfUp, money.RoundFloor, money.RoundCeil}
	rapid.Check(t, func(t *rapid.T) {
		s := genSchedule(t)
		draw := func(label string) money.RoundingMode { return rapid.SampledFrom(modes).Draw(t, label) }
		s.Rounding = RoundingPolicy{
			BuyNotional: draw("buy"), SellNotional: draw("sell"), Spread: draw("spread"), PercentageFee: draw("pct"),
			VenuePassThrough: draw("venue"), StrategyFee: draw("strategy"), FeeCap: draw("cap"), SlippageReserve: draw("reserve"),
			AllowPlatformFavourable: true,
		}
		in := genInput(t, s)
		quote, ok := quoteOrSkip(t, s, in)
		if !ok {
			return
		}
		if err := VerifyQuote(s, in, quote); err != nil {
			t.Fatalf("%v %v", err, fieldKeys(err))
		}
		notionalMode, notionalRole := s.Rounding.notionalMode(in.Side)
		want := map[Component]bool{
			ComponentReferenceNotional: favoursPlatform(notionalRole, notionalMode),
			ComponentSpread:            favoursPlatform(roleCharge, s.Rounding.Spread),
			ComponentPercentageFee:     favoursPlatform(roleCharge, s.Rounding.PercentageFee),
			ComponentVenuePassThrough:  favoursPlatform(roleCharge, s.Rounding.VenuePassThrough),
			ComponentStrategyFee:       favoursPlatform(roleCharge, s.Rounding.StrategyFee),
			ComponentFeeCap:            favoursPlatform(roleCharge, s.Rounding.FeeCap),
		}
		r := quote.Disclosure().Rounding
		any := false
		for _, m := range r.Components {
			if m.FavoursPlatform != want[m.Component] {
				t.Fatalf("%s mode %s: favours_platform %v, want %v", m.Component, m.Mode, m.FavoursPlatform, want[m.Component])
			}
			any = any || m.FavoursPlatform
		}
		if r.AnyFavoursPlatform != any || !r.PlatformFavourableAllowed {
			t.Fatal("the record does not state the platform-favourable rounding it used")
		}
	})
}
