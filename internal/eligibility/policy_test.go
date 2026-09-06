package eligibility

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

func fixtureRules(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/policies/" + name + ".json")
	require.NoError(t, err)
	var pf policyFixture
	require.NoError(t, json.Unmarshal(raw, &pf))
	return pf.Rules
}

func mutateRules(t *testing.T, rules json.RawMessage, path []string, value any) json.RawMessage {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(rules, &doc))
	cur := doc
	for _, k := range path[:len(path)-1] {
		next, ok := cur[k].(map[string]any)
		require.True(t, ok, "rules path %v: %q is not an object", path, k)
		cur = next
	}
	cur[path[len(path)-1]] = value
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	return out
}

func TestParsePolicy_AcceptsFixtureAndDefault(t *testing.T) {
	p, err := ParsePolicy(fixtureRules(t, "us_baseline"))
	require.NoError(t, err)
	assert.Equal(t, []string{"CA", "KR", "US"}, p.AllowedCountries, "lists are normalised to sorted order")
	assert.True(t, p.Missing(), "ParsePolicy never invents a version")

	d := MustParsePolicy([]byte(DefaultPolicyJSON))
	assert.Empty(t, d.AllowedCountries, "the shipped default allows no country")
	assert.Equal(t, []string{"ACTIVE"}, d.Instruments.Statuses)
	assert.Len(t, d.Contexts, 5)
}

func TestParsePolicy_RejectsUnknownKeys(t *testing.T) {
	rules := fixtureRules(t, "us_baseline")
	cases := map[string]json.RawMessage{
		"top level":   mutateRules(t, rules, []string{"allow_everyone"}, true),
		"context":     mutateRules(t, rules, []string{"contexts", "TRADE", "max_notional"}, 5),
		"provider":    mutateRules(t, rules, []string{"providers", "STRIPE", "enabled"}, true),
		"instruments": mutateRules(t, rules, []string{"instruments", "leverage"}, 2),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePolicy(raw)
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			assert.Contains(t, err.Error(), "unknown field")
		})
	}
}

func TestParsePolicy_RejectsFractionalIntegers(t *testing.T) {
	rules := fixtureRules(t, "us_baseline")
	for name, raw := range map[string]json.RawMessage{
		"minimum_age 18.5":      mutateRules(t, rules, []string{"minimum_age"}, json.Number("18.5")),
		"minimum_age 1e1":       mutateRules(t, rules, []string{"minimum_age"}, json.Number("1e1")),
		"by_country 19.0":       mutateRules(t, rules, []string{"minimum_age_by_country", "KR"}, json.Number("19.0")),
		"attests as string":     mutateRules(t, rules, []string{"age_verification_attests"}, "18"),
		"minimum_age negative":  mutateRules(t, rules, []string{"minimum_age"}, -1),
		"minimum_age huge":      mutateRules(t, rules, []string{"minimum_age"}, 999),
		"country lower-case":    mutateRules(t, rules, []string{"allowed_countries"}, []string{"us"}),
		"unknown identity":      mutateRules(t, rules, []string{"identity_states"}, []string{"VERIFIED", "TRUSTED"}),
		"unknown sanctions":     mutateRules(t, rules, []string{"sanctions_states"}, []string{"FINE"}),
		"unknown account state": mutateRules(t, rules, []string{"account_statuses"}, []string{"OPEN"}),
		"unknown context":       mutateRules(t, rules, []string{"contexts", "LENDING"}, map[string]any{}),
		"unknown risk class":    mutateRules(t, rules, []string{"instruments", "risk_classes_by_country", "*"}, []string{"MEME"}),
		"unknown status":        mutateRules(t, rules, []string{"instruments", "statuses"}, []string{"PAUSED"}),
		"bad region":            mutateRules(t, rules, []string{"blocked_regions", "US"}, []string{"ny"}),
		"provider bad context":  mutateRules(t, rules, []string{"providers", "STRIPE", "contexts"}, []string{"LENDING"}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePolicy(raw)
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

func TestParsePolicy_RejectsNonObjectsAndTrailingData(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":    "",
		"null":     "null",
		"array":    "[]",
		"string":   `"policy"`,
		"trailing": `{"allowed_countries": []} {}`,
		"garbage":  `{"allowed_countries": [`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePolicy(json.RawMessage(raw))
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

func TestPolicy_HashIsCanonical(t *testing.T) {
	a, err := ParsePolicy(fixtureRules(t, "us_baseline"))
	require.NoError(t, err)
	reordered := mutateRules(t, fixtureRules(t, "us_baseline"), []string{"allowed_countries"}, []string{"US", "KR", "CA", "US"})
	b, err := ParsePolicy(reordered)
	require.NoError(t, err)
	assert.Equal(t, a.Hash(), b.Hash(), "list order and duplicates do not change the hash")

	c, err := ParsePolicy(mutateRules(t, fixtureRules(t, "us_baseline"), []string{"minimum_age"}, 21))
	require.NoError(t, err)
	assert.NotEqual(t, a.Hash(), c.Hash())

	a.Version = "v1"
	b.Version = "v2"
	assert.Equal(t, a.Hash(), b.Hash(), "version is not part of the rules hash")
	assert.Len(t, a.Hash(), 64)
}

func TestReasonCodes_SortedAndUnique(t *testing.T) {
	codes := ReasonCodes()
	seen := map[string]bool{}
	for i, c := range codes {
		assert.True(t, strings.HasPrefix(c, "ELIGIBILITY_"), c)
		assert.False(t, seen[c], "duplicate %s", c)
		seen[c] = true
		if i > 0 {
			assert.Less(t, codes[i-1], c)
		}
	}
	assert.Contains(t, codes, ReasonPolicyMissing)
	assert.Contains(t, codes, ReasonJurisdictionUnknown)
}

// TestNoFloatingPointInSource enforces PART 17 mechanically for this package.
func TestNoFloatingPointInSource(t *testing.T) {
	banned := []string{"float32", "float64", "big.Float", "big.Rat", "ParseFloat", "FormatFloat", "math/rand", "time.Now"}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		checked++
		for _, tok := range banned {
			assert.NotContains(t, string(src), tok, "%s must not mention %q", name, tok)
		}
	}
	require.GreaterOrEqual(t, checked, 4)
}
