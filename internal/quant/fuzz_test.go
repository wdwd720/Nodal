package quant

import (
	"regexp"
	"strings"
	"testing"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// isPackageError reports whether err is an *errs.Error with one of the codes
// the parsers document.
func isPackageError(err error) bool {
	e, ok := errs.As(err)
	if !ok {
		return false
	}
	switch e.Code {
	case errs.CodeValidationFailed, errs.CodePrecisionLoss, errs.CodeOverflow:
		return true
	}
	return false
}

// canonicalDec is the grammar of Dec.String: no leading zeros, no "+", a
// fractional part only when non-zero and without trailing zeros.
var canonicalDec = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]{0,39}[1-9])?$`)

func FuzzParseDec(f *testing.F) {
	for _, s := range []string{
		"0", "-0", "1", "-1", "007", "1.50", "-0.000", "+1", "1.", ".5", "1e3", "", "-", " 1", "١", "NaN", "Inf",
		"0.0000000000000000000000000000000000000001", "0.00000000000000000000000000000000000000005",
		"1.000000000000000000000000000000000000000000000", strings.Repeat("9", 128), "1" + strings.Repeat("0", 128),
		"-" + strings.Repeat("9", 128) + "." + strings.Repeat("9", 40), strings.Repeat("0", 300) + "1.5",
		"3.14159265358979323846264338327950288419716939937510", "1.2.3", "--1", "0x1F", "1_000",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := ParseDec(s)
		if err != nil {
			if !isPackageError(err) {
				t.Fatalf("ParseDec(%q): unexpected error type %v", s, err)
			}
			// Rounding can only rescue precision loss, never bad syntax.
			if errs.CodeOf(err) == errs.CodeValidationFailed {
				if _, rerr := ParseDecRound(s, money.RoundHalfEven); rerr == nil {
					t.Fatalf("ParseDecRound accepted %q that ParseDec rejected: %v", s, err)
				}
			}
			return
		}
		canonical := v.String()
		back, err := ParseDec(canonical)
		if err != nil || !back.Equal(v) {
			t.Fatalf("round trip %q -> %q: %s, %v", s, canonical, back, err)
		}
		if back.String() != canonical {
			t.Fatalf("String not canonical: %q vs %q", back.String(), canonical)
		}
		if !canonicalDec.MatchString(canonical) || canonical == "-0" {
			t.Fatalf("non-canonical rendering %q of %q", canonical, s)
		}
		// Every mode agrees with the exact parse when no rounding is needed.
		for _, mode := range allModes() {
			r, err := ParseDecRound(s, mode)
			if err != nil || !r.Equal(v) {
				t.Fatalf("ParseDecRound(%q, %s) = %s, %v; want %s", s, mode, r, err, v)
			}
		}
	})
}

func FuzzParseDecRound(f *testing.F) {
	for _, s := range []string{"0.00000000000000000000000000000000000000005", "-0.00000000000000000000000000000000000000015", "1", "abc", "9.99999999999999999999999999999999999999999"} {
		for m := range 9 {
			f.Add(s, uint8(m))
		}
	}
	f.Fuzz(func(t *testing.T, s string, modeByte uint8) {
		mode := money.RoundingMode(int(modeByte) % 9) // includes the invalid 0 and 8
		v, err := ParseDecRound(s, mode)
		if err != nil {
			if !isPackageError(err) {
				t.Fatalf("ParseDecRound(%q, %d): unexpected error %v", s, mode, err)
			}
			if !mode.Valid() && errs.CodeOf(err) != errs.CodeValidationFailed {
				t.Fatalf("invalid mode %d must fail validation, got %v", mode, err)
			}
			return
		}
		if !mode.Valid() {
			t.Fatalf("accepted invalid mode %d", mode)
		}
		if back, err := ParseDec(v.String()); err != nil || !back.Equal(v) {
			t.Fatalf("round trip of %s: %v", v, err)
		}
	})
}

// FuzzBlackScholes drives the pricer with arbitrary small integers scaled
// into (and out of) its domain: it must never panic, every failure must be
// VALIDATION_FAILED, and every success must respect the price bounds.
func FuzzBlackScholes(f *testing.F) {
	f.Add(int64(10000), int64(10000), int64(1000), int64(2000), int64(500), int64(0), true)
	f.Add(int64(1), int64(1_000_000_000), int64(1), int64(1), int64(-10000), int64(10000), false)
	f.Add(int64(0), int64(-5), int64(0), int64(0), int64(20000), int64(-20000), true)
	f.Add(int64(1_000_000_000_000), int64(1), int64(100000), int64(100000), int64(10000), int64(-10000), false)
	f.Fuzz(func(t *testing.T, sCents, kCents, tMilli, volBP, rBP, qBP int64, call bool) {
		kind := Put
		if call {
			kind = Call
		}
		in := BSMInput{
			Kind: kind, Spot: scaled(sCents, 2), Strike: scaled(kCents, 2), TimeYears: scaled(tMilli, 3),
			Vol: scaled(volBP, 4), Rate: scaled(rBP, 4), DividendYield: scaled(qBP, 4),
		}
		res, err := BlackScholes(in)
		if err != nil {
			if errs.CodeOf(err) != errs.CodeValidationFailed {
				t.Fatalf("BlackScholes(%+v): %v", in, err)
			}
			return
		}
		if res.Price.IsNegative() || res.Gamma.IsNegative() || res.Vega.IsNegative() {
			t.Fatalf("negative price or convexity: %+v", res)
		}
		if _, err := ImpliedVol(ivInput(in, res.Price)); err != nil && errs.CodeOf(err) != errs.CodeValidationFailed {
			t.Fatalf("ImpliedVol: %v", err)
		}
	})
}
