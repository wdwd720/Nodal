package legalrouter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/valuedomain"
)

// GateRefusal must be unforgeable.
//
// The settlement compiler treats one DENY differently from every other: the one
// this router synthesizes when a policy PERMITS and its capability is off. That
// is a real distinction — the operator's next step is activating a gate, not
// editing a policy (F-17) — and the compiler used to draw it by comparing
// Decision.ReasonCode against the literal "CAPABILITY_NOT_ACTIVE".
//
// A reason code is a string a policy author writes. A hand-authored Deny rule
// carrying that code, with no RequiredCapability to make the compiler's gate
// check fire, produced a Route with no reasons at all and Permitted true: the
// deployment's own policy said no and the compiler said yes.
//
// Two defences. Validate reserves the code, so such a policy cannot be built
// through New — that is tested in internal/settlement, where the compiler is.
// This tests the second: even a Router constructed around an unvalidated policy
// reports GateRefusal only where the gate actually refused. It is in-package
// because building that Router requires reaching past New, which is exactly the
// hole the field exists to close.

func unvalidatedRouter(t *testing.T, rules ...Rule) *Router {
	t.Helper()
	p := Policy{Version: "unvalidated", Rules: rules}
	require.Error(t, p.Validate(), "this helper is for policies New would refuse; use New otherwise")
	return &Router{policy: p, hash: "unvalidated"}
}

func nativeKey() Key {
	// Every dimension stated: Key.Validate refuses an under-specified query
	// rather than guessing, so a partial key is MALFORMED_KEY and would tell
	// this test nothing about GateRefusal.
	return Key{
		Jurisdiction:   "US-CA",
		Provider:       "NONE",
		Rail:           string(valuedomain.RailNativeInternal),
		Product:        ProductNativeMarketTrade,
		Asset:          "0193b2e0-0000-7000-8000-0000000000c1",
		AgentAuthority: "NONE",
		ValueOrigin:    "NONE",
		PayoutMode:     PayoutModeNone,
		Compensation:   "NONE",
		Verification:   string(valuedomain.VerificationNodalIdentity),
	}
}

func TestRoute_GateRefusalIsSetOnlyWhereTheGateRefused(t *testing.T) {
	t.Parallel()

	// A hand-written DENY that claims the gate's reason code. Validate refuses
	// this policy; a Router built around it anyway must still not claim the
	// refusal came from a gate.
	forged := unvalidatedRouter(t, Rule{
		Match:      Key{Product: ProductNativeMarketTrade},
		Outcome:    Deny,
		ReasonCode: ReasonCapabilityNotActive,
		Detail:     "a policy denial wearing the gate's name",
	})
	d := forged.Route(nativeKey(), map[valuedomain.CapabilityKey]bool{})
	assert.Equal(t, Deny, d.Outcome)
	assert.False(t, d.GateRefusal,
		"the policy refused this; only the gate may claim a gate refusal")

	// The genuine article: the policy permits, the capability is off.
	real := unvalidatedRouter(t, Rule{
		Match:              Key{Product: ProductNativeMarketTrade},
		Outcome:            Allow,
		ReasonCode:         "TEST_ONLY",
		Detail:             "a test policy, not a legal determination",
		ApprovalReference:  "NOT-AN-APPROVAL-TEST-ONLY",
		RequiredCapability: valuedomain.CapNativeMarketTrading,
	})
	d = real.Route(nativeKey(), map[valuedomain.CapabilityKey]bool{})
	assert.Equal(t, Deny, d.Outcome)
	assert.True(t, d.GateRefusal)
	assert.Equal(t, ReasonCapabilityNotActive, d.ReasonCode)
	assert.False(t, d.CapabilityActive)

	// And with the gate on, it is an ALLOW that claims nothing.
	d = real.Route(nativeKey(), map[valuedomain.CapabilityKey]bool{
		valuedomain.CapNativeMarketTrading: true,
	})
	assert.Equal(t, Allow, d.Outcome)
	assert.False(t, d.GateRefusal)
	assert.True(t, d.CapabilityActive)
}

// TestRoute_TheUnmatchedFallbackClaimsNoGateRefusal: a key nobody wrote a rule
// for is denied by the fallback, and that is a policy gap rather than a gate
// being off.
func TestRoute_TheUnmatchedFallbackClaimsNoGateRefusal(t *testing.T) {
	t.Parallel()
	empty := &Router{policy: Policy{Version: "no-rules"}, hash: "no-rules"}
	d := empty.Route(nativeKey(), map[valuedomain.CapabilityKey]bool{})
	assert.Equal(t, Deny, d.Outcome)
	assert.Equal(t, "NO_MATCHING_RULE", d.ReasonCode)
	assert.False(t, d.GateRefusal)
}
