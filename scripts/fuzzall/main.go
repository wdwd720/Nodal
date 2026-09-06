// Command fuzzall discovers every native Go fuzz target (func FuzzXxx(f *testing.F))
// under the repository's source trees and runs each one for a bounded time.
//
// Go fuzzes only one target per `go test` invocation, so targets run sequentially:
//
//	go test -run=^$ -fuzz=^FuzzXxx$ -fuzztime=30s ./internal/pkg
//
// Usage:
//
//	go run ./scripts/fuzzall [-fuzztime 30s] [-pkg regexp] [-tags integration] [-list] [-v]
//
// Exit status is 1 when any target fails, 130 when interrupted.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
)

// DefaultDirs are the source trees searched relative to the repository root.
var DefaultDirs = []string{"internal", "cmd", "test", "packages"}

// Target is one discovered fuzz function.
type Target struct {
	Pkg  string // package pattern relative to root, e.g. "./internal/money"
	Name string // function name, e.g. "FuzzParseUSD"
	File string // file path relative to root, slash-separated
}

// Discover walks dirs under root and returns every Fuzz* function whose file
// satisfies the build constraints for the current platform plus tags.
func Discover(root string, dirs, tags []string) ([]Target, error) {
	bctx := build.Default
	bctx.BuildTags = append([]string(nil), tags...)
	fset := token.NewFileSet()
	var targets []Target
	for _, d := range dirs {
		dir := filepath.Join(root, d)
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
			if !strings.HasSuffix(de.Name(), "_test.go") {
				return nil
			}
			ok, err := bctx.MatchFile(filepath.Dir(p), de.Name())
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			if !ok {
				return nil
			}
			f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			rel, err := filepath.Rel(root, filepath.Dir(p))
			if err != nil {
				return err
			}
			relFile, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			pkg := "./" + filepath.ToSlash(rel)
			if rel == "." {
				pkg = "."
			}
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if ok && isFuzzTarget(fd) {
					targets = append(targets, Target{Pkg: pkg, Name: fd.Name.Name, File: filepath.ToSlash(relFile)})
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Pkg != targets[j].Pkg {
			return targets[i].Pkg < targets[j].Pkg
		}
		return targets[i].Name < targets[j].Name
	})
	return targets, nil
}

func skipDir(name string) bool {
	switch name {
	case "testdata", "vendor", "node_modules":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// isFuzzTarget reports whether fd is `func FuzzX(f *testing.F)` at top level,
// applying the same naming rule as go vet: "Fuzz" followed by a non-lowercase rune.
func isFuzzTarget(fd *ast.FuncDecl) bool {
	name := fd.Name.Name
	if fd.Recv != nil || !strings.HasPrefix(name, "Fuzz") {
		return false
	}
	if rest := []rune(name[len("Fuzz"):]); len(rest) > 0 && unicode.IsLower(rest[0]) {
		return false
	}
	ft := fd.Type
	if ft.Results != nil && len(ft.Results.List) > 0 {
		return false
	}
	if ft.Params == nil || len(ft.Params.List) != 1 || len(ft.Params.List[0].Names) > 1 {
		return false
	}
	star, ok := ft.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "testing" && sel.Sel.Name == "F"
}

// Result is the outcome of running one target.
type Result struct {
	Target   Target
	Passed   bool
	Duration time.Duration
	Output   []byte
	Err      error
}

// Run executes one target with `go test -fuzz`.
func Run(ctx context.Context, goExe, root string, t Target, fuzztime string, tags []string, stream io.Writer) Result {
	args := []string{"test", "-run=^$", "-fuzz=^" + regexp.QuoteMeta(t.Name) + "$", "-fuzztime=" + fuzztime}
	if len(tags) > 0 {
		args = append(args, "-tags="+strings.Join(tags, ","))
	}
	args = append(args, t.Pkg)
	cmd := exec.CommandContext(ctx, goExe, args...) // #nosec G204 -- go test with a discovered target name and a validated fuzztime
	cmd.Dir = root
	var buf bytes.Buffer
	var out io.Writer = &buf
	if stream != nil {
		out = io.MultiWriter(&buf, stream)
	}
	cmd.Stdout, cmd.Stderr = out, out
	start := time.Now()
	err := cmd.Run()
	return Result{Target: t, Passed: err == nil, Duration: time.Since(start), Output: buf.Bytes(), Err: err}
}

func goExe() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return "go"
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

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validFuzztime(s string) bool {
	if strings.HasSuffix(s, "x") {
		_, err := fmt.Sscanf(strings.TrimSuffix(s, "x"), "%d", new(int))
		return err == nil
	}
	_, err := time.ParseDuration(s)
	return err == nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fuzzall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fuzztime := fs.String("fuzztime", "30s", "time (or Nx iterations) to fuzz each target")
	pkgFilter := fs.String("pkg", "", "only run targets whose package pattern matches this regexp")
	tagList := fs.String("tags", "", "comma-separated build tags passed to go test")
	dirList := fs.String("dirs", strings.Join(DefaultDirs, ","), "comma-separated source trees to search")
	list := fs.Bool("list", false, "list targets and exit")
	verbose := fs.Bool("v", false, "stream go test output while fuzzing")
	require := fs.Bool("require", false, "fail when no fuzz targets are found")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if !validFuzztime(*fuzztime) {
		fmt.Fprintf(stderr, "fuzzall: invalid -fuzztime %q (want a duration like 30s or a count like 1000x)\n", *fuzztime)
		return 2
	}
	var filter *regexp.Regexp
	if *pkgFilter != "" {
		re, err := regexp.Compile(*pkgFilter)
		if err != nil {
			fmt.Fprintf(stderr, "fuzzall: invalid -pkg: %v\n", err)
			return 2
		}
		filter = re
	}
	root := repoRoot()
	tags := splitList(*tagList)
	targets, err := Discover(root, splitList(*dirList), tags)
	if err != nil {
		fmt.Fprintf(stderr, "fuzzall: %v\n", err)
		return 1
	}
	if filter != nil {
		kept := targets[:0]
		for _, t := range targets {
			if filter.MatchString(t.Pkg) {
				kept = append(kept, t)
			}
		}
		targets = kept
	}
	if len(targets) == 0 {
		fmt.Fprintln(stderr, "fuzzall: no fuzz targets found")
		if *require {
			return 1
		}
		return 0
	}
	if *list {
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PACKAGE\tTARGET\tFILE")
		for _, t := range targets {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", t.Pkg, t.Name, t.File)
		}
		return flush(tw, stderr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var stream io.Writer
	if *verbose {
		stream = stderr
	}
	results := make([]Result, 0, len(targets))
	failed := 0
	for i, t := range targets {
		fmt.Fprintf(stderr, "[%d/%d] fuzzing %s.%s for %s\n", i+1, len(targets), t.Pkg, t.Name, *fuzztime)
		r := Run(ctx, goExe(), root, t, *fuzztime, tags, stream)
		results = append(results, r)
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "fuzzall: interrupted")
			return 130
		}
		if !r.Passed {
			failed++
			if !*verbose {
				_, _ = stderr.Write(r.Output)
			}
			var ee *exec.ExitError
			if !errors.As(r.Err, &ee) {
				fmt.Fprintf(stderr, "fuzzall: %s.%s: %v\n", t.Pkg, t.Name, r.Err)
			}
		}
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PACKAGE\tTARGET\tRESULT\tTIME")
	for _, r := range results {
		status := "ok"
		if !r.Passed {
			status = "FAIL"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Target.Pkg, r.Target.Name, status, r.Duration.Round(time.Millisecond))
	}
	if code := flush(tw, stderr); code != 0 {
		return code
	}
	fmt.Fprintf(stdout, "\n%d targets, %d failed\n", len(results), failed)
	if failed > 0 {
		return 1
	}
	return 0
}

func flush(tw *tabwriter.Writer, stderr io.Writer) int {
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "fuzzall: write output: %v\n", err)
		return 1
	}
	return 0
}
