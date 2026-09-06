package positions

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func q(t testing.TB, s string) money.Quantity {
	t.Helper()
	v, err := money.ParseQuantity(s)
	require.NoError(t, err)
	return v
}

func usd(t testing.TB, s string) money.USD {
	t.Helper()
	v, err := money.ParseUSD(s)
	require.NoError(t, err)
	return v
}

func lot(t testing.TB, original, open, basis string, offset time.Duration) openLot {
	t.Helper()
	return openLot{ID: NewLotID(), Original: q(t, original), Open: q(t, open), CostBasis: usd(t, basis), AcquiredAt: t0.Add(offset)}
}

func TestCumulativeShare(t *testing.T) {
	cases := []struct {
		total       string
		part, whole string
		want        string
	}{
		{"100.00", "0", "3", "0.00"},
		{"100.00", "1", "3", "33.33"},
		{"100.00", "2", "3", "66.67"},
		{"100.00", "3", "3", "100.00"},
		{"0.01", "1", "2", "0.00"},                       // half-even: 0.5 → 0
		{"0.03", "1", "2", "0.02"},                       // 1.5 → 2
		{"225.19", "1500000000", "3000000000", "112.60"}, // 112.595 → even
	}
	for _, tc := range cases {
		got, err := cumulativeShare(usd(t, tc.total), q(t, tc.part), q(t, tc.whole))
		require.NoError(t, err)
		assert.Equal(t, tc.want, got.String(), "%s × %s/%s", tc.total, tc.part, tc.whole)
	}
	_, err := cumulativeShare(usd(t, "1.00"), q(t, "4"), q(t, "3"))
	require.Error(t, err)
	_, err = cumulativeShare(usd(t, "1.00"), q(t, "1"), q(t, "0"))
	require.Error(t, err)
}

func TestAllocateFIFO_Table(t *testing.T) {
	type want struct {
		take, basis, proceeds, fees, pnl string
	}
	cases := []struct {
		name          string
		lots          []openLot
		qty           string
		proceeds      string
		fees          string
		want          []want
		wantErrCode   errs.Code
		remainingOpen []string // per lot after allocation
	}{
		{
			name: "single lot, partial",
			lots: []openLot{lot(t, "1000000000", "1000000000", "150.00", 0)}, // 1 SOL basis 150
			qty:  "400000000", proceeds: "64.00", fees: "0.10",
			want:          []want{{"400000000", "60.00", "64.00", "0.10", "3.90"}},
			remainingOpen: []string{"600000000"},
		},
		{
			name: "single lot, full",
			lots: []openLot{lot(t, "1000000000", "1000000000", "150.00", 0)},
			qty:  "1000000000", proceeds: "140.00", fees: "0.00",
			want:          []want{{"1000000000", "150.00", "140.00", "0.00", "-10.00"}},
			remainingOpen: []string{"0"},
		},
		{
			name: "two lots, FIFO order, spans both",
			lots: []openLot{
				lot(t, "1000000000", "1000000000", "100.00", 0),
				lot(t, "1000000000", "1000000000", "200.00", time.Hour),
			},
			qty: "1500000000", proceeds: "300.00", fees: "3.00",
			want: []want{
				{"1000000000", "100.00", "200.00", "2.00", "98.00"},
				{"500000000", "100.00", "100.00", "1.00", "-1.00"},
			},
			remainingOpen: []string{"0", "500000000"},
		},
		{
			name: "three lots, remainder cents land on last piece",
			lots: []openLot{
				lot(t, "1", "1", "0.01", 0),
				lot(t, "1", "1", "0.01", time.Hour),
				lot(t, "1", "1", "0.01", 2*time.Hour),
			},
			qty: "3", proceeds: "0.10", fees: "0.02",
			// proceeds cumulative: 0.03, 0.07, 0.10 → 0.03, 0.04, 0.03; fees: 0.01, 0.01, 0.02 → 0.01, 0.00, 0.01
			want: []want{
				{"1", "0.01", "0.03", "0.01", "0.01"},
				{"1", "0.01", "0.04", "0.00", "0.03"},
				{"1", "0.01", "0.03", "0.01", "0.01"},
			},
			remainingOpen: []string{"0", "0", "0"},
		},
		{
			name: "partially consumed lot continues where it left off",
			// original 3, open 1 (2 already consumed): cumulative basis at 2/3 of 1.00 is 0.67, so the last unit gets 0.33.
			lots: []openLot{lot(t, "3", "1", "1.00", 0)},
			qty:  "1", proceeds: "0.50", fees: "0.00",
			want:          []want{{"1", "0.33", "0.50", "0.00", "0.17"}},
			remainingOpen: []string{"0"},
		},
		{
			name: "closed lots with zero open are skipped",
			lots: []openLot{
				lot(t, "5", "0", "5.00", 0),
				lot(t, "5", "5", "6.00", time.Hour),
			},
			qty: "2", proceeds: "4.00", fees: "0.00",
			want:          []want{{"2", "2.40", "4.00", "0.00", "1.60"}},
			remainingOpen: []string{"0", "3"},
		},
		{
			name: "insufficient open quantity",
			lots: []openLot{lot(t, "10", "4", "1.00", 0), lot(t, "10", "5", "1.00", time.Hour)},
			qty:  "10", proceeds: "1.00", fees: "0.00",
			wantErrCode: errs.CodeValidationFailed,
		},
		{
			name: "no lots at all",
			lots: nil,
			qty:  "1", proceeds: "1.00", fees: "0.00",
			wantErrCode: errs.CodeValidationFailed,
		},
		{
			name: "zero quantity",
			lots: []openLot{lot(t, "10", "10", "1.00", 0)},
			qty:  "0", proceeds: "1.00", fees: "0.00",
			wantErrCode: errs.CodeValidationFailed,
		},
		{
			name: "negative proceeds",
			lots: []openLot{lot(t, "10", "10", "1.00", 0)},
			qty:  "1", proceeds: "-1.00", fees: "0.00",
			wantErrCode: errs.CodeValidationFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := allocateFIFO(tc.lots, q(t, tc.qty), usd(t, tc.proceeds), usd(t, tc.fees))
			if tc.wantErrCode != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErrCode, errs.CodeOf(err))
				return
			}
			require.NoError(t, err)
			require.Len(t, got, len(tc.want))
			for i, w := range tc.want {
				a := got[i]
				assert.Equal(t, w.take, a.Take.String(), "take[%d]", i)
				assert.Equal(t, w.basis, a.Basis.String(), "basis[%d]", i)
				assert.Equal(t, w.proceeds, a.Proceeds.String(), "proceeds[%d]", i)
				assert.Equal(t, w.fees, a.Fees.String(), "fees[%d]", i)
				pnl, err := a.pnl()
				require.NoError(t, err)
				assert.Equal(t, w.pnl, pnl.String(), "pnl[%d]", i)
			}
			// Remaining open per lot (lots untouched by the allocation keep their open quantity).
			open := map[LotID]money.Quantity{}
			for _, l := range tc.lots {
				open[l.ID] = l.Open
			}
			for _, a := range got {
				open[a.Lot.ID] = a.OpenAfter
			}
			for i, l := range tc.lots {
				assert.Equal(t, tc.remainingOpen[i], open[l.ID].String(), "open after[%d]", i)
			}
			// The allocations' proceeds and fees always sum to the totals.
			var sp, sf money.USD
			for _, a := range got {
				sp, _ = sp.Add(a.Proceeds)
				sf, _ = sf.Add(a.Fees)
			}
			assert.Equal(t, tc.proceeds, sp.String())
			assert.Equal(t, tc.fees, sf.String())
		})
	}
}

func TestRealizedPnL_SignCases(t *testing.T) {
	l := []openLot{lot(t, "100", "100", "100.00", 0)}
	cases := []struct {
		name     string
		proceeds string
		fees     string
		pnl      string
	}{
		{"gain", "150.00", "1.00", "49.00"},
		{"loss", "80.00", "1.00", "-21.00"},
		{"break even before fees", "100.00", "0.00", "0.00"},
		{"fees turn break-even into loss", "100.00", "0.01", "-0.01"},
		{"zero proceeds (network fee paid in kind)", "0.00", "0.00", "-100.00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := allocateFIFO(l, q(t, "100"), usd(t, tc.proceeds), usd(t, tc.fees))
			require.NoError(t, err)
			require.Len(t, got, 1)
			pnl, err := got[0].pnl()
			require.NoError(t, err)
			assert.Equal(t, tc.pnl, pnl.String())
		})
	}
}

func TestRemainingBasis(t *testing.T) {
	full := lot(t, "3", "3", "1.00", 0)
	rem, err := remainingBasis(full)
	require.NoError(t, err)
	assert.Equal(t, "1.00", rem.String())

	twoThirdsGone := lot(t, "3", "1", "1.00", 0)
	rem, err = remainingBasis(twoThirdsGone)
	require.NoError(t, err)
	assert.Equal(t, "0.33", rem.String()) // 1.00 − round(0.6667) = 1.00 − 0.67

	closed := lot(t, "3", "0", "1.00", 0)
	rem, err = remainingBasis(closed)
	require.NoError(t, err)
	assert.True(t, rem.IsZero())
}

func TestAverageBasis(t *testing.T) {
	// 225.19 USD for 1.5 SOL → 150.12666667 per SOL.
	assert.Equal(t, "150.12666667", averageBasis(usd(t, "225.19"), q(t, "1500000000"), 9))
	// 23.45 USD for 1,000,000 BONK → 0.00002345 per BONK.
	assert.Equal(t, "0.00002345", averageBasis(usd(t, "23.45"), q(t, "100000000000"), 5))
	assert.Equal(t, "0.00000000", averageBasis(usd(t, "1.00"), q(t, "0"), 5))
}

// TestProp_BasisConserved drives the pure allocator through random
// sequences of acquisitions and FIFO disposals and checks, after every
// step, that Σ disposition basis + Σ remaining lot basis == Σ acquisition
// basis exactly, that Σ disposition proceeds/fees equal the totals
// disposed, and that no lot is ever over-consumed.
func TestProp_BasisConserved(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		var lots []openLot
		var acquired money.USD // Σ acquisition basis
		var disposedBasis, disposedProceeds, disposedFees money.USD
		var wantProceeds, wantFees money.USD
		steps := rapid.IntRange(1, 40).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			var open money.Quantity
			for _, l := range lots {
				open = open.Add(l.Open)
			}
			acquire := open.IsZero() || rapid.Bool().Draw(rt, "acquire")
			if acquire {
				qty := money.QuantityFromInt64(rapid.Int64Range(1, 1_000_000_000_000).Draw(rt, "qty"))
				basis := money.USDFromMinor(rapid.Int64Range(0, 10_000_000_00).Draw(rt, "basis"))
				lots = append(lots, openLot{ID: NewLotID(), Original: qty, Open: qty, CostBasis: basis, AcquiredAt: t0.Add(time.Duration(i) * time.Second)})
				var err error
				acquired, err = acquired.Add(basis)
				require.NoError(rt, err)
				continue
			}
			openInt, err := open.Int64()
			require.NoError(rt, err)
			take := money.QuantityFromInt64(rapid.Int64Range(1, openInt).Draw(rt, "take"))
			proceeds := money.USDFromMinor(rapid.Int64Range(0, 10_000_000_00).Draw(rt, "proceeds"))
			fees := money.USDFromMinor(rapid.Int64Range(0, 1_000_00).Draw(rt, "fees"))
			allocs, err := allocateFIFO(lots, take, proceeds, fees)
			require.NoError(rt, err)
			wantProceeds, _ = wantProceeds.Add(proceeds)
			wantFees, _ = wantFees.Add(fees)
			var took money.Quantity
			for _, a := range allocs {
				require.False(rt, a.Take.IsNegative() || a.Take.Cmp(a.Lot.Open) > 0, "over-consumed lot")
				require.False(rt, a.Basis.IsNegative(), "negative basis allocation")
				took = took.Add(a.Take)
				disposedBasis, _ = disposedBasis.Add(a.Basis)
				disposedProceeds, _ = disposedProceeds.Add(a.Proceeds)
				disposedFees, _ = disposedFees.Add(a.Fees)
				// Apply to the model.
				for j := range lots {
					if lots[j].ID == a.Lot.ID {
						lots[j].Open = a.OpenAfter
					}
				}
			}
			require.True(rt, took.Equal(take), "allocated %s of %s", took, take)
			// FIFO: every lot after a lot with remaining open quantity is untouched.
			seenOpen := false
			for _, l := range lots {
				if seenOpen {
					require.True(rt, l.Open.Equal(l.Original), "lot consumed out of FIFO order")
				}
				if l.Open.IsPositive() {
					seenOpen = true
				}
			}
			// Conservation after this step.
			var remaining money.USD
			for _, l := range lots {
				r, err := remainingBasis(l)
				require.NoError(rt, err)
				require.False(rt, r.IsNegative())
				remaining, _ = remaining.Add(r)
			}
			sum, _ := disposedBasis.Add(remaining)
			require.True(rt, sum.Equal(acquired), "Σ disposition basis %s + remaining %s != Σ acquired %s", disposedBasis, remaining, acquired)
			require.True(rt, disposedProceeds.Equal(wantProceeds), "proceeds %s != %s", disposedProceeds, wantProceeds)
			require.True(rt, disposedFees.Equal(wantFees), "fees %s != %s", disposedFees, wantFees)
		}
	})
}
