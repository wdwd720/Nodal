package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// TestCapabilities_ResolverAnswersEverythingTheCompilerCanRequire is F-26's
// fix, and the reason it is a test rather than a longer comment.
//
// The settlement compiler refuses an action whose required capability is not
// reported ACTIVE. gateCapabilityResolver answers only about the keys in
// conversionCapabilities, and a key it does not answer about is reported
// inactive. So a capability in a compiler profile but missing from that list is
// not merely refused — it is UNSATISFIABLE. Three principals can approve the
// gate with evidence, an operator can enable it in configuration, and the
// action is still refused CAPABILITY_NOT_ACTIVE, indistinguishable from a gate
// nobody ever approved.
//
// MARKETPLACE was missing exactly that way, which made the entire internal
// marketplace unreachable in every deployment.
func TestCapabilities_ResolverAnswersEverythingTheCompilerCanRequire(t *testing.T) {
	answered := map[valuedomain.CapabilityKey]bool{}
	for _, c := range conversionCapabilities {
		answered[c] = true
	}
	for _, required := range settlement.AllRequiredCapabilities() {
		require.True(t, answered[required],
			"the settlement compiler can require %s and this resolver never answers about it, "+
				"so no gate activation could ever satisfy it", required)
	}
}

// TestCapabilities_EveryAnsweredKeyIsADeclaredGate: a key this resolver asks
// about that is not a declared capability is reported inactive forever, which
// is the same unsatisfiable shape pointing the other way.
func TestCapabilities_EveryAnsweredKeyIsADeclaredGate(t *testing.T) {
	for _, c := range conversionCapabilities {
		require.True(t, gates.Capability(string(c)).Valid(),
			"%s is answered about but is not a declared capability gate", c)
	}
}
