package config

// Adversarial audit (goal §54), area accounts-auth.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// F-accounts-auth-10. RuleBootstrapOperators narrows the declaration on
// `env == EnvProd` alone, while every sibling auth rule in the same block --
// RuleNoDebugAuth, RuleCookieHostOnly, RuleCookieSecure -- is `prodLike`
// (STAGING and PROD). STAGING is an internet-reachable deployment with the same
// cookie topology, and it may declare an unbounded standing staff directory in
// an environment variable. The grant is permanent once written: removing the
// declaration stops it being re-offered and revokes nothing.
func TestAudit_StagingAcceptsAnUnboundedBootstrapDeclaration(t *testing.T) {
	t.Parallel()
	decl := "https://idp.test|sub-1=ADMIN,https://idp.test|sub-2=SECURITY," +
		"https://idp.test|sub-3=COMPLIANCE,https://idp.test|sub-4=FINANCE," +
		"https://idp.test|sub-5=OPERATIONS,https://idp.test|sub-6=RISK"

	prod := validProdConfig(t)
	prod.Auth.BootstrapOperators = decl
	err := prod.Validate()
	require.Error(t, err, "PROD refuses it, which is ADR-0024 §5")

	staging := asStaging(validProdConfig(t))
	staging.Auth.BootstrapOperators = decl
	assert.NoError(t, staging.Validate(),
		"STAGING accepted six standing operator roles declared in an environment variable")
}

// F-accounts-auth-11. operatorroles.Directory() is derived as "every role except
// BREAK_GLASS", which includes CUSTOMER -- a role that is not an operator role
// at all. Declaring it produces a session whose ActorType is OPERATOR carrying
// only customer permissions, so that person's own terms acceptance is written
// with actor_type = 'OPERATOR', which migration 00759 documents as meaning
// "an acceptance recorded on somebody's behalf by an operator".
func TestAudit_TheOperatorDirectoryMayNameCUSTOMER(t *testing.T) {
	t.Parallel()
	c := asStaging(validProdConfig(t))
	c.Auth.BootstrapOperators = "https://idp.test|sub-1=CUSTOMER"
	assert.NoError(t, c.Validate(),
		"CUSTOMER is accepted as an operator-directory role")
}
