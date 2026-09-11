package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nodal/controlplane/internal/config"
)

// The sandbox tier's settlement window (D-086).
//
// Only SETTLED Credit value is payout-eligible under any policy in this build.
// With STAGING's real 720-hour window, nothing bought on the tier whose whole
// purpose is rehearsing the product could ever be withdrawn -- and the failure
// was invisible, because it is a clock rather than a refusal.
func TestCreditSettlement_ASandboxTierSettlesInMinutesAndProdNeverDoes(t *testing.T) {
	t.Parallel()

	sandbox := &config.Config{Env: config.EnvStaging}
	sandbox.API.LegalPolicy = config.LegalPolicySandbox
	sandbox.Credit.SettlementWindow = 720 * time.Hour
	window, interval := creditSettlement(sandbox)
	assert.Equal(t, sandboxSettleWait, window,
		"a sandbox tier settles in minutes; nothing was charged and nothing can be charged back")
	assert.Equal(t, sandboxSettleInterval, interval,
		"a two-minute window swept every fifteen minutes is a fifteen-minute window")
	assert.Less(t, interval, window, "the sweep has to tick inside the window it is sweeping")

	// PROD keeps its determination, whatever a config file claims. Two guards:
	// SandboxTier refuses the declaration in PROD on its own account, and the
	// branch checks the environment again.
	prod := &config.Config{Env: config.EnvProd}
	prod.API.LegalPolicy = config.LegalPolicySandbox
	prod.Credit.SettlementWindow = 720 * time.Hour
	window, interval = creditSettlement(prod)
	assert.Equal(t, 720*time.Hour, window, "PROD keeps the chargeback window it recorded")
	assert.Equal(t, settleInterval, interval)

	// A deployment that is not a sandbox tier keeps its own window too.
	staging := &config.Config{Env: config.EnvStaging}
	staging.Credit.SettlementWindow = 720 * time.Hour
	window, interval = creditSettlement(staging)
	assert.Equal(t, 720*time.Hour, window)
	assert.Equal(t, settleInterval, interval)

	// And a window nobody configured falls back to the conservative default
	// rather than to zero, which would settle a payment the instant it was
	// captured.
	window, _ = creditSettlement(&config.Config{Env: config.EnvLocal})
	assert.Equal(t, defaultSettleWait, window)
	window, _ = creditSettlement(nil)
	assert.Equal(t, defaultSettleWait, window)
}
