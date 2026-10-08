package reachability

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// reachedThroughDriver lists mutators a deployment reaches only through an
// exported driver of their own package: a method that runs the mutator --
// usually as one stage among several, each in its own transaction -- and that
// a deployment calls. The name-and-import matching above cannot see that
// path, because the mutator's only caller is inside its package.
//
// Each entry names the driver as Type.Method, and both halves of the claim are
// checked mechanically (driverReaches, driverCalledOutside): the driver's body
// reaches a call of the mutator through functions of the same package, and a
// non-test file outside the package that imports it calls the driver. An entry
// whose claim stops being true fails the test, like a stale exemption.
//
// This is the same bar reachedThroughInterface sets -- "a deployment does run
// this, and here is the path" -- with the path checked by the test instead of
// by a reader.
var reachedThroughDriver = map[string]string{
	// The stable asset's workflows own their transactions: the submission
	// happens between them, so the stages are separate mutators.
	"internal/stableasset.PrepareMint":   "Service.Mint",
	"internal/stableasset.SettleMint":    "Service.Mint",
	"internal/stableasset.PrepareBurn":   "Service.Burn",
	"internal/stableasset.SettleBurn":    "Service.Burn",
	"internal/stableasset.PayRedemption": "Service.ProcessRedemptions",

	// An order runs Accept, Approve, Reserve, Execute and Complete in a
	// transaction each, through the pipeline the HTTP adapter calls.
	"internal/orders.Accept":   "Pipeline.Submit",
	"internal/orders.Reserve":  "Pipeline.Submit",
	"internal/orders.Execute":  "Pipeline.Submit",
	"internal/orders.Complete": "Pipeline.Submit",
	"internal/orders.Cancel":   "Pipeline.Cancel",
	"internal/orders.Replace":  "Pipeline.Replace",

	// A bridge leg is driven step by step across two chains.
	"internal/bridge.PrepareLeg":        "Service.Bridge",
	"internal/bridge.SettleSource":      "Service.Advance",
	"internal/bridge.PrepareRelease":    "Service.Advance",
	"internal/bridge.SettleDestination": "Service.Advance",
	// A refund is recorded, submitted once between transactions, and settled
	// when it is final on the source chain.
	"internal/bridge.SettleRefund": "Service.Refund",
}

func TestReachability_EveryDriverClaimIsTrue(t *testing.T) {
	root := repoRoot(t)
	keys := make([]string, 0, len(reachedThroughDriver))
	for k := range reachedThroughDriver {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		idx := strings.LastIndex(key, ".")
		if idx < 0 || !strings.HasPrefix(key, "internal/") {
			t.Errorf("%s is not internal/<pkg>.<Mutator>", key)
			continue
		}
		pkg, mutatorName := key[:idx], key[idx+1:]
		driver := reachedThroughDriver[key]
		funcs := packageFuncs(t, filepath.Join(root, filepath.FromSlash(pkg)))
		if _, declared := funcs[driver]; !declared {
			t.Errorf("%s names the driver %s, which %s does not declare", key, driver, pkg)
			continue
		}
		if !ast.IsExported(driver[strings.Index(driver, ".")+1:]) {
			t.Errorf("%s names the driver %s, which is not exported", key, driver)
			continue
		}
		if !driverReaches(funcs, driver, mutatorName) {
			t.Errorf("%s: the driver %s does not reach a call of %s inside %s", key, driver, mutatorName, pkg)
		}
		if !driverCalledOutside(t, root, pkg, driver[strings.Index(driver, ".")+1:]) {
			t.Errorf("%s: nothing a deployment runs calls the driver %s", key, driver)
		}
	}
}

// TestReachability_TheDriverCheckCanFail is the negative control: a mutator
// whose only in-package caller is not called from outside must not count as
// reached, and a driver that does not call the mutator must not either.
func TestReachability_TheDriverCheckCanFail(t *testing.T) {
	src := `package p
type Service struct{}
func (s *Service) Entry() { s.helper() }
func (s *Service) helper() { s.Stage() }
func (s *Service) Stage() {}
func (s *Service) Other() {}
func (s *Service) Orphan() { s.Lonely() }
func (s *Service) Lonely() {}
type Pipe struct{ svc *Service }
func (p *Pipe) Run() { stages := []func(){p.svc.Valued}; for _, f := range stages { f() } }
func (s *Service) Valued() {}`
	funcs := funcsOf(t, src)
	if !driverReaches(funcs, "Service.Entry", "Stage") {
		t.Fatal("the walk must follow Entry -> helper -> Stage")
	}
	if driverReaches(funcs, "Service.Other", "Stage") {
		t.Fatal("Other does not call Stage")
	}
	if driverReaches(funcs, "Service.Entry", "Lonely") {
		t.Fatal("Entry does not reach Lonely")
	}
	if !driverReaches(funcs, "Pipe.Run", "Valued") {
		t.Fatal("a stage taken as a method value is reached")
	}
}

// packageFuncs parses a package's non-test files and returns its functions
// and methods by Type.Method (or Func for a plain function).
func packageFuncs(t *testing.T, dir string) map[string]*ast.FuncDecl {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.FuncDecl{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		collectFuncs(f, out)
	}
	return out
}

func funcsOf(t *testing.T, src string) map[string]*ast.FuncDecl {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := map[string]*ast.FuncDecl{}
	collectFuncs(f, out)
	return out
}

func collectFuncs(f *ast.File, out map[string]*ast.FuncDecl) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		out[funcKey(fd)] = fd
	}
}

func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	typ := fd.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if idx, ok := typ.(*ast.IndexExpr); ok {
		typ = idx.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// driverReaches walks the package's call graph from the driver, following
// every call whose name a function of the package declares, and reports
// whether it reaches a call of the mutator. Calls are matched by name within
// the package, which can over-approximate when two types of one package share
// a method name; the walk starts at a driver an entry names, so a reader can
// still check the path.
func driverReaches(funcs map[string]*ast.FuncDecl, driver, mutator string) bool {
	byName := map[string][]string{}
	for key := range funcs {
		name := key[strings.LastIndex(key, ".")+1:]
		byName[name] = append(byName[name], key)
	}
	seen := map[string]bool{driver: true}
	queue := []string{driver}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		fd := funcs[key]
		if fd == nil {
			continue
		}
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			// A selector names a method whether it is called (s.Stage(...))
			// or taken as a value (stages := []func{p.svc.Stage}); a plain
			// identifier is a call of a package-level function.
			var name string
			switch x := n.(type) {
			case *ast.SelectorExpr:
				name = x.Sel.Name
			case *ast.CallExpr:
				id, ok := x.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				name = id.Name
			default:
				return true
			}
			if name == mutator {
				found = true
				return false
			}
			for _, next := range byName[name] {
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// driverCalled finds a call of a method by name, with any arguments.
var driverCalled = regexp.MustCompile(`\.([A-Z]\w*)\(`)

// driverCalledOutside reports whether a non-test file outside the package, in
// a tree a deployment runs, imports the package and calls the method.
func driverCalledOutside(t *testing.T, root, pkg, method string) bool {
	t.Helper()
	found := false
	for _, tree := range reachableFrom {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, d os.DirEntry, err error) error {
			if err != nil || found {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			if filepath.ToSlash(mustRel(t, root, filepath.Dir(path))) == pkg {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if !strings.Contains(string(src), `"github.com/nodal/controlplane/`+pkg+`"`) {
				return nil
			}
			for _, m := range driverCalled.FindAllStringSubmatch(string(src), -1) {
				if m[1] == method {
					found = true
					return filepath.SkipAll
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}
	return found
}
