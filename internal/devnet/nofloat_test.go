package devnet

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNoFloatingPointInSource enforces the "no floating-point financial
// values" non-negotiable mechanically, as internal/money does: no non-test
// source file in this package may mention a binary floating point type or
// conversion, not even in a comment.
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
		src, err := os.ReadFile(name) // #nosec G304 -- this package's own sources
		require.NoError(t, err)
		checked++
		text := string(src)
		for _, tok := range banned {
			if idx := strings.Index(text, tok); idx >= 0 {
				line := 1 + strings.Count(text[:idx], "\n")
				t.Errorf("%s:%d: forbidden token %q in non-test devnet source", name, line, tok)
			}
		}
	}
	require.Greater(t, checked, 10, "expected to scan the package's non-test sources")
}
