package security

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
// passed as the second argument to secBreak, keyed by the file it appears in.
//
// It parses the source rather than reading a hand-maintained list, and it
// parses every .go file in the directory regardless of build tags, so the
// catalog is checked identically in the tagged and untagged builds. A
// hand-maintained list is the drift, not the guard against it.
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
		f, perr := parser.ParseFile(fset, filepath.Join(wd, e.Name()), nil, 0)
		require.NoErrorf(t, perr, "parse %s", e.Name())
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "secBreak" || len(call.Args) != 2 {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s:%d: secBreak must be called with a string literal so the catalog can be checked",
					e.Name(), fset.Position(call.Args[1].Pos()).Line)
				return true
			}
			name, uerr := strconv.Unquote(lit.Value)
			require.NoError(t, uerr)
			out[name] = e.Name()
			return true
		})
	}
	return out
}
