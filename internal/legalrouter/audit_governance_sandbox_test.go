package legalrouter

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Adversarial audit (goal §54), area governance-sandbox-tier.
//
// F-gov-6. ADR-0023 §3 stated, without qualification: "Every permission's
// approval reference reads `NOT-AN-APPROVAL-SANDBOX-TIER-ONLY`." One of the six
// permissions in `SandboxPolicy` carries `PRODUCT-SIM-001` instead, which is the
// reference a real product decision carries.
//
// The code was right and the sentence was wrong, so the sentence moved (F-164):
// simulation is permitted by a standing product decision that holds on EVERY
// deployment -- the conservative and development policies carry the same rule
// under the same reference -- and it is the one rule here that is NOT a
// sandbox-tier concession. Writing `NOT-AN-APPROVAL-SANDBOX-TIER-ONLY` on it
// would say a sandbox tier is what makes simulation permissible, which would be
// false in the direction that matters: it would imply the other deployments
// permit it on nothing.
//
// So the rule this test holds is the corrected one, stated positively in both
// halves rather than as an exemption: the simulation rule carries the product
// decision, and every other permission says it is not an approval.
func TestAUDIT_EverySandboxPermissionsApprovalReferenceSaysItIsNotAnApproval(t *testing.T) {
	t.Parallel()
	const want = "NOT-AN-APPROVAL-SANDBOX-TIER-ONLY"
	const simulation = "PRODUCT-SIM-001"
	p := SandboxPolicy()
	sawSimulation := false
	for i, r := range p.Rules {
		if r.Outcome != Allow {
			continue
		}
		if r.Match.Product == ProductSimulation {
			sawSimulation = true
			assert.Equal(t, simulation, r.ApprovalReference,
				"rule %d permits simulation, which every policy permits under the same standing product "+
					"decision; it reads %q", i, r.ApprovalReference)
			continue
		}
		assert.Equal(t, want, r.ApprovalReference,
			"rule %d (%s/%s) permits under approval reference %q; ADR-0023 §3 says every permission in "+
				"this policy but the simulation rule reads %q", i, r.Match.Product, r.ReasonCode, r.ApprovalReference, want)
	}
	assert.True(t, sawSimulation,
		"the sandbox policy permits no simulation at all, so the exception this test documents is vacuous")
}
