//go:build integration

package assets

import (
	"sort"
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
			var sqlAccepts bool
			require.NoError(t, testDB.QueryRow(t.Context(),
				`SELECT coalesce(
				          ($1 = 'CREDIT' AND $2 = 'INTERNAL_CREDIT')
				       OR ($1 = 'NATIVE_ASSET' AND $2 = 'INTERNAL_NATIVE_ASSET')
				       OR ($1 = 'FIAT' AND $2 IS NULL)
				       OR ($1 IN ('NATIVE','SPL_TOKEN','SPL_TOKEN_2022')
				           AND $2 IN ('SELF_CUSTODIAL_CRYPTO','HOSTED_CRYPTO','SIMULATED')), true)
				   AND coalesce(($1 = 'FIAT') = ($2 IS NULL), true)`,
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
