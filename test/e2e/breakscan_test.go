//go:build integration && e2e

package e2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// breakNamesUsedInSource parses this package and returns every string literal
// passed as the second argument to e2eBreak, keyed by the file it appears in.
// Parsing the source rather than maintaining a list by hand is the whole
// point: a hand-maintained list is the drift, not the guard against it.
func breakNamesUsedInSource(t *testing.T) map[string]string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	entries, err := os.ReadDir(wd)
	require.NoError(t, err)

	out := map[string]string{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		// The declaration site itself is not a use.
		if e.Name() == "break_test.go" {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(wd, e.Name()), nil, 0)
		require.NoErrorf(t, err, "parse %s", e.Name())
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "e2eBreak" || len(call.Args) != 2 {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: e2eBreak must be called with a string literal so the catalog can be checked",
					e.Name())
				return true
			}
			name, err := strconv.Unquote(lit.Value)
			require.NoError(t, err)
			out[name] = e.Name()
			return true
		})
	}
	return out
}
