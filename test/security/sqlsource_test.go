package security

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The injection probes in injection_test.go show that no payload a client can
// send produces a 5xx or a leaked driver message. That is evidence, but it is
// evidence about the payloads that were tried. This file proves the structural
// half: every SQL statement the API can execute is assembled from compile-time
// constants, and every value that came from a request travels as a bound
// parameter. A statement that is constant cannot be injected into, whatever the
// payload, so the two tests together say "not observed" AND "not possible".
//
// The engine is a conservative constant-derivation analysis over the AST. It
// answers one question about an expression: can this string have been
// influenced by anything other than source text in this repository? Anything it
// cannot prove constant is reported, so the failure mode is a false alarm that
// a human resolves, never a silent pass.

// sqlQueryMethods are the pgx/Querier methods that execute a statement, mapped
// to the argument index holding the SQL. Every one of them takes the context
// first and the statement second.
var sqlQueryMethods = map[string]int{"Query": 1, "QueryRow": 1, "Exec": 1}

// sqlScanSkip are the trees deliberately outside the scan, with the reason.
// Both are asserted below to be off the API's Postgres path, so excluding them
// cannot hide a statement a request can reach.
var sqlScanSkip = map[string]string{
	"internal/testkit": "test-only database provisioning; it issues CREATE/DROP DATABASE with quoted identifiers and is never linked into a server binary",
	"internal/reality": "ClickHouse, not Postgres: a different driver whose statements bind with @named parameters and whose DDL manages suffixed table names",
}

// numericToString are the strconv conversions that turn a number into text. A
// number cannot carry SQL, so their results are constant-derived.
var numericToString = map[string]bool{
	"Itoa": true, "FormatInt": true, "FormatUint": true, "FormatFloat": true, "FormatBool": true,
}

// formatVerb matches a printf verb so a format string can be split into the
// arguments it renders as text and the ones it renders as numbers.
var formatVerb = regexp.MustCompile(`%[-+ #0]*[0-9*]*(\.[0-9*]+)?[a-zA-Z%]`)

// constPkg is one parsed package: its package-level constants and variables,
// its function declarations, and its files.
type constPkg struct {
	dir    string
	fset   *token.FileSet
	consts map[string]ast.Expr
	funcs  map[string][]*ast.FuncDecl
	files  []*ast.File
	// paramsAreConstant is the negative control. With it set, a function
	// parameter counts as constant-derived instead of being resolved through
	// the call sites that supply it, which is exactly the shortcut that would
	// make a concatenated parameter invisible.
	paramsAreConstant bool
}

// constFn is the constant-derivation context of one function: which names are
// parameters (and of what), what locals were assigned, what was appended to
// which slice, what was written to which strings.Builder, and which range
// variables walk a composite literal.
type constFn struct {
	pkg       *constPkg
	params    map[string]int
	owner     map[string]string
	locals    map[string][]ast.Expr
	appends   map[string][]ast.Expr
	writes    map[string][][]ast.Expr
	rangeLits map[string]*ast.CompositeLit
}

func newConstFn(p *constPkg, name string, ftype *ast.FuncType, body *ast.BlockStmt) *constFn {
	c := &constFn{
		pkg:     p,
		params:  map[string]int{},
		owner:   map[string]string{},
		locals:  map[string][]ast.Expr{},
		appends: map[string][]ast.Expr{},
		writes:  map[string][][]ast.Expr{},

		rangeLits: map[string]*ast.CompositeLit{},
	}
	c.addParams(name, ftype)
	if body == nil {
		return c
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch a := n.(type) {
		case *ast.AssignStmt:
			c.recordAssign(a)
		case *ast.ValueSpec:
			for i, nm := range a.Names {
				if i < len(a.Values) {
					c.locals[nm.Name] = append(c.locals[nm.Name], a.Values[i])
				}
			}
		case *ast.RangeStmt:
			if id, ok := a.Value.(*ast.Ident); ok {
				if lit := c.compositeOf(a.X); lit != nil {
					c.rangeLits[id.Name] = lit
				}
			}
		case *ast.CallExpr:
			c.recordWrite(a)
		}
		return true
	})
	return c
}

func (c *constFn) recordAssign(a *ast.AssignStmt) {
	if len(a.Rhs) != len(a.Lhs) {
		return // multi-value assignment: nothing is provably constant
	}
	for i, lhs := range a.Lhs {
		id, ok := lhs.(*ast.Ident)
		if !ok {
			continue
		}
		rhs := a.Rhs[i]
		// A closure assigned to a name: its parameters are resolved through
		// the calls to that name, exactly like a declared function's.
		if fl, ok := rhs.(*ast.FuncLit); ok {
			c.addParams(id.Name, fl.Type)
			continue
		}
		// x = append(x, e...): the appended expressions become the slice's.
		if call, ok := rhs.(*ast.CallExpr); ok {
			if fn, isIdent := call.Fun.(*ast.Ident); isIdent && fn.Name == "append" && len(call.Args) >= 2 {
				if base, isBase := call.Args[0].(*ast.Ident); isBase && base.Name == id.Name {
					c.appends[id.Name] = append(c.appends[id.Name], call.Args[1:]...)
					continue
				}
			}
		}
		c.locals[id.Name] = append(c.locals[id.Name], rhs)
	}
}

// recordWrite captures the two ways text reaches a strings.Builder.
func (c *constFn) recordWrite(a *ast.CallExpr) {
	sel, ok := a.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	if id, isIdent := sel.X.(*ast.Ident); isIdent && strings.HasPrefix(sel.Sel.Name, "Write") && len(a.Args) == 1 {
		c.writes[id.Name] = append(c.writes[id.Name], []ast.Expr{a.Args[0]})
	}
	if pkgName, isIdent := sel.X.(*ast.Ident); isIdent && pkgName.Name == "fmt" && sel.Sel.Name == "Fprintf" && len(a.Args) >= 2 {
		if u, isUnary := a.Args[0].(*ast.UnaryExpr); isUnary {
			if id, isIdent := u.X.(*ast.Ident); isIdent {
				c.writes[id.Name] = append(c.writes[id.Name], a.Args[1:])
			}
		}
	}
}

func (c *constFn) addParams(owner string, ftype *ast.FuncType) {
	if ftype == nil || ftype.Params == nil {
		return
	}
	i := 0
	for _, field := range ftype.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, nm := range field.Names {
			if _, dup := c.params[nm.Name]; !dup {
				c.params[nm.Name] = i
				c.owner[nm.Name] = owner
			}
			i++
		}
	}
}

// compositeOf resolves an expression to a composite literal, following one
// level of local initialisation ("queries := []struct{...}{...}").
func (c *constFn) compositeOf(e ast.Expr) *ast.CompositeLit {
	switch x := e.(type) {
	case *ast.CompositeLit:
		return x
	case *ast.Ident:
		if inits := c.locals[x.Name]; len(inits) == 1 {
			lit, _ := inits[0].(*ast.CompositeLit)
			return lit
		}
	}
	return nil
}

// visited breaks recursion through mutually referring functions.
type visited map[string]bool

// constant reports whether e can only have come from source text: a string
// literal, a declared constant, a concatenation of those, a number rendered as
// text, or a parameter every caller supplies with such an expression.
func (c *constFn) constant(e ast.Expr, v visited, depth int) bool {
	if depth > 32 {
		return false // pathological nesting: refuse to claim it is constant
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		return x.Kind == token.STRING
	case *ast.ParenExpr:
		return c.constant(x.X, v, depth+1)
	case *ast.BinaryExpr:
		return x.Op == token.ADD && c.constant(x.X, v, depth+1) && c.constant(x.Y, v, depth+1)
	case *ast.Ident:
		return c.identConstant(x, v, depth)
	case *ast.SelectorExpr:
		return c.fieldConstant(x, v, depth)
	case *ast.IndexExpr:
		return c.indexConstant(x, v, depth)
	case *ast.CallExpr:
		return c.callConstant(x, v, depth)
	}
	return false
}

func (c *constFn) identConstant(x *ast.Ident, v visited, depth int) bool {
	if idx, isParam := c.params[x.Name]; isParam {
		return c.paramConstant(x.Name, idx, v, depth)
	}
	if exprs, isLocal := c.locals[x.Name]; isLocal && len(exprs) > 0 {
		for _, ex := range exprs {
			if !c.constant(ex, v, depth+1) {
				return false
			}
		}
		return true
	}
	if decl, isPkgLevel := c.pkg.consts[x.Name]; isPkgLevel {
		return c.constant(decl, v, depth+1)
	}
	return false
}

// fieldConstant resolves x.f where x ranges over a composite literal of
// structs, which is how several packages hold a table of statements.
func (c *constFn) fieldConstant(x *ast.SelectorExpr, v visited, depth int) bool {
	id, ok := x.X.(*ast.Ident)
	if !ok {
		return false
	}
	lit, ok := c.rangeLits[id.Name]
	if !ok || len(lit.Elts) == 0 {
		return false
	}
	at, ok := structFieldIndex(lit, x.Sel.Name)
	if !ok {
		return false
	}
	for _, el := range lit.Elts {
		cl, isLit := el.(*ast.CompositeLit)
		if !isLit {
			return false
		}
		var got ast.Expr
		for i, fe := range cl.Elts {
			if kv, isKV := fe.(*ast.KeyValueExpr); isKV {
				if k, isIdent := kv.Key.(*ast.Ident); isIdent && k.Name == x.Sel.Name {
					got = kv.Value
				}
				continue
			}
			if i == at {
				got = fe
			}
		}
		if got == nil || !c.constant(got, v, depth+1) {
			return false
		}
	}
	return true
}

func structFieldIndex(lit *ast.CompositeLit, field string) (int, bool) {
	arr, ok := lit.Type.(*ast.ArrayType)
	if !ok {
		return 0, false
	}
	st, ok := arr.Elt.(*ast.StructType)
	if !ok || st.Fields == nil {
		return 0, false
	}
	i := 0
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			i++
			continue
		}
		for _, nm := range f.Names {
			if nm.Name == field {
				return i, true
			}
			i++
		}
	}
	return 0, false
}

// indexConstant resolves table[k]: a lookup can only yield what the table
// holds, so every value in it must itself be constant.
func (c *constFn) indexConstant(x *ast.IndexExpr, v visited, depth int) bool {
	id, ok := x.X.(*ast.Ident)
	if !ok {
		return false
	}
	lit := c.compositeOf(x.X)
	if lit == nil {
		if decl, isPkgLevel := c.pkg.consts[id.Name]; isPkgLevel {
			lit, _ = decl.(*ast.CompositeLit)
		}
	}
	if lit == nil || len(lit.Elts) == 0 {
		return false
	}
	for _, el := range lit.Elts {
		val := el
		if kv, isKV := el.(*ast.KeyValueExpr); isKV {
			val = kv.Value
		}
		if !c.constant(val, v, depth+1) {
			return false
		}
	}
	return true
}

func (c *constFn) callConstant(x *ast.CallExpr, v visited, depth int) bool {
	if id, ok := x.Fun.(*ast.Ident); ok {
		return c.returnsConstant(id.Name, v, depth)
	}
	sel, ok := x.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if recv, isIdent := sel.X.(*ast.Ident); isIdent {
		switch recv.Name {
		case "fmt":
			return sel.Sel.Name == "Sprintf" && c.formatConstant(x.Args, v, depth)
		case "strconv":
			return numericToString[sel.Sel.Name]
		case "strings":
			switch {
			case sel.Sel.Name == "Join" && len(x.Args) == 2:
				return c.sliceConstant(x.Args[0], v, depth) && c.constant(x.Args[1], v, depth+1)
			case sel.Sel.Name == "Repeat" && len(x.Args) == 2:
				return c.constant(x.Args[0], v, depth+1)
			}
			return false
		}
		// b.String() on a strings.Builder written only with constant text.
		if sel.Sel.Name == "String" && len(x.Args) == 0 {
			if writes, built := c.writes[recv.Name]; built && len(writes) > 0 {
				return c.writesConstant(writes, v, depth)
			}
		}
	}
	return c.returnsConstant(sel.Sel.Name, v, depth)
}

func (c *constFn) writesConstant(writes [][]ast.Expr, v visited, depth int) bool {
	for _, w := range writes {
		if len(w) == 1 {
			if !c.constant(w[0], v, depth+1) {
				return false
			}
			continue
		}
		if !c.formatConstant(w, v, depth+1) {
			return false
		}
	}
	return true
}

// formatConstant checks a printf-style call. The format itself must be a
// constant string, and every argument a verb renders AS TEXT (%s %q %v %T)
// must itself be constant. Numeric and boolean verbs accept anything: an
// integer rendered with %d cannot introduce SQL.
func (c *constFn) formatConstant(args []ast.Expr, v visited, depth int) bool {
	if len(args) == 0 || !c.constant(args[0], v, depth+1) {
		return false
	}
	lit, ok := args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	format, err := strconv.Unquote(lit.Value)
	if err != nil {
		format = lit.Value
	}
	rest := args[1:]
	i := 0
	for _, verb := range formatVerb.FindAllString(format, -1) {
		last := verb[len(verb)-1]
		if last == '%' {
			continue
		}
		if strings.Contains(verb, "*") {
			return false // width or precision taken from an argument
		}
		switch last {
		case 'd', 'f', 'g', 'G', 'e', 'E', 'x', 'X', 'o', 'O', 'b', 'c', 't', 'U', 'p':
			// rendered as a number, a bool or a pointer: cannot carry SQL text
		case 's', 'q', 'v', 'T':
			if i >= len(rest) || !c.constant(rest[i], v, depth+1) {
				return false
			}
		default:
			return false
		}
		i++
	}
	return true
}

// sliceConstant reports whether every string that can reach a slice is
// constant, so strings.Join over it is too.
func (c *constFn) sliceConstant(e ast.Expr, v visited, depth int) bool {
	id, ok := e.(*ast.Ident)
	if !ok {
		return false
	}
	seen := false
	for _, init := range c.locals[id.Name] {
		lit, isLit := init.(*ast.CompositeLit)
		if !isLit {
			return false
		}
		for _, el := range lit.Elts {
			if !c.constant(el, v, depth+1) {
				return false
			}
		}
		seen = true
	}
	for _, appended := range c.appends[id.Name] {
		if !c.constant(appended, v, depth+1) {
			return false
		}
		seen = true
	}
	return seen
}

// returnsConstant reports whether every single-value return of every function
// with this name in the package is constant.
func (c *constFn) returnsConstant(name string, v visited, depth int) bool {
	decls := c.pkg.funcs[name]
	if len(decls) == 0 {
		return false
	}
	key := "return:" + name
	if v[key] {
		return false
	}
	v[key] = true
	defer delete(v, key)

	for _, d := range decls {
		if d.Body == nil {
			return false
		}
		inner := newConstFn(c.pkg, d.Name.Name, d.Type, d.Body)
		found, allConstant := false, true
		ast.Inspect(d.Body, func(n ast.Node) bool {
			r, isReturn := n.(*ast.ReturnStmt)
			if !isReturn || len(r.Results) != 1 {
				return true
			}
			found = true
			if !inner.constant(r.Results[0], v, depth+1) {
				allConstant = false
			}
			return true
		})
		if !found || !allConstant {
			return false
		}
	}
	return true
}

// paramConstant is the heart of the analysis, and the thing the negative
// control disables. A parameter is not constant because it is a parameter: it
// is constant only when EVERY call in the package supplies a constant at that
// position. That is what makes a repository full of
// `func (r *Repo) list(ctx, q, sql string, args ...any)` helpers analysable
// without declaring parameters trustworthy — and it is what makes
// `q.Query(ctx, "... WHERE x = '"+name+"'")` reportable when name is one.
func (c *constFn) paramConstant(name string, idx int, v visited, depth int) bool {
	if c.pkg.paramsAreConstant {
		return true
	}
	owner := c.owner[name]
	// A Querier implementation relaying its own sql parameter to the pool:
	// every caller of it is itself an X.Query/QueryRow/Exec site that the
	// repository-wide sweep checks directly, so the obligation is discharged
	// there rather than here.
	if _, isQueryMethod := sqlQueryMethods[owner]; isQueryMethod {
		return true
	}
	key := "param:" + owner + ":" + name
	if v[key] {
		return false
	}
	v[key] = true
	defer delete(v, key)

	callSites, allConstant := 0, true
	for _, f := range c.pkg.files {
		ast.Inspect(f, func(n ast.Node) bool {
			fn, isFunc := n.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil {
				return true
			}
			caller := newConstFn(c.pkg, fn.Name.Name, fn.Type, fn.Body)
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				call, isCall := m.(*ast.CallExpr)
				if !isCall {
					return true
				}
				var called string
				switch fx := call.Fun.(type) {
				case *ast.Ident:
					called = fx.Name
				case *ast.SelectorExpr:
					called = fx.Sel.Name
				default:
					return true
				}
				if called != owner || len(call.Args) <= idx {
					return true
				}
				callSites++
				if !caller.constant(call.Args[idx], v, depth+1) {
					allConstant = false
				}
				return true
			})
			return true
		})
	}
	// No call site in the package means nothing supplies the parameter here,
	// so it cannot be proven constant.
	return callSites > 0 && allConstant
}

// --- scanning --------------------------------------------------------------

// sqlFinding is one statement that could not be proven constant.
type sqlFinding struct {
	Pos  string
	Expr string
}

// scanPackage returns every query call site in the package and the ones whose
// statement is not constant-derived.
func scanPackage(p *constPkg) (sites int, findings []sqlFinding) {
	for _, f := range p.files {
		ast.Inspect(f, func(n ast.Node) bool {
			fn, isFunc := n.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil {
				return true
			}
			c := newConstFn(p, fn.Name.Name, fn.Type, fn.Body)
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				call, isCall := m.(*ast.CallExpr)
				if !isCall {
					return true
				}
				sel, isSel := call.Fun.(*ast.SelectorExpr)
				if !isSel {
					return true
				}
				idx, isQuery := sqlQueryMethods[sel.Sel.Name]
				if !isQuery || len(call.Args) <= idx {
					return true
				}
				sites++
				arg := call.Args[idx]
				if !c.constant(arg, visited{}, 0) {
					pos := p.fset.Position(arg.Pos())
					findings = append(findings, sqlFinding{
						Pos:  fmt.Sprintf("%s:%d", filepath.ToSlash(pos.Filename), pos.Line),
						Expr: exprText(p.fset, arg),
					})
				}
				return true
			})
			return true
		})
	}
	return sites, findings
}

func exprText(fset *token.FileSet, e ast.Expr) string {
	start := fset.Position(e.Pos())
	src, err := os.ReadFile(start.Filename)
	if err != nil {
		return "<source unavailable>"
	}
	end := fset.Position(e.End())
	if start.Offset < 0 || end.Offset > len(src) || end.Offset <= start.Offset {
		return "<out of range>"
	}
	text := strings.Join(strings.Fields(string(src[start.Offset:end.Offset])), " ")
	if len(text) > 160 {
		text = text[:160] + "..."
	}
	return text
}

// parsePackageDir parses the non-test Go files of one directory. It uses
// ParseFile rather than ParseDir so nothing depends on the deprecated
// ast.Package, and so the file set positions are absolute.
func parsePackageDir(t *testing.T, dir string, paramsAreConstant bool) *constPkg {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	p := &constPkg{
		dir: dir, fset: token.NewFileSet(),
		consts: map[string]ast.Expr{}, funcs: map[string][]*ast.FuncDecl{},
		paramsAreConstant: paramsAreConstant,
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		f, perr := parser.ParseFile(p.fset, filepath.Join(dir, name), nil, 0)
		require.NoErrorf(t, perr, "parse %s", name)
		p.addFile(f)
	}
	return p
}

func (p *constPkg) addFile(f *ast.File) {
	p.files = append(p.files, f)
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			if d.Tok != token.CONST && d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				vs, isValue := spec.(*ast.ValueSpec)
				if !isValue {
					continue
				}
				for i, nm := range vs.Names {
					if i < len(vs.Values) {
						p.consts[nm.Name] = vs.Values[i]
					}
				}
			}
		case *ast.FuncDecl:
			p.funcs[d.Name.Name] = append(p.funcs[d.Name.Name], d)
		}
	}
}

// parseFixture parses one in-memory Go file as a package of its own, so a
// deliberately unsafe (or deliberately safe) construction can be scanned by
// the same engine that scans the repository.
func parseFixture(t *testing.T, name, src string, paramsAreConstant bool) *constPkg {
	t.Helper()
	p := &constPkg{
		dir: name, fset: token.NewFileSet(),
		consts: map[string]ast.Expr{}, funcs: map[string][]*ast.FuncDecl{},
		paramsAreConstant: paramsAreConstant,
	}
	f, err := parser.ParseFile(p.fset, name, src, 0)
	require.NoError(t, err)
	p.addFile(f)
	return p
}

// injectableFixture is the planted defect: a request-supplied name is
// concatenated into the statement instead of being bound. Nothing in the
// repository looks like this, which is the point — the scanner must be able to
// see one if it ever appears.
const injectableFixture = `package fixture

import "context"

type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (any, error)
}

const accountColumns = "id, owner_user_id, status"

// SearchByName concatenates caller-supplied text into the statement.
func SearchByName(ctx context.Context, q Querier, name string) (any, error) {
	return q.Query(ctx, "SELECT "+accountColumns+" FROM accounts WHERE display_name = '"+name+"'")
}
`

// parameterisedFixture is the same query written correctly. The scanner must
// NOT report it, or "zero findings in the repository" would only mean the
// scanner reports nothing at all.
const parameterisedFixture = `package fixture

import "context"

type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (any, error)
}

const accountColumns = "id, owner_user_id, status"

// SearchByName binds the caller-supplied text as a parameter.
func SearchByName(ctx context.Context, q Querier, name string) (any, error) {
	return q.Query(ctx, "SELECT "+accountColumns+" FROM accounts WHERE display_name = $1", name)
}
`

// TestSQLInjection_EveryStatementIsBuiltFromConstants proves that no SQL the
// API can execute is assembled from anything but source text, so the injection
// payloads swept in TestInjection_PayloadsNeverReachAnInterpreter cannot reach
// an interpreter even in principle.
//
// It is three assertions, and all three are needed:
//
//   - the scanner reports the planted concatenation (sensitivity),
//   - the scanner does not report the parameterised equivalent (specificity),
//   - the repository produces no findings at all (the property itself).
//
// The negative control makes a function parameter count as constant-derived.
// That is the one shortcut that would let a repository of SQL-forwarding
// helpers analyze "clean" while a concatenation sat inside one, so with it set
// the planted fixture goes unreported and the first assertion fires.
func TestSQLInjection_EveryStatementIsBuiltFromConstants(t *testing.T) {
	paramsAreConstant := secBreak(t, "sqlscan_treats_parameters_as_constant")

	planted := parseFixture(t, "injectable.go", injectableFixture, paramsAreConstant)
	sites, findings := scanPackage(planted)
	require.Equal(t, 1, sites, "the fixture must contain exactly one query call site")
	require.Len(t, findings, 1,
		"the scanner did not report a statement built by concatenating a function parameter; "+
			"it cannot be trusted to report one in the repository")

	clean := parseFixture(t, "parameterised.go", parameterisedFixture, paramsAreConstant)
	sites, findings = scanPackage(clean)
	require.Equal(t, 1, sites)
	require.Empty(t, findings,
		"the scanner reported a correctly parameterised statement; it reports everything and proves nothing")

	root := repoRoot(t)
	dirs := sqlScanDirs(t, root)
	require.NotEmpty(t, dirs, "no source directories were scanned")

	totalSites := 0
	var repoFindings []sqlFinding
	for _, dir := range dirs {
		p := parsePackageDir(t, dir, paramsAreConstant)
		if len(p.files) == 0 {
			continue
		}
		n, f := scanPackage(p)
		totalSites += n
		repoFindings = append(repoFindings, f...)
	}

	// A scan that found no call sites would report no findings for the wrong
	// reason. The floor is well under the current count and only guards
	// against the analysis silently walking an empty tree.
	require.Greater(t, totalSites, 200,
		"only %d query call sites were found; the scan is not reaching the repository's SQL", totalSites)

	for _, f := range repoFindings {
		rel, err := filepath.Rel(root, filepath.FromSlash(f.Pos))
		if err != nil {
			rel = f.Pos
		}
		t.Errorf("SQL statement not provably built from constants at %s: %s", filepath.ToSlash(rel), f.Expr)
	}
	require.Empty(t, repoFindings,
		"%d of %d SQL statements could not be proven constant; a request-derived string may be concatenated into one",
		len(repoFindings), totalSites)
	t.Logf("scanned %d SQL call sites across %d packages with no findings", totalSites, len(dirs))
}

// sqlScanDirs returns every source directory under internal/ and cmd/ except
// the declared exclusions.
func sqlScanDirs(t *testing.T, root string) []string {
	t.Helper()
	var dirs []string
	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			rel := filepath.ToSlash(mustRel(t, root, p))
			for skip := range sqlScanSkip {
				if rel == skip || strings.HasPrefix(rel, skip+"/") {
					return filepath.SkipDir
				}
			}
			dirs = append(dirs, p)
			return nil
		})
		require.NoError(t, err)
	}
	sort.Strings(dirs)
	return dirs
}

// TestSQLInjection_ExcludedTreesAreOffTheRequestPath keeps the exclusions
// honest. A tree may be left out of the constant-derivation scan only while no
// request can reach its statements, so each one is asserted to exist and to be
// unreachable from the HTTP server and its binary.
func TestSQLInjection_ExcludedTreesAreOffTheRequestPath(t *testing.T) {
	root := repoRoot(t)
	for tree, why := range sqlScanSkip {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(tree)))
		require.NoErrorf(t, err, "excluded tree %s does not exist; delete the exclusion (%s)", tree, why)
		require.True(t, info.IsDir())
	}
	// The API server and its composition root must not import an excluded
	// tree, or a request could reach statements this scan never saw.
	for _, reachable := range []string{"internal/httpapi", "cmd/api"} {
		for file, imports := range importsOf(t, root, reachable) {
			for _, imp := range imports {
				for tree := range sqlScanSkip {
					require.Falsef(t, imp == tree || strings.HasPrefix(imp, tree+"/"),
						"%s imports %s, which the SQL scan excludes; the exclusion is no longer sound", file, imp)
				}
			}
		}
	}
}

func mustRel(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	require.NoError(t, err)
	return rel
}
