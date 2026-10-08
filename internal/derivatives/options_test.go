package derivatives

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
)

// TestOptionPayoff_IsExactAcrossAGrid checks intrinsic value and settlement
// value per contract for every (spot, strike, right) of a grid against the
// definition: max(S − K, 0) for a call, max(K − S, 0) for a put, and one
// contract's value = intrinsic / tick × tick value.
func TestOptionPayoff_IsExactAcrossAGrid(t *testing.T) {
	spots := []int64{0, 50, 3_250_000, 6_499_950, 6_500_000, 6_500_050, 9_750_000, 13_000_000, 1_000_000_000}
	strikes := []int64{50, 3_250_000, 6_500_000, 9_750_000}
	for _, right := range []instruments.OptionRight{instruments.Call, instruments.Put} {
		for _, k := range strikes {
			s := optionSpec(t, right, k)
			for _, spot := range spots {
				want := spot - k
				if right == instruments.Put {
					want = k - spot
				}
				if want < 0 {
					want = 0
				}
				ip, err := IntrinsicPrice(s, q(spot))
				require.NoError(t, err)
				assert.Equal(t, q(want).String(), ip.String(), "%s K=%d S=%d", right, k, spot)
				per, err := IntrinsicPerContract(s, q(spot))
				require.NoError(t, err)
				assert.Equal(t, q(want/50).Mul(q(5_000)).String(), per.String(), "%s K=%d S=%d per contract", right, k, spot)
			}
		}
	}
	s := optionSpec(t, instruments.Call, 6_500_000)
	_, err := IntrinsicPrice(s, q(6_500_010))
	require.Error(t, err, "a settlement price off the tick grid is refused, never rounded silently")
	_, err = IntrinsicPrice(perpSpec(t), q(6_500_000))
	require.Error(t, err)
}

// TestProp_OptionPayoffIsExactAndPutCallParityHolds: for any tick-aligned
// spot and strike, call − put = S − K exactly (parity of intrinsic values).
func TestProp_OptionPayoffIsExactAndPutCallParityHolds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		k := rapid.Int64Range(1, 1_000_000).Draw(t, "strike_ticks") * 50
		spot := rapid.Int64Range(0, 2_000_000).Draw(t, "spot_ticks") * 50
		c, err := IntrinsicPrice(optionSpec(t, instruments.Call, k), q(spot))
		if err != nil {
			t.Fatal(err)
		}
		p, err := IntrinsicPrice(optionSpec(t, instruments.Put, k), q(spot))
		if err != nil {
			t.Fatal(err)
		}
		if !c.Sub(p).Equal(q(spot - k)) {
			t.Fatalf("call %s − put %s != S − K = %d", c, p, spot-k)
		}
		if c.IsNegative() || p.IsNegative() || (!c.IsZero() && !p.IsZero()) {
			t.Fatalf("intrinsic values %s, %s", c, p)
		}
	})
}

func TestAssignFIFO_AssignsWritersInOrderThenTheHouse(t *testing.T) {
	allocs, err := AssignFIFO([]money.Quantity{q(5), q(4)}, []money.Quantity{q(3), q(2), q(1)})
	require.NoError(t, err)
	want := []Allocation{
		{Exercise: 0, Writer: 0, Quantity: q(3)},
		{Exercise: 0, Writer: 1, Quantity: q(2)},
		{Exercise: 1, Writer: 2, Quantity: q(1)},
		{Exercise: 1, Writer: HouseWriter, Quantity: q(3)},
	}
	require.Len(t, allocs, len(want))
	for i := range want {
		assert.Equal(t, want[i].Exercise, allocs[i].Exercise, i)
		assert.Equal(t, want[i].Writer, allocs[i].Writer, i)
		assert.Equal(t, want[i].Quantity.String(), allocs[i].Quantity.String(), i)
	}
	_, err = AssignFIFO([]money.Quantity{q(0)}, nil)
	require.Error(t, err)
	_, err = AssignFIFO([]money.Quantity{q(1)}, []money.Quantity{q(-1)})
	require.Error(t, err)
}

// TestProp_AssignFIFOConserves: every exercised contract is assigned exactly
// once; no writer is assigned more than it wrote; writers are consumed in
// order, each exhausted before the next is touched; the clearing house takes
// only what no writer is left to take.
func TestProp_AssignFIFOConserves(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ex := rapid.SliceOfN(rapid.Int64Range(1, 100), 1, 6).Draw(t, "exercised")
		wr := rapid.SliceOfN(rapid.Int64Range(1, 100), 0, 6).Draw(t, "writers")
		exQ := make([]money.Quantity, len(ex))
		var totalEx, totalWr int64
		for i, v := range ex {
			exQ[i] = q(v)
			totalEx += v
		}
		wrQ := make([]money.Quantity, len(wr))
		for i, v := range wr {
			wrQ[i] = q(v)
			totalWr += v
		}
		allocs, err := AssignFIFO(exQ, wrQ)
		if err != nil {
			t.Fatal(err)
		}
		perEx := make([]int64, len(ex))
		perWr := make([]int64, len(wr))
		var house int64
		lastWriter := 0
		for _, a := range allocs {
			n, _ := a.Quantity.Int64()
			if n <= 0 {
				t.Fatalf("non-positive allocation %+v", a)
			}
			perEx[a.Exercise] += n
			if a.Writer == HouseWriter {
				house += n
				continue
			}
			if a.Writer < lastWriter {
				t.Fatal("writers assigned out of order")
			}
			for j := lastWriter; j < a.Writer; j++ {
				if perWr[j] != wr[j] {
					t.Fatalf("writer %d moved past before it was exhausted", j)
				}
			}
			lastWriter = a.Writer
			perWr[a.Writer] += n
		}
		for i := range ex {
			if perEx[i] != ex[i] {
				t.Fatalf("exercise %d allocated %d of %d", i, perEx[i], ex[i])
			}
		}
		var toWriters int64
		for j := range wr {
			if perWr[j] > wr[j] {
				t.Fatalf("writer %d assigned %d of %d", j, perWr[j], wr[j])
			}
			toWriters += perWr[j]
		}
		wantWriters := min(totalEx, totalWr)
		if toWriters != wantWriters || house != totalEx-wantWriters {
			t.Fatalf("writers %d house %d for exercised %d written %d", toWriters, house, totalEx, totalWr)
		}
	})
}

func TestOptionDivergence_IsTheDistanceOutsideTheModelFreeBounds(t *testing.T) {
	call := optionSpec(t, instruments.Call, 6_500_000)
	// Index 66 000.00: intrinsic 1 000.00.
	d, err := OptionDivergenceBPS(call, q(150_000), q(6_600_000), 2)
	require.NoError(t, err)
	assert.Equal(t, money.BPS(0), d, "a premium above intrinsic and below spot is consistent")
	d, err = OptionDivergenceBPS(call, q(90_000), q(6_600_000), 2)
	require.NoError(t, err)
	// 100.00 below intrinsic over 66 000.00 = 15.15.. bps -> 16
	assert.Equal(t, money.BPS(16), d)
	d, err = OptionDivergenceBPS(call, q(6_930_000), q(6_600_000), 2)
	require.NoError(t, err)
	assert.Equal(t, money.BPS(500), d, "a call priced 5% above its underlying")
	put := optionSpec(t, instruments.Put, 6_500_000)
	d, err = OptionDivergenceBPS(put, q(0), q(6_600_000), 2)
	require.NoError(t, err)
	assert.Equal(t, money.BPS(0), d, "an out-of-the-money put at zero is consistent")
	d, err = OptionDivergenceBPS(put, q(6_825_000), q(6_600_000_000), 5)
	require.NoError(t, err)
	// 3 250.00 above the strike over 66 000.00000 = 492.42.. bps -> 493
	assert.Equal(t, money.BPS(493), d, "a put above its strike, index at a finer scale")
	_, err = OptionDivergenceBPS(put, q(0), q(0), 2)
	require.Error(t, err)
}
