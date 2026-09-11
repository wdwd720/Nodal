package docsref

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/security"
)

// Numbers in the documents, checked against the code that produces them.
//
// The file next to this one checks one class of claim: that a cited test
// exists. Nothing checked the counts, and they drifted. The build state said
// "capabilities: 20, of which 9 are high-risk" while `gates.IsHighRisk` returned
// true for eighteen, and had done since F-16 moved MARKETPLACE and the rest of
// the internal economy across. It said 59 permissions when there were 60. The
// backup runbook cited a restore drill that had reached schema version 604,
// which was a hundred and eleven migrations ago.
//
// Each of those was written accurately and then went quietly false, which is
// F-14 and F-18's failure in its slowest form: a document that was true when
// somebody looked. The fix is the same one this package already applies to test
// citations -- derive it, do not recall it.
//
// # The migration version is a staleness check, not a typo check
//
// `BACKUP_RESTORE.md` reports the version the last local drill reached. Held
// against the newest migration in the tree, it fails whenever migrations land
// without the drill being re-run -- which is the point. A restore drill that
// last ran three schema changes ago has not exercised the schema anybody would
// actually be restoring.
//
// # What is deliberately not here
//
// The browser suite's size. Deriving it means expanding the Playwright loops,
// and a derivation that is itself wrong, asserting confidently against a number
// that is right, is worse than no check at all. `npx playwright test --list`
// prints the total and is the authority.

type countClaim struct {
	doc     string
	pattern *regexp.Regexp
	want    func(root string) int
	what    string
}

func countClaims() []countClaim {
	return []countClaim{
		{
			doc:     "docs/build/MASTER_BUILD_STATE.md",
			pattern: regexp.MustCompile(`capabilities: (\d+), of which \d+ are high-risk`),
			want:    func(string) int { return len(gates.AllCapabilities()) },
			what:    "declared capabilities",
		},
		{
			doc:     "docs/build/MASTER_BUILD_STATE.md",
			pattern: regexp.MustCompile(`capabilities: \d+, of which (\d+) are high-risk`),
			want:    func(string) int { return highRiskCapabilities() },
			what:    "high-risk capabilities",
		},
		{
			doc:     "docs/build/MASTER_BUILD_STATE.md",
			pattern: regexp.MustCompile(`(?m)^(\d+), up from 42 at the baseline`),
			want:    func(string) int { return len(security.AllPermissions()) },
			what:    "declared permissions",
		},
		{
			// Section 10 listed ten of twenty capabilities for a long time,
			// omitting the entire internal economy including CREDIT_PURCHASE --
			// while section 3 of the same file said "capabilities: 20" and was
			// machine-checked and passing. The document contradicted its own
			// verified number 1,570 lines later, and nothing noticed because
			// only one of the two was derived (F-111).
			doc:     "docs/build/MASTER_BUILD_STATE.md",
			pattern: regexp.MustCompile(`\*\*This table lists (\d+) capabilities\.\*\*`),
			want:    func(string) int { return len(gates.AllCapabilities()) },
			what:    "capabilities in the production-capability table",
		},
		{
			doc:     "docs/operations/BACKUP_RESTORE.md",
			pattern: regexp.MustCompile(`version (\d+) on both sides`),
			want:    highestMigration,
			what:    "the schema version its last restore drill reached",
		},
	}
}

func highRiskCapabilities() int {
	n := 0
	for _, c := range gates.AllCapabilities() {
		if gates.IsHighRisk(c) {
			n++
		}
	}
	return n
}

// highestMigration is the newest migration in the tree, which is the version a
// freshly migrated database reports.
func highestMigration(root string) int {
	entries, err := os.ReadDir(filepath.Join(root, "migrations"))
	if err != nil {
		return -1
	}
	num := regexp.MustCompile(`^(\d+)_`)
	highest := 0
	for _, e := range entries {
		m := num.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if v, cerr := strconv.Atoi(m[1]); cerr == nil && v > highest {
			highest = v
		}
	}
	return highest
}

func TestDocs_CountsMatchTheCode(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, c := range countClaims() {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(c.doc)))
		require(t, err == nil, "reading %s: %v", c.doc, err)

		m := c.pattern.FindSubmatch(body)
		require(t, m != nil,
			"%s no longer contains the sentence this check reads (/%s/); a claim that "+
				"moved is a claim nobody is checking any more", c.doc, c.pattern)

		claimed, convErr := strconv.Atoi(string(m[1]))
		require(t, convErr == nil, "%s: %q is not a number", c.doc, m[1])

		if want := c.want(root); claimed != want {
			t.Errorf("%s says %d %s; the code says %d", c.doc, claimed, c.what, want)
		}
	}
}

// TestDocs_CountsCheckIsNotVacuous is the positive signal this check needs
// before its agreements mean anything (F-32, F-46). Every derivation must
// produce a real number, and they must not all produce the same one -- a set of
// `want` functions that had quietly collapsed to zero, or to each other, would
// agree with a document that had also gone wrong and report nothing.
func TestDocs_CountsCheckIsNotVacuous(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	seen := map[int]bool{}
	for _, c := range countClaims() {
		got := c.want(root)
		if got <= 0 {
			t.Errorf("%s derives %s as %d, which cannot be right", c.doc, c.what, got)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Errorf("every derivation returns the same number (%v); they are not measuring different things", seen)
	}
}

// The traceability summary is derived from its own rows (F-111).
//
// The table has now been wrong twice in the same way. Its own change log
// records the first: "the summary table previously said 103/138; the rows
// actually said 102/139". The second was IN_PROGRESS 59 against 60 rows and
// VERIFIED 226 against 225, left behind when F-65 correctly lowered R-053-12
// and nobody recomputed the tally.
//
// That matters more than an off-by-one, because MASTER_BUILD_STATE's stopping
// condition 12 -- "documentation reflects reality" -- cites these counts as its
// evidence. A tally recomputed by hand drifts from the thing it counts, and the
// document whose whole purpose is to be accurate is the worst place for that.
func TestDocs_TraceabilitySummaryMatchesItsRows(t *testing.T) {
	t.Parallel()
	const path = "docs/build/REQUIREMENTS_TRACEABILITY.md"
	body, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(path)))
	require(t, err == nil, "reading %s: %v", path, err)
	lines := strings.Split(string(body), "\n")

	states := []string{
		"NOT_STARTED", "IN_PROGRESS", "IMPLEMENTED",
		"VERIFIED", "BLOCKED_EXTERNAL", "DEFERRED_OUT_OF_SCOPE",
	}
	counted := map[string]int{}
	rows := 0
	for _, line := range lines {
		if !strings.HasPrefix(line, "| R-") {
			continue
		}
		rows++
		cells := strings.Split(line, "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		matched := ""
		for _, st := range states {
			if slices.Contains(cells, st) {
				matched = st
				break
			}
		}
		require(t, matched != "", "%s: a row has no recognised state: %s", path, line)
		counted[matched]++
	}
	require(t, rows > 0, "%s: no requirement rows found; this test is looking in the wrong place", path)

	// The declared table, read back out of the document.
	declared := map[string]int{}
	declaredRows := 0
	for _, line := range lines {
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			continue
		}
		name, value := strings.TrimSpace(cells[1]), strings.TrimSpace(cells[2])
		n, convErr := strconv.Atoi(value)
		if convErr != nil {
			continue
		}
		switch {
		case slices.Contains(states, name):
			declared[name] = n
		case name == "**Total rows**":
			declaredRows = n
		}
	}

	if declaredRows != rows {
		t.Errorf("%s: the summary says %d rows in total; there are %d", path, declaredRows, rows)
	}
	for _, st := range states {
		if declared[st] != counted[st] {
			t.Errorf("%s: the summary says %d rows are %s; %d are", path, declared[st], st, counted[st])
		}
	}
}
