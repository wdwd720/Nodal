package sandboxrt

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
)

func TestLimits_ValidateBoundsAndHash(t *testing.T) {
	require.NoError(t, DefaultLimits().Validate())
	h1, err := DefaultLimits().Hash()
	require.NoError(t, err)
	h2, err := DefaultLimits().Hash()
	require.NoError(t, err)
	assert.Equal(t, h1, h2)
	assert.Len(t, h1, 64)
	changed := DefaultLimits()
	changed.WallTimeMS++
	h3, err := changed.Hash()
	require.NoError(t, err)
	assert.NotEqual(t, h1, h3, "a changed limit is a different policy")

	cases := map[string]func(*Limits){
		"no version":     func(l *Limits) { l.Version = "" },
		"padded version": func(l *Limits) { l.Version = " v " },
		"tiny memory":    func(l *Limits) { l.MemoryLimitPages = 1 },
		"huge memory":    func(l *Limits) { l.MemoryLimitPages = maxMemoryPages + 1 },
		"no wall time":   func(l *Limits) { l.WallTimeMS = 0 },
		"endless":        func(l *Limits) { l.WallTimeMS = maxWallTimeMS + 1 },
		"no output":      func(l *Limits) { l.MaxOutputBytes = 0 },
		"huge input":     func(l *Limits) { l.MaxInputBytes = maxIOBytes + 1 },
		"huge stderr":    func(l *Limits) { l.MaxStderrBytes = maxStderrBytes + 1 },
		"no records":     func(l *Limits) { l.MaxRecords = 0 },
	}
	for name, mutate := range cases {
		l := DefaultLimits()
		mutate(&l)
		assert.Error(t, l.Validate(), name)
	}
}

func TestInput_CanonicalIsExactlyWhatTheStrategyReads(t *testing.T) {
	in := testInput()
	b, err := in.Canonical()
	require.NoError(t, err)
	snap, err := nodalsdk.DecodeSnapshot(b)
	require.NoError(t, err)
	assert.Equal(t, "2026-10-07T12:00:00Z", snap.AsOf)
	require.Len(t, snap.Instruments, 2)
	assert.Equal(t, insA, snap.Instruments[0].ID)
	assert.Equal(t, "150.25", snap.Instruments[0].Mark.String())
	assert.Equal(t, "1000000", snap.Config["order_units"])

	// Order of the caller's slices and maps does not change the document.
	swapped := testInput()
	swapped.Instruments[0], swapped.Instruments[1] = swapped.Instruments[1], swapped.Instruments[0]
	b2, err := swapped.Canonical()
	require.NoError(t, err)
	assert.Equal(t, b, b2)

	// Nil collections render as empty, never null.
	bare := Input{AsOf: in.AsOf, Portfolio: InputPortfolio{Cash: "0"}}
	b3, err := bare.Canonical()
	require.NoError(t, err)
	assert.NotContains(t, string(b3), "null")
}

func TestInput_MalformedDocumentsAreRefused(t *testing.T) {
	cases := map[string]func(*Input){
		"no as-of":            func(in *Input) { in.AsOf = time.Time{} },
		"negative mark":       func(in *Input) { in.Instruments[0].Mark = "-1.00" },
		"zero mark":           func(in *Input) { in.Instruments[0].Mark = "0" },
		"float mark":          func(in *Input) { in.Instruments[0].Mark = "1.5e2" },
		"bid above ask":       func(in *Input) { in.Instruments[0].BestBid = "200.00" },
		"duplicate id":        func(in *Input) { in.Instruments[1].InstrumentID = insA },
		"id not canonical":    func(in *Input) { in.Instruments[0].InstrumentID = "ABC" },
		"kind lower":          func(in *Input) { in.Instruments[0].Kind = "spot" },
		"bad close":           func(in *Input) { in.Instruments[0].Closes = []string{"x"} },
		"fractional position": func(in *Input) { in.Portfolio.Positions[0].Quantity = "1.5" },
		"negative cash":       func(in *Input) { in.Portfolio.Cash = "-1" },
		"bad config key":      func(in *Input) { in.Config = map[string]string{"Bad Key": "x"} },
		"too many closes": func(in *Input) {
			in.Instruments[0].Closes = make([]string, nodalsdk.MaxClosesPerSeries+1)
			for i := range in.Instruments[0].Closes {
				in.Instruments[0].Closes[i] = "1"
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := testInput()
			mutate(&in)
			_, err := in.Canonical()
			assert.Error(t, err)
		})
	}
}

func TestInput_EquityIsExact(t *testing.T) {
	// 100000.00 cash + 2.000000 units x 150.25 = 100300.50, at full scale.
	eq, err := testInput().Equity()
	require.NoError(t, err)
	assert.Equal(t, "100300.50000000", eq.String())

	orphan := testInput()
	orphan.Portfolio.Positions = append(orphan.Portfolio.Positions, InputPosition{InstrumentID: "0193b2e0-0000-7000-8000-0000000000ff", Quantity: "1"})
	_, err = orphan.Equity()
	assert.Error(t, err, "a position without a mark cannot be valued and is not guessed")
}

// TestProp_InputCanonicalIsDeterministic: any valid input renders to the same
// bytes every time, regardless of the order its collections were built in,
// and the SDK reads back the same numbers.
func TestProp_InputCanonicalIsDeterministic(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 6).Draw(rt, "n")
		in := Input{AsOf: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rapid.Int64Range(0, 1e15).Draw(rt, "t"))),
			Portfolio: InputPortfolio{Cash: strconv.Itoa(rapid.IntRange(0, 1e9).Draw(rt, "cash")) + ".25"}, Config: map[string]string{}}
		for i := 0; i < n; i++ {
			id := "0193b2e0-0000-7000-8000-0000000000" + strconv.FormatInt(int64(16+i), 16)
			in.Instruments = append(in.Instruments, InputInstrument{InstrumentID: id, Kind: "SPOT_PAIR", BaseDecimals: 6, QuoteDecimals: 6,
				Mark: strconv.Itoa(rapid.IntRange(1, 1e6).Draw(rt, "mark")) + ".5", Closes: []string{"1.0", "2.0"}})
			in.Config["k"+strconv.Itoa(i)] = rapid.StringMatching(`[a-z0-9 ]{0,20}`).Draw(rt, "v")
		}
		a, err := in.Canonical()
		if err != nil {
			rt.Fatal(err)
		}
		shuffled := in
		shuffled.Instruments = append([]InputInstrument(nil), in.Instruments...)
		for i := len(shuffled.Instruments) - 1; i > 0; i-- {
			j := rapid.IntRange(0, i).Draw(rt, "swap")
			shuffled.Instruments[i], shuffled.Instruments[j] = shuffled.Instruments[j], shuffled.Instruments[i]
		}
		b, err := shuffled.Canonical()
		if err != nil {
			rt.Fatal(err)
		}
		if string(a) != string(b) {
			rt.Fatalf("canonical form depends on order:\n%s\n%s", a, b)
		}
		snap, err := nodalsdk.DecodeSnapshot(a)
		if err != nil {
			rt.Fatal(err)
		}
		for i, ins := range snap.Instruments {
			if i > 0 && snap.Instruments[i-1].ID >= ins.ID {
				rt.Fatal("instruments not sorted")
			}
		}
	})
}
