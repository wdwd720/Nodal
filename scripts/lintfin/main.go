// Command lintfin enforces the financial-code invariants that generic linters
// cannot express precisely (see docs/architecture/CONVENTIONS.md):
//
//  1. No floating point in the financial packages. Production (non-_test) files
//     under internal/{money,ledger,capital,risk,positions,valuation,quote,settlement}
//     may not contain float32/float64 identifiers, float literals,
//     strconv.ParseFloat/FormatFloat, big.Float/big.NewFloat, or .Float64()/.Float32() calls.
//  2. Production files under internal/ and cmd/ never import test-only packages:
//     internal/testkit/... or any <pkg>/<pkg>test package.
//  3. Agent-facing code (internal/agent/**, internal/strategy/**) never imports
//     internal/signing, internal/wallet, internal/admin, internal/capital or internal/risk/policy.
//
// Suppress one finding with a trailing comment `// lintfin:allow <reason>` on the
// offending line. Output is file:line:col: message; exit status 1 when anything is reported.
//
// Usage:
//
//	go run ./scripts/lintfin [-root .]
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Config selects what Run scans.
type Config struct {
	Root        string
	Module      string   // module path from go.mod
	ScanDirs    []string // production trees, relative to Root
	FinDirs     []string // packages (and subpackages) where floats are forbidden
	AgentDirs   []string // packages (and subpackages) subject to AgentDenied
	AgentDenied []string // module-relative package prefixes agents may not import
}

// DefaultConfig returns the repository policy for root and module.
func DefaultConfig(root, module string) Config {
	return Config{
		Root:     root,
		Module:   module,
		ScanDirs: []string{"internal", "cmd"},
		FinDirs: []string{
			"internal/money", "internal/ledger", "internal/capital", "internal/risk",
			"internal/positions", "internal/valuation", "internal/quote", "internal/settlement",
		},
		AgentDirs: []string{"internal/agent", "internal/strategy"},
		AgentDenied: []string{
			"internal/signing", "internal/wallet", "internal/admin", "internal/capital", "internal/risk/policy",
		},
	}
}

// Finding is one violation.
type Finding struct {
	File string // slash-separated, relative to root
	Line int
	Col  int
	Msg  string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d: %s", f.File, f.Line, f.Col, f.Msg)
}

const allowMarker = "lintfin:allow"

// Run scans the configured trees and returns findings sorted by position.
func Run(cfg Config) ([]Finding, error) {
	var findings []Finding
	fset := token.NewFileSet()
	for _, d := range cfg.ScanDirs {
		dir := filepath.Join(cfg.Root, d)
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		err := filepath.WalkDir(dir, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() {
				if p != dir && skipDir(de.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			name := de.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(cfg.Root, p)
			if err != nil {
				return err
			}
			found, err := checkFile(cfg, fset, p, filepath.ToSlash(rel))
			if err != nil {
				return err
			}
			findings = append(findings, found...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Col < b.Col
	})
	return findings, nil
}

func skipDir(name string) bool {
	switch name {
	case "testdata", "vendor", "node_modules":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func inTree(rel string, trees []string) bool {
	for _, t := range trees {
		if rel == t || strings.HasPrefix(rel, t+"/") {
			return true
		}
	}
	return false
}

func checkFile(cfg Config, fset *token.FileSet, abs, rel string) ([]Finding, error) {
	src, err := os.ReadFile(abs) // #nosec G304 -- path produced by walking the repository
	if err != nil {
		return nil, err
	}
	f, err := parser.ParseFile(fset, abs, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	allowed := allowedLines(src)
	var out []Finding
	add := func(pos token.Pos, msg string) {
		p := fset.Position(pos)
		if allowed[p.Line] {
			return
		}
		out = append(out, Finding{File: rel, Line: p.Line, Col: p.Column, Msg: msg})
	}

	dir := path.Dir(rel)
	agent := inTree(dir, cfg.AgentDirs)
	for _, imp := range f.Imports {
		ip, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if isTestOnlyImport(cfg.Module, ip) {
			add(imp.Pos(), fmt.Sprintf("production code imports test-only package %q", ip))
		}
		if agent {
			for _, denied := range cfg.AgentDenied {
				full := cfg.Module + "/" + denied
				if ip == full || strings.HasPrefix(ip, full+"/") {
					add(imp.Pos(), fmt.Sprintf("agent-facing package imports %q (forbidden: agents get read tools, prediction and intent creation only)", ip))
				}
			}
		}
	}

	if inTree(dir, cfg.FinDirs) {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == "float32" || x.Name == "float64" {
					add(x.Pos(), fmt.Sprintf("%s is forbidden in financial packages: use money.USD, money.BPS, money.Quantity or money.Price", x.Name))
				}
			case *ast.BasicLit:
				if x.Kind == token.FLOAT {
					add(x.Pos(), fmt.Sprintf("floating-point literal %s is forbidden in financial packages", x.Value))
				}
			case *ast.SelectorExpr:
				sel := x.Sel.Name
				pkg := ""
				if id, ok := x.X.(*ast.Ident); ok {
					pkg = id.Name
				}
				switch {
				case pkg == "strconv" && (sel == "ParseFloat" || sel == "FormatFloat"):
					add(x.Pos(), fmt.Sprintf("strconv.%s is forbidden in financial packages: parse decimals exactly", sel))
				case pkg == "big" && (sel == "Float" || sel == "NewFloat" || sel == "ParseFloat"):
					add(x.Pos(), fmt.Sprintf("big.%s is forbidden in financial packages: use big.Int-backed money.Quantity", sel))
				case sel == "Float64" || sel == "Float32":
					add(x.Sel.Pos(), fmt.Sprintf(".%s() is forbidden in financial packages", sel))
				}
			}
			return true
		})
	}
	return out, nil
}

// isTestOnlyImport reports whether ip is a test-only package of the module:
// internal/testkit/... or a <pkg>/<pkg>test package (e.g. internal/event/eventtest).
func isTestOnlyImport(module, ip string) bool {
	if !strings.HasPrefix(ip, module+"/") {
		return false
	}
	rel := strings.TrimPrefix(ip, module+"/")
	if rel == "internal/testkit" || strings.HasPrefix(rel, "internal/testkit/") {
		return true
	}
	parts := strings.Split(rel, "/")
	if len(parts) < 2 {
		return false
	}
	last, parent := parts[len(parts)-1], parts[len(parts)-2]
	return last == parent+"test"
}

// allowedLines returns the 1-based lines carrying a lintfin:allow marker in a comment.
func allowedLines(src []byte) map[int]bool {
	out := map[int]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(src)))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for i := 1; sc.Scan(); i++ {
		line := sc.Text()
		if idx := strings.Index(line, "//"); idx >= 0 && strings.Contains(line[idx:], allowMarker) {
			out[i] = true
		}
	}
	return out
}

// ModulePath reads the module directive from root/go.mod.
func ModulePath(root string) (string, error) {
	gomod := filepath.Join(root, "go.mod")
	data, err := os.ReadFile(gomod) // #nosec G304 -- go.mod at the resolved repository root
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`), nil
		}
	}
	return "", fmt.Errorf("%s: no module directive", gomod)
}

func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lintfin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "repository root (default: nearest go.mod above the working directory)")
	finDirs := fs.String("fin-dirs", "", "comma-separated override of the float-forbidden package trees")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" {
		*root = repoRoot()
	}
	module, err := ModulePath(*root)
	if err != nil {
		fmt.Fprintf(stderr, "lintfin: %v\n", err)
		return 2
	}
	cfg := DefaultConfig(*root, module)
	if *finDirs != "" {
		cfg.FinDirs = nil
		for _, d := range strings.Split(*finDirs, ",") {
			if d = strings.TrimSpace(d); d != "" {
				cfg.FinDirs = append(cfg.FinDirs, filepath.ToSlash(d))
			}
		}
	}
	findings, err := Run(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "lintfin: %v\n", err)
		return 2
	}
	for _, f := range findings {
		fmt.Fprintln(stdout, f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(stderr, "lintfin: %d finding(s)\n", len(findings))
		return 1
	}
	return 0
}
