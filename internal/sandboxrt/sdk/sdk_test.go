package nodalsdk

import (
	"bytes"
	"errors"
	"go/parser"
	"go/token"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func dec(t *testing.T, s string) Decimal {
	t.Helper()
	d, err := ParseDecimal(s)
	require.NoError(t, err, s)
	return d
}

func TestParseDecimal_AcceptsExactlyThePlainGrammar(t *testing.T) {
	for _, ok := range []string{"0", "1", "-1", "0.5", "150.10", "-0.001", "123456789012345678901234567890.123", "0.00"} {
		d, err := ParseDecimal(ok)
		require.NoError(t, err, ok)
		assert.Equal(t, ok, d.String(), "scale is preserved exactly")
	}
	for _, bad := range []string{"", "-", "+1", "01", "1.", ".5", "1e5", "1.5e2", " 1", "1 ", "1,5", "--1", "-0", "-0.00", "NaN", "Inf", "0x10",
		"1." + strings.Repeat("0", 39), strings.Repeat("9", 101)} {
		_, err := ParseDecimal(bad)
		assert.Error(t, err, "%q must be refused", bad)
	}
	u, err := ParseUnits("42")
	require.NoError(t, err)
	assert.Equal(t, "42", u.String())
	_, err = ParseUnits("4.2")
	assert.Error(t, err)
}

func TestDecimal_ArithmeticIsExact(t *testing.T) {
	a, b := dec(t, "150.25"), dec(t, "0.0001")
	assert.Equal(t, "150.2501", a.Add(b).String())
	assert.Equal(t, "150.2499", a.Sub(b).String())
	m, err := a.Mul(b)
	require.NoError(t, err)
	assert.Equal(t, "0.015025", m.String())
	assert.Equal(t, 0, dec(t, "1.0").Cmp(dec(t, "1.000")))
	assert.Equal(t, "-150.25", a.Neg().String())
	assert.Equal(t, "150.25", a.Neg().Abs().String())
	assert.Equal(t, "300.50", a.MulInt(2).String())
	moved, err := IntDecimal(1_500_000).MovePointLeft(6)
	require.NoError(t, err)
	assert.Equal(t, "1.500000", moved.String())
	_, err = dec(t, "0."+strings.Repeat("1", 30)).Mul(dec(t, "0."+strings.Repeat("1", 10)))
	assert.ErrorIs(t, err, ErrScale)
	_, err = a.Quo(Decimal{}, 2, RoundDown)
	assert.ErrorIs(t, err, ErrDivisionByZero)
	_, err = a.Quo(b, 2, Rounding(99))
	assert.ErrorIs(t, err, ErrRounding)
	assert.True(t, dec(t, "3.000").IsInteger())
	assert.False(t, dec(t, "3.001").IsInteger())
}

func TestDecimal_EveryRoundingModeIsNamedAndExact(t *testing.T) {
	cases := []struct {
		value string
		mode  Rounding
		want  string
	}{
		{"2.5", RoundHalfEven, "2"}, {"3.5", RoundHalfEven, "4"}, {"-2.5", RoundHalfEven, "-2"},
		{"2.5", RoundHalfUp, "3"}, {"-2.5", RoundHalfUp, "-3"}, {"2.4", RoundHalfUp, "2"},
		{"2.1", RoundUp, "3"}, {"-2.1", RoundUp, "-3"},
		{"2.9", RoundDown, "2"}, {"-2.9", RoundDown, "-2"},
		{"2.1", RoundCeil, "3"}, {"-2.9", RoundCeil, "-2"},
		{"2.9", RoundFloor, "2"}, {"-2.1", RoundFloor, "-3"},
	}
	for _, c := range cases {
		got, err := dec(t, c.value).Rescale(0, c.mode)
		require.NoError(t, err)
		assert.Equal(t, c.want, got.String(), "%s rounded with %d", c.value, c.mode)
	}
	third, err := IntDecimal(1).QuoInt(3, 6, RoundHalfEven)
	require.NoError(t, err)
	assert.Equal(t, "0.333333", third.String())
	twoThirds, err := IntDecimal(2).QuoInt(3, 6, RoundUp)
	require.NoError(t, err)
	assert.Equal(t, "0.666667", twoThirds.String())
}

// TestProp_QuoBracketsTheExactQuotient: for every mode, the rounded quotient
// q at scale s satisfies |q - x/y| < 10^-s, and the direction matches the
// mode -- checked with integers only.
func TestProp_QuoBracketsTheExactQuotient(t *testing.T) {
	modes := []Rounding{RoundDown, RoundUp, RoundHalfEven, RoundHalfUp, RoundFloor, RoundCeil}
	rapid.Check(t, func(rt *rapid.T) {
		x := IntDecimal(rapid.Int64Range(-1e12, 1e12).Draw(rt, "x"))
		yv := rapid.Int64Range(-1e6, 1e6).Filter(func(v int64) bool { return v != 0 }).Draw(rt, "y")
		y := IntDecimal(yv)
		s := int32(rapid.IntRange(0, 8).Draw(rt, "scale"))
		mode := rapid.SampledFrom(modes).Draw(rt, "mode")
		q, err := x.Quo(y, s, mode)
		if err != nil {
			rt.Fatal(err)
		}
		// Compare q x y x 10^0 with x x 10^s: diff = q.unscaled*y - x*10^s.
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(s)), nil)
		lhs := new(big.Int).Mul(q.Unscaled(), big.NewInt(yv))
		rhs := new(big.Int).Mul(x.Unscaled(), scale)
		diff := new(big.Int).Sub(lhs, rhs)
		absY := new(big.Int).Abs(big.NewInt(yv))
		if new(big.Int).Abs(diff).Cmp(absY) >= 0 {
			rt.Fatalf("q=%s is more than one unit from %s/%d", q, x, yv)
		}
		// Direction of q relative to the exact quotient: sign(diff) x sign(y).
		dir := diff.Sign() * big.NewInt(yv).Sign()
		switch mode {
		case RoundFloor:
			if dir > 0 {
				rt.Fatalf("floor rounded up: %s", q)
			}
		case RoundCeil:
			if dir < 0 {
				rt.Fatalf("ceil rounded down: %s", q)
			}
		case RoundDown:
			if dir != 0 && dir == q.Sign() {
				rt.Fatalf("down rounded away from zero: %s", q)
			}
		case RoundUp:
			if dir != 0 && dir != q.Sign() && q.Sign() != 0 {
				rt.Fatalf("up rounded toward zero: %s", q)
			}
		}
	})
}

func TestProp_DecimalStringRoundTrips(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		d, err := NewDecimal(rapid.Int64().Draw(rt, "u"), int32(rapid.IntRange(0, int(MaxScale)).Draw(rt, "s")))
		if err != nil {
			rt.Fatal(err)
		}
		back, err := ParseDecimal(d.String())
		if d.IsZero() && strings.HasPrefix(d.String(), "-") {
			rt.Fatal("negative zero rendered")
		}
		if err != nil {
			rt.Fatalf("%s does not parse back: %v", d, err)
		}
		if back.Cmp(d) != 0 || back.Scale() != d.Scale() || back.String() != d.String() {
			rt.Fatalf("%s round-tripped to %s", d, back)
		}
	})
}

func TestIndicators_AreExact(t *testing.T) {
	series := []Decimal{dec(t, "10"), dec(t, "11"), dec(t, "12"), dec(t, "13"), dec(t, "15")}
	sma, err := SMA(series, 3, 4, RoundHalfEven)
	require.NoError(t, err)
	assert.Equal(t, "13.3333", sma.String())
	// EMA(3): seed SMA(10,11,12)=11; alpha=2/4: 11+(13-11)/2=12; 12+(15-12)/2=13.5
	ema, err := EMA(series, 3, 2, RoundHalfEven)
	require.NoError(t, err)
	assert.Equal(t, "13.50", ema.String())
	lo, err := Min(series)
	require.NoError(t, err)
	hi, err := Max(series)
	require.NoError(t, err)
	assert.Equal(t, "10", lo.String())
	assert.Equal(t, "15", hi.String())
	rets, err := Returns(series, 4, RoundHalfEven)
	require.NoError(t, err)
	got := make([]string, len(rets))
	for i, r := range rets {
		got[i] = r.String()
	}
	assert.Equal(t, []string{"0.1000", "0.0909", "0.0833", "0.1538"}, got)
	assert.Equal(t, "61", Sum(series).String())

	_, err = SMA(series, 0, 2, RoundDown)
	assert.ErrorIs(t, err, ErrWindow)
	_, err = SMA(series, 6, 2, RoundDown)
	assert.ErrorIs(t, err, ErrWindow)
	_, err = Min(nil)
	assert.ErrorIs(t, err, ErrEmpty)
	_, err = Returns([]Decimal{dec(t, "0"), dec(t, "1")}, 2, RoundDown)
	assert.ErrorIs(t, err, ErrDivisionByZero, "a zero base is an error, not an infinity")
}

func TestCheckedInt64_RefusesOverflow(t *testing.T) {
	const max, min = int64(1<<63 - 1), int64(-1 << 63)
	_, err := AddInt64(max, 1)
	assert.ErrorIs(t, err, ErrOverflow)
	_, err = SubInt64(min, 1)
	assert.ErrorIs(t, err, ErrOverflow)
	_, err = MulInt64(max, 2)
	assert.ErrorIs(t, err, ErrOverflow)
	_, err = MulInt64(min, -1)
	assert.ErrorIs(t, err, ErrOverflow)
	v, err := MulInt64(-3, 4)
	require.NoError(t, err)
	assert.Equal(t, int64(-12), v)
	v, err = AddInt64(max, -1)
	require.NoError(t, err)
	assert.Equal(t, max-1, v)
}

const sampleInput = `{"as_of":"2026-10-07T12:00:00Z","config":{"order_units":"5"},"instruments":[` +
	`{"base_decimals":6,"best_ask":"10.2","best_bid":"10.0","closes":["9.5","10.0"],"instrument_id":"0193b2e0-0000-7000-8000-00000000000a","kind":"SPOT_PAIR","mark":"10.1","quote_decimals":6}],` +
	`"portfolio":{"cash":"100.00","positions":[]},"schema":"nodal.sandbox.input/v1"}`

func TestSession_WritesOnlyProtocolRecords(t *testing.T) {
	var out, diag bytes.Buffer
	code := run(strings.NewReader(sampleInput), &out, &diag, func(s *Session) error {
		ins := s.Instruments()
		require.Len(t, ins, 1)
		units, _ := s.Config("order_units")
		if err := s.EmitIntent("ACQUIRE_EXPOSURE", ins[0].ID, "BUY", units, "10.15"); err != nil {
			return err
		}
		// Refused locally: nothing is written for these.
		assert.Error(t, s.EmitIntent("WITHDRAW", ins[0].ID, "SELL", "1", ""))
		assert.Error(t, s.EmitIntent("ACQUIRE_EXPOSURE", "0193b2e0-0000-7000-8000-0000000000ff", "BUY", "1", ""))
		assert.Error(t, s.EmitIntent("ACQUIRE_EXPOSURE", ins[0].ID, "SELL", "1", ""))
		assert.Error(t, s.EmitIntent("ACQUIRE_EXPOSURE", ins[0].ID, "BUY", "1.5", ""))
		assert.Error(t, s.Log("Bad Event", nil))
		assert.Error(t, s.Log("ok", map[string]string{"k": strings.Repeat("x", MaxLogValueBytes+1)}))
		return s.Log("done", map[string]string{"b": "2", "a": "1"})
	})
	require.Equal(t, exitOK, code, diag.String())
	assert.Equal(t,
		`{"type":"intent","seq":1,"kind":"ACQUIRE_EXPOSURE","instrument_id":"0193b2e0-0000-7000-8000-00000000000a","side":"BUY","quantity":"5","limit_price":"10.15"}`+"\n"+
			`{"type":"log","seq":2,"event":"done","fields":{"a":"1","b":"2"}}`+"\n",
		out.String())
}

func TestRun_ExitStatusTellsTheHostWhatHappened(t *testing.T) {
	var out, diag bytes.Buffer
	assert.Equal(t, exitInputInvalid, run(strings.NewReader("{}"), &out, &diag, func(*Session) error { return nil }))
	assert.Equal(t, exitInputInvalid, run(strings.NewReader(sampleInput+"{}"), &out, &diag, func(*Session) error { return nil }))
	assert.Equal(t, exitStrategyError, run(strings.NewReader(sampleInput), &out, &diag, func(*Session) error { return errors.New("no") }))
	assert.Contains(t, diag.String(), "strategy error")
	assert.Equal(t, exitOK, run(strings.NewReader(sampleInput), &out, &diag, func(*Session) error { return nil }))
}

func TestDecodeSnapshot_RefusesAnythingButTheDocumentedFormat(t *testing.T) {
	_, err := DecodeSnapshot([]byte(sampleInput))
	require.NoError(t, err)
	for name, doc := range map[string]string{
		"unknown field":   strings.Replace(sampleInput, `"schema"`, `"secret":"x","schema"`, 1),
		"wrong schema":    strings.Replace(sampleInput, "nodal.sandbox.input/v1", "v0", 1),
		"float mark":      strings.Replace(sampleInput, `"mark":"10.1"`, `"mark":10.1`, 1),
		"bid above ask":   strings.Replace(sampleInput, `"best_bid":"10.0"`, `"best_bid":"11.0"`, 1),
		"trailing":        sampleInput + " []",
		"negative cash":   strings.Replace(sampleInput, `"cash":"100.00"`, `"cash":"-1"`, 1),
		"uppercase id":    strings.Replace(sampleInput, "00000000000a", "00000000000A", 1),
		"not json at all": "hello",
	} {
		_, err := DecodeSnapshot([]byte(doc))
		assert.ErrorIs(t, err, ErrInvalidInput, name)
	}
}

func FuzzParseDecimal(f *testing.F) {
	for _, s := range []string{"0", "-1.5", "150.10", "1e5", "", "-0", "00.1", "9999999999999999999999.99999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDecimal(s)
		if err != nil {
			return
		}
		if d.String() != s {
			t.Fatalf("%q parsed but renders as %q: the grammar is not canonical", s, d.String())
		}
		r, err := d.Rescale(2, RoundHalfEven)
		if err == nil && r.Scale() != 2 {
			t.Fatal("rescale lost its scale")
		}
	})
}

func FuzzDecodeSnapshot(f *testing.F) {
	f.Add([]byte(sampleInput))
	f.Add([]byte("{}"))
	f.Add([]byte(`{"schema":"nodal.sandbox.input/v1","as_of":"x","instruments":null,"portfolio":{"cash":"0"},"config":null}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		snap, err := DecodeSnapshot(b)
		if err != nil {
			return
		}
		for i, ins := range snap.Instruments {
			if !ValidID(ins.ID) || ins.Mark.Sign() <= 0 || (i > 0 && snap.Instruments[i-1].ID >= ins.ID) {
				t.Fatalf("an invalid snapshot decoded: %+v", ins)
			}
		}
	})
}

// TestSDK_ImportsOnlyItsStandardLibrarySet pins what the SDK -- which is
// compiled into every strategy -- may import.
func TestSDK_ImportsOnlyItsStandardLibrarySet(t *testing.T) {
	allowed := map[string]bool{"bufio": true, "bytes": true, "encoding/json": true, "errors": true, "io": true, "math/big": true,
		"os": true, "sort": true, "strconv": true, "strings": true, "unicode/utf8": true}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var seen []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			assert.True(t, allowed[p], "%s imports %s", e.Name(), p)
			seen = append(seen, p)
		}
	}
	sort.Strings(seen)
	assert.NotEmpty(t, seen)
}
