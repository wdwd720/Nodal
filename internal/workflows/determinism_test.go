package workflows_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// workflowFiles are the files that contain workflow functions — code that
// Temporal replays. Everything else in the package is activity code, which
// runs once and may do whatever it needs.
var workflowFiles = []string{"funding.go", "escalation.go", "workflows.go"}

// forbiddenSelectors are the qualified identifiers that make a workflow
// non-deterministic. A replay re-executes workflow code against a recorded
// history: if the code consults anything the history does not contain, the
// second execution can decide differently from the first, and Temporal
// detects that as a non-determinism error — after the workflow has already
// been corrupted.
//
// This test is the cheap half of the guarantee. The expensive half is
// replay_test.go, which replays real recorded histories.
var forbiddenSelectors = map[string][]string{
	"time":      {"Now", "Since", "Tick", "After", "AfterFunc", "NewTimer", "NewTicker", "Sleep"},
	"rand":      {"*"},
	"os":        {"*"},
	"net":       {"*"},
	"http":      {"*"},
	"sql":       {"*"},
	"pgx":       {"*"},
	"uuid":      {"*"},
	"crypto":    {"*"},
	"runtime":   {"*"},
	"sync":      {"*"},
	"atomic":    {"*"},
	"context":   {"Background", "TODO", "WithTimeout", "WithDeadline", "WithCancel"},
	"fmt":       {"Print", "Printf", "Println", "Fprint", "Fprintf", "Fprintln"},
	"log":       {"*"},
	"slog":      {"*"},
	"io":        {"*"},
	"bufio":     {"*"},
	"exec":      {"*"},
	"filepath":  {"*"},
	"ioutil":    {"*"},
	"math/rand": {"*"},
}

// TestDeterminism_WorkflowCodeHasNoAmbientDependencies parses the workflow
// source and fails on any call that reads the wall clock, uses randomness,
// touches the filesystem or the network, or otherwise depends on something a
// history cannot replay.
func TestDeterminism_WorkflowCodeHasNoAmbientDependencies(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	for _, name := range workflowFiles {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
			require.NoError(t, err)

			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				banned, forbidden := forbiddenSelectors[pkg.Name]
				if !forbidden {
					return true
				}
				for _, fn := range banned {
					if fn == "*" || fn == sel.Sel.Name {
						pos := fset.Position(call.Pos())
						t.Errorf("%s:%d: workflow code calls %s.%s, which a replay cannot reproduce; move it into an activity",
							name, pos.Line, pkg.Name, sel.Sel.Name)
					}
				}
				return true
			})
		})
	}
}

// TestDeterminism_WorkflowCodeDeclaresNoMaps forbids maps in workflow files
// outright. Go randomizes map iteration order per process, so a workflow that
// ranged over one would emit its commands in a different order on replay. The
// rule is "no maps at all" rather than "no map iteration" because it is
// mechanically checkable and costs a workflow nothing: ordered data belongs in
// a slice, which replays identically.
func TestDeterminism_WorkflowCodeDeclaresNoMaps(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	for _, name := range workflowFiles {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			if m, ok := n.(*ast.MapType); ok {
				pos := fset.Position(m.Pos())
				t.Errorf("%s:%d: workflow code declares a map; map order is randomized per process, so use a slice",
					name, pos.Line)
			}
			return true
		})
	}
}

// TestDeterminism_NoPackageLevelMutableState fails on a package-level `var`
// in workflow files. A workflow that reads one would decide differently after
// a deploy that changed it, mid-execution.
func TestDeterminism_NoPackageLevelMutableState(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	for _, name := range workflowFiles {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, id := range vs.Names {
					pos := fset.Position(id.Pos())
					t.Errorf("%s:%d: workflow file declares package-level var %q; use a const or a function returning a fresh value",
						name, pos.Line, id.Name)
				}
			}
		}
	}
}

// TestDeterminism_WorkflowFilesImportNothingAmbient checks the import list
// itself, so a helper added later cannot smuggle in an ambient dependency
// that the call-site scan would miss.
func TestDeterminism_WorkflowFilesImportNothingAmbient(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{
		"time":                        true, // types and durations only; the call scan forbids time.Now
		"go.temporal.io/sdk/workflow": true,
		"go.temporal.io/sdk/temporal": true,
		"github.com/nodal/controlplane/internal/errs": true,
	}
	fset := token.NewFileSet()
	for _, name := range workflowFiles {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution|parser.ImportsOnly)
		require.NoError(t, err)
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			assert.True(t, allowed[path], "%s imports %q, which workflow code may not depend on", name, path)
		}
	}
}
