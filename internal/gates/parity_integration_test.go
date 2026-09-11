//go:build integration

package gates

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Go and SQL must agree about which capabilities are high risk.
//
// `IsHighRisk` and `cp_gate_is_high_risk` are the same list written twice, in
// two languages, in two files. Migration 00701's header says why the second
// copy exists: it is "the line that holds when the Go check is bypassed" —
// GT003 refuses to approve a high-risk gate without four evidence references
// and three distinct principals, whatever the caller is.
//
// They diverged. F-16 moved MARKETPLACE from low to high risk in Go, because it
// gates the only legitimate way withdrawable creator-earning provenance comes
// into existence, and the SQL mirror was not updated (F-43). Eighteen
// capabilities in Go, seventeen in SQL, and the one they disagreed about was
// the one the finding was about.
//
// A list duplicated in two languages diverges the moment somebody edits one of
// them. The only defence is a test that reads both, which is what the ledger's
// value-domain matrix already does — `TestIntegration_GoAndSQLAgreeOnEveryOrderedDomainPair`
// is the same idea about a different table.

func TestIntegration_GoAndSQLAgreeOnEveryCapabilitysRisk(t *testing.T) {
	requireEnv(t)
	caps := AllCapabilities()
	require.NotEmpty(t, caps, "no capabilities declared; this test would compare two empty sets")

	var disagreements []string
	var highInGo []string
	for _, c := range caps {
		var sqlSays bool
		require.NoError(t, testDB.QueryRow(t.Context(),
			`SELECT cp_gate_is_high_risk($1)`, string(c)).Scan(&sqlSays),
			"cp_gate_is_high_risk(%q)", c)
		goSays := IsHighRisk(c)
		if goSays {
			highInGo = append(highInGo, string(c))
		}
		if goSays != sqlSays {
			disagreements = append(disagreements,
				string(c)+": Go says "+yesNo(goSays)+", SQL says "+yesNo(sqlSays))
		}
	}
	sort.Strings(disagreements)
	assert.Empty(t, disagreements,
		"%d capability(ies) are classified differently by gates.IsHighRisk and cp_gate_is_high_risk; "+
			"the SQL copy is the check that holds when the Go one is bypassed:\n  %v",
		len(disagreements), disagreements)

	// A negative control. If every capability were low risk in both, the loop
	// above would agree perfectly and prove nothing about a control that is
	// supposed to demand four evidence references from somebody.
	assert.NotEmpty(t, highInGo, "no capability is high risk anywhere; the comparison is vacuous")
}

// TestIntegration_SQLKnowsEveryCapabilityGoDeclares: the function answers for
// every declared capability rather than silently returning false for one it has
// never heard of.
//
// `cp_gate_is_high_risk` is a membership test against a literal list, so an
// unknown capability is indistinguishable from a low-risk one — fail-open in
// the direction that matters. The gate table's own CHECK constraint is what
// makes an unknown capability unstorable, so this asserts the two lists cover
// the same names.
func TestIntegration_SQLKnowsEveryCapabilityGoDeclares(t *testing.T) {
	requireEnv(t)
	for _, c := range AllCapabilities() {
		var accepted bool
		require.NoError(t, testDB.QueryRow(t.Context(),
			`SELECT EXISTS (
			   SELECT 1 FROM pg_constraint
			    WHERE conrelid = 'capability_gates'::regclass
			      AND contype = 'c'
			      AND pg_get_constraintdef(oid) LIKE '%' || $1 || '%')`, string(c)).Scan(&accepted))
		assert.True(t, accepted,
			"capability %q is declared in Go and named by no CHECK constraint on capability_gates; "+
				"a gate row for it could not be stored, so the capability is unreachable", c)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// TestIntegration_GoAndSQLAgreeOnEveryTransitionPair is the same idea about the
// other list this package keeps twice.
//
// `gates.transitions` and `cp_gate_can_transition` are the legal-transition
// table written in Go and in SQL. The SQL copy is the one that holds when the Go
// one is bypassed: cp_gate_transition and cp_gate_sandbox both re-derive the
// edge from the STORED state and refuse GT002 whatever the caller believes. A
// disagreement in either direction is a defect -- an edge Go permits and SQL
// refuses is an operation that fails at the database with an internal error, and
// an edge SQL permits and Go refuses is a hole in the control that only the Go
// check is closing.
//
// Migration 00755 added five sandbox edges to both copies by hand. Nothing
// compared them, which is exactly the shape of F-43: eighteen capabilities in
// Go, seventeen in SQL, and the one they disagreed about was the one the finding
// was about. All 8x8 ordered pairs are compared rather than the legal ones, so
// an edge ADDED to one copy is caught as well as an edge dropped.
func TestIntegration_GoAndSQLAgreeOnEveryTransitionPair(t *testing.T) {
	requireEnv(t)
	states := AllStates()
	require.Len(t, states, 8, "the state list changed; this test compares every ordered pair of it")

	var disagreements []string
	legal := 0
	for _, from := range states {
		for _, to := range states {
			var sqlSays bool
			require.NoError(t, testDB.QueryRow(t.Context(),
				`SELECT cp_gate_can_transition($1, $2)`, string(from), string(to)).Scan(&sqlSays),
				"cp_gate_can_transition(%q, %q)", from, to)
			goSays := CanTransition(from, to)
			if goSays {
				legal++
			}
			if goSays != sqlSays {
				disagreements = append(disagreements,
					string(from)+" -> "+string(to)+": Go says "+yesNo(goSays)+", SQL says "+yesNo(sqlSays))
			}
		}
	}
	sort.Strings(disagreements)
	assert.Empty(t, disagreements,
		"%d of the %d ordered state pairs are judged differently by gates.CanTransition and "+
			"cp_gate_can_transition; the SQL copy is the check that holds when the Go one is bypassed:\n  %v",
		len(disagreements), len(states)*len(states), disagreements)

	// A negative control: if both said no to everything the loop would agree
	// perfectly and prove nothing about a table that is supposed to permit a
	// ceremony.
	assert.Equal(t, 20, legal, "the transition table has 20 legal edges; it now has %d", legal)
}
