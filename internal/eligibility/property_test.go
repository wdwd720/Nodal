package eligibility

import (
	"sort"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
)

func genInput(rt *rapid.T) Input {
	pick := func(label string, opts ...string) string {
		return rapid.SampledFrom(opts).Draw(rt, label)
	}
	restrictions := rapid.SliceOfN(rapid.SampledFrom([]string{"NO_TRADING", "NO_FUNDING", "NO_WITHDRAWALS", "NO_AGENTS", "COMPLIANCE_HOLD", "OTHER"}), 0, 4).Draw(rt, "restrictions")
	caps := map[string]bool{}
	for _, c := range []string{"LIVE_MANUAL_TRADING", "LIVE_FUNDING", "WITHDRAWALS", "LIVE_AGENT_TRADING"} {
		switch rapid.IntRange(0, 2).Draw(rt, "cap_"+c) {
		case 0:
			caps[c] = true
		case 1:
			caps[c] = false
		}
	}
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	if rapid.Bool().Draw(rt, "zero_now") {
		now = time.Time{}
	}
	return Input{
		Context:             ContextKind(pick("context", "TRADE", "FUNDING", "WITHDRAWAL", "AGENT_RUN", "STRATEGY_PROMOTION", "BOGUS", "")),
		IdentityState:       pick("identity", IdentityUnverified, IdentityPending, IdentityVerified, IdentityRejected, IdentityExpired, "FOO"),
		AgeVerified:         rapid.Bool().Draw(rt, "age"),
		JurisdictionCountry: pick("country", "US", "CA", "KR", "DE", "", "us"),
		JurisdictionRegion:  pick("region", "CA", "NY", "HI", "ON", "", "ny"),
		ResidencyCountry:    pick("residency", "US", "CA", "KR", "DE", ""),
		SanctionsState:      pick("sanctions", SanctionsUnknown, SanctionsClear, SanctionsHit, SanctionsReview, "MAYBE"),
		AccountStatus:       accounts.Status(pick("account", "ACTIVE", "RESTRICTED", "FROZEN", "CLOSED", "LIMBO")),
		Restrictions:        restrictions,
		InstrumentRiskClass: assets.RiskClass(pick("risk_class", "SETTLEMENT", "MAJOR", "STANDARD", "SPECULATIVE", "UNSUPPORTED", "MEME", "")),
		InstrumentStatus:    assets.Status(pick("status", "ACTIVE", "CLOSE_ONLY", "RESTRICTED", "HALTED", "DELISTING", "DELISTED", "PAUSED", "")),
		AssetClass:          pick("asset_class", "CRYPTO_SPOT", "PREDICTION_MARKET", ""),
		Venue:               pick("venue", "JUPITER", "RAYDIUM", ""),
		Provider:            pick("provider", "STRIPE", "SOLANA_RPC", "COINBASE", ""),
		Capabilities:        caps,
		Now:                 now,
	}
}

// TestProp_ReasonCodesSorted: for any input the reason codes are sorted,
// unique, known, and eligibility is exactly "no reason codes".
func TestProp_ReasonCodesSorted(t *testing.T) {
	base := loadPolicyFixture(t, "us_baseline")
	known := map[string]bool{}
	for _, c := range ReasonCodes() {
		known[c] = true
	}
	rapid.Check(t, func(rt *rapid.T) {
		in := genInput(rt)
		p := base
		if rapid.Bool().Draw(rt, "missing_policy") {
			p = Policy{}
		}
		d := Evaluate(p, in)
		if !sort.StringsAreSorted(d.ReasonCodes) {
			rt.Fatalf("reason codes not sorted: %v", d.ReasonCodes)
		}
		for i := 1; i < len(d.ReasonCodes); i++ {
			if d.ReasonCodes[i] == d.ReasonCodes[i-1] {
				rt.Fatalf("duplicate reason code %s", d.ReasonCodes[i])
			}
		}
		for _, c := range d.ReasonCodes {
			if !known[c] {
				rt.Fatalf("unknown reason code %s", c)
			}
		}
		if d.Eligible != (len(d.ReasonCodes) == 0) {
			rt.Fatalf("eligible=%v with codes %v", d.Eligible, d.ReasonCodes)
		}
		if p.Missing() && (len(d.ReasonCodes) == 0 || d.ReasonCodes[0] != ReasonPolicyMissing && !containsCode(d.ReasonCodes, ReasonPolicyMissing)) {
			rt.Fatalf("missing policy must yield %s, got %v", ReasonPolicyMissing, d.ReasonCodes)
		}
		if d.Hash != d.ComputeHash() || d.Hash != Evaluate(p, in).Hash {
			rt.Fatal("decision hash is not a pure function of (policy, input)")
		}
	})
}

func containsCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// TestProp_InputOrderIrrelevant: the order of restrictions and the insertion
// order of capabilities never change the decision or its hash.
func TestProp_InputOrderIrrelevant(t *testing.T) {
	p := loadPolicyFixture(t, "us_baseline")
	rapid.Check(t, func(rt *rapid.T) {
		in := genInput(rt)
		a := Evaluate(p, in)

		shuffled := in
		shuffled.Restrictions = append([]string(nil), in.Restrictions...)
		perm := rapid.Permutation(shuffled.Restrictions).Draw(rt, "perm")
		shuffled.Restrictions = perm
		shuffled.Capabilities = map[string]bool{}
		keys := make([]string, 0, len(in.Capabilities))
		for k := range in.Capabilities {
			keys = append(keys, k)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
		for _, k := range keys {
			shuffled.Capabilities[k] = in.Capabilities[k]
		}
		b := Evaluate(p, shuffled)
		if a.Hash != b.Hash || a.ContextHash != b.ContextHash {
			rt.Fatalf("input ordering leaked into the decision: %v vs %v", a.ReasonCodes, b.ReasonCodes)
		}
	})
}

// TestProp_UnknownCountryNeverEligible: whatever else is true, an input
// without a jurisdiction or with an unlisted one is never eligible.
func TestProp_UnknownCountryNeverEligible(t *testing.T) {
	p := loadPolicyFixture(t, "us_baseline")
	rapid.Check(t, func(rt *rapid.T) {
		in := genInput(rt)
		in.JurisdictionCountry = rapid.SampledFrom([]string{"", "DE", "FR", "XX"}).Draw(rt, "country")
		d := Evaluate(p, in)
		if d.Eligible {
			rt.Fatalf("eligible with jurisdiction %q", in.JurisdictionCountry)
		}
		if !containsCode(d.ReasonCodes, ReasonJurisdiction) && !containsCode(d.ReasonCodes, ReasonJurisdictionUnknown) {
			rt.Fatalf("expected a jurisdiction reason, got %v", d.ReasonCodes)
		}
	})
}
