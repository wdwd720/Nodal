package config

// Adversarial audit (goal §54), area accounts-auth. These two demonstrated
// defects; they are now the regressions for the fixes (F-180, F-179).

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// F-180. RuleBootstrapOperators narrowed the declaration on `env == EnvProd`
// alone, while every sibling auth rule in the same block -- RuleNoDebugAuth,
// RuleCookieHostOnly, RuleCookieSecure -- is `prodLike` (STAGING and PROD).
// STAGING is an internet-reachable deployment with the same cookie topology, and
// it could declare an unbounded standing staff directory in an environment
// variable. The grant is permanent once written: removing the declaration stops
// it being re-offered and revokes nothing (ADR-0024 §3).
//
// The rule is `prodLike` now, so STAGING is narrowed exactly as PROD is.
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
	err = staging.Validate()
	require.Error(t, err,
		"STAGING accepted six standing operator roles declared in an environment variable")
	assert.Contains(t, err.Error(), "BOOTSTRAP_OPERATORS")
	assert.Contains(t, err.Error(), "STAGING or PROD")

	// The control this narrowing must not break: one ADMIN is what the variable
	// exists for, and STAGING still accepts it.
	first := asStaging(validProdConfig(t))
	first.Auth.BootstrapOperators = "https://idp.test|sub-1=ADMIN"
	assert.NoError(t, first.Validate(),
		"a STAGING deployment can still name its first operator")
}

// F-179. operatorroles.Directory() was derived as "every role except
// BREAK_GLASS", which included CUSTOMER -- a role that is not an operator role
// at all. Declaring it produced a session whose ActorType is OPERATOR carrying
// only customer permissions, so that person's own terms acceptance was written
// with actor_type = 'OPERATOR', which migration 00759 documents as meaning "an
// acceptance recorded on somebody's behalf by an operator".
//
// The directory excludes it now, in Go and in 00800's CHECK, and the refusal
// names the reason rather than reporting an unknown role.
func TestAudit_TheOperatorDirectoryMayNameCUSTOMER(t *testing.T) {
	t.Parallel()
	c := asStaging(validProdConfig(t))
	c.Auth.BootstrapOperators = "https://idp.test|sub-1=CUSTOMER"
	err := c.Validate()
	require.Error(t, err, "CUSTOMER is accepted as an operator-directory role")
	assert.Contains(t, err.Error(), "CUSTOMER is not an operator role")

	// Every environment refuses it, not only the internet-reachable ones: this
	// is a declaration that cannot mean anything, not a declaration that is too
	// broad for a tier.
	local := validProdConfig(t)
	local.Env = EnvLocal
	local.Auth.BootstrapOperators = "https://idp.test|sub-1=CUSTOMER"
	assert.Error(t, local.Validate())
}
