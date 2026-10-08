package equities

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

var (
	testAccount = accounts.AccountID(NewOrderID())
	testShare   = assets.NewAssetID()
	testCash    = assets.NewAssetID()
)

func drawSchedule(rt *rapid.T) fees.Schedule {
	s := fees.ZeroSchedule(testCash, 6)
	s.Version = "equities-test-v1"
	s.SpreadBPS = money.BPS(rapid.Int64Range(0, 500).Draw(rt, "spread"))
	s.PercentageFeeBPS = money.BPS(rapid.Int64Range(0, 100).Draw(rt, "pct"))
	s.VenuePassThroughBPS = money.BPS(rapid.Int64Range(0, 30).Draw(rt, "venue"))
	s.FixedFee = q(rapid.Int64Range(0, 2_000_000).Draw(rt, "fixed"))
	s.SlippageReserveBPS = money.BPS(rapid.Int64Range(0, 200).Draw(rt, "reserve"))
	s.StrategyFeeBPS = money.BPS(rapid.Int64Range(0, 50).Draw(rt, "strategy"))
	if rapid.Bool().Draw(rt, "min") {
		s.MinFee = q(rapid.Int64Range(0, 1_000_000).Draw(rt, "minFee"))
	}
	if rapid.Bool().Draw(rt, "cap") {
		s.MaxTotalFeeBPS = money.BPS(rapid.Int64Range(1, 100).Draw(rt, "capBps"))
		if s.MinFee.IsPositive() {
			s.MaxTotalFee = s.MinFee.Add(q(rapid.Int64Range(0, 1_000_000).Draw(rt, "capAbs")))
		}
	}
	return s
}

func posting(kind ledger.Kind, e []ledger.Entry) ledger.Posting {
	return ledger.Posting{Kind: kind, IdempotencyKey: "equities-test", Reference: ledger.FinancialEventReference{Type: "test", ID: "1"},
		EffectiveAt: time.Unix(1_800_000_000, 0).UTC(), Entries: e}
}

// net returns Σ debits − Σ credits of the entries on (code, asset).
func net(e []ledger.Entry, code ledger.Code, asset assets.AssetID) money.Quantity {
	var n money.Quantity
	for _, x := range e {
		if x.Account.Code != code || x.Account.AssetID != asset {
			continue
		}
		if x.Side == ledger.Debit {
			n = n.Add(x.Quantity)
		} else {
			n = n.Sub(x.Quantity)
		}
	}
	return n
}

// TestProp_TradePostingsBalancePerPartyAndDiscloseEveryCharge: for any
// schedule, side, price and quantity the SIM_TRADE posting passes the
// ledger's own validation (balanced per asset and per party, simulated-only
// kind), the customer's WALLET moves by exactly the quote's total, the spread
// booked to FEES_SPREAD and SPREAD_REVENUE is exactly the disclosed spread,
// and the venue's notional leaves through TRADING_OUTFLOW or arrives through
// TRADING_INFLOW unbooked anywhere else.
func TestProp_TradePostingsBalancePerPartyAndDiscloseEveryCharge(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		sch := drawSchedule(rt)
		side := rapid.SampledFrom(AllSides()).Draw(rt, "side")
		qty := q(rapid.Int64Range(1, 10_000_000_000).Draw(rt, "qty"))
		price, err := money.NewPrice(q(rapid.Int64Range(1, 10_000_000).Draw(rt, "price")), 2, testCash.String(), "test", time.Unix(1, 0))
		if err != nil {
			rt.Fatal(err)
		}
		in := fees.QuoteInput{Side: feeSide(side), BaseQty: qty, BaseDecimals: 6, QuoteDecimals: 6, ReferencePrice: price,
			StrategyOriginated: rapid.Bool().Draw(rt, "strategyOriginated")}
		quote, err := sch.Quote(in)
		if err != nil {
			return // a sell too small to cover its charges is refused by the schedule
		}
		if err := fees.VerifyQuote(sch, in, quote); err != nil {
			rt.Fatalf("quote does not verify: %v", err)
		}
		c, err := ChargesOf(quote)
		if err != nil {
			if errs.CodeOf(err) != errs.CodeValidationFailed {
				rt.Fatalf("charges: %v", err)
			}
			return
		}
		e, err := TradeEntries(testAccount, side, testShare, testCash, qty, c)
		if err != nil {
			rt.Fatal(err)
		}
		if err := posting(ledger.KindSimTrade, e).Validate(); err != nil {
			rt.Fatalf("trade posting refused by the ledger's validation: %v", err)
		}
		if !net(e, ledger.CodeFeesSpread, testCash).Equal(quote.Spread.Amount) || !net(e, ledger.CodeSpreadRevenue, testCash).Neg().Equal(quote.Spread.Amount) {
			rt.Fatalf("spread booked is not the disclosed spread")
		}
		// Every explicit fee the platform keeps (fixed, percentage, minimum
		// top-up and strategy) is platform fee revenue; the venue's share is
		// not the platform's.
		platformFees := quote.Fees.Fixed.Charged.Add(quote.Fees.Percentage.Charged).Add(quote.Fees.MinimumTopUp).Add(quote.Fees.Strategy.Charged)
		if !net(e, ledger.CodePlatformFeeRevenue, testCash).Neg().Equal(platformFees) ||
			!net(e, ledger.CodeFeesVenue, testCash).Equal(quote.Fees.VenuePassThrough.Charged) {
			rt.Fatalf("explicit fees booked are not the disclosed fees")
		}
		wallet := net(e, ledger.CodeWallet, testCash)
		switch side {
		case SideBuy:
			if !wallet.Neg().Equal(quote.TotalDebit) || !net(e, ledger.CodeUnsettledReceivable, testShare).Equal(qty) {
				rt.Fatalf("a buy pays its total from WALLET and receives its shares unsettled")
			}
			if !net(e, ledger.CodeTradingOutflow, testCash).Equal(quote.NotionalAtReference) {
				rt.Fatalf("the notional leaves to the venue")
			}
		case SideSell:
			if !wallet.IsZero() || !net(e, ledger.CodeUnsettledReceivable, testCash).Equal(quote.TotalCredit) ||
				!net(e, ledger.CodeWallet, testShare).Neg().Equal(qty) {
				rt.Fatalf("a sell delivers its shares at trade date and receives net proceeds unsettled")
			}
			if !net(e, ledger.CodeTradingInflow, testCash).Neg().Equal(quote.NotionalAtReference) {
				rt.Fatalf("the notional arrives from the venue")
			}
		}
		// Settlement is balanced too and moves exactly what waited.
		deliver, amount := testShare, qty
		if side == SideSell {
			deliver, amount = testCash, quote.TotalCredit
		}
		se, err := SettlementEntries(testAccount, deliver, amount)
		if err != nil {
			rt.Fatal(err)
		}
		if err := posting(ledger.KindEquitySettlement, se).Validate(); err != nil {
			rt.Fatalf("settlement refused: %v", err)
		}
	})
}

// TestProp_CorporateActionPostingsBalancePerParty: every split and dividend
// posting passes the ledger's validation, and its WALLET legs are exactly
// the outcome's share delta and cash.
func TestProp_CorporateActionPostingsBalancePerParty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		typ := rapid.SampledFrom([]CorporateActionType{ActionSplit, ActionReverseSplit}).Draw(rt, "type")
		a := rapid.Int64Range(1, 20).Draw(rt, "a")
		b := rapid.Int64Range(a+1, a+20).Draw(rt, "b")
		ratio := Ratio{b, a}
		if typ == ActionReverseSplit {
			ratio = Ratio{a, b}
		}
		old := q(rapid.Int64Range(1, 1_000_000_000).Draw(rt, "old"))
		price := &PricePoint{Mantissa: q(rapid.Int64Range(1, 1_000_000).Draw(rt, "price")), Scale: 2}
		o, err := ComputeSplit(old, ratio, typ, 6, 6, price, RoundingSharesDownCashHalfEven)
		if err != nil {
			rt.Fatal(err)
		}
		e := SplitEntries(testAccount, testShare, testCash, o)
		if len(e) > 0 {
			if err := posting(ledger.KindCorporateAction, e).Validate(); err != nil {
				rt.Fatalf("split posting refused: %v", err)
			}
		}
		if !net(e, ledger.CodeWallet, testShare).Equal(o.SharesDelta()) || !net(e, ledger.CodeWallet, testCash).Equal(o.CashInLieu) {
			rt.Fatalf("WALLET does not move by the outcome")
		}
		amount := q(rapid.Int64Range(0, 1_000_000).Draw(rt, "dividend"))
		de := DividendEntries(testAccount, testCash, amount)
		if amount.IsZero() != (len(de) == 0) {
			rt.Fatalf("a zero dividend posts nothing and a positive one posts")
		}
		if len(de) > 0 {
			if err := posting(ledger.KindCorporateAction, de).Validate(); err != nil {
				rt.Fatalf("dividend posting refused: %v", err)
			}
		}
	})
}

func TestChargesOf_RefusesWhatAnEquityTradeCannotCarry(t *testing.T) {
	sch := fees.ZeroSchedule(testCash, 6)
	price, err := money.NewPrice(q(1), 2, testCash.String(), "t", time.Unix(1, 0))
	require.NoError(t, err)
	// One micro-share at a cent rounds to a zero notional: nothing would be paid.
	quote, err := sch.Quote(fees.QuoteInput{Side: fees.SideBuy, BaseQty: q(1), BaseDecimals: 6, QuoteDecimals: 6, ReferencePrice: price})
	require.NoError(t, err)
	_, err = ChargesOf(quote)
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	other := assets.NewAssetID()
	sch.NetworkFee = fees.NetworkFeeEstimate{Asset: other, Amount: q(5)}
	price, err = money.NewPrice(q(10_000), 2, testCash.String(), "t", time.Unix(1, 0))
	require.NoError(t, err)
	quote, err = sch.Quote(fees.QuoteInput{Side: fees.SideBuy, BaseQty: q(1_000_000), BaseDecimals: 6, QuoteDecimals: 6, ReferencePrice: price})
	require.NoError(t, err)
	_, err = ChargesOf(quote)
	require.Error(t, err, "an equity trade carries no network cost")
}

// TestProp_ScheduleRecordRoundTripsTheHash: the stored form of a schedule
// decodes to a schedule with the same hash, so a stored disclosure can be
// re-verified from the database alone.
func TestProp_ScheduleRecordRoundTripsTheHash(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := drawSchedule(rt)
		if rapid.Bool().Draw(rt, "tiers") {
			s.Tiers = []fees.VolumeTier{
				{MinTrailingVolume: money.USDFromMinor(100_000_00), OverridesPercentageFee: true, PercentageFeeBPS: 5},
				{MinTrailingVolume: money.USDFromMinor(1_000_000_00), OverridesSpread: true, SpreadBPS: 1},
			}
		}
		if rapid.Bool().Draw(rt, "favourable") {
			s.Rounding.Spread = money.RoundHalfEven
			s.Rounding.AllowPlatformFavourable = true
		}
		want, err := s.Hash()
		if err != nil {
			rt.Fatal(err)
		}
		b, err := EncodeSchedule(s)
		if err != nil {
			rt.Fatal(err)
		}
		back, err := DecodeSchedule(b)
		if err != nil {
			rt.Fatalf("decode: %v", err)
		}
		got, err := back.Hash()
		if err != nil || got != want {
			rt.Fatalf("hash changed through storage: %s != %s (%v)", got, want, err)
		}
	})
}

func TestDecodeDisclosure_RequiresTheCanonicalBytes(t *testing.T) {
	sch := fees.ZeroSchedule(testCash, 6)
	sch.SpreadBPS = 25
	price, err := money.NewPrice(q(15_000), 2, testCash.String(), "t", time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	in := fees.QuoteInput{Side: fees.SideBuy, BaseQty: q(1_234_567), BaseDecimals: 6, QuoteDecimals: 6, ReferencePrice: price}
	quote, err := sch.Quote(in)
	require.NoError(t, err)
	b, err := quote.Disclosure().Canonical()
	require.NoError(t, err)
	d, err := DecodeDisclosure(b)
	require.NoError(t, err)
	require.NoError(t, fees.VerifyDisclosure(sch, d))
	// Re-ordered keys are the same JSON value but not the record that was
	// written, and the record is verified byte for byte.
	var generic map[string]any
	require.NoError(t, json.Unmarshal(b, &generic))
	reordered, err := json.Marshal(generic)
	require.NoError(t, err)
	require.NotEqual(t, string(b), string(reordered))
	_, err = DecodeDisclosure(reordered)
	require.Error(t, err)
	_, err = DecodeSchedule([]byte(`{"version":"x","unknown":1}`))
	require.Error(t, err)
}

func FuzzDecodeSchedule(f *testing.F) {
	if b, err := EncodeSchedule(fees.ZeroSchedule(testCash, 6)); err == nil {
		f.Add(b)
	}
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := DecodeSchedule(data)
		if err != nil {
			return
		}
		if err := s.Validate(); err != nil {
			t.Fatalf("a decoded schedule must validate: %v", err)
		}
		b, err := EncodeSchedule(s)
		if err != nil {
			t.Fatalf("a decoded schedule must encode: %v", err)
		}
		again, err := DecodeSchedule(b)
		if err != nil {
			t.Fatal(err)
		}
		h1, _ := s.Hash()
		h2, _ := again.Hash()
		if h1 != h2 {
			t.Fatalf("hash changed through a round trip")
		}
	})
}
