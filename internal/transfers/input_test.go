package transfers

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func TestParseAmount_IsExactAndNeverRounds(t *testing.T) {
	for _, c := range []struct {
		in       string
		decimals uint8
		want     string
		code     errs.Code
	}{
		{"1", 6, "1000000", ""},
		{"12.5", 6, "12500000", ""},
		{"0.000001", 6, "1", ""},
		{"1.50", 2, "150", ""},
		{"7", 0, "7", ""},
		{"0.0000001", 6, "", errs.CodePrecisionLoss},
		{"1.0000005", 6, "", errs.CodePrecisionLoss},
		{"1.5", 0, "", errs.CodePrecisionLoss},
		{"0", 6, "", errs.CodeValidationFailed},
		{"0.000000", 6, "", errs.CodeValidationFailed},
		{"-1", 6, "", errs.CodeValidationFailed},
		{"+1", 6, "", errs.CodeValidationFailed},
		{"1e3", 6, "", errs.CodeValidationFailed},
		{" 1", 6, "", errs.CodeValidationFailed},
		{"1,000", 6, "", errs.CodeValidationFailed},
		{"", 6, "", errs.CodeValidationFailed},
		{strings.Repeat("9", 65), 0, "", errs.CodeValidationFailed},
	} {
		got, err := ParseAmount(c.in, c.decimals)
		if c.code != "" {
			require.Error(t, err, c.in)
			assert.Equal(t, c.code, errs.CodeOf(err), c.in)
			continue
		}
		require.NoError(t, err, c.in)
		assert.Equal(t, c.want, got.String(), c.in)
	}
}

// FuzzParseAmount: never panics; anything accepted is positive and renders
// back to the same number, so what moves is exactly what was typed.
func FuzzParseAmount(f *testing.F) {
	for _, s := range []string{"1", "12.5", "0.000001", "1.0000005", "-1", "1e3", "", "00012.300000", "9999999999999999999999999"} {
		f.Add(s, uint8(6))
	}
	f.Fuzz(func(t *testing.T, s string, decimals uint8) {
		decimals %= 19
		q, err := ParseAmount(s, decimals)
		if err != nil {
			if _, ok := errs.As(err); !ok {
				t.Fatalf("non-API error %v", err)
			}
			return
		}
		if !q.IsPositive() {
			t.Fatalf("accepted a non-positive amount %q -> %s", s, q)
		}
		back, err := money.QuantityFromDecimalString(q.ToDecimalString(decimals), decimals, money.RoundExact)
		if err != nil || !back.Equal(q) {
			t.Fatalf("%q -> %s does not round-trip: %v", s, q, err)
		}
	})
}

func TestValidateMemo(t *testing.T) {
	for memo, ok := range map[string]bool{
		"":                              true,
		"rent for october":              true,
		"café ☕ thanks":                 true,
		strings.Repeat("a", 140):        true,
		strings.Repeat("a", 141):        false,
		strings.Repeat("é", 140):        true,
		"line\nbreak":                   false,
		"tab\there":                     false,
		"bidi ‮ override":               false,
		"zero​width":                    false,
		" leading":                      false,
		"trailing ":                     false,
		string([]byte{0xff, 0xfe, 'a'}): false,
	} {
		err := ValidateMemo(memo, 140)
		if ok {
			assert.NoError(t, err, "%q", memo)
		} else {
			assert.Error(t, err, "%q", memo)
		}
	}
	assert.Error(t, ValidateMemo(strings.Repeat("a", MaxMemoRunes+1), 10_000), "the hard bound cannot be raised")
	assert.Empty(t, MemoDigest(""))
	assert.Len(t, MemoDigest("x"), 64)
}

// FuzzValidateMemo: an accepted memo is valid UTF-8, within the bound, made
// only of printable characters and fits the column.
func FuzzValidateMemo(f *testing.F) {
	for _, s := range []string{"", "hi", "a‮b", "\x00", strings.Repeat("z", 300)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if ValidateMemo(s, 140) != nil {
			return
		}
		if !utf8.ValidString(s) || utf8.RuneCountInString(s) > 140 {
			t.Fatalf("accepted %q", s)
		}
		for _, r := range s {
			if !unicode.IsPrint(r) {
				t.Fatalf("accepted non-printable %U", r)
			}
		}
	})
}

func TestNormalizeHandle(t *testing.T) {
	for in, want := range map[string]string{
		"alice": "alice", "@Alice": "alice", "BOB_42": "bob_42", "a.b-c": "a.b-c",
	} {
		got, err := NormalizeHandle(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got)
	}
	for _, bad := range []string{"", "ab", "_alice", ".alice", "al ice", "alïce", "аlice", strings.Repeat("a", 33), "@@alice", "alice!"} {
		_, err := NormalizeHandle(bad)
		assert.Error(t, err, "%q", bad)
	}
}

// FuzzNormalizeHandle: an accepted handle matches the database grammar and
// normalizing it again changes nothing.
func FuzzNormalizeHandle(f *testing.F) {
	for _, s := range []string{"alice", "@Bob", "аlice", "a", "x.y_z-0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		h, err := NormalizeHandle(s)
		if err != nil {
			return
		}
		if !handleRE.MatchString(h) {
			t.Fatalf("%q normalized to %q, outside the grammar", s, h)
		}
		again, err := NormalizeHandle(h)
		if err != nil || again != h {
			t.Fatalf("not idempotent: %q -> %q -> %q (%v)", s, h, again, err)
		}
	})
}
