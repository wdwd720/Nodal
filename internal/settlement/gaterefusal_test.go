package settlement

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// A policy DENY is a refusal, whatever reason code it carries.
//
// The compiler treats one router DENY specially: the one the GATE produced,
// where the policy said yes and the capability is simply off. Reporting that as
// LEGAL_ROUTER_DENIED would send an operator to change a policy that is already
// correct (F-17).
//
// It used to ask that question of the reason code — a string a policy author
// writes. A hand-authored Deny rule carrying "CAPABILITY_NOT_ACTIVE", with no
// RequiredCapability to make the gate check fire, produced a Route with no
// reasons at all and `Permitted: true`. The deployment's own policy said no and
// the compiler said yes.
//
// Two things changed: the router records the fact in a field nobody can write,
// and Policy.Validate reserves the code.

func denyRule(reason string) legalrouter.Rule {
	return legalrouter.Rule{
		Match:      legalrouter.Key{Jurisdiction: "US-NY"},
		Outcome:    legalrouter.Deny,
		ReasonCode: reason,
		Detail:     "this jurisdiction is not served",
	}
}

func catchAllDeny() legalrouter.Rule {
	return legalrouter.Rule{
		Outcome: legalrouter.Deny, ReasonCode: "NO_APPROVAL_ON_RECORD",
		Detail: "nothing permits this",
	}
}

// TestCompile_APolicyDenyIsNeverSwallowed: the property, from the outside.
//
// This one is a regression guard rather than the proof. The exploit it is about
// can no longer be CONSTRUCTED through legalrouter.New — Validate reserves the
// code — so it passes against the old string comparison too. The two tests that
// were observed failing are TestPolicy_TheGatesReasonCodeIsReserved below and
// legalrouter's TestRoute_GateRefusalIsSetOnlyWhereTheGateRefused. Saying so is
// the point: a test that cannot fail is worth keeping only when it is labelled
// as the thing it is.
func TestCompile_APolicyDenyIsNeverSwallowed(t *testing.T) {
	t.Parallel()
	// A reason code that is NOT reserved, so the policy validates, and which
	// is deliberately close to the gate's. The compiler must refuse on the
	// fact that this was a policy denial, not on how it is spelled.
	router, err := legalrouter.New(legalrouter.Policy{
		Version: "denies-new-york",
		Rules:   []legalrouter.Rule{denyRule("CAPABILITY_NOT_ACTIVE_IN_NY"), catchAllDeny()},
	})
	require.NoError(t, err)

	fi := intentFor(ActionBuyNativeAsset)
	fi.Jurisdiction = "US-NY"
	route := compile(t, fi, router, map[valuedomain.CapabilityKey]bool{
		valuedomain.CapNativeMarketTrading: true,
	})
	assert.False(t, route.Permitted, "the deployment's policy denied this and the route permits it")
	assert.Contains(t, route.Reasons, ReasonLegalRouterDenied)
}

// TestPolicy_TheGatesReasonCodeIsReserved: the belt to the braces.
//
// Even with the compiler asking the right question, a hand-written rule
// carrying the gate's code would make every refusal message lie about which of
// the policy and the gate refused. Validate refuses the policy instead.
func TestPolicy_TheGatesReasonCodeIsReserved(t *testing.T) {
	t.Parallel()
	_, err := legalrouter.New(legalrouter.Policy{
		Version: "claims-the-gates-code",
		Rules:   []legalrouter.Rule{denyRule(legalrouter.ReasonCapabilityNotActive), catchAllDeny()},
	})
	require.Error(t, err)
	e, ok := errs.As(err)
	require.True(t, ok, "the refusal must reach a caller as an API error: %v", err)
	problems, ok := e.Fields["problems"].([]string)
	require.True(t, ok, "Validate reports its problems in a field, so an author is told which rule: %v", e.Fields)
	assert.Contains(t, strings.Join(problems, "; "), "reserved reason code")
}

// TestCompile_AGateRefusalIsStillNotAPolicyRefusal keeps F-17 fixed. The
// distinction the string comparison was making is a real one, and this is the
// case it exists for: the policy PERMITS and the capability is off.
func TestCompile_AGateRefusalIsStillNotAPolicyRefusal(t *testing.T) {
	t.Parallel()
	router, err := legalrouter.New(legalrouter.Policy{
		Version: "permits-with-a-gate",
		Rules: []legalrouter.Rule{
			{
				Match:              legalrouter.Key{Product: legalrouter.ProductNativeMarketTrade},
				Outcome:            legalrouter.Allow,
				ReasonCode:         "TEST_ONLY",
				Detail:             "a test policy, not a legal determination",
				ApprovalReference:  "NOT-AN-APPROVAL-TEST-ONLY",
				RequiredCapability: valuedomain.CapNativeMarketTrading,
			},
			catchAllDeny(),
		},
	})
	require.NoError(t, err)

	// The gate is off.
	route := compile(t, intentFor(ActionBuyNativeAsset), router, map[valuedomain.CapabilityKey]bool{})
	assert.False(t, route.Permitted)
	assert.Contains(t, route.Reasons, ReasonCapabilityNotActive,
		"the next step is activating the capability, and the refusal must say so")
	assert.NotContains(t, route.Reasons, ReasonLegalRouterDenied,
		"the policy said yes; reporting a policy refusal sends an operator to the wrong fix (F-17)")
	assert.True(t, route.Legal.GateRefusal)
}
