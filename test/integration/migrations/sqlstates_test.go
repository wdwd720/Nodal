//go:build integration

package migrations_test

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/migrations"
)

// Every custom SQLSTATE a migration documents is a SQLSTATE something raises.
//
// The migrations declare their error codes in a header comment:
//
//	-- Custom SQLSTATEs: PO001 reservation invariant, PO002 illegal state
//	-- transition, PO003 provenance mismatch, PO004 amount mismatch.
//
// Four of them were raised by nothing at all: CR002, CR005, PO002, PO004. Each
// labelled an invariant that a *different* mechanism enforces under a different
// code, so the property held and the documentation was fiction (F-58). Nothing
// checked, so nothing said.
//
// The failure mode is small but it is the one that compounds: an application
// cannot tell these refusals apart from any other unique or check violation, and
// a header that has been wrong for a year is a header nobody reads. It also
// recurs -- migration 00720, written in the same session as this test,
// documented a `PL002` that turned out to be a CHECK raising 23514.
//
// A code is satisfied by being raised, or by being withdrawn in a later
// migration's `-- Withdrawn SQLSTATEs:` line. Withdrawal is not an exemption
// list: 00722 records what actually enforces each withdrawn invariant on the
// schema object itself, and
// TestIntegration_TheWithdrawnInvariantsAreStillEnforced drives all four and
// watches the refusals happen. A withdrawal that was really an abandonment
// fails that test, not this one.

var (
	documentedRe = regexp.MustCompile(`(?m)^--\s*Custom SQLSTATEs[^\n]*(?:\n--[^\n]*)*`)
	// The withdrawal marker is ONE line, unlike the documentation block, which
	// wraps. A multi-line match here swallowed the paragraph explaining what
	// enforces each withdrawn invariant and read every code named in it -- AU001
	// among them -- as itself withdrawn. The first version of this test said
	// 00722 withdrew AU001, which is the opposite of what 00722 says.
	// The withdrawal marker is ONE line, unlike the documentation block, which
	// wraps. A multi-line match swallowed the paragraph explaining what enforces
	// each withdrawn invariant and read every code named there -- AU001 among
	// them -- as itself withdrawn, so the first version of this test reported
	// that 00722 withdrew AU001, which is the opposite of what 00722 says.
	withdrawnRe = regexp.MustCompile(`(?m)^--\s*Withdrawn SQLSTATEs[^\n]*`)
	codeRe      = regexp.MustCompile(`\b([A-Z]{2}[0-9]{3})\b`)
	raiseRe     = regexp.MustCompile(`ERRCODE\s*=\s*'([A-Z0-9]{5})'`)
)

func TestIntegration_EveryDocumentedSQLStateIsRaised(t *testing.T) {
	entries, err := migrations.FS.ReadDir(".")
	require.NoError(t, err)

	documented := map[string]string{} // code -> the migration that documents it
	withdrawn := map[string]string{}
	raised := map[string]string{}

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, rerr := migrations.FS.ReadFile(e.Name())
		require.NoError(t, rerr)
		text := string(body)

		for _, block := range documentedRe.FindAllString(text, -1) {
			for _, m := range codeRe.FindAllStringSubmatch(block, -1) {
				if _, seen := documented[m[1]]; !seen {
					documented[m[1]] = e.Name()
				}
			}
		}
		for _, block := range withdrawnRe.FindAllString(text, -1) {
			for _, m := range codeRe.FindAllStringSubmatch(block, -1) {
				withdrawn[m[1]] = e.Name()
			}
		}
		for _, m := range raiseRe.FindAllStringSubmatch(text, -1) {
			if _, seen := raised[m[1]]; !seen {
				raised[m[1]] = e.Name()
			}
		}
	}

	// The positive signal. A regex that stopped matching would leave every map
	// empty and this test agreeing with anything (F-32, F-46).
	require.Greater(t, len(documented), 20, "found only %d documented codes; the header scan is broken, not the tree", len(documented))
	require.Greater(t, len(raised), 20, "found only %d raise sites; the ERRCODE scan is broken", len(raised))

	var unraised []string
	for code, where := range documented {
		if _, ok := raised[code]; ok {
			continue
		}
		if w, ok := withdrawn[code]; ok {
			t.Logf("%s (documented in %s) is withdrawn by %s", code, where, w)
			continue
		}
		unraised = append(unraised, code+" (documented in "+where+")")
	}
	sort.Strings(unraised)
	assert.Empty(t, unraised,
		"these SQLSTATEs are documented in a migration header and raised by nothing.\n"+
			"Either raise them, or withdraw them in a later migration with a\n"+
			"`-- Withdrawn SQLSTATEs:` line naming what enforces the invariant instead:\n  %s",
		strings.Join(unraised, "\n  "))

	// A withdrawal must be of something that was documented. Withdrawing a code
	// nobody declared is a typo, and a typo here silently excuses the code it
	// was meant to name.
	for code, where := range withdrawn {
		_, ok := documented[code]
		assert.True(t, ok, "%s withdraws %s, which no migration header documents", where, code)
	}
}
