package legalrouter

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Adversarial audit (goal §54), area governance-sandbox-tier.
//
// F-gov-6. ADR-0023 §3 states, without qualification: "Every permission's
// approval reference reads `NOT-AN-APPROVAL-SANDBOX-TIER-ONLY`." One of the
// six permissions in `SandboxPolicy` carries `PRODUCT-SIM-001` instead, which
// is the reference a real product decision would carry. The rule is correct --
// simulated capital moves nothing -- but the ADR sentence is the one a reader
// checks the policy against, and it is false as written.
func TestAUDIT_EverySandboxPermissionsApprovalReferenceSaysItIsNotAnApproval(t *testing.T) {
	t.Parallel()
	const want = "NOT-AN-APPROVAL-SANDBOX-TIER-ONLY"
	p := SandboxPolicy()
	for i, r := range p.Rules {
		if r.Outcome != Allow {
			continue
		}
		assert.Equal(t, want, r.ApprovalReference,
			"rule %d (%s/%s) permits under approval reference %q; ADR-0023 §3 says every permission in "+
				"this policy reads %q", i, r.Match.Product, r.ReasonCode, r.ApprovalReference, want)
	}
}
