package eligibility

import (
	"bytes"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

const determinismRuns = 1000

// TestGolden_Eligibility evaluates every fixture in testdata/cases and
// compares the full sorted reason-code set (PART 56, 57).
func TestGolden_Eligibility(t *testing.T) {
	cases := loadCases(t)
	require.GreaterOrEqual(t, len(cases), 30, "golden corpus must hold at least 30 cases")
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
			require.Equal(t, c.Expect.Eligible, d.Eligible)
			require.Equal(t, len(d.ReasonCodes) == 0, d.Eligible, "eligible iff no reason codes")
			require.True(t, sort.StringsAreSorted(d.ReasonCodes))
			for _, code := range d.ReasonCodes {
				require.Contains(t, known, code)
			}
			require.Equal(t, p.Version, d.PolicyVersion)
			require.Equal(t, in.Now.UTC(), d.EvaluatedAt)
			require.Len(t, d.Hash, 64)
			require.Len(t, d.ContextHash, 64)
			require.Equal(t, d.ComputeHash(), d.Hash)
			if p.Missing() {
				require.Empty(t, d.PolicyHash)
			} else {
				require.Equal(t, p.Hash(), d.PolicyHash)
			}
		})
	}
}

// TestGolden_Determinism1000 evaluates every fixture 1,000 times and requires
// a byte-identical decision (PART 224, POLICY_AUTHORITY §4 determinism test).
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
	for _, c := range cases[:8] {
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

// TestEvaluate_ContextHashCoversPolicyAndInput: a different policy version or
// a different input changes the context hash; the same context never does.
func TestEvaluate_ContextHashCoversPolicyAndInput(t *testing.T) {
	cases := loadCases(t)
	p := loadPolicyFixture(t, "us_baseline")
	in := caseInput(t, cases[0])
	a := Evaluate(p, in)
	require.Equal(t, a.ContextHash, Evaluate(p, in).ContextHash)

	p2 := p
	p2.Version = "us-baseline-v2"
	require.NotEqual(t, a.ContextHash, Evaluate(p2, in).ContextHash)

	in2 := in
	in2.JurisdictionRegion = "OR"
	require.NotEqual(t, a.ContextHash, Evaluate(p, in2).ContextHash)
}
