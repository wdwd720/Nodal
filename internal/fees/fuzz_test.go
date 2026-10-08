package fees

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The seeds below are kilobytes of JSON, so the fuzzer's default minimization
// of every new input (up to 60 s each, during which no executions are
// reported) dominates a short run. Run these with a bounded minimization:
//
//	go test ./internal/fees -run '^$' -fuzz '^FuzzVerifyDisclosure$' -fuzztime 60s -fuzzminimizetime 50x

// FuzzVerifyDisclosure: a stored disclosure is external input to the
// auditor. Whatever bytes it holds, verification never panics, and a record
// that verifies is byte-for-byte the quote recomputed from its own input.
func FuzzVerifyDisclosure(f *testing.F) {
	s := workedSchedule()
	for _, side := range []Side{SideBuy, SideSell} {
		quote, err := s.Quote(workedInput(side))
		require.NoError(f, err)
		raw, err := json.Marshal(quote.Disclosure())
		require.NoError(f, err)
		f.Add(raw)
	}
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"input":{"side":"SELL","base_qty":"1","base_decimals":255,"quote_decimals":6,"reference_price":` +
		`{"mantissa":"1","scale":38,"quote_asset":"01920000-0000-7000-8000-0000000000a1","source":"x","at":"2026-01-01T00:00:00Z"}}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var d Disclosure
		if err := json.Unmarshal(raw, &d); err != nil {
			return
		}
		if err := VerifyDisclosure(s, d); err != nil {
			return
		}
		quote, err := s.Quote(d.Input.QuoteInput())
		require.NoError(t, err)
		want, err := quote.Disclosure().Canonical()
		require.NoError(t, err)
		got, err := d.Canonical()
		require.NoError(t, err)
		require.Equal(t, string(want), string(got))
	})
}

// FuzzVerifyPerformanceStatement: a stored statement is external input to
// the replay. Whatever bytes it holds, verification never panics, and any
// statement that verifies is a sandbox record whose lines reconcile to its
// fee.
func FuzzVerifyPerformanceStatement(f *testing.F) {
	terms := perfTerms(f, "100000.00", "100000.00")
	terms.HurdleAnnualBPS = 500
	terms.Flows = []CapitalFlow{flow(f, date(2026, time.January, 12), "-20000.00", "104000.00")}
	p, err := OpenPerformancePeriod(terms)
	require.NoError(f, err)
	_, err = p.Accrue(NAVSnapshot{At: date(2026, time.January, 10), NAV: usd(f, "104000.00")})
	require.NoError(f, err)
	// Gross PnL is 90,000 − 100,000 + 20,000 = 10,000, split across a winner
	// and a loser.
	st, err := p.Crystallize(usd(f, "90000.00"), []StrategyContribution{{"a", usd(f, "12000.00")}, {"b", usd(f, "-2000.00")}})
	require.NoError(f, err)
	raw, err := json.Marshal(st)
	require.NoError(f, err)
	f.Add(raw)
	f.Add([]byte(`{"lines":[{"kind":"ACCRUAL"},{"kind":"CRYSTALLIZATION"}]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var got PerformanceStatement
		if err := json.Unmarshal(raw, &got); err != nil || len(got.Lines) > 64 {
			return
		}
		if err := VerifyPerformanceStatement(terms, got); err != nil {
			return
		}
		require.Equal(t, PerformanceModeSandbox, got.Mode)
		require.False(t, got.FeeAmount.IsNegative())
		last := got.Lines[len(got.Lines)-1]
		require.Equal(t, LineCrystallization, last.Kind)
		require.Equal(t, got.FeeAmount, last.Amount)
		net, err := got.ClosingNAVGross.Sub(got.FeeAmount)
		require.NoError(t, err)
		require.Equal(t, net, got.ClosingNAVNet)
	})
}
