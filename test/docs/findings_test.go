package docsref

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The findings register states every finding's status twice: once in the
// summary table at the top, once in the heading of its own section. Nothing
// checked that the two agreed, and four of them did not.
//
//	F-42   table=fixed  heading=open
//	F-85   table=fixed  heading=open
//	F-100  table=fixed  heading=open
//	F-105  table=fixed  heading=part
//
// Every one was stale in the same direction: the work was done, the row was
// updated, the heading was not. So a reader who scrolled to the detail of the
// audit binding, the buffered request body, the un-parked funding or the
// unprunable security trail was told the fix had not landed — in the document
// this repository keeps to be the thing that does not overstate.
//
// This is the defect class the register names most often, turned on the
// register itself: a list duplicated in two places diverges, and the copy
// nobody greps is the one that rots. F-130.
var (
	findingRow     = regexp.MustCompile(`(?m)^\| (F-\d+) \| (P\d) \| [A-Z_]+ \| \*{0,2}(\w+)\*{0,2} \|`)
	findingHeading = regexp.MustCompile(`(?m)^## (F-\d+) .*?· (\w+)\s*$`)
	anyFindingHead = regexp.MustCompile(`(?m)^## (F-\d+) .*$`)
)

// sectionless are the findings that have a summary row and no section of their
// own. All five are BASELINE-era, all are fixed, and all predate the convention
// that a finding carries its evidence. They are listed by name rather than
// tolerated by a rule, so that a finding added tomorrow without a section fails
// this test instead of joining them silently.
//
// Reconstructing their evidence now would be archaeology rather than audit, and
// inventing it would be worse than the gap.
var sectionless = map[string]string{
	"F-05": "the 00603 state-binding could not guard a table keyed by anything but id",
	"F-06": "the 00603 state-binding could not express two transitions in one transaction",
	"F-08": "a test created a Credit asset per call where the schema permits one",
	"F-09": "internal/valuation's fixture predated required value domains",
	"F-40": "this register claimed a database guarantee the trigger it named does not make",
}

func findingsRegister(t *testing.T) string {
	t.Helper()
	const path = "docs/audit/AUDIT_FINDINGS.md"
	body, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(path)))
	require(t, err == nil, "reading %s: %v", path, err)
	return string(body)
}

// TestDocs_EveryFindingSaysTheSameThingTwice holds the summary table against
// the section headings.
func TestDocs_EveryFindingSaysTheSameThingTwice(t *testing.T) {
	t.Parallel()
	body := findingsRegister(t)

	rows := map[string]string{}
	for _, m := range findingRow.FindAllStringSubmatch(body, -1) {
		require(t, rows[m[1]] == "", "%s appears twice in the summary table", m[1])
		rows[m[1]] = strings.ToLower(m[3])
	}
	require(t, len(rows) > 100, "only %d findings parsed; the table format changed and this test stopped seeing it", len(rows))

	heads := map[string]string{}
	for _, m := range findingHeading.FindAllStringSubmatch(body, -1) {
		heads[m[1]] = strings.ToLower(m[2])
	}

	// Every section that exists must agree with its row.
	for _, id := range sortedFindings(rows) {
		head, ok := heads[id]
		if !ok {
			continue // covered by the next test
		}
		if head != rows[id] {
			t.Errorf("%s: the summary table says %q and its own section heading says %q; "+
				"one of them is stale and a reader believes whichever they reached first",
				id, rows[id], head)
		}
	}

	// And every section must belong to a finding the table lists, so a section
	// cannot outlive its row either.
	for _, m := range anyFindingHead.FindAllStringSubmatch(body, -1) {
		if _, ok := rows[m[1]]; !ok {
			t.Errorf("%s has a section but no row in the summary table", m[1])
		}
	}
}

// TestDocs_EveryFindingCarriesItsEvidence requires a section per finding, with
// the five pre-convention exceptions named rather than inferred.
func TestDocs_EveryFindingCarriesItsEvidence(t *testing.T) {
	t.Parallel()
	body := findingsRegister(t)

	rows := map[string]bool{}
	for _, m := range findingRow.FindAllStringSubmatch(body, -1) {
		rows[m[1]] = true
	}
	heads := map[string]bool{}
	for _, m := range anyFindingHead.FindAllStringSubmatch(body, -1) {
		heads[m[1]] = true
	}

	for _, id := range sortedFindings(rows) {
		if heads[id] {
			if _, excused := sectionless[id]; excused {
				t.Errorf("%s now has a section, so it should come off the sectionless list", id)
			}
			continue
		}
		if _, excused := sectionless[id]; excused {
			continue
		}
		t.Errorf("%s has a summary row and no section: a finding without its evidence is a claim, "+
			"which is what this register exists not to be", id)
	}

	// The exception list cannot name a finding that does not exist.
	for id := range sectionless {
		if !rows[id] {
			t.Errorf("sectionless names %s, which is not in the register", id)
		}
	}
}

func sortedFindings[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.TrimPrefix(out[i], "F-"))
		b, _ := strconv.Atoi(strings.TrimPrefix(out[j], "F-"))
		return a < b
	})
	return out
}
