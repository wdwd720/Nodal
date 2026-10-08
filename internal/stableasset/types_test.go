package stableasset

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
)

// TestDefinition_NeverTakesTheSandboxDollarSymbol: the platform has exactly
// one sandbox dollar, owned by econ; the controller refuses to register a
// stable asset under its symbol, in any case.
func TestDefinition_NeverTakesTheSandboxDollarSymbol(t *testing.T) {
	for _, sym := range []string{"SBX-USD", "sbx-usd", " SBX-USD "} {
		err := Definition{Name: "psa", Symbol: sym, PegCurrency: "USD"}.Validate()
		require.Error(t, err)
		e, ok := errs.As(err)
		require.True(t, ok)
		problems, ok := e.Fields["problems"].([]string)
		require.True(t, ok)
		assert.Contains(t, strings.Join(problems, "; "), "sandbox dollar", sym)
	}
}

func TestStateMachines_TerminalStatesHaveNoSuccessors(t *testing.T) {
	for _, s := range AllMintStatuses() {
		for _, to := range AllMintStatuses() {
			if s.Terminal() {
				assert.False(t, CanTransitionMint(s, to), "%s is terminal but may move to %s", s, to)
			}
		}
		assert.False(t, CanTransitionMint(s, s), "no self-loops")
	}
	for _, s := range AllBurnStatuses() {
		for _, to := range AllBurnStatuses() {
			if s.Terminal() {
				assert.False(t, CanTransitionBurn(s, to), "%s is terminal but may move to %s", s, to)
			}
		}
	}
	// Posting needs finality: a submitted request reaches ISSUED / REDEEMED
	// directly only from the states where the chain outcome is being awaited.
	assert.True(t, CanTransitionMint(MintSubmissionUnknown, MintIssued))
	assert.False(t, CanTransitionMint(MintFailed, MintIssued), "a failed settlement can never become an issuance")
	assert.False(t, CanTransitionBurn(BurnRedemptionQueued, BurnFailed), "a burn final on chain is an obligation")
	assert.True(t, CanTransition(StatusActive, StatusPaused))
	assert.True(t, CanTransition(StatusPaused, StatusActive))
	assert.False(t, CanTransition(StatusRetired, StatusActive), "RETIRED is terminal")
	assert.False(t, StatusPaused.AllowsMint())
	assert.False(t, StatusPaused.AllowsBurn(), "the breaker freezes supply changes")
	assert.True(t, StatusRetired.AllowsBurn(), "a retired asset still redeems")
}

func TestEnumLists_AreUnique(t *testing.T) {
	check := func(name string, xs []string) {
		seen := map[string]bool{}
		for _, x := range xs {
			require.False(t, seen[x], "%s lists %s twice", name, x)
			seen[x] = true
		}
	}
	str := func(xs ...string) []string { return xs }
	var ms, bs, ss, rk, ak []string
	for _, x := range AllMintStatuses() {
		ms = append(ms, string(x))
	}
	for _, x := range AllBurnStatuses() {
		bs = append(bs, string(x))
	}
	for _, x := range AllStatuses() {
		ss = append(ss, string(x))
	}
	for _, x := range AllReserveKinds() {
		rk = append(rk, string(x))
	}
	for _, x := range AllAlertKinds() {
		ak = append(ak, string(x))
	}
	check("mint", ms)
	check("burn", bs)
	check("status", ss)
	check("reserve kind", rk)
	check("alert", ak)
	check("modes", str("SANDBOX", "DEVNET"))
	assert.Len(t, RequestModes(), 2)
}

func TestPlans_HashCommitsToEveryField(t *testing.T) {
	issuer := NewIssuerSigner(seed("issuer"))
	plan := testPlan(issuer)
	h1, b1, err := plan.Hash()
	require.NoError(t, err)
	h2, b2, err := plan.Hash()
	require.NoError(t, err)
	assert.Equal(t, h1, h2)
	assert.Equal(t, b1, b2)
	for _, mutate := range []func(p *MintPlan){
		func(p *MintPlan) { p.Amount = "1000001" },
		func(p *MintPlan) { p.Recipient = addr("x").String() },
		func(p *MintPlan) { p.ReserveQuantity = "1" },
		func(p *MintPlan) { p.RequestID = "other" },
		func(p *MintPlan) { p.ReservePrice.Mantissa = "2" },
	} {
		p := plan
		mutate(&p)
		h, _, err := p.Hash()
		require.NoError(t, err)
		assert.NotEqual(t, h1, h)
	}
	signing, err := plan.SigningPlan()
	require.NoError(t, err)
	assert.Equal(t, []string{issuer.PublicKey().String()}, signing.Signers)
	assert.Equal(t, []string{devnet.ProgramTokenMintTo.String(), devnet.ProgramMemo.String()}, signing.Programs)
}

// FuzzInspectMintTransaction: inspecting any bytes never panics, and nothing
// but the plan's own signed transaction passes.
func FuzzInspectMintTransaction(f *testing.F) {
	issuer := NewIssuerSigner(seed("issuer"))
	plan := testPlan(issuer)
	msg, err := plan.Message()
	if err != nil {
		f.Fatal(err)
	}
	signed, err := issuer.SignMint(plan, msg)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(signed.Raw)
	f.Add([]byte("ndvt"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if _, err := InspectMintTransaction(plan, raw); err == nil && string(raw) != string(signed.Raw) {
			t.Fatalf("a transaction other than the plan's passed inspection")
		}
	})
}

// TestNoFloatingPointInSource enforces the "no floating-point financial
// values" non-negotiable mechanically, as internal/money does.
func TestNoFloatingPointInSource(t *testing.T) {
	banned := []string{
		"float32", "float64",
		"big.Float", "big.Rat",
		"ParseFloat", "FormatFloat",
		"math.Round", "math.Floor", "math.Ceil", "math.Trunc",
	}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) // #nosec G304 -- this package's own sources
		require.NoError(t, err)
		checked++
		text := string(src)
		for _, tok := range banned {
			if idx := strings.Index(text, tok); idx >= 0 {
				line := 1 + strings.Count(text[:idx], "\n")
				t.Errorf("%s:%d: forbidden token %q in non-test stableasset source", name, line, tok)
			}
		}
	}
	require.Greater(t, checked, 10, "expected to scan the package's non-test sources")
}
