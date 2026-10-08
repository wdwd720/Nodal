package fees

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// TestDisclosure_RoundTripsThroughJSONAndVerifies: the record an auditor
// stores is enough, with the schedule, to recompute and prove the quote.
func TestDisclosure_RoundTripsThroughJSONAndVerifies(t *testing.T) {
	for _, s := range []Schedule{workedSchedule(), ladder(t), leanSchedule()} {
		for _, side := range []Side{SideBuy, SideSell} {
			in := workedInput(side)
			in.StrategyOriginated, in.HasSubscription = true, true
			in.TrailingVolume = usd(t, "10000.00")
			d := mustQuote(t, s, in).Disclosure()

			raw, err := json.Marshal(d)
			require.NoError(t, err)
			var stored Disclosure
			require.NoError(t, json.Unmarshal(raw, &stored))
			require.NoError(t, VerifyDisclosure(s, stored))

			want, err := d.Canonical()
			require.NoError(t, err)
			got, err := stored.Canonical()
			require.NoError(t, err)
			require.Equal(t, string(want), string(got), "the stored record re-canonicalises to the same bytes")
		}
	}
}

// TestDisclosure_TamperedRecordFailsVerification: editing any part of a
// stored record -- an amount, a rate, a rounding statement, the schedule
// identity -- breaks verification and names the section.
func TestDisclosure_TamperedRecordFailsVerification(t *testing.T) {
	s := workedSchedule()
	cases := []struct {
		name   string
		tamper func(*Disclosure)
		field  string
	}{
		{"spread amount", func(d *Disclosure) { d.Spread.Amount = d.Spread.Amount.Add(one()) }, "spread"},
		{"total debit", func(d *Disclosure) { d.TotalDebit = d.TotalDebit.Add(one()) }, "total_debit"},
		{"fee line", func(d *Disclosure) { d.Fees.Lines[1].Charged = d.Fees.Lines[1].Charged.Sub(one()) }, "fees"},
		{"rounding statement", func(d *Disclosure) { d.Rounding.Components[1].FavoursPlatform = true }, "rounding"},
		{"schedule identity", func(d *Disclosure) { d.ScheduleVersion = "fees-sched-v0" }, "schedule_version"},
		{"reserve passed off as held", func(d *Disclosure) { d.SlippageReserve.HeldNotCharged = false }, "slippage_reserve"},
		{"record kind", func(d *Disclosure) { d.Kind = "fees.quote_disclosure.v0" }, "kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := mustQuote(t, s, workedInput(SideBuy)).Disclosure()
			c.tamper(&d)
			requireFields(t, VerifyDisclosure(s, d), errs.CodeValidationFailed, c.field)
		})
	}

	// The recorded input is re-validated: a record whose input the
	// schedule refuses cannot verify.
	d := mustQuote(t, s, workedInput(SideBuy)).Disclosure()
	d.Input.QuoteDecimals = 2
	requireFields(t, VerifyDisclosure(s, d), errs.CodeValidationFailed, "quote_decimals")
}

func TestDisclosure_StatesEveryRoundingModeUsed(t *testing.T) {
	r := mustQuote(t, workedSchedule(), workedInput(SideSell)).Disclosure().Rounding
	got := make([]string, 0, len(r.Components))
	for _, m := range r.Components {
		got = append(got, string(m.Component)+"="+m.Mode)
		require.False(t, m.FavoursPlatform, "%s", m.Component)
	}
	require.Equal(t, []string{
		"REFERENCE_NOTIONAL=up", "SPREAD=down", "PERCENTAGE_FEE=down", "VENUE_PASS_THROUGH=down",
		"STRATEGY_FEE=down", "FEE_CAP=down", "SLIPPAGE_RESERVE=up", "EFFECTIVE_PRICE=down",
	}, got)
	require.False(t, r.AnyFavoursPlatform)
	require.False(t, r.PlatformFavourableAllowed)
}

// TestDisclosure_CanonicalForm pins the audit record format, byte for byte
// (key order included, since it is hashed), for a small quote: 100 whole
// units at 12.5 in a two-decimal quote asset under a 10 bps fee, so a
// 1250.00 notional, a 1.25 fee and a 12.5125 all-in price. Changing the
// format changes every stored record hash, so it must be a deliberate,
// versioned act (DisclosureKind).
func TestDisclosure_CanonicalForm(t *testing.T) {
	in := QuoteInput{Side: SideBuy, BaseQty: q("100"), BaseDecimals: 0, QuoteDecimals: 2, ReferencePrice: refPrice("125", 1)}
	s := leanSchedule()
	s.QuoteDecimals = 2
	d := mustQuote(t, s, in).Disclosure()
	b, err := d.Canonical()
	require.NoError(t, err)
	const asset = "01920000-0000-7000-8000-0000000000a1"
	const at = "2026-10-07T12:00:00Z"
	price := func(mantissa string, scale int, source string) string {
		return `{"mantissa":"` + mantissa + `","scale":` + strconv.Itoa(scale) + `,"quote_asset":"` + asset + `","source":"` + source + `","at":"` + at + `"}`
	}
	line := func(component string, rate int, rounding, amount string) string {
		return `{"component":"` + component + `","rate_bps":` + strconv.Itoa(rate) + `,"discount_bps":0,"fixed_amount":"0","rounding":"` + rounding +
			`","computed":"` + amount + `","cap_reduction":"0","charged":"` + amount + `"}`
	}
	mode := func(component, m string) string {
		return `{"component":"` + component + `","mode":"` + m + `","favours_platform":false}`
	}
	want := `{"kind":"fees.quote_disclosure.v1","schedule_version":"lean-v1",` +
		`"schedule_hash":"dd355df5b0032183066270b7ef55f1795793e7744c2be4b926a481671c73965b",` +
		`"input":{"side":"BUY","base_qty":"100","base_decimals":0,"quote_decimals":2,"reference_price":` + price("125", 1, "pyth") + `,` +
		`"trailing_volume_usd":"0.00","has_subscription":false,"strategy_originated":false},` +
		`"quote_asset":"` + asset + `",` +
		`"tier":{"index":-1,"min_trailing_volume_usd":"0.00","percentage_fee_bps":10,"spread_bps":0},` +
		`"reference_price":` + price("125", 1, "pyth") + `,` +
		`"execution_price":` + price("125", 1, "pyth+spread@lean-v1") + `,` +
		`"notional_at_reference":"125000","notional_at_execution":"125000",` +
		`"spread":{"bps":0,"amount":"0","rounding":"down"},` +
		`"fees":{"lines":[` + line("FIXED_FEE", 0, "exact", "0") + `,` + line("PERCENTAGE_FEE", 10, "down", "125") + `,` +
		line("VENUE_PASS_THROUGH", 0, "down", "0") + `,` + line("STRATEGY_FEE", 0, "down", "0") + `],` +
		`"minimum_fee_top_up":"0","computed_total":"125","total":"125","min_fee":"0","max_total_fee_bps":0,` +
		`"max_total_fee_bps_amount":"0","max_total_fee_bps_rounding":"down","max_total_fee":"0",` +
		`"minimum_applied":false,"max_bps_applied":false,"max_absolute_applied":false},` +
		`"network":{"asset":"00000000-0000-0000-0000-000000000000","amount":"0","in_quote_asset":false},` +
		`"slippage_reserve":{"bps":0,"amount":"0","rounding":"up","held_not_charged":true},` +
		`"total_debit":"125125","total_credit":"0","worst_case_total":"125125",` +
		`"effective_price":` + price("125125", 4, "pyth+all-in@lean-v1") + `,` +
		`"rounding":{"components":[` + mode("REFERENCE_NOTIONAL", "down") + `,` + mode("SPREAD", "down") + `,` +
		mode("PERCENTAGE_FEE", "down") + `,` + mode("VENUE_PASS_THROUGH", "down") + `,` + mode("STRATEGY_FEE", "down") + `,` +
		mode("FEE_CAP", "down") + `,` + mode("SLIPPAGE_RESERVE", "up") + `,` + mode("EFFECTIVE_PRICE", "up") + `],` +
		`"platform_favourable_allowed":false,"any_favours_platform":false}}`
	require.Equal(t, want, string(b))

	h, err := d.Hash()
	require.NoError(t, err)
	require.Len(t, h, 64)
	require.Equal(t, money.RoundDown.String(), d.Spread.Rounding)
}
