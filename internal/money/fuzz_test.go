package money

import (
	"errors"
	"strings"
	"testing"
)

// isSentinel reports whether err wraps one of the package's exported sentinels.
func isSentinel(err error) bool {
	for _, s := range []error{ErrOverflow, ErrPrecisionLoss, ErrInvalidFormat, ErrDivisionByZero, ErrInvalidRoundingMode, ErrInvalidPrice} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

func FuzzParseUSD(f *testing.F) {
	for _, s := range []string{
		"0", "0.00", "-0", "1234.56", "-0.01", "+5", "1.234", "1.230", "", " 1", "1.", ".5", "1e5", "NaN", "Inf", "0x10",
		"92233720368547758.07", "-92233720368547758.08", "92233720368547758.08", "-92233720368547758.09",
		"9223372036854775807", "00000000000000000000000000000000001.00", strings.Repeat("9", 70), "1_000", "1,000", "١",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, err := ParseUSD(s)
		if err != nil {
			if !isSentinel(err) {
				t.Fatalf("ParseUSD(%q): non-sentinel error %v", s, err)
			}
			// ParseUSDRound must not accept anything ParseUSD rejects for a
			// reason other than precision.
			if !errors.Is(err, ErrPrecisionLoss) {
				if _, err2 := ParseUSDRound(s, RoundHalfEven); err2 == nil {
					t.Fatalf("ParseUSDRound accepted %q that ParseUSD rejected with %v", s, err)
				}
			}
			return
		}
		rendered := u.String()
		back, err := ParseUSD(rendered)
		if err != nil || back != u {
			t.Fatalf("round trip %q -> %s -> %q: %s, %v", s, u, rendered, back, err)
		}
		if back.String() != rendered {
			t.Fatalf("String not canonical for %q: %q vs %q", s, back.String(), rendered)
		}
		for _, mode := range allModes {
			r, err := ParseUSDRound(s, mode)
			if err != nil || r != u {
				t.Fatalf("ParseUSDRound(%q, %s) = %s, %v; want %s", s, mode, r, err, u)
			}
		}
		js, err := u.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var viaJSON USD
		if err := viaJSON.UnmarshalJSON(js); err != nil || viaJSON != u {
			t.Fatalf("JSON round trip of %q: %s, %v", s, viaJSON, err)
		}
	})
}

func FuzzParseUSDRound(f *testing.F) {
	for _, s := range []string{"0.005", "0.015", "-0.025", "1.9999", "92233720368547758.075", "-92233720368547758.085", "1", "abc"} {
		for m := 0; m <= 8; m++ {
			f.Add(s, uint8(m))
		}
	}
	f.Fuzz(func(t *testing.T, s string, modeByte uint8) {
		mode := RoundingMode(int(modeByte) % 9) // includes the invalid 0 and 8
		u, err := ParseUSDRound(s, mode)
		if err != nil {
			if !isSentinel(err) {
				t.Fatalf("ParseUSDRound(%q, %s): non-sentinel error %v", s, mode, err)
			}
			if !mode.Valid() && !errors.Is(err, ErrInvalidRoundingMode) {
				t.Fatalf("invalid mode %d must yield ErrInvalidRoundingMode, got %v", int(mode), err)
			}
			return
		}
		if !mode.Valid() {
			t.Fatalf("ParseUSDRound accepted invalid mode %d", int(mode))
		}
		if back, err := ParseUSD(u.String()); err != nil || back != u {
			t.Fatalf("round trip of %s: %s, %v", u, back, err)
		}
		// Rounded results are within one cent of every other mode's result.
		// At the int64 boundary a mode that rounds outward may overflow
		// where another does not; that is the only permitted failure.
		for _, other := range inexactModes {
			o, err := ParseUSDRound(s, other)
			if err != nil {
				if errors.Is(err, ErrOverflow) {
					continue
				}
				t.Fatalf("ParseUSDRound(%q, %s): %v", s, other, err)
			}
			d := o.Minor() - u.Minor()
			if d < -1 || d > 1 {
				t.Fatalf("%q: %s=%s and %s=%s differ by more than one cent", s, mode, u, other, o)
			}
		}
	})
}

func FuzzParseQuantity(f *testing.F) {
	for _, s := range []string{
		"0", "-0", "1", "-1", "007", "+1", "1.0", "1e3", "", "-", " 1", "١", "abc",
		"9223372036854775807", "9223372036854775808", "-9223372036854775809", uint256Max,
		strings.Repeat("9", 128), "1" + strings.Repeat("0", 128), strings.Repeat("0", 200) + "5",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q, err := ParseQuantity(s)
		if err != nil {
			if !isSentinel(err) {
				t.Fatalf("ParseQuantity(%q): non-sentinel error %v", s, err)
			}
			return
		}
		rendered := q.String()
		back, err := ParseQuantity(rendered)
		if err != nil || !back.Equal(q) {
			t.Fatalf("round trip %q -> %q: %s, %v", s, rendered, back, err)
		}
		if back.String() != rendered {
			t.Fatalf("String not canonical for %q: %q vs %q", s, back.String(), rendered)
		}
		if len(strings.TrimPrefix(rendered, "-")) > MaxQuantityDigits {
			t.Fatalf("accepted %q beyond MaxQuantityDigits", s)
		}
		if q.ToDecimalString(0) != rendered {
			t.Fatalf("ToDecimalString(0) %q != String %q", q.ToDecimalString(0), rendered)
		}
		js, err := q.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var viaJSON Quantity
		if err := viaJSON.UnmarshalJSON(js); err != nil || !viaJSON.Equal(q) {
			t.Fatalf("JSON round trip of %q: %s, %v", s, viaJSON, err)
		}
		var viaScan Quantity
		if err := viaScan.Scan(rendered); err != nil || !viaScan.Equal(q) {
			t.Fatalf("Scan round trip of %q: %s, %v", s, viaScan, err)
		}
		if scanned, err := ScanQuantity(s); err != nil || !scanned.Equal(q) {
			t.Fatalf("ScanQuantity(%q) = %s, %v; ParseQuantity accepted it", s, scanned, err)
		}
	})
}

func FuzzQuantityFromDecimalString(f *testing.F) {
	seeds := []string{"1.5", "-1.5", "0", "0.0", "1.5000000005", "-0.000001", "123.000", "1.", ".5", "+1", "abc", "1e9", uint256Max + ".5", "0." + strings.Repeat("9", 300)}
	for _, s := range seeds {
		for _, d := range []uint8{0, 2, 6, 9, 18, 255} {
			for m := 0; m <= 8; m++ {
				f.Add(s, d, uint8(m))
			}
		}
	}
	f.Fuzz(func(t *testing.T, s string, decimals, modeByte uint8) {
		mode := RoundingMode(int(modeByte) % 9) // includes the invalid 0 and 8
		q, err := QuantityFromDecimalString(s, decimals, mode)
		if err != nil {
			if !isSentinel(err) {
				t.Fatalf("QuantityFromDecimalString(%q, %d, %s): non-sentinel error %v", s, decimals, mode, err)
			}
			if !mode.Valid() && !errors.Is(err, ErrInvalidRoundingMode) {
				t.Fatalf("invalid mode %d must yield ErrInvalidRoundingMode, got %v", int(mode), err)
			}
			return
		}
		if !mode.Valid() {
			t.Fatalf("accepted invalid mode %d", int(mode))
		}
		rendered := q.ToDecimalString(decimals)
		back, err := QuantityFromDecimalString(rendered, decimals, RoundExact)
		if err != nil || !back.Equal(q) {
			t.Fatalf("round trip %q -> %s -> %q: %s, %v", s, q, rendered, back, err)
		}
		if back.ToDecimalString(decimals) != rendered {
			t.Fatalf("ToDecimalString not canonical: %q vs %q", back.ToDecimalString(decimals), rendered)
		}
		// Every inexact mode must succeed on accepted input and stay within one unit of each other.
		for _, other := range inexactModes {
			o, err := QuantityFromDecimalString(s, decimals, other)
			if err != nil {
				t.Fatalf("QuantityFromDecimalString(%q, %d, %s): %v", s, decimals, other, err)
			}
			d := o.Sub(q).Abs()
			if d.Cmp(q64(1)) > 0 {
				t.Fatalf("%q @%d: %s=%s and %s=%s differ by more than one unit", s, decimals, mode, q, other, o)
			}
		}
		if mode == RoundExact {
			// Exact acceptance means the digits beyond decimals were all zero,
			// so the truncating mode must agree.
			d, err := QuantityFromDecimalString(s, decimals, RoundDown)
			if err != nil || !d.Equal(q) {
				t.Fatalf("exact %q @%d: RoundDown gave %s, %v", s, decimals, d, err)
			}
		}
	})
}

func FuzzScanQuantity(f *testing.F) {
	for _, s := range []string{"0", "-5", "123.000", "123.5", "NaN", "Infinity", "", "1e3", uint256Max} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q, err := ScanQuantity(s)
		if err != nil {
			if !isSentinel(err) {
				t.Fatalf("ScanQuantity(%q): non-sentinel error %v", s, err)
			}
			return
		}
		v, _ := q.Value()
		var back Quantity
		if err := back.Scan(v); err != nil || !back.Equal(q) {
			t.Fatalf("Scan(Value()) round trip of %q: %s, %v", s, back, err)
		}
	})
}
