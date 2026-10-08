package feeledger

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/money"
)

// A stored schedule, disclosure or statement is external input to the
// auditor's replay. Whatever bytes a row holds, the decoders never panic, and
// anything they accept is byte-for-byte the canonical form of the value they
// return, so the hash recorded with the bytes names that value.
//
// The seeds are kilobytes of JSON; bound minimization on a long run:
//
//	go test ./internal/feeledger -run '^$' -fuzz '^FuzzDecodeSchedule$' -fuzztime 60s -fuzzminimizetime 50x

func FuzzDecodeSchedule(f *testing.F) {
	full, err := fullSchedule().Canonical()
	require.NoError(f, err)
	plain, err := fees.ZeroSchedule(quoteAsset, 6).Canonical()
	require.NoError(f, err)
	for _, valid := range [][]byte{full, plain} {
		_, err := DecodeSchedule(valid)
		require.NoError(f, err, "the valid seeds exercise the accepting path")
	}
	for _, seed := range [][]byte{full, plain, nil, []byte(`{}`), []byte(`null`), append(append([]byte(nil), full...), []byte(`{}`)...),
		[]byte(`{"kind":"fees.schedule.v1","extra":1}`)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		s, err := DecodeSchedule(raw)
		if err != nil {
			return
		}
		require.NoError(t, s.Validate())
		again, err := s.Canonical()
		require.NoError(t, err)
		require.Equal(t, string(raw), string(again), "an accepted schedule is exactly its canonical form")
		h, err := s.Hash()
		require.NoError(t, err)
		require.Equal(t, hashHex(raw), h)
	})
}

func FuzzDecodeDisclosure(f *testing.F) {
	s := fullSchedule()
	for _, side := range []fees.Side{fees.SideBuy, fees.SideSell} {
		quote, err := s.Quote(fees.QuoteInput{Side: side, BaseQty: q(2_000_000), BaseDecimals: 6, QuoteDecimals: 6,
			ReferencePrice: price(f, "100.00"), HasSubscription: true, StrategyOriginated: true})
		require.NoError(f, err)
		b, err := quote.Disclosure().Canonical()
		require.NoError(f, err)
		_, err = decodeDisclosure(b, hashHex(b))
		require.NoError(f, err, "the valid seeds exercise the accepting path")
		f.Add(b)
	}
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		d, err := decodeDisclosure(raw, hashHex(raw))
		if err != nil {
			return
		}
		again, err := d.Canonical()
		require.NoError(t, err)
		require.Equal(t, string(raw), string(again), "an accepted disclosure is exactly its canonical form")
		_, err = decodeDisclosure(raw, hashHex(append(append([]byte(nil), raw...), ' ')))
		require.Error(t, err, "a disclosure is accepted only under its own hash")
	})
}

func FuzzDecodeStatement(f *testing.F) {
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	terms := fees.PerformanceTerms{
		AccountRef: "acct/strategy", TermsVersion: "perf-terms-v1", PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0),
		OpeningNAV: money.USDFromMinor(10_000_000), PriorHWM: money.USDFromMinor(10_000_000), HurdleAnnualBPS: 500, FeeRateBPS: 2_000,
		Crystallization: fees.CrystallizeAtPeriodEnd,
		Flows: []fees.CapitalFlow{{At: start.AddDate(0, 0, 11), Amount: money.USDFromMinor(-2_000_000),
			NAVBefore: money.USDFromMinor(10_400_000)}},
	}
	// Gross PnL is 9,000,000 - 10,000,000 + 2,000,000 = 1,000,000 minor,
	// split across a winner and a loser.
	st, err := fees.ComputePerformanceFee(terms, money.USDFromMinor(9_000_000), []fees.StrategyContribution{
		{Strategy: "a", GrossPnL: money.USDFromMinor(1_200_000)}, {Strategy: "b", GrossPnL: money.USDFromMinor(-200_000)}})
	require.NoError(f, err)
	b, err := st.Canonical()
	require.NoError(f, err)
	_, err = decodeStatement(b, hashHex(b))
	require.NoError(f, err, "the valid seed exercises the accepting path")
	f.Add(b)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"lines":[{"kind":"ACCRUAL"},{"kind":"CRYSTALLIZATION"}]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		got, err := decodeStatement(raw, hashHex(raw))
		if err != nil {
			return
		}
		again, err := got.Canonical()
		require.NoError(t, err)
		require.Equal(t, string(raw), string(again), "an accepted statement is exactly its canonical form")
		_, err = decodeStatement(raw, hashHex(append(append([]byte(nil), raw...), ' ')))
		require.Error(t, err, "a statement is accepted only under its own hash")
	})
}
