package derivatives

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/money"
)

func TestDefaultPolicyIsValidAndHashedDeterministically(t *testing.T) {
	p := DefaultPolicy()
	require.NoError(t, p.Validate())
	assert.Len(t, p.Hash(), 64)
	assert.Equal(t, p.Hash(), DefaultPolicy().Hash())
	q := DefaultPolicy()
	q.PaperSlippageBPS++
	assert.NotEqual(t, p.Hash(), q.Hash(), "every limit is covered by the hash")
}

func TestPolicyRefusesWhatCannotBeEvaluatedSafely(t *testing.T) {
	mutate := []func(*Policy){
		func(p *Policy) { p.Version = "" },
		func(p *Policy) { p.Linear = nil },
		func(p *Policy) { p.Linear = append(p.Linear, p.Linear[0]) },
		func(p *Policy) { p.Linear[0].MaintenanceBPS = p.Linear[0].InitialBPS + 1 },
		func(p *Policy) { p.Linear[0].InitialBPS = 0 },
		func(p *Policy) { p.MaxFundingRateE8 = 0 },
		func(p *Policy) { p.OptionShocksBPS = nil },
		func(p *Policy) { p.OptionShocksBPS = []money.BPS{-10_001} },
		func(p *Policy) { p.OptionMinMarginBPS = 0 },
		func(p *Policy) { p.OptionMaintenanceRatioBPS = 10_001 },
		func(p *Policy) { p.PaperSlippageBPS = 10_000 },
		func(p *Policy) { p.VariationMarginIntervalMS = 0 },
		func(p *Policy) { p.ExpiryGapShockBPS = -1 },
		func(p *Policy) { p.MaxMarkLagMS = 0 },
		func(p *Policy) { p.SettlementOverrideMaxBPS = 0 },
		func(p *Policy) { p.SettlementOverrideMaxBPS = 10_001 },
	}
	for i, m := range mutate {
		p := DefaultPolicy()
		m(&p)
		assert.Error(t, p.Validate(), "mutation %d", i)
	}
	_, err := DefaultPolicy().linearRates(assets.RiskUnsupported)
	require.Error(t, err, "no default rate: an unrated class cannot be traded")
	assert.Equal(t, money.BPS(5_000), DefaultPolicy().maxInitialBPS())
}
