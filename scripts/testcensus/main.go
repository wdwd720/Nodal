// Command testcensus counts the tests the repository declares, by category and
// package, so a document that states how many tests prove something quotes a
// command rather than a memory.
//
//	go run ./scripts/testcensus [-pkg regexp] [-advanced] [-json]
//
// Categories, decided per declaration:
//
//   - fuzz: func FuzzXxx(f *testing.F)
//   - property: func TestProp_Xxx (the repository's convention for rapid properties)
//   - integration: any other test in a file built only with the integration tag
//   - unit: everything else
//
// It also counts the web app's unit tests (apps/web/src/**/*.test.ts[x]) and
// Playwright tests (apps/web/e2e/*.spec.ts) by their test( and it( calls.
//
// It declares what exists; it runs nothing. Whether the tests pass is what
// `go test`, scripts/inttest and the web test commands report.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
)

// advancedPackages are the packages of the advanced capabilities (ADR-0020).
var advancedPackages = []string{
	"internal/econ", "internal/risk", "internal/capital/buyingpower", "internal/ledger",
	"internal/transfers", "internal/sandboxfund", "internal/margin", "internal/derivatives", "internal/quant",
	"internal/fees", "internal/matching", "internal/orders", "internal/equities", "internal/devnet",
	"internal/devnethost", "internal/stableasset", "internal/bridge", "internal/sandboxchain",
	"internal/predmarket", "internal/marketplace", "internal/sandboxrt", "internal/treasury",
	"internal/feeledger", "internal/delegation", "internal/advancedjobs", "internal/advancedmetrics",
	"internal/compliance", "internal/instruments",
}

var (
	declPattern  = regexp.MustCompile(`(?m)^func\s+((?:Test|Fuzz)[A-Za-z0-9_]*)\s*\(`)
	integrationT = regexp.MustCompile(`(?m)^//go:build\s+.*\bintegration\b`)
	webTest      = regexp.MustCompile(`(?m)^\s*(?:test|it)(?:\.(?:only|skip))?\(\s*['"\x60]`)
)

// Counts are the tests of one package or of the whole census.
type Counts struct {
	Unit        int `json:"unit"`
	Property    int `json:"property"`
	Fuzz        int `json:"fuzz"`
	Integration int `json:"integration"`
}

// Total is every category together.
func (c Counts) Total() int { return c.Unit + c.Property + c.Fuzz + c.Integration }

func (c *Counts) add(o Counts) {
	c.Unit += o.Unit
	c.Property += o.Property
	c.Fuzz += o.Fuzz
	c.Integration += o.Integration
}

// Report is the census.
type Report struct {
	Packages map[string]Counts `json:"packages"`
	Total    Counts            `json:"total"`
	WebUnit  int               `json:"web_unit"`
	WebE2E   int               `json:"web_e2e"`
}

func main() {
	pkgFilter := flag.String("pkg", "", "only packages whose path matches this regexp")
	advanced := flag.Bool("advanced", false, "only the advanced capabilities' packages (internal/httpapi excluded)")
	asJSON := flag.Bool("json", false, "print JSON")
	flag.Parse()

	var filter *regexp.Regexp
	if *pkgFilter != "" {
		var err error
		if filter, err = regexp.Compile(*pkgFilter); err != nil {
			fmt.Fprintln(os.Stderr, "testcensus:", err)
			os.Exit(2)
		}
	}
	keep := func(pkg string) bool {
		if filter != nil && !filter.MatchString(pkg) {
			return false
		}
		if *advanced {
			for _, a := range advancedPackages {
				if pkg == a {
					return true
				}
			}
			return false
		}
		return true
	}

	rep := Report{Packages: map[string]Counts{}}
	for _, root := range []string{"internal", "cmd", "scripts", "test"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			pkg := filepath.ToSlash(filepath.Dir(path))
			if !keep(pkg) {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			c := classify(string(src))
			cur := rep.Packages[pkg]
			cur.add(c)
			rep.Packages[pkg] = cur
			rep.Total.add(c)
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "testcensus:", err)
			os.Exit(1)
		}
	}
	if !*advanced && filter == nil {
		rep.WebUnit = countWeb("apps/web/src", ".test.ts", ".test.tsx")
		rep.WebE2E = countWeb("apps/web/e2e", ".spec.ts")
	}
	if *asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
		return
	}
	print(rep)
}

func classify(src string) Counts {
	var c Counts
	integration := integrationT.MatchString(src)
	for _, m := range declPattern.FindAllStringSubmatch(src, -1) {
		name := m[1]
		switch {
		case name == "TestMain":
		case strings.HasPrefix(name, "Fuzz"):
			c.Fuzz++
		case strings.HasPrefix(name, "TestProp"):
			c.Property++
		case integration:
			c.Integration++
		default:
			c.Unit++
		}
	}
	return c
}

func countWeb(dir string, suffixes ...string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		for _, s := range suffixes {
			if strings.HasSuffix(path, s) {
				f, err := os.Open(path)
				if err != nil {
					return nil
				}
				sc := bufio.NewScanner(f)
				sc.Buffer(make([]byte, 1024*1024), 1024*1024)
				for sc.Scan() {
					if webTest.MatchString(sc.Text()) {
						n++
					}
				}
				_ = f.Close()
			}
		}
		return nil
	})
	return n
}

func print(rep Report) {
	pkgs := make([]string, 0, len(rep.Packages))
	for p := range rep.Packages {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "package\tunit\tproperty\tfuzz\tintegration\ttotal\t")
	for _, p := range pkgs {
		c := rep.Packages[p]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t\n", p, c.Unit, c.Property, c.Fuzz, c.Integration, c.Total())
	}
	fmt.Fprintf(tw, "TOTAL (%d packages)\t%d\t%d\t%d\t%d\t%d\t\n", len(pkgs), rep.Total.Unit, rep.Total.Property, rep.Total.Fuzz,
		rep.Total.Integration, rep.Total.Total())
	_ = tw.Flush()
	if rep.WebUnit > 0 || rep.WebE2E > 0 {
		fmt.Printf("\nweb unit tests: %d   Playwright tests: %d\n", rep.WebUnit, rep.WebE2E)
	}
}
