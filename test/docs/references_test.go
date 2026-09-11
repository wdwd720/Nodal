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
	// The two a reviewer would use to score SECURITY posture, and the two that
	// nothing checked. Empirically they were also the two that decayed furthest
	// -- both understating the system, including a top-ten residual risk that
	// migration 00701 had closed, and a whole section of DESIGNED tags naming
	// packages that exist. An overstatement and an understatement are the same
	// defect when the document's purpose is to be accurate (F-111).
	"docs/security/SECURITY.md",
	"docs/threat-model/THREAT_MODEL.md",
	// The register of WHY every control is shaped the way it is -- the document
	// a fixer opens before changing one. Its citations had never been resolved
	// by anything, and one of them (D-024's "Test change") named a function that
	// has never existed under that name: the F-25 shape surviving four audits by
	// living one document outside this list (F-248).
	"docs/build/DECISION_REGISTER.md",
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

// pathScope is inScope plus every ADR, for the citation check below.
//
// An ADR's Evidence section is the list a reviewer opens to check the decision
// against the code, and ADR-0023's named `test/integration/gates` -- a directory
// that has never existed, for four properties a reader most needs proven. The
// check next to this one never saw it, because it resolves Go test-function
// NAMES and that citation is a path (F-165).
func pathScope(t *testing.T, root string) []string {
	t.Helper()
	out := append([]string(nil), inScope...)
	adrs, err := filepath.Glob(filepath.Join(root, "docs", "adr", "*.md"))
	require(t, err == nil, "globbing docs/adr: %v", err)
	require(t, len(adrs) > 10, "expected the ADR tree, found %d files", len(adrs))
	for _, a := range adrs {
		rel, rerr := filepath.Rel(root, a)
		require(t, rerr == nil, "relative path for %s: %v", a, rerr)
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

var (
	// pathRef finds a backticked repository path. Only citations that name a Go
	// TEST file or a directory under test/ are held to this check: those are the
	// ones a reader follows to see a property proven, and they are cheap to
	// resolve exactly. A partial path (`stripecredit/wire.go`), an import path
	// (`coreos/go-oidc`) and a package selector (`internal/gates.Checker`) are
	// deliberately outside it -- a check that guessed at those would produce
	// noise, and noise is how a control stops being read.
	pathRef      = regexp.MustCompile("`([A-Za-z0-9_][A-Za-z0-9_./-]*/[A-Za-z0-9_./-]*)`")
	testFilePath = regexp.MustCompile(`_test\.go$`)
	// plannedPhrase is the absence vocabulary this check adds. SECURITY.md's
	// PART 155 matrix has a column headed "Planned (traceability)" listing the
	// adversarial suites that are not written yet, by the path they will take.
	// That is a document stating an absence, which is exactly what it should do,
	// and a check that could not tell a plan from a claim would force it to stop
	// saying so.
	plannedPhrase = regexp.MustCompile(`(?i)planned|not yet written`)
)

// TestDocs_EveryPathTheyNameExists resolves the path-shaped test citations.
//
// Same rule as the function-name check, same exemption for a paragraph that
// states the thing is missing: a document may name an absent test only while
// saying it is absent.
func TestDocs_EveryPathTheyNameExists(t *testing.T) {
	root := repoRoot(t)
	var problems []string
	cited := 0
	for _, rel := range pathScope(t, root) {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, para := range paragraphs(string(body)) {
			excused := absencePhrase.MatchString(para) || plannedPhrase.MatchString(para)
			for _, p := range distinct(pathRef.FindAllStringSubmatch(para, -1)) {
				p = strings.TrimSuffix(p, "/")
				if !isTestCitation(p) {
					continue
				}
				cited++
				if excused {
					continue
				}
				if _, serr := os.Stat(filepath.Join(root, filepath.FromSlash(p))); serr != nil {
					problems = append(problems, rel+" names "+p+", which is not in the repository")
				}
			}
		}
	}
	require(t, cited > 10, "only %d path-shaped test citations found; the check stopped seeing them", cited)
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("a document a reviewer relies on cites %d test path(s) that do not exist:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// isTestCitation reports whether a cited path is one this check resolves: a Go
// test file anywhere, or a path under test/ that is not a file of another kind.
func isTestCitation(p string) bool {
	if testFilePath.MatchString(p) {
		return true
	}
	return strings.HasPrefix(p, "test/") && !strings.Contains(filepath.Base(p), ".")
}

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
