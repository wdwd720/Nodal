package security

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The boundary PROVIDER_BOUNDARY.md §2 rule 1 promises, enforced structurally
// rather than by convention:
//
//	Verification never mutates Credits. A Role B decision changes the
//	customer's financial profile; it does not touch a lot, a balance or a
//	value domain. There is no `UPDATE credits SET redeemable = true`
//	anywhere, and `test/security` will assert that no writer of the Credit
//	tables reads the verification tables.
//
// This is that assertion. It is an import-graph rule for the same reason
// TestAgentTreesNeverImportAuthority is: a comment saying "we do not do that"
// is true until somebody writes the line, and an import rule is true until
// somebody deletes the test.
//
// The direction matters in both senses, so both are checked. Verification
// cannot reach the ledger, so it cannot mint; and the Credit writer cannot
// reach verification, so it cannot BRANCH on a verification state — which is
// the subtler half. A `if verified { origin = PURCHASED }` in the Credit
// service would satisfy "verification does not write Credits" and still be the
// thing the boundary exists to prevent.

// creditWriters are the packages that write the Credit tables: lots, lot
// events and the journal rows behind them.
var creditWriters = []string{"internal/credit", "internal/ledger"}

// verificationTrees are the packages that decide and record who somebody is.
var verificationTrees = []string{
	"internal/verification", "internal/verification/rules",
	"internal/compliance", "internal/provider/verifysandbox",
}

// TestVerificationNeverReachesTheCreditLedger: a verification decision changes
// what a person may ASK for, and touches no lot, balance or journal row.
func TestVerificationNeverReachesTheCreditLedger(t *testing.T) {
	root := repoRoot(t)
	var problems []string
	for _, tree := range verificationTrees {
		for file, imports := range importsOf(t, root, tree) {
			if strings.HasSuffix(file, "_test.go") {
				// A test may count the Credit tables to PROVE the boundary
				// holds, which is the opposite of breaking it.
				continue
			}
			for _, imp := range imports {
				for _, writer := range creditWriters {
					if imp == writer || strings.HasPrefix(imp, writer+"/") {
						problems = append(problems, file+" imports "+imp)
					}
				}
			}
		}
	}
	assert.Empty(t, problems,
		"a verification package reached the Credit ledger. PROVIDER_BOUNDARY §2: a Role B decision "+
			"changes the customer's financial profile; it does not touch a lot, a balance or a value domain:\n  %s",
		strings.Join(problems, "\n  "))
}

// TestTheCreditWriterNeverReadsAVerificationState is the other direction, and
// the subtler one. A Credit writer that could read a verification state could
// branch on it — minting a different provenance for a verified person, say —
// which satisfies "verification does not write Credits" and is exactly what
// the boundary exists to prevent.
//
// What decides which Credits may LEAVE is `valuedomain.Policy`, evaluated by
// `internal/payout` with the verification level handed to it as an input. That
// is the one place the two facts meet, and it is a pure function of values
// somebody else resolved.
func TestTheCreditWriterNeverReadsAVerificationState(t *testing.T) {
	root := repoRoot(t)
	var problems []string
	for _, writer := range creditWriters {
		for file, imports := range importsOf(t, root, writer) {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			for _, imp := range imports {
				for _, tree := range verificationTrees {
					if imp == tree || strings.HasPrefix(imp, tree+"/") {
						problems = append(problems, file+" imports "+imp)
					}
				}
			}
		}
	}
	assert.Empty(t, problems,
		"a Credit writer reached a verification package. It could then branch on whether somebody is "+
			"verified while minting or moving Credits, which is what PROVIDER_BOUNDARY §2 forbids:\n  %s",
		strings.Join(problems, "\n  "))
}

// TestTheValueBoundaryCheckIsNotVacuous is the positive signal the two rules
// above need before their agreement means anything (F-32, F-46). If
// `importsOf` stopped finding anything — a moved tree, a renamed package — both
// would pass on an empty set and report nothing.
func TestTheValueBoundaryCheckIsNotVacuous(t *testing.T) {
	root := repoRoot(t)
	for _, tree := range []string{
		"internal/verification", "internal/compliance", "internal/provider/verifysandbox",
		"internal/credit", "internal/ledger",
	} {
		files := importsOf(t, root, tree)
		assert.NotEmptyf(t, files, "%s has no module-internal imports at all; the scan is broken, not the tree", tree)
	}
	// internal/verification/rules is deliberately absent from that list: it
	// imports nothing module-internal at all, which is the property it is
	// supposed to have. The age and jurisdiction tables are a pure leaf that
	// reads no clock, no database and no other package, so asserting the
	// ABSENCE is the check worth making.
	assert.Empty(t, importsOf(t, root, "internal/verification/rules"),
		"the rule tables gained a module-internal dependency; they are meant to be a pure leaf")
	// And the rule has something to bite on: verification does import the
	// value-domain vocabulary, which is how it reports a level at all.
	found := false
	for _, imports := range importsOf(t, root, "internal/verification") {
		for _, imp := range imports {
			if imp == "internal/valuedomain" {
				found = true
			}
		}
	}
	assert.True(t, found,
		"internal/verification no longer imports internal/valuedomain; it can no longer report a "+
			"verification level, and these rules are checking the wrong thing")
}
