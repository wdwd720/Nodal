package sandboxrt

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNoFloatingPointInSource: no non-test source of this package or of the
// SDK mentions a binary floating point type or conversion (as
// internal/money/nofloat_test.go does for money).
func TestNoFloatingPointInSource(t *testing.T) {
	banned := []string{
		"float32", "float64",
		"big.Float", "big.Rat",
		"ParseFloat", "FormatFloat",
		"math.Round", "math.Floor", "math.Ceil", "math.Trunc",
	}
	checked := 0
	for _, dir := range []string{".", "sdk"} {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- this package's own sources
			require.NoError(t, err)
			checked++
			text := string(src)
			for _, tok := range banned {
				if idx := strings.Index(text, tok); idx >= 0 {
					line := 1 + strings.Count(text[:idx], "\n")
					t.Errorf("%s/%s:%d: forbidden token %q", dir, name, line, tok)
				}
			}
		}
	}
	require.Greater(t, checked, 10, "expected to scan the package's non-test sources")
}

// forbiddenAuthority are the packages untrusted strategy output must never be
// able to reach: the signing service, wallets, withdrawals and delegated
// agent authority.
var forbiddenAuthority = []string{"internal/signing", "internal/wallet", "internal/withdrawal", "internal/delegation"}

// TestImportBoundary_UntrustedOutputCannotReachAuthority walks every Go file
// (tests and fixtures included) of the sandbox runtime and the marketplace and
// refuses any import of signing, wallet, withdrawal or delegation code. The
// code that runs or represents untrusted strategy output has no path to an
// authority it could misuse.
func TestImportBoundary_UntrustedOutputCannotReachAuthority(t *testing.T) {
	const module = "github.com/nodal/controlplane/"
	checked := 0
	for _, root := range []string{".", filepath.Join("..", "marketplace")} {
		if _, err := os.Stat(root); err != nil {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			checked++
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				rel := strings.TrimPrefix(p, module)
				for _, bad := range forbiddenAuthority {
					assert.False(t, rel == bad || strings.HasPrefix(rel, bad+"/"), "%s imports %s", path, p)
				}
			}
			return nil
		})
		require.NoError(t, err)
	}
	require.Greater(t, checked, 20)
}
