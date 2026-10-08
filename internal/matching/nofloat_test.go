package matching

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// floatLiteral matches Go floating-point literals: decimal with a fraction
// (1.5, 1_000.25), decimal with an exponent (1e3, 2E+10), a bare fraction
// (.5) and hexadecimal floats (0x1p-2).
var floatLiteral = regexp.MustCompile(`\b\d[\d_]*\.\d|\b\d[\d_]*[eE][+-]?\d|(^|[^\w.])\.\d|\b0[xX][\da-fA-F_]*\.?[\da-fA-F_]*[pP][+-]?\d`)

func TestFloatLiteralPattern(t *testing.T) {
	for _, s := range []string{"1.5", "x := 1_000.25", "1e3", "2E+10", "y = .5", "0x1p-2", "0x1.8p1"} {
		assert.True(t, floatLiteral.MatchString(s), "should match %q", s)
	}
	for _, s := range []string{"sha256.Sum256", "int64", "10^15", "o.price", "1<<63 - 1", "x[1:2]", "0x9e37", "e2e", "b.Seq"} {
		assert.False(t, floatLiteral.MatchString(s), "should not match %q", s)
	}
}

// TestNoFloatingPointInSource enforces PART 17 ("absolutely no floating-point
// financial values") and the determinism rules mechanically: no non-test
// source file in this package may mention a binary floating-point type,
// conversion or literal -- not even in a comment -- nor import a clock,
// randomness or unsafe memory access, nor carry a TODO or FIXME.
func TestNoFloatingPointInSource(t *testing.T) {
	banned := []string{
		"float32", "float64",
		"big.Float", "big.Rat",
		"ParseFloat", "FormatFloat",
		"math.Round", "math.Floor", "math.Ceil", "math.Trunc",
		`"math"`, `"math/rand`, `"crypto/rand"`, `"time"`, `"unsafe"`,
		"TODO", "FIXME",
	}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		checked++
		text := string(src)
		for _, tok := range banned {
			if idx := strings.Index(text, tok); idx >= 0 {
				line := 1 + strings.Count(text[:idx], "\n")
				t.Errorf("%s:%d: forbidden token %q in non-test matching source", name, line, tok)
			}
		}
		for i, line := range strings.Split(text, "\n") {
			if m := floatLiteral.FindString(line); m != "" {
				t.Errorf("%s:%d: floating-point literal %q in non-test matching source", name, i+1, strings.TrimSpace(m))
			}
		}
	}
	require.GreaterOrEqual(t, checked, 12, "expected to scan the package's non-test sources")
}
