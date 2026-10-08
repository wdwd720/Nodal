package marketplace

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/sandboxrt"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
	"github.com/nodal/controlplane/internal/security"
)

func dec(t require.TestingT, s string) nodalsdk.Decimal {
	d, err := nodalsdk.ParseDecimal(s)
	require.NoError(t, err)
	return d
}

func TestPaperBook_FillsMoveCashAndPositionsExactly(t *testing.T) {
	book := paperBook{cash: dec(t, "10000.00"), positions: map[string]nodalsdk.Decimal{}}
	const ins = "0193b2e0-0000-7000-8000-00000000000a"
	require.NoError(t, book.apply(paperFill{InstrumentID: ins, Side: sandboxrt.SideBuy, Quantity: money.QuantityFromInt64(2_000_000),
		Mark: dec(t, "150.25"), Notional: dec(t, "300.50000000")}))
	assert.Equal(t, "9699.50000000", book.cash.String())
	assert.Equal(t, "2000000", book.positions[ins].String())
	assert.Equal(t, []string{ins}, book.held())
	require.NoError(t, book.apply(paperFill{InstrumentID: ins, Side: sandboxrt.SideSell, Quantity: money.QuantityFromInt64(2_000_000),
		Mark: dec(t, "160.00"), Notional: dec(t, "320.00000000")}))
	assert.Equal(t, "10019.50000000", book.cash.String(), "the round trip's profit is in the cash, exactly")
	assert.Empty(t, book.held(), "a flat position is not held")
	assert.Error(t, book.apply(paperFill{InstrumentID: ins, Side: "HOLD", Quantity: money.QuantityFromInt64(1), Notional: dec(t, "1")}))
}

// TestProp_AFillAtItsReferencePriceNeverChangesTheBooksValue: a paper fill
// exchanges cash for position at the reference price, so valued at that price
// the book is worth exactly what it was before -- value enters or leaves a
// paper book only through price moves, never through a fill.
func TestProp_AFillAtItsReferencePriceNeverChangesTheBooksValue(t *testing.T) {
	const ins = "0193b2e0-0000-7000-8000-00000000000a"
	rapid.Check(t, func(rt *rapid.T) {
		book := paperBook{cash: nodalsdk.IntDecimal(rapid.Int64Range(1, 1_000_000_000).Draw(rt, "capital")), positions: map[string]nodalsdk.Decimal{}}
		decimals := uint8(rapid.IntRange(0, 18).Draw(rt, "decimals"))
		value := func(mark nodalsdk.Decimal) nodalsdk.Decimal {
			whole, err := book.positions[ins].MovePointLeft(int32(decimals))
			require.NoError(rt, err)
			v, err := whole.Mul(mark)
			require.NoError(rt, err)
			return book.cash.Add(v)
		}
		for i := 0; i < rapid.IntRange(1, 12).Draw(rt, "fills"); i++ {
			mark, err := nodalsdk.NewDecimal(rapid.Int64Range(1, 10_000_000).Draw(rt, "mark"), int32(rapid.IntRange(0, 6).Draw(rt, "scale")))
			require.NoError(rt, err)
			qty := money.QuantityFromInt64(rapid.Int64Range(1, 1_000_000_000).Draw(rt, "qty"))
			notional, err := sandboxrt.Notional(qty.String(), decimals, mark)
			require.NoError(rt, err)
			side := rapid.SampledFrom(sandboxrt.AllSides()).Draw(rt, "side")
			before := value(mark)
			require.NoError(rt, book.apply(paperFill{InstrumentID: ins, Side: side, Quantity: qty, Mark: mark, Notional: notional}))
			if after := value(mark); after.Cmp(before) != 0 {
				rt.Fatalf("a %s fill at %s moved the book from %s to %s", side, mark, before, after)
			}
		}
	})
}

func TestDecimalOf_RendersAStoredPriceExactly(t *testing.T) {
	for _, c := range []struct {
		mantissa string
		scale    int32
		want     string
	}{
		{"15025", 2, "150.25"}, {"15025", 0, "15025"}, {"5", 3, "0.005"}, {"0", 2, "0.00"}, {"000150", 2, "1.50"},
		{"123456789012345678901234567890", 20, "1234567890.12345678901234567890"},
	} {
		d, err := decimalOf(c.mantissa, c.scale)
		require.NoError(t, err, c.mantissa)
		assert.Equal(t, c.want, d.String())
	}
	for _, bad := range []struct {
		mantissa string
		scale    int32
	}{{"", 0}, {"-1", 0}, {"1.5", 0}, {"12", -1}, {"12", 39}, {"1e5", 0}} {
		_, err := decimalOf(bad.mantissa, bad.scale)
		assert.Error(t, err, "%+v", bad)
	}
}

// FuzzDecimalOf: whatever a stored price row holds, decimalOf either refuses
// it or renders exactly mantissa x 10^-scale.
func FuzzDecimalOf(f *testing.F) {
	f.Add("15025", int32(2))
	f.Add("0", int32(0))
	f.Add("99999999999999999999999999999999999999", int32(38))
	f.Fuzz(func(t *testing.T, mantissa string, scale int32) {
		d, err := decimalOf(mantissa, scale)
		if err != nil {
			return
		}
		want, ok := new(big.Int).SetString(mantissa, 10)
		require.True(t, ok)
		got := d.Unscaled()
		got.Mul(got, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale-d.Scale())), nil))
		if d.Scale() > scale {
			t.Fatalf("scale grew from %d to %d", scale, d.Scale())
		}
		assert.Equal(t, 0, want.Cmp(got), "%s at %d rendered %s", mantissa, scale, d)
	})
}

func TestUSDRounding_NeverOverstatesWhatABookMaySpend(t *testing.T) {
	floor, err := usdFloor(dec(t, "149.999999"))
	require.NoError(t, err)
	assert.Equal(t, money.USDFromMinor(14999), floor)
	ceil, err := usdCeil(dec(t, "149.990001"))
	require.NoError(t, err)
	assert.Equal(t, money.USDFromMinor(15000), ceil)
	exact, err := usdCeil(dec(t, "150.25000000"))
	require.NoError(t, err)
	assert.Equal(t, money.USDFromMinor(15025), exact)
}

func TestUniverse_TheBookComesFirstAndTheInputStaysBounded(t *testing.T) {
	v := Version{Manifest: sandboxrt.Manifest{AllowedInstrumentIDs: []string{"0193b2e0-0000-7000-8000-00000000000b"}},
		SupportedInstrumentIDs: []string{"0193b2e0-0000-7000-8000-00000000000c", "0193b2e0-0000-7000-8000-00000000000b"}}
	book := paperBook{positions: map[string]nodalsdk.Decimal{"0193b2e0-0000-7000-8000-00000000000a": nodalsdk.IntDecimal(5),
		"0193b2e0-0000-7000-8000-00000000000d": {}}}
	assert.Equal(t, []string{"0193b2e0-0000-7000-8000-00000000000a", "0193b2e0-0000-7000-8000-00000000000b",
		"0193b2e0-0000-7000-8000-00000000000c"}, universe(v, book))
	var many []string
	for i := 0; i < 600; i++ {
		many = append(many, econ.NewIntentID().String())
	}
	v.SupportedInstrumentIDs = many
	assert.Len(t, universe(v, book), nodalsdk.MaxInstruments)
}

func TestFollowInput_IsTheBookAndTheReferencePricesAndNothingElse(t *testing.T) {
	const a, b = "0193b2e0-0000-7000-8000-00000000000a", "0193b2e0-0000-7000-8000-00000000000b"
	asOf := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	refs := map[string]reference{
		b: {InstrumentID: b, Kind: "SPOT_PAIR", BaseDecimals: 6, QuoteDecimals: 6, Mark: dec(t, "40.10"), Closes: []nodalsdk.Decimal{dec(t, "41.00"), dec(t, "40.10")}},
		a: {InstrumentID: a, Kind: "SPOT_PAIR", BaseDecimals: 6, QuoteDecimals: 6, Mark: dec(t, "150.25"), Closes: []nodalsdk.Decimal{dec(t, "150.25")}},
	}
	book := paperBook{cash: dec(t, "9849.75000000"), positions: map[string]nodalsdk.Decimal{a: nodalsdk.IntDecimal(1_000_000)}}
	in := followInput(asOf, refs, book, map[string]string{"order_units": "1000000"})
	snap, err := in.Snapshot()
	require.NoError(t, err)
	require.Len(t, snap.Instruments, 2)
	assert.Equal(t, a, snap.Instruments[0].ID)
	assert.False(t, snap.Instruments[0].HasBid, "no order book is invented")
	equity, err := in.Equity()
	require.NoError(t, err)
	assert.Equal(t, "10000.00000000", equity.String(), "9849.75 + one whole unit at 150.25")
	assert.Equal(t, "1000000", in.Config["order_units"])
}

// TestPaperFollow_ACallersSnapshotIsRefusedBeforeAnythingRuns: the service
// builds a follow's input itself; a request that carries one is refused,
// never silently ignored -- and a scheduled follow is the scheduler's.
func TestPaperFollow_ACallersSnapshotIsRefusedBeforeAnythingRuns(t *testing.T) {
	s := &Service{}
	owner := econ.Actor{Type: security.ActorUser, ID: "0193b2e0-0000-7000-8000-0000000000aa"}
	in := sandboxrt.Input{AsOf: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), Portfolio: sandboxrt.InputPortfolio{Cash: "1000000.00"}}
	_, err := s.PaperFollow(context.Background(), nil, FollowRequest{InstallID: InstallID(econ.NewIntentID()), Input: in, Trigger: TriggerExplicit,
		Actor: owner})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "a caller's snapshot is refused")
	_, err = s.PaperFollow(context.Background(), nil, FollowRequest{InstallID: InstallID(econ.NewIntentID()), Trigger: TriggerScheduled,
		Actor: SchedulerActor()})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "a scheduled follow runs through FollowScheduled")
	assert.True(t, inputIsEmpty(sandboxrt.Input{}))
	assert.False(t, inputIsEmpty(sandboxrt.Input{Config: map[string]string{"x": "y"}}))
}

// TestValidate_ABuildNeverRunsInsideATransaction: the transactional form of
// validation refuses without touching its transaction.
func TestValidate_ABuildNeverRunsInsideATransaction(t *testing.T) {
	s := &Service{}
	_, _, err := s.Validate(context.Background(), nil, VersionID(econ.NewIntentID()), econ.SystemActor("validator"))
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	assert.True(t, strings.Contains(err.Error(), "RunValidation"))
}
