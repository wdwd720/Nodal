package orders

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// floatLiteral matches Go floating-point literals (1.5, 1e3, .5, 0x1p-2).
var floatLiteral = regexp.MustCompile(`\b\d[\d_]*\.\d|\b\d[\d_]*[eE][+-]?\d|(^|[^\w.])\.\d|\b0[xX][\da-fA-F_]*\.?[\da-fA-F_]*[pP][+-]?\d`)

// TestNoFloatingPointInSource enforces CONVENTIONS non-negotiable #1 ("no
// floating-point financial values") mechanically, mirroring
// internal/money/nofloat_test.go: no non-test source file in this package may
// mention a binary floating-point type, conversion or literal, not even in a
// comment, nor carry a TODO or FIXME.
func TestNoFloatingPointInSource(t *testing.T) {
	banned := []string{
		"float32", "float64",
		"big.Float", "big.Rat",
		"ParseFloat", "FormatFloat",
		"math.Round", "math.Floor", "math.Ceil", "math.Trunc",
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
				t.Errorf("%s:%d: forbidden token %q in non-test orders source", name, line, tok)
			}
		}
		for i, line := range strings.Split(text, "\n") {
			if m := floatLiteral.FindString(line); m != "" {
				t.Errorf("%s:%d: floating-point literal %q in non-test orders source", name, i+1, strings.TrimSpace(m))
			}
		}
	}
	require.GreaterOrEqual(t, checked, 15, "expected to scan every non-test source file of the package")
}
