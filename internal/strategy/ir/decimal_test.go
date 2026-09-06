package ir_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// TestNoFloatingPointInIR is the structural guarantee behind every other
// numeric claim in this package: no float type, float literal or float
// conversion exists in the production sources, so a rounding difference
// between two machines is not expressible (goal PART 224).
func TestNoFloatingPointInIR(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		require.NoError(t, perr)

		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				assert.NotContains(t, []string{"float32", "float64"}, x.Name,
					"%s:%d uses %s; IR numbers are exact decimals", name, fset.Position(x.Pos()).Line, x.Name)
			case *ast.BasicLit:
				if x.Kind == token.FLOAT || x.Kind == token.IMAG {
					t.Errorf("%s:%d has the floating-point literal %s", name, fset.Position(x.Pos()).Line, x.Value)
				}
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok {
					sel := pkg.Name + "." + x.Sel.Name
					for _, banned := range []string{"strconv.ParseFloat", "strconv.FormatFloat", "big.NewFloat", "big.ParseFloat", "math.Round", "math.Floor", "math.Ceil"} {
						assert.NotEqual(t, banned, sel, "%s:%d calls %s", name, fset.Position(x.Pos()).Line, banned)
					}
				}
			}
			return true
		})
	}
	require.Greater(t, checked, 0, "no production sources were inspected")
}

func TestParseDecimalString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		mantissa string
		scale    uint8
	}{
		{"0", "0", 0},
		{"7", "7", 0},
		{"-7", "-7", 0},
		{"123.45", "12345", 2},
		{"-0.5", "-5", 1},
		{"0.000", "0", 3},
		{"+1.25", "125", 2},
		{"  42.0  ", "420", 1},
		{"0.0200", "200", 4},
		{"00123.4", "1234", 1},
		{"0.000000000000000001", "1", 18},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			d, err := ir.ParseDecimalString(c.in)
			require.NoError(t, err)
			assert.Equal(t, c.mantissa, d.Mantissa)
			assert.Equal(t, c.scale, d.Scale)
			require.NoError(t, d.Validate())
		})
	}
}

func TestParseDecimalString_Rejects(t *testing.T) {
	t.Parallel()
	// Every float spelling a model or an SDK might emit is refused: the IR
	// has no exponent form and no special values.
	for _, bad := range []string{
		"", "   ", ".", "-", "+", "1.", ".5", "1.2.3", "1e5", "1E5", "1.5e-3",
		"NaN", "Inf", "-Inf", "0x10", "1,000", "abc", "1 2", "--1", "1-", "٣",
		"0.0000000000000000001", // 19 fractional digits, beyond MaxScale
	} {
		t.Run(bad, func(t *testing.T) {
			_, err := ir.ParseDecimalString(bad)
			assert.Error(t, err, "%q must not parse", bad)
		})
	}

	// Beyond the mantissa digit limit.
	_, err := ir.ParseDecimalString(strings.Repeat("9", ir.MaxMantissaDigits+1))
	assert.ErrorIs(t, err, ir.ErrDecimalOverflow)
}

func TestDecimal_Validate(t *testing.T) {
	t.Parallel()
	valid := []ir.Decimal{{Mantissa: "0", Scale: 0}, {Mantissa: "-1", Scale: 18}, {Mantissa: "12345", Scale: 4}}
	for _, d := range valid {
		assert.NoError(t, d.Validate(), "%+v", d)
	}
	invalid := []ir.Decimal{
		{Mantissa: "", Scale: 0},
		{Mantissa: "007", Scale: 0},   // leading zeros are not canonical
		{Mantissa: "-0", Scale: 0},    // negative zero has one spelling
		{Mantissa: "1.5", Scale: 0},   // the mantissa is an integer
		{Mantissa: "1e5", Scale: 0},   // no exponent form
		{Mantissa: " 1", Scale: 0},    // no padding
		{Mantissa: "+1", Scale: 0},    // no explicit plus
		{Mantissa: "1", Scale: 19},    // beyond MaxScale
		{Mantissa: "abc", Scale: 0},   // not digits
		{Mantissa: "0", Scale: 255},   // beyond MaxScale
		{Mantissa: "-", Scale: 0},     // sign with no digits
		{Mantissa: "1_000", Scale: 0}, // no separators
	}
	for _, d := range invalid {
		assert.Error(t, d.Validate(), "%+v must be rejected", d)
	}
}

func TestDecimal_String(t *testing.T) {
	t.Parallel()
	cases := map[string]ir.Decimal{
		"0":       {Mantissa: "0", Scale: 0},
		"0.00":    {Mantissa: "0", Scale: 2},
		"1.2345":  {Mantissa: "12345", Scale: 4},
		"-0.5":    {Mantissa: "-5", Scale: 1},
		"0.0200":  {Mantissa: "200", Scale: 4},
		"-12.345": {Mantissa: "-12345", Scale: 3},
		"0.001":   {Mantissa: "1", Scale: 3},
	}
	for want, d := range cases {
		assert.Equal(t, want, d.String(), "%+v", d)
	}
	assert.Equal(t, "<invalid>", ir.Decimal{Mantissa: "bad"}.String(), "an invalid decimal never renders as a number")
}

// TestDecimal_StringRoundTrip: rendering and re-parsing is lossless, which
// is what lets the same value cross the Go/TypeScript boundary.
func TestDecimal_StringRoundTrip(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		digits := rapid.StringMatching(`-?[1-9][0-9]{0,20}`).Draw(rt, "mantissa")
		scale := uint8(rapid.IntRange(0, ir.MaxScale).Draw(rt, "scale"))
		d := ir.Decimal{Mantissa: digits, Scale: scale}
		if d.Validate() != nil {
			rt.Skip("generated an invalid decimal")
		}
		back, err := ir.ParseDecimalString(d.String())
		if err != nil {
			rt.Fatalf("re-parsing %q failed: %v", d.String(), err)
		}
		cmp, err := d.Cmp(back)
		if err != nil {
			rt.Fatalf("comparing round-tripped %q: %v", d.String(), err)
		}
		if cmp != 0 {
			rt.Fatalf("round trip changed the value: %q -> %q", d.String(), back.String())
		}
	})
}

// TestDecimal_CmpRequiresEqualScale: comparing across scales is a typed
// error rather than an implicit coercion (STRATEGY_IR.md §2).
func TestDecimal_CmpRequiresEqualScale(t *testing.T) {
	t.Parallel()
	a := ir.Decimal{Mantissa: "100", Scale: 2}  // 1.00
	b := ir.Decimal{Mantissa: "1000", Scale: 3} // 1.000 — numerically equal
	_, err := a.Cmp(b)
	require.ErrorIs(t, err, ir.ErrScaleMismatch, "equal value at a different scale is still a scale error")

	same, err := a.Cmp(ir.Decimal{Mantissa: "100", Scale: 2})
	require.NoError(t, err)
	assert.Equal(t, 0, same)

	less, err := a.Cmp(ir.Decimal{Mantissa: "200", Scale: 2})
	require.NoError(t, err)
	assert.Equal(t, -1, less)
}

func TestDecimal_Arithmetic(t *testing.T) {
	t.Parallel()
	d := func(s string) ir.Decimal { return dec(t, s) }

	cases := []struct {
		name  string
		got   func() (ir.Decimal, error)
		want  string
		scale uint8
	}{
		{"add", func() (ir.Decimal, error) { return ir.Add(d("1.50"), d("2.25"), 2, money.RoundHalfEven) }, "3.75", 2},
		{"add across scales", func() (ir.Decimal, error) { return ir.Add(d("1.5"), d("2.25"), 2, money.RoundHalfEven) }, "3.75", 2},
		{"sub", func() (ir.Decimal, error) { return ir.Sub(d("1.50"), d("2.25"), 2, money.RoundHalfEven) }, "-0.75", 2},
		{"mul", func() (ir.Decimal, error) { return ir.Mul(d("1.50"), d("2.00"), 2, money.RoundHalfEven) }, "3.00", 2},
		{"mul rescales", func() (ir.Decimal, error) { return ir.Mul(d("0.10"), d("0.10"), 4, money.RoundHalfEven) }, "0.0100", 4},
		{"div exact", func() (ir.Decimal, error) { return ir.Div(d("1.00"), d("4.00"), 2, money.RoundHalfEven) }, "0.25", 2},
		{"div repeating", func() (ir.Decimal, error) { return ir.Div(d("1"), d("3"), 4, money.RoundHalfEven) }, "0.3333", 4},
		{"min", func() (ir.Decimal, error) { return ir.Min(d("1.50"), d("2.25"), 2, money.RoundHalfEven) }, "1.50", 2},
		{"max", func() (ir.Decimal, error) { return ir.Max(d("1.50"), d("2.25"), 2, money.RoundHalfEven) }, "2.25", 2},
		{"negative mul", func() (ir.Decimal, error) { return ir.Mul(d("-1.50"), d("2.00"), 2, money.RoundHalfEven) }, "-3.00", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.got()
			require.NoError(t, err)
			assert.Equal(t, c.want, got.String())
			assert.Equal(t, c.scale, got.Scale)
			require.NoError(t, got.Validate())
		})
	}
}

// TestDecimal_DivByZero: never an infinity, never a NaN.
func TestDecimal_DivByZero(t *testing.T) {
	t.Parallel()
	_, err := ir.Div(dec(t, "1.00"), dec(t, "0.00"), 2, money.RoundHalfEven)
	require.ErrorIs(t, err, ir.ErrDivisionByZero)
}

// TestDecimal_RoundingModes checks each mode against a hand-computed table.
// Rounding is always explicit: there is no default.
func TestDecimal_RoundingModes(t *testing.T) {
	t.Parallel()
	// 1.005 and -1.005 at scale 2 exercise every tie rule.
	cases := []struct {
		mode      money.RoundingMode
		pos, neg  string
		modeLabel string
	}{
		{money.RoundDown, "1.00", "-1.00", "down (toward zero)"},
		{money.RoundUp, "1.01", "-1.01", "up (away from zero)"},
		{money.RoundFloor, "1.00", "-1.01", "floor"},
		{money.RoundCeil, "1.01", "-1.00", "ceil"},
		{money.RoundHalfUp, "1.01", "-1.01", "half up"},
		{money.RoundHalfEven, "1.00", "-1.00", "half even ties to 1.00"},
	}
	for _, c := range cases {
		t.Run(c.modeLabel, func(t *testing.T) {
			pos, err := dec(t, "1.005").Rescale(2, c.mode)
			require.NoError(t, err)
			assert.Equal(t, c.pos, pos.String())

			neg, err := dec(t, "-1.005").Rescale(2, c.mode)
			require.NoError(t, err)
			assert.Equal(t, c.neg, neg.String())
		})
	}

	// RoundExact refuses to lose a digit rather than silently rounding.
	_, err := dec(t, "1.005").Rescale(2, money.RoundExact)
	require.ErrorIs(t, err, money.ErrPrecisionLoss)

	exact, err := dec(t, "1.500").Rescale(2, money.RoundExact)
	require.NoError(t, err, "dropping a trailing zero loses nothing")
	assert.Equal(t, "1.50", exact.String())
}

// TestDecimal_RescaleUpIsExact: widening never changes the value.
func TestDecimal_RescaleUpIsExact(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		mantissa := rapid.StringMatching(`-?[1-9][0-9]{0,15}`).Draw(rt, "mantissa")
		from := uint8(rapid.IntRange(0, 9).Draw(rt, "from"))
		to := uint8(rapid.IntRange(int(from), ir.MaxScale).Draw(rt, "to"))
		d := ir.Decimal{Mantissa: mantissa, Scale: from}
		if d.Validate() != nil {
			rt.Skip("invalid generated decimal")
		}
		up, err := d.Rescale(to, money.RoundExact)
		if err != nil {
			rt.Fatalf("widening %s from scale %d to %d failed: %v", d, from, to, err)
		}
		if up.String() != withScale(d.String(), int(to)-int(from)) {
			rt.Fatalf("widening changed the rendered value: %s -> %s", d, up)
		}
		back, err := up.Rescale(from, money.RoundExact)
		if err != nil {
			rt.Fatalf("narrowing back failed: %v", err)
		}
		if back.Mantissa != d.Mantissa || back.Scale != d.Scale {
			rt.Fatalf("round trip changed %+v into %+v", d, back)
		}
	})
}

// withScale appends n zeros to a rendered decimal, adding the point when the
// value had no fractional part.
func withScale(s string, n int) string {
	if n == 0 {
		return s
	}
	if !strings.Contains(s, ".") {
		return s + "." + strings.Repeat("0", n)
	}
	return s + strings.Repeat("0", n)
}

// TestDecimal_AdditionIsAssociativeAtFixedScale: with an explicit scale and
// exact rounding, the arithmetic behaves like the integers it is, not like
// floating point.
func TestDecimal_AdditionIsAssociative(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		gen := func(label string) ir.Decimal {
			return ir.Decimal{
				Mantissa: rapid.StringMatching(`-?[1-9][0-9]{0,12}`).Draw(rt, label),
				Scale:    4,
			}
		}
		a, b, c := gen("a"), gen("b"), gen("c")
		for _, d := range []ir.Decimal{a, b, c} {
			if d.Validate() != nil {
				rt.Skip("invalid generated decimal")
			}
		}
		left, err1 := ir.Add(a, b, 4, money.RoundExact)
		if err1 != nil {
			rt.Skip("overflow")
		}
		left, err1 = ir.Add(left, c, 4, money.RoundExact)
		right, err2 := ir.Add(b, c, 4, money.RoundExact)
		if err2 != nil {
			rt.Skip("overflow")
		}
		right, err2 = ir.Add(a, right, 4, money.RoundExact)
		if err1 != nil || err2 != nil {
			rt.Skip("overflow")
		}
		if left.Mantissa != right.Mantissa || left.Scale != right.Scale {
			rt.Fatalf("(a+b)+c = %s but a+(b+c) = %s", left, right)
		}
	})
}

func TestDecimal_Probability(t *testing.T) {
	t.Parallel()
	valid := []string{"0.0000", "1.0000", "0.6500", "0.0001"}
	for _, s := range valid {
		assert.True(t, dec(t, s).IsProbability(), "%s is a probability", s)
	}
	// Out of range, or the right value at the wrong scale: both refused, so a
	// probability column can never receive 1.5 or a scale-2 approximation.
	assert.False(t, dec(t, "1.0001").IsProbability(), "above one")
	assert.False(t, dec(t, "-0.0001").IsProbability(), "below zero")
	assert.False(t, dec(t, "0.65").IsProbability(), "wrong scale")
	assert.False(t, dec(t, "1").IsProbability(), "wrong scale")
	assert.False(t, ir.Decimal{Mantissa: "bad", Scale: 4}.IsProbability())
}

func TestDecimal_MoneyConversions(t *testing.T) {
	t.Parallel()
	u, err := dec(t, "50.00").ToUSD()
	require.NoError(t, err)
	assert.Equal(t, int64(5000), u.Minor())
	assert.Equal(t, "50.00", u.String())

	// A value finer than a cent is a conversion error, never a silent round.
	_, err = dec(t, "50.005").ToUSD()
	require.ErrorIs(t, err, money.ErrPrecisionLoss)

	neg, err := dec(t, "-1.23").ToUSD()
	require.NoError(t, err)
	assert.Equal(t, int64(-123), neg.Minor())

	assert.Equal(t, "50.00", ir.DecimalFromUSD(money.USDFromMinor(5000)).String())
	assert.Equal(t, uint8(2), ir.DecimalFromUSD(money.USDFromMinor(5000)).Scale)

	bps, err := dec(t, "150").ToBPS()
	require.NoError(t, err)
	assert.Equal(t, money.BPS(150), bps)

	_, err = dec(t, "1.50").ToBPS()
	require.ErrorIs(t, err, ir.ErrScaleMismatch, "bps are whole basis points")
}

func TestDecimal_Constructors(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "42", ir.DecimalFromInt64(42).String())
	assert.Equal(t, "-42", ir.DecimalFromInt64(-42).String())
	assert.Equal(t, "0", ir.DecimalFromInt64(0).String())

	d := ir.NewDecimal(big.NewInt(12345), 3)
	assert.Equal(t, "12.345", d.String())
	require.NoError(t, d.Validate())

	assert.Equal(t, 1, dec(t, "0.01").Sign())
	assert.Equal(t, -1, dec(t, "-0.01").Sign())
	assert.Equal(t, 0, dec(t, "0.00").Sign())
	assert.True(t, dec(t, "0.000").IsZero())
	assert.False(t, dec(t, "0.001").IsZero())
	assert.Equal(t, 0, ir.Decimal{Mantissa: "garbage"}.Sign(), "an invalid decimal has no sign")
}

func TestParseRounding(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]money.RoundingMode{
		"down": money.RoundDown, "up": money.RoundUp, "half_even": money.RoundHalfEven,
		"half_up": money.RoundHalfUp, "floor": money.RoundFloor, "ceil": money.RoundCeil, "exact": money.RoundExact,
	} {
		got, err := ir.ParseRounding(name)
		require.NoError(t, err, name)
		assert.Equal(t, want, got, name)
	}
	_, err := ir.ParseRounding("banker")
	assert.ErrorIs(t, err, ir.ErrInvalidRounding)
	_, err = ir.ParseRounding("")
	assert.ErrorIs(t, err, ir.ErrInvalidRounding)
}

// TestDecimal_ArithmeticRejectsInvalidOperands: a malformed decimal cannot
// be laundered into a valid one by arithmetic.
func TestDecimal_ArithmeticRejectsInvalidOperands(t *testing.T) {
	t.Parallel()
	bad := ir.Decimal{Mantissa: "not-a-number", Scale: 2}
	good := dec(t, "1.00")
	ops := map[string]func() (ir.Decimal, error){
		"add": func() (ir.Decimal, error) { return ir.Add(bad, good, 2, money.RoundHalfEven) },
		"sub": func() (ir.Decimal, error) { return ir.Sub(good, bad, 2, money.RoundHalfEven) },
		"mul": func() (ir.Decimal, error) { return ir.Mul(bad, good, 2, money.RoundHalfEven) },
		"div": func() (ir.Decimal, error) { return ir.Div(good, bad, 2, money.RoundHalfEven) },
		"min": func() (ir.Decimal, error) { return ir.Min(bad, good, 2, money.RoundHalfEven) },
		"max": func() (ir.Decimal, error) { return ir.Max(bad, good, 2, money.RoundHalfEven) },
	}
	for name, op := range ops {
		_, err := op()
		assert.Error(t, err, "%s must reject an invalid operand", name)
	}
}

// FuzzParseDecimalString: arbitrary text never panics, and anything that
// parses is canonical and re-renders to an equal value.
func FuzzParseDecimalString(f *testing.F) {
	for _, s := range []string{"0", "1.5", "-0.001", "1e5", "", ".", "99999999999999999999.9", "0.0200", "NaN"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ir.ParseDecimalString(s)
		if err != nil {
			return
		}
		require.NoError(t, d.Validate(), "parsed %q into an invalid decimal %+v", s, d)
		again, err := ir.ParseDecimalString(d.String())
		require.NoError(t, err, "rendering of %q did not re-parse", s)
		cmp, err := d.Cmp(again)
		require.NoError(t, err)
		require.Equal(t, 0, cmp, "round trip changed %q", s)
	})
}
