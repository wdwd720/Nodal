//go:build integration

package assets

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/valuedomain"
)

// An asset's kind and its value domain, compared across the two languages that
// decide them.
//
// `valueDomainProblems` carries this comment:
//
//	It mirrors the assets_kind_domain_agree constraint added by migration
//	00710; a test asserts the two agree for every kind.
//
// No such test existed. The two rules happened to agree, so the sentence was a
// false claim about a control rather than a divergence — the F-14/F-18/F-25/F-40
// shape, in the place a reader is most likely to take on trust: directly above
// the function it describes. F-43 and F-49 are what that shape looks like when
// the two copies actually differ.
//
// This drives every (kind, domain) pair, including the empty domain, and asks
// both sides. It is exhaustive rather than sampled: there are six kinds and nine
// domains plus the empty one, which is sixty pairs, and there is no reason to
// guess at a subset.

// TestIntegration_GoAndSQLAgreeOnEveryKindDomainPair.
func TestIntegration_GoAndSQLAgreeOnEveryKindDomainPair(t *testing.T) {
	requireEnv(t)

	domains := append([]valuedomain.Domain{""}, valuedomain.AllDomains()...)
	kinds := AllKinds()
	require.NotEmpty(t, kinds, "no asset kinds declared; this test would compare nothing")

	kindDomainExpr := checkExpression(t, "assets_kind_domain_agree")
	presenceExpr := checkExpression(t, "assets_value_domain_presence")

	var disagreements []string
	accepted := 0
	for _, k := range kinds {
		for _, d := range domains {
			goAccepts := len(Asset{Kind: k, ValueDomain: d}.valueDomainProblems()) == 0

			// The database's answer, asked of the constraint expressions
			// themselves rather than by inserting a row: an INSERT would also
			// have to satisfy the chain, mint, decimals and risk-class rules,
			// and this is a question about two constraints.
			//
			// BOTH of them, and coalesced the way a CHECK behaves: a
			// constraint fails only on FALSE, so a NULL expression is an
			// acceptance. That is not a detail -- it is F-50. The first
			// constraint alone evaluates to NULL for every non-FIAT kind with
			// no domain, and accepted every one of them.
			//
			// The expressions are read from the live catalogue, not copied
			// here. A copy went stale the day migration 00731 added the
			// SIMULATED kind: the constraint changed and the copy did not, so
			// the test reported a disagreement that only existed in itself.
			var sqlAccepts bool
			require.NoError(t, testDB.QueryRow(t.Context(),
				`SELECT coalesce(`+kindDomainExpr+`, true) AND coalesce(`+presenceExpr+`, true)
				   FROM (SELECT $1::text AS kind, $2::text AS value_domain) AS assets`,
				string(k), nullIfEmpty(d)).Scan(&sqlAccepts))

			if goAccepts {
				accepted++
			}
			if goAccepts != sqlAccepts {
				disagreements = append(disagreements,
					string(k)+" + "+domainName(d)+": Go "+verdict(goAccepts)+", SQL "+verdict(sqlAccepts))
			}
		}
	}
	sort.Strings(disagreements)
	assert.Empty(t, disagreements,
		"%d (kind, domain) pair(s) are judged differently by internal/assets and by "+
			"assets_kind_domain_agree; the constraint is what actually holds, so a disagreement means "+
			"the service accepts what the database refuses or explains a refusal it did not cause:\n  %v",
		len(disagreements), disagreements)

	// Negative controls: a rule that accepted everything or nothing would agree
	// with any other such rule.
	assert.Positive(t, accepted, "no pair is acceptable; the comparison is vacuous")
	assert.Less(t, accepted, len(kinds)*len(domains), "every pair is acceptable; there is no rule to mirror")
}

// checkExpression returns the boolean expression of a CHECK constraint on
// assets exactly as the database stores it, without the CHECK keyword.
func checkExpression(t *testing.T, name string) string {
	t.Helper()
	var def string
	require.NoError(t, testDB.QueryRow(t.Context(),
		`SELECT pg_get_constraintdef(c.oid)
		   FROM pg_constraint c
		   JOIN pg_class r ON r.oid = c.conrelid
		  WHERE r.relname = 'assets' AND c.conname = $1 AND c.contype = 'c'`, name).Scan(&def),
		"constraint %s is missing from assets", name)
	expr, ok := strings.CutPrefix(def, "CHECK ")
	require.True(t, ok, "unexpected constraint definition for %s: %s", name, def)
	return expr
}

// nullIfEmpty renders the absent domain as SQL NULL, which is what a FIAT row
// carries and what the constraint tests for.
func nullIfEmpty(d valuedomain.Domain) any {
	if d == "" {
		return nil
	}
	return string(d)
}

func domainName(d valuedomain.Domain) string {
	if d == "" {
		return "(no domain)"
	}
	return string(d)
}

func verdict(ok bool) string {
	if ok {
		return "accepts"
	}
	return "refuses"
}
