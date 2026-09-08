// Package docsref checks that the documents a reviewer relies on cite tests
// that exist.
//
// F-18 was a readiness-report sentence that got four adversarial items wrong in
// both directions, and F-14 was a claim about a test run that never happened.
// Both are the same failure: a document asserting something about the test
// suite, written by recalling what had been built rather than by checking.
//
// `REQUIREMENTS_TRACEABILITY.md` carries a line saying "all 609 Go test-function
// references resolve to a func Test/func Fuzz that exists", verified by hand on
// one afternoon. A hand-verified claim about 609 things is a claim that will be
// false within a week and nobody will know which week.
//
// So it is checked here instead. This is the executable version of a promise
// the documents were already making.
//
// # What is in scope
//
// The documents whose accuracy is load-bearing for a readiness decision. Not
// the whole docs tree: THREAT_MODEL.md and SECURITY.md legitimately QUOTE the
// names of tests they are asserting do NOT exist, and a check that could not
// tell an assertion from a quotation would force those documents to lie.
// Widening the scope is a documentation project; this is an audit control.
//
// # Naming a test that does not exist
//
// A findings register has to be able to do exactly that -- F-25 is a finding
// ABOUT a citation that pointed at nothing, and it has to say which. So a name
// is exempt when the paragraph it appears in says, in words, that the thing is
// missing. That is not a loophole: it is the rule that a document may name an
// absent test only while stating the absence, which is the property worth
// having. A silent citation of a fiction is what this test exists to catch.
package docsref

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// inScope are the documents this test holds to the standard. Each is one a
// reviewer would use to decide whether the system is ready.
var inScope = []string{
	"docs/release/PRODUCTION_READINESS_REPORT.md",
	// The root copy is a different document against a different goal file, and
	// MASTER_BUILD_STATE cites it for stopping-criterion 13 -- which made it the
	// one readiness report a reviewer relies on whose citations had never been
	// machine-checked (F-57).
	"docs/PRODUCTION_READINESS_REPORT.md",
	"docs/build/REQUIREMENTS_TRACEABILITY.md",
	"docs/audit/AUDIT_FINDINGS.md",
	"docs/audit/INDEPENDENT_AUDIT.md",
	"docs/build/MASTER_BUILD_STATE.md",
	"docs/build/ADVERSARIAL_VALIDATION.md",
}

var (
	// declPattern finds a test or fuzz declaration in Go source. It is a
	// regexp over the file rather than a parse, because these files carry
	// several build tags and a parse would have to pick one -- which would
	// make a test invisible to this check for the reason that it is
	// integration-tagged.
	declPattern = regexp.MustCompile(`(?m)^func\s+((?:Test|Fuzz)[A-Za-z0-9_]*)\s*\(`)
	// refPattern finds a name a document cites. The four-character floor keeps
	// the word "Test" itself out.
	refPattern = regexp.MustCompile(`\b((?:Test|Fuzz)[A-Z][A-Za-z0-9_]{3,})\b`)
)

func TestDocs_EveryTestTheyNameExists(t *testing.T) {
	root := repoRoot(t)
	declared := declaredTests(t, root)
	require(t, len(declared) > 500, "expected to find the repository's tests, found %d", len(declared))

	var problems []string
	for _, rel := range inScope {
		path := filepath.Join(root, filepath.FromSlash(rel))
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, para := range paragraphs(string(body)) {
			declaresAbsence := absencePhrase.MatchString(para)
			for _, name := range distinct(refPattern.FindAllStringSubmatch(para, -1)) {
				if satisfied(name, declared) || declaresAbsence {
					continue
				}
				problems = append(problems, rel+" names "+name+", which is not declared anywhere")
			}
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("a document a reviewer relies on cites %d test(s) that do not exist:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// absencePhrase matches a paragraph that states, in words, that what it names
// is missing. Kept deliberately small: each phrase is one a person would write
// on purpose, not one that appears by accident near a test name.
var absencePhrase = regexp.MustCompile(`(?i)does not exist|do not exist|no such function|lists as absent|is absent|are absent`)

// paragraphs splits markdown on blank lines. A markdown table row is its own
// line but not its own paragraph, which is what makes a table cell able to say
// "which does not exist" about the name in the same cell.
func paragraphs(body string) []string {
	return blankLine.Split(body, -1)
}

var blankLine = regexp.MustCompile("\n[ \t]*\n")

// satisfied reports whether a cited name resolves.
//
// A name ending in an underscore is a FAMILY: prose that says "the TestProp_
// suite" is naming a group, not claiming one function. A name with an exact
// declaration resolves, and so does one that is the prefix of a declaration at
// an underscore boundary -- "TestIDOR" for TestIDOR_CoversEveryAccountScopedRoute.
// Anything else is a claim about a specific function, and that function has to
// be there.
func satisfied(name string, declared map[string]bool) bool {
	if declared[name] {
		return true
	}
	prefix := name
	if !strings.HasSuffix(prefix, "_") {
		prefix += "_"
	}
	for d := range declared {
		if strings.HasPrefix(d, prefix) {
			return true
		}
	}
	return false
}

func declaredTests(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	skip := map[string]bool{".git": true, "node_modules": true, "bin": true, ".claude": true}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range declPattern.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}
	return out
}

func distinct(matches [][]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// repoRoot walks up to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}

func require(t *testing.T, cond bool, format string, args ...any) {
	t.Helper()
	if !cond {
		t.Fatalf(format, args...)
	}
}
