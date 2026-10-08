package quant

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// productionFiles parses every non-test Go file of the package.
func productionFiles(t *testing.T) (map[string]*ast.File, map[string]string, *token.FileSet) {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	sources := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) // #nosec G304 -- a file of this package
		require.NoError(t, err)
		f, err := parser.ParseFile(fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
		require.NoError(t, err)
		files[name] = f
		sources[name] = string(src)
	}
	require.GreaterOrEqual(t, len(files), 9, "expected to scan the package's non-test sources")
	return files, sources, fset
}

// TestNoFloatingPointInSource enforces CONVENTIONS non-negotiable #1 the way
// internal/money does, and more strictly: no non-test file may mention a
// binary floating point type, conversion or function, not even in a
// comment, nor contain a floating point or imaginary literal. Test files may
// use big.Float, and only as the independent oracle.
func TestNoFloatingPointInSource(t *testing.T) {
	banned := []string{
		"float32", "float64", "complex64", "complex128",
		"big.Float", "big.Rat", "NewFloat", "ParseFloat", "FormatFloat", "SetFloat64", "Float64(", "Float32(",
		"math.Round", "math.Floor", "math.Ceil", "math.Trunc", "math.Exp", "math.Log", "math.Sqrt",
		"math.Erf", "math.Pow", "math.Inf", "math.NaN", "math.Abs", "math.Mod", "math.Sin", "math.Cos",
	}
	// Integer helpers are the only part of package math a non-test file
	// may name.
	mathAllowed := map[string]bool{
		"MaxInt": true, "MinInt": true, "MaxInt8": true, "MinInt8": true, "MaxInt16": true, "MinInt16": true,
		"MaxInt32": true, "MinInt32": true, "MaxInt64": true, "MinInt64": true,
		"MaxUint": true, "MaxUint8": true, "MaxUint16": true, "MaxUint32": true, "MaxUint64": true,
	}
	files, sources, fset := productionFiles(t)
	for name, src := range sources {
		for _, tok := range banned {
			if idx := strings.Index(src, tok); idx >= 0 {
				t.Errorf("%s:%d: forbidden token %q in non-test quant source", name, 1+strings.Count(src[:idx], "\n"), tok)
			}
		}
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind == token.FLOAT || x.Kind == token.IMAG {
					t.Errorf("%s: floating point literal %s", fset.Position(x.Pos()), x.Value)
				}
			case *ast.Ident:
				switch x.Name {
				case "float32", "float64", "complex64", "complex128":
					t.Errorf("%s: floating point type %s", fset.Position(x.Pos()), x.Name)
				}
			case *ast.SelectorExpr:
				pkg := ""
				if id, ok := x.X.(*ast.Ident); ok {
					pkg = id.Name
				}
				sel := x.Sel.Name
				switch {
				case pkg == "math" && !mathAllowed[sel]:
					t.Errorf("%s: math.%s is not an integer helper", fset.Position(x.Pos()), sel)
				case pkg == "big" && (sel == "Float" || sel == "NewFloat" || sel == "ParseFloat" || sel == "Rat" || sel == "NewRat"):
					t.Errorf("%s: big.%s is forbidden outside test oracles", fset.Position(x.Pos()), sel)
				case sel == "Float64" || sel == "Float32":
					t.Errorf("%s: .%s() is forbidden", fset.Position(x.Pos()), sel)
				case pkg == "time" && (sel == "Now" || sel == "Since" || sel == "Until" || sel == "Sleep" ||
					sel == "Tick" || sel == "After" || sel == "AfterFunc" || sel == "NewTimer" || sel == "NewTicker"):
					t.Errorf("%s: time.%s reads or waits on a clock", fset.Position(x.Pos()), sel)
				}
			}
			return true
		})
	}
}

// TestNoPackageLevelVariables: the package keeps no global state at all,
// mutable or not; constants are const and big integers are built on demand.
func TestNoPackageLevelVariables(t *testing.T) {
	files, _, fset := productionFiles(t)
	for _, f := range files {
		for _, decl := range f.Decls {
			if g, ok := decl.(*ast.GenDecl); ok && g.Tok == token.VAR {
				t.Errorf("%s: package-level variable declaration", fset.Position(g.Pos()))
			}
		}
	}
}

// TestImportsAreRestricted: the package depends on the standard library's
// exact-arithmetic and string packages and on money and errs only. It
// therefore cannot reach a ledger, a database, a clock, the network or a
// random source, whatever its code does.
func TestImportsAreRestricted(t *testing.T) {
	allowed := map[string]bool{
		"errors": true, "math/big": true, "strconv": true, "strings": true,
		"time": true, // the time.Time parameter of Dec.ToPrice; no clock reads (see above)
		"github.com/nodal/controlplane/internal/errs":  true,
		"github.com/nodal/controlplane/internal/money": true,
	}
	files, _, fset := productionFiles(t)
	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if !allowed[path] {
				t.Errorf("%s: import %q is not allowed in quant", fset.Position(imp.Pos()), path)
			}
		}
	}
}
