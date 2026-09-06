package risk

import (
	"bytes"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
)

const determinismRuns = 1000

// TestGolden_Risk evaluates every fixture in testdata/cases and compares the
// verdict, the full sorted reason-code set and, where the fixture says so,
// the action class, effective notional and resulting constraints.
func TestGolden_Risk(t *testing.T) {
	cases := loadCases(t)
	require.GreaterOrEqual(t, len(cases), 40, "golden corpus must hold at least 40 cases")
	known := ReasonCodes()
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			p := loadPolicyFixture(t, c.Policy)
			in := caseInput(t, c)
			d := Evaluate(p, in)

			expect := c.Expect.ReasonCodes
			if expect == nil {
				expect = []string{}
			}
			require.Equal(t, expect, d.ReasonCodes)
			require.Equal(t, c.Expect.Decision, d.Verdict)
			require.Equal(t, len(d.ReasonCodes) == 0, d.Verdict == Allow, "ALLOW iff no reason codes")
			require.True(t, sort.StringsAreSorted(d.ReasonCodes))
			for _, code := range d.ReasonCodes {
				require.Contains(t, known, code)
			}
			if c.Expect.ActionClass != "" {
				require.Equal(t, c.Expect.ActionClass, d.ActionClass)
			}
			if c.Expect.EffectiveNotionalUSD != "" {
				require.Equal(t, c.Expect.EffectiveNotionalUSD, d.EffectiveNotionalUSD.String())
			}
			if c.Expect.MaxNotionalUSD != "" {
				require.Equal(t, c.Expect.MaxNotionalUSD, d.Constraints.MaxNotionalUSD.String())
			}
			if c.Expect.MaxSlippageBPS != nil {
				require.Equal(t, money.BPS(*c.Expect.MaxSlippageBPS), d.Constraints.MaxSlippageBPS)
			}
			require.Equal(t, p.Version, d.PolicyVersion)
			require.Equal(t, EvaluatorVersion, d.EvaluatorVersion)
			require.Equal(t, in.Now.UTC(), d.EvaluatedAt)
			require.Len(t, d.Hash, 64)
			require.Len(t, d.InputHash, 64)
			require.Equal(t, d.ComputeHash(), d.Hash)
			require.NotNil(t, d.MatchedKillSwitches)
			if p.Missing() {
				require.Empty(t, d.PolicyHash)
			} else {
				require.Equal(t, p.Hash(), d.PolicyHash)
			}
			if containsCode(d.ReasonCodes, ReasonKillSwitch) {
				require.NotEmpty(t, d.MatchedKillSwitches)
			} else {
				require.Empty(t, d.MatchedKillSwitches)
			}
		})
	}
}

// TestGolden_Determinism1000 evaluates every fixture 1,000 times and requires
// a byte-identical decision (PART 224; POLICY_AUTHORITY §4 determinism test).
func TestGolden_Determinism1000(t *testing.T) {
	for _, c := range loadCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			p := loadPolicyFixture(t, c.Policy)
			in := caseInput(t, c)
			first := Evaluate(p, in)
			firstJSON, err := first.CanonicalJSON()
			require.NoError(t, err)
			for i := 0; i < determinismRuns; i++ {
				d := Evaluate(p, in)
				if d.Hash != first.Hash {
					t.Fatalf("run %d: hash %s != %s", i, d.Hash, first.Hash)
				}
				b, err := d.CanonicalJSON()
				if err != nil || !bytes.Equal(b, firstJSON) {
					t.Fatalf("run %d: canonical JSON differs", i)
				}
			}
		})
	}
}

// TestGolden_DeterminismAcrossGoroutines evaluates the same policy and input
// concurrently; under -race this also proves Evaluate shares no mutable state.
func TestGolden_DeterminismAcrossGoroutines(t *testing.T) {
	cases := loadCases(t)
	for _, c := range cases[:10] {
		t.Run(c.Name, func(t *testing.T) {
			p := loadPolicyFixture(t, c.Policy)
			in := caseInput(t, c)
			want := Evaluate(p, in).Hash
			var wg sync.WaitGroup
			hashes := make([]string, 8)
			for g := range hashes {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					h := ""
					for i := 0; i < 100; i++ {
						h = Evaluate(p, in).Hash
					}
					hashes[g] = h
				}(g)
			}
			wg.Wait()
			for _, h := range hashes {
				require.Equal(t, want, h)
			}
		})
	}
}

func containsCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}
