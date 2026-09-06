// Package eventtopics_test proves that every outbox topic any package emits is
// registered in internal/event's topic registry.
//
// Why this exists. internal/capital deliberately does not import internal/event:
// it declares a narrow Emitter interface taking a topic string, so the financial
// core does not depend on the transport. That decoupling is correct, but it means
// nothing forced capital's topic constants and the registry to agree, and they
// silently drifted: capital emitted thirteen topics while the registry knew four
// of them, so the very first capital.reservation.locked emitted through a real
// outbox failed with "event: unknown topic". Nothing caught it, because both
// packages' own tests passed — each was self-consistent. It surfaced only when an
// agent building the reconciliation engine wired a real *event.Outbox as
// capital.Emitter and hit it at the first LockForOrder.
//
// The registry's own test keeps a hand-maintained list of constants, which cannot
// catch this: it proves the registry agrees with itself, not that it agrees with
// its producers. So this test reads the source of every internal package with
// go/parser, finds the string constants that name topics, and asserts each one is
// registered. A new producer topic that nobody registers fails here, at the point
// it is written, instead of at the first live emission.
package eventtopics_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/event"
)

// topicGrammar mirrors internal/event's documented event type grammar. A
// constant only counts as a topic if its value looks like one: at least two
// dot-separated lowercase segments.
var topicGrammar = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)+$`)

// skipDirs are packages whose topic-shaped constants are not producer topics.
//   - internal/event owns the registry itself; its constants ARE the registry.
//   - internal/gen is generated code.
var skipDirs = map[string]bool{
	"internal/event": true,
	"internal/gen":   true,
}

type found struct {
	value string
	name  string
	file  string
	line  int
}

func TestEveryProducerTopicIsRegistered(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	var hits []found

	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, rerr := filepath.Rel(root, path)
			if rerr == nil && skipDirs[filepath.ToSlash(rel)] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		hits = append(hits, topicConstsIn(t, path)...)
		return nil
	})
	require.NoError(t, err)

	require.NotEmpty(t, hits, "found no topic constants at all — the scanner is broken, which would make this test vacuous")

	var unregistered []string
	for _, h := range hits {
		if _, ok := event.Lookup(event.Topic(h.value)); !ok {
			rel, _ := filepath.Rel(root, h.file)
			unregistered = append(unregistered,
				h.value+"  ("+h.name+" at "+filepath.ToSlash(rel)+":"+strconv.Itoa(h.line)+")")
		}
	}
	sort.Strings(unregistered)

	assert.Emptyf(t, unregistered,
		"these topic constants are emitted by producers but are not in internal/event's registry, "+
			"so the first real Enqueue of each fails with \"event: unknown topic\":\n  %s",
		strings.Join(unregistered, "\n  "))
}

// topicConstsIn returns every const in the file whose name begins with "Topic"
// and whose value is a string literal shaped like a topic.
func topicConstsIn(t *testing.T, path string) []found {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	require.NoErrorf(t, err, "parse %s", path)

	var out []found
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Topic") || i >= len(vs.Values) {
					continue
				}
				lit, ok := unwrapString(vs.Values[i])
				if !ok || !topicGrammar.MatchString(lit) {
					continue
				}
				out = append(out, found{
					value: lit,
					name:  name.Name,
					file:  path,
					line:  fset.Position(name.Pos()).Line,
				})
			}
		}
	}
	return out
}

// unwrapString reads a string literal, seeing through a conversion such as
// Topic("x") so a typed constant is still recognized.
func unwrapString(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.CallExpr:
		if len(v.Args) == 1 {
			return unwrapString(v.Args[0])
		}
	}
	return "", false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		if _, err := filepath.Glob(filepath.Join(dir, "go.mod")); err == nil {
			if matches, _ := filepath.Glob(filepath.Join(dir, "go.mod")); len(matches) == 1 {
				return dir
			}
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate repo root (no go.mod found walking up)")
	return ""
}
