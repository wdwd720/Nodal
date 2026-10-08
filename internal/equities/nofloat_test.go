package equities

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNoFloatingPointInSource enforces CONVENTIONS non-negotiable #1 ("no
// floating-point financial values") mechanically, mirroring
// internal/money/nofloat_test.go: no non-test source file in this package may
// mention a binary floating point type or conversion, not even in a comment.
func TestNoFloatingPointInSource(t *testing.T) {
	banned := []string{
		"float32", "float64",
		"big.Float", "big.Rat",
		"ParseFloat", "FormatFloat",
		"math.Round", "math.Floor", "math.Ceil", "math.Trunc",
	}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) // #nosec G304 -- the package's own source files
		require.NoError(t, err)
		checked++
		text := string(src)
		for _, tok := range banned {
			if idx := strings.Index(text, tok); idx >= 0 {
				line := 1 + strings.Count(text[:idx], "\n")
				t.Errorf("%s:%d: forbidden token %q in non-test equities source", name, line, tok)
			}
		}
	}
	require.GreaterOrEqual(t, checked, 14, "expected to scan every non-test source file of the package")
}
