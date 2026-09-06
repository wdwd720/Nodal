// Command maketargets prints the documented targets of a Makefile, aligned and
// grouped by section. It backs `make help`.
//
// A documented target is a rule whose line carries a "##" comment:
//
//	build: ## Build all Go binaries into ./bin
//
// Sections are the banner comments used in this repository:
//
//	# ---------------------------------------------------------------------------
//	# Build
//	# ---------------------------------------------------------------------------
//
// Usage:
//
//	go run ./scripts/maketargets [Makefile]
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// Target is one documented Makefile target.
type Target struct {
	Name    string
	Help    string
	Section string
}

var (
	// targetRe matches "name [name ...]: [deps] ## help". Variable assignments
	// ("X := y"), special targets (".PHONY") and tab-indented recipe lines never
	// match because the first character must be alphanumeric and the text between
	// the colon and "##" may not contain "=".
	targetRe  = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_./ -]*?)\s*::?[^#=]*##\s*(.*?)\s*$`)
	bannerRe  = regexp.MustCompile(`^#\s*-{3,}\s*$`)
	sectionRe = regexp.MustCompile(`^#\s+(\S.*?)\s*$`)
)

// Parse extracts documented targets in file order, tagging each with the most
// recent banner section title.
func Parse(r io.Reader) ([]Target, error) {
	var (
		targets     []Target
		section     string
		afterBanner bool
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if bannerRe.MatchString(line) {
			afterBanner = true
			continue
		}
		if afterBanner {
			afterBanner = false
			if m := sectionRe.FindStringSubmatch(line); m != nil {
				section = m[1]
				continue
			}
		}
		m := targetRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, name := range strings.Fields(m[1]) {
			targets = append(targets, Target{Name: name, Help: m[2], Section: section})
		}
	}
	return targets, sc.Err()
}

// Render writes the targets aligned in two columns, grouped by section.
func Render(w io.Writer, targets []Target) {
	width := 0
	for _, t := range targets {
		if len(t.Name) > width {
			width = len(t.Name)
		}
	}
	fmt.Fprintln(w, "Usage: make <target>")
	current, first := "", true
	for _, t := range targets {
		if first || t.Section != current {
			first = false
			current = t.Section
			fmt.Fprintln(w)
			if current != "" {
				fmt.Fprintln(w, current)
			}
		}
		fmt.Fprintf(w, "  %-*s  %s\n", width, t.Name, t.Help)
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	file := "Makefile"
	if len(args) > 0 {
		file = args[0]
	}
	data, err := os.ReadFile(file) // #nosec G304 G703 -- the Makefile path is this tool's CLI argument
	if err != nil {
		fmt.Fprintf(stderr, "maketargets: %v\n", err)
		return 1
	}
	targets, err := Parse(bytes.NewReader(data))
	if err != nil {
		fmt.Fprintf(stderr, "maketargets: %s: %v\n", file, err)
		return 1
	}
	if len(targets) == 0 {
		fmt.Fprintf(stderr, "maketargets: %s: no documented targets (add \"## help\" comments to rules)\n", file)
		return 1
	}
	Render(stdout, targets)
	return 0
}
