package derivatives

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
)

// farFromExpiry is well outside the 24-hour expiry window of the test specs
// (expiry 2026-12-25 08:00 UTC).
var farFromExpiry = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func markAtDollars(mark, index int64) MarkInput {
	return MarkInput{Mark: dollars(mark), HasIndex: true, Index: dollars(index), IndexScale: 2}
}

// TestMarginRequirements_GiveFixedNumbers pins the margin model on fixed
// positions. A contract is 0.01 BTC; at $65 000 it is worth $650.
func TestMarginRequirements_GiveFixedNumbers(t *testing.T) {
	p := DefaultPolicy()
	cases := []struct {
		name                 string
		spec                 Spec
		qty                  int64
		mark                 MarkInput
		now                  time.Time
		notional, im, mm, sl string
	}{
		// MAJOR: 10% / 5% of $6 500, plus the perpetual's funding buffer of
		// 3 intervals × 0.75% = 2.25% = $146.25.
		{"perpetual long", perpSpec(t), 10, markAtDollars(65_000, 65_000), farFromExpiry,
			"6500000000", "796250000", "325000000", "0"},
		{"perpetual short is margined like a long", perpSpec(t), -10, markAtDollars(65_000, 65_000), farFromExpiry,
			"6500000000", "796250000", "325000000", "0"},
		{"future far from expiry", futureSpec(t), 10, markAtDollars(65_000, 65_000), farFromExpiry,
			"6500000000", "650000000", "325000000", "0"},
		// Inside the last 24 hours the 50% expiry gap dominates.
		{"future in the expiry window", futureSpec(t), 10, markAtDollars(65_000, 65_000),
			time.Date(2026, 12, 24, 20, 0, 0, 0, time.UTC), "6500000000", "3250000000", "325000000", "3250000000"},
		// A holder risks only the premium it paid.
		{"long call", optionSpec(t, instruments.Call, 6_500_000), 2, markAtDollars(1_000, 65_000), farFromExpiry,
			"1300000000", "0", "0", "0"},
		// Writer of 2 calls at K 65 000, premium $1 000 per BTC ($10 a contract):
		// worst grid point +30% -> S 84 500, intrinsic $195 a contract,
		// $390 against a $20 liability now = $370; plus 10% of $1 300.
		{"short call", optionSpec(t, instruments.Call, 6_500_000), -2, markAtDollars(1_000, 65_000), farFromExpiry,
			"1300000000", "500000000", "400000000", "370000000"},
		{"short put mirrors it at −30%", optionSpec(t, instruments.Put, 6_500_000), -2, markAtDollars(1_000, 65_000), farFromExpiry,
			"1300000000", "500000000", "400000000", "370000000"},
		// In the expiry window the grid gains ±50%: S 97 500, $325 a contract.
		{"short call in the expiry window", optionSpec(t, instruments.Call, 6_500_000), -2, markAtDollars(1_000, 65_000),
			time.Date(2026, 12, 24, 20, 0, 0, 0, time.UTC), "1300000000", "760000000", "608000000", "630000000"},
		// Deep out of the money: no grid point pays, the minimum remains.
		{"far out-of-the-money short call", optionSpec(t, instruments.Call, 13_000_000), -2, markAtDollars(10, 65_000), farFromExpiry,
			"1300000000", "130000000", "104000000", "0"},
	}
	for _, c := range cases {
		r, err := RequirementFor(p, c.spec, q(c.qty), c.mark, c.now)
		require.NoError(t, err, c.name)
		assert.Equal(t, c.notional, r.Notional.String(), "%s notional", c.name)
		assert.Equal(t, c.im, r.Initial.String(), "%s initial", c.name)
		assert.Equal(t, c.mm, r.Maintenance.String(), "%s maintenance", c.name)
		assert.Equal(t, c.sl, r.ScenarioLoss.String(), "%s scenario loss", c.name)
	}
}

func TestMarginRequirements_FailClosed(t *testing.T) {
	p := DefaultPolicy()
	_, err := RequirementFor(p, optionSpec(t, instruments.Call, 6_500_000), q(-1), MarkInput{Mark: dollars(1_000)}, farFromExpiry)
	require.Error(t, err)
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err), "an option is never margined without its index")

	usd, btc := testAssets()
	btc.RiskClass = assets.RiskUnsupported
	s := specFor(t, testTerms(instruments.TypePerpetual, usd, btc), usd, btc)
	_, err = RequirementFor(p, s, q(1), markAtDollars(65_000, 65_000), farFromExpiry)
	require.Error(t, err, "an underlying risk class the policy does not rate cannot be traded")

	r, err := RequirementFor(p, perpSpec(t), q(0), markAtDollars(65_000, 65_000), farFromExpiry)
	require.NoError(t, err)
	assert.True(t, r.Initial.IsZero())
}

// TestStressBook_GapScenariosOnAFixedBookGiveFixedNumbers revalues one fixed
// book -- long 10 perpetuals at $65 000 and short 2 calls struck at $65 000
// (premium $1 000) -- under gap shocks. The perpetual moves $650 per 1% per 10
// contracts; the calls cost the writer their intrinsic value above the
// premium.
func TestStressBook_GapScenariosOnAFixedBookGiveFixedNumbers(t *testing.T) {
	book := []StressPosition{
		{Spec: perpSpec(t), Quantity: q(10), Mark: markAtDollars(65_000, 65_000)},
		{Spec: optionSpec(t, instruments.Call, 6_500_000), Quantity: q(-2), Mark: markAtDollars(1_000, 65_000)},
	}
	rep, err := StressBook(book, []money.BPS{-5_000, -1_000, 0, 1_000, 5_000})
	require.NoError(t, err)
	want := []struct {
		shock money.BPS
		pnl   string
	}{
		{-5_000, "-3230000000"}, // perp −3 250, calls expire worthless +20
		{-1_000, "-630000000"},  // perp −650, calls +20
		{0, "20000000"},         // the premium's time value
		{1_000, "540000000"},    // perp +650, calls −110 (2 × 65 − 20)
		{5_000, "2620000000"},   // perp +3 250, calls −630 (2 × 325 − 20)
	}
	require.Len(t, rep.Scenarios, len(want))
	for i, w := range want {
		assert.Equal(t, w.shock, rep.Scenarios[i].ShockBPS)
		assert.Equal(t, w.pnl, rep.Scenarios[i].PnL.String(), "shock %s", w.shock)
	}
	assert.Equal(t, "-3230000000", rep.WorstPnL.String())
	assert.Equal(t, money.BPS(-5_000), rep.WorstShockBPS)

	again, err := StressBook(book, []money.BPS{-5_000, -1_000, 0, 1_000, 5_000})
	require.NoError(t, err)
	assert.Equal(t, rep, again, "a stress test is deterministic")
	_, err = StressBook(book, nil)
	require.Error(t, err)
	_, err = StressBook(book, []money.BPS{-10_001})
	require.Error(t, err)
}

// TestProp_WrittenOptionMarginCoversEveryScenarioPlusTheMinimum and a held
// option needs nothing beyond its paid premium.
func TestProp_WrittenOptionMarginCoversEveryScenarioPlusTheMinimum(t *testing.T) {
	p := DefaultPolicy()
	rapid.Check(t, func(t *rapid.T) {
		right := instruments.Call
		if rapid.Bool().Draw(t, "put") {
			right = instruments.Put
		}
		k := rapid.Int64Range(1, 400_000).Draw(t, "strike_ticks") * 50
		s := optionSpec(t, right, k)
		n := rapid.Int64Range(1, 100).Draw(t, "contracts")
		m := MarkInput{
			Mark:     q(rapid.Int64Range(0, 100_000).Draw(t, "premium_ticks") * 50),
			HasIndex: true, Index: q(rapid.Int64Range(1, 20_000_000).Draw(t, "index")), IndexScale: 2,
		}
		long, err := RequirementFor(p, s, q(n), m, farFromExpiry)
		if err != nil {
			t.Fatal(err)
		}
		if !long.Initial.IsZero() || !long.Maintenance.IsZero() {
			t.Fatal("a held option needs margin beyond its premium")
		}
		short, err := RequirementFor(p, s, q(-n), m, farFromExpiry)
		if err != nil {
			t.Fatal(err)
		}
		minMargin, _ := short.Notional.MulBPSChecked(p.OptionMinMarginBPS, money.RoundCeil)
		if short.Initial.Cmp(minMargin) < 0 {
			t.Fatal("below the minimum margin")
		}
		rep, err := StressBook([]StressPosition{{Spec: s, Quantity: q(-n), Mark: m}}, p.OptionShocksBPS)
		if err != nil {
			t.Fatal(err)
		}
		// Every scenario loss is covered (the requirement rounds up, the
		// stress revaluation down, so coverage can only be generous).
		if rep.WorstPnL.Neg().Sub(money.QuantityFromInt64(n)).Cmp(short.ScenarioLoss) > 0 {
			t.Fatalf("worst stress loss %s exceeds scenario loss %s", rep.WorstPnL.Neg(), short.ScenarioLoss)
		}
		if short.Maintenance.Cmp(short.Initial) > 0 {
			t.Fatal("maintenance above initial")
		}
	})
}

// TestProp_LinearMarginGrowsWithSize: more contracts never need less margin.
func TestProp_LinearMarginGrowsWithSize(t *testing.T) {
	p := DefaultPolicy()
	rapid.Check(t, func(t *rapid.T) {
		s := perpSpec(t)
		m := MarkInput{Mark: q(rapid.Int64Range(1, 400_000).Draw(t, "mark_ticks") * 50)}
		n := rapid.Int64Range(1, 10_000).Draw(t, "n")
		a, err := RequirementFor(p, s, q(n), m, farFromExpiry)
		if err != nil {
			t.Fatal(err)
		}
		b, err := RequirementFor(p, s, q(n+1), m, farFromExpiry)
		if err != nil {
			t.Fatal(err)
		}
		if b.Initial.Cmp(a.Initial) < 0 || b.Maintenance.Cmp(a.Maintenance) < 0 {
			t.Fatal("margin fell as the position grew")
		}
		if a.Maintenance.Cmp(a.Initial) > 0 {
			t.Fatal("maintenance above initial")
		}
	})
}
