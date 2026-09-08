// Command inttest runs every integration-tagged Go test package, each against
// its own freshly provisioned database.
//
// It exists because `make integration` used to be `go test -tags=integration
// ./test/integration/...`, and ./test/integration/ contains exactly one
// package: the migration suite. The database-backed proof of the financial
// core — ledger, capital, settlement, execution, reconciliation, signing,
// httpapi, and thirty more — lives in *_test.go files behind //go:build
// integration inside internal/ and cmd/. None of them ran locally. `make
// test-all` reported success having executed none of them.
//
// CI grew its own inline shell loop to work around this, with a comment saying
// the exclusion would live there "until that target takes a package list". This
// program is that fix: one implementation both the Makefile and CI call, so the
// two can no longer disagree about what "the integration tests" means.
//
// Packages are enumerated from the build tag itself, so a new integration
// package is picked up without editing anything.
//
// Each package gets its OWN database. Sharing one does not work and never did:
// internal/event truncates outbox_events while other suites assert on global
// row counts, and the migration suite drops and recreates the schema mid-run.
//
// Usage:
//
//	go run ./scripts/inttest [-race] [-pkg regexp] [-timeout 30m] [-list] [-keep]
//
// Exit status is 1 when any package fails, and the failing package list is
// printed last so it is the thing on screen when the run ends.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// searchDirs are the trees walked for integration-tagged test files.
var searchDirs = []string{"internal", "cmd", "test"}

// buildTagLine is the exact directive a file must carry to be counted.
const buildTagLine = "//go:build integration"

// minPackages guards against a broken enumeration silently "passing". The
// original defect was a job that ran zero packages and exited 0; refusing to
// run at all is the correct response to finding almost none.
const minPackages = 2

func main() {
	os.Exit(run())
}

func run() int {
	var (
		race     = flag.Bool("race", false, "run under the race detector")
		pkgRe    = flag.String("pkg", "", "only run packages matching this regexp")
		timeout  = flag.Duration("timeout", 30*time.Minute, "per-package go test timeout")
		list     = flag.Bool("list", false, "list the packages and exit")
		keep     = flag.Bool("keep", false, "keep each package's database instead of dropping it")
		extraTag = flag.String("tags", "", "additional build tags, comma separated")
	)
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "inttest:", err)
		return 1
	}
	pkgs, err := discover(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "inttest:", err)
		return 1
	}
	if *pkgRe != "" {
		re, err := regexp.Compile(*pkgRe)
		if err != nil {
			fmt.Fprintln(os.Stderr, "inttest: bad -pkg regexp:", err)
			return 1
		}
		var kept []string
		for _, p := range pkgs {
			if re.MatchString(p) {
				kept = append(kept, p)
			}
		}
		pkgs = kept
	} else if len(pkgs) < minPackages {
		fmt.Fprintf(os.Stderr,
			"inttest: found only %d integration packages; the enumeration is broken, not the tree\n", len(pkgs))
		return 1
	}
	if len(pkgs) == 0 {
		fmt.Fprintln(os.Stderr, "inttest: no packages matched")
		return 1
	}
	if *list {
		for _, p := range pkgs {
			fmt.Println(p)
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tags := "integration"
	if *extraTag != "" {
		tags += "," + *extraTag
	}

	fmt.Printf("inttest: %d packages, one database each\n", len(pkgs))
	var failed []string
	start := time.Now()
	for i, pkg := range pkgs {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "inttest: interrupted")
			return 130
		}
		name := dbName(pkg)
		fmt.Printf("\n=== [%d/%d] %s (database controlplane_test_%s)\n", i+1, len(pkgs), pkg, name)

		env, err := provision(ctx, root, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "inttest: provision %s: %v\n", name, err)
			failed = append(failed, pkg+" (provision failed)")
			continue
		}
		args := []string{"test", "-count=1", "-timeout=" + timeout.String(), "-tags=" + tags}
		if *race {
			args = append(args, "-race")
		}
		args = append(args, pkg)

		// #nosec G204 -- args are built here from a fixed list of flags and a
		// package path this program enumerated from the filesystem. Nothing in
		// them comes from a network or a user.
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			failed = append(failed, pkg)
		}
		if !*keep {
			drop(ctx, root, name)
		}
	}

	fmt.Printf("\ninttest: %d packages in %s\n", len(pkgs), time.Since(start).Round(time.Second))
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "inttest: FAILED (%d):\n", len(failed))
		for _, f := range failed {
			fmt.Fprintln(os.Stderr, "  "+f)
		}
		return 1
	}
	fmt.Println("inttest: all packages passed")
	return 0
}

// repoRoot walks up from the working directory to the directory holding go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod found above the working directory")
		}
		dir = parent
	}
}

// discover returns the import paths of every package containing at least one
// file whose first non-blank, non-comment lines include the integration build
// tag. Only the head of each file is read: a build directive must precede the
// package clause, so anything later is not one.
func discover(root string) ([]string, error) {
	seen := map[string]bool{}
	for _, dir := range searchDirs {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "node_modules" || name == "testdata" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			tagged, err := hasIntegrationTag(path)
			if err != nil || !tagged {
				return err
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			seen["./"+filepath.ToSlash(rel)] = true
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

func hasIntegrationTag(path string) (bool, error) {
	f, err := os.Open(path) // #nosec G304 -- path comes from walking the repository
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == buildTagLine {
			return true, nil
		}
		// Build directives precede the package clause; once it appears there
		// are no more.
		if strings.HasPrefix(line, "package ") {
			return false, nil
		}
	}
	return false, sc.Err()
}

// dbNameUnsafe matches everything a PostgreSQL identifier should not contain.
var dbNameUnsafe = regexp.MustCompile(`[^a-z0-9_]+`)

// dbName derives a short, stable, lowercase database suffix from an import
// path. PostgreSQL truncates identifiers at 63 bytes and testdb prefixes
// "controlplane_test_", so the tail is kept rather than the head: the tail is
// the part that distinguishes ./internal/capital from ./internal/capital/buyingpower.
func dbName(pkg string) string {
	s := strings.ToLower(strings.TrimPrefix(pkg, "./"))
	s = dbNameUnsafe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	const max = 34
	if len(s) > max {
		s = s[len(s)-max:]
		s = strings.TrimLeft(s, "_")
	}
	return "it_" + s
}

// provision runs scripts/testdb and parses the `export NAME=value` lines it
// prints, returning them as environment entries.
func provision(ctx context.Context, root, name string) ([]string, error) {
	// #nosec G204 -- a constant command line; `name` is derived from a package
	// path this program enumerated, not from input.
	cmd := exec.CommandContext(ctx, "go", "run", "./scripts/testdb", "-name", name, "-export")
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var env []string
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "export ")
		if k, _, ok := strings.Cut(line, "="); ok && k != "" {
			env = append(env, line)
		}
	}
	if len(env) == 0 {
		return nil, errors.New("testdb printed no environment")
	}
	return env, nil
}

// drop removes a package's database. A failure here is reported and ignored:
// leaking a local test database is untidy, not a test result.
func drop(ctx context.Context, root, name string) {
	// #nosec G204 -- a constant command line; `name` is derived from a package
	// path this program enumerated, not from input.
	cmd := exec.CommandContext(ctx, "go", "run", "./scripts/testdb", "-name", name, "-drop")
	cmd.Dir = root
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "inttest: could not drop controlplane_test_%s: %v\n", name, err)
	}
}
