package valuation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
)

func validNewPolicy() NewPolicy {
	return NewPolicy{
		AssetID:          solID,
		Status:           assets.StatusActive,
		CollateralFactor: 8000,
		MaxPriceAge:      30 * time.Second,
		PolicyVersion:    "risk-2026-09-05.1",
		EffectiveAt:      observedAt,
		ActorType:        "OPERATOR",
		ActorID:          "ops-1",
		Reason:           "initial policy",
	}
}

func TestNewPolicy_Validate(t *testing.T) {
	require.NoError(t, validNewPolicy().Validate())

	cases := []struct {
		name   string
		mutate func(*NewPolicy)
		code   errs.Code
	}{
		{"agent actor", func(p *NewPolicy) { p.ActorType = "AGENT" }, errs.CodeForbidden},
		{"empty actor type", func(p *NewPolicy) { p.ActorType = "" }, errs.CodeForbidden},
		{"zero asset", func(p *NewPolicy) { p.AssetID = assets.AssetID{} }, errs.CodeValidationFailed},
		{"unknown status", func(p *NewPolicy) { p.Status = "WEIRD" }, errs.CodeValidationFailed},
		{"factor above 100%", func(p *NewPolicy) { p.CollateralFactor = 10001 }, errs.CodeValidationFailed},
		{"negative factor", func(p *NewPolicy) { p.CollateralFactor = -1 }, errs.CodeValidationFailed},
		{"unknown stablecoin status", func(p *NewPolicy) { p.StablecoinStatus = "MEH" }, errs.CodeValidationFailed},
		{"zero max age", func(p *NewPolicy) { p.MaxPriceAge = 0 }, errs.CodeValidationFailed},
		{"sub-millisecond max age", func(p *NewPolicy) { p.MaxPriceAge = 1500 * time.Microsecond }, errs.CodeValidationFailed},
		{"max age beyond int32 ms", func(p *NewPolicy) { p.MaxPriceAge = 25 * 24 * time.Hour * 1000 }, errs.CodeValidationFailed},
		{"blank version", func(p *NewPolicy) { p.PolicyVersion = "  " }, errs.CodeValidationFailed},
		{"zero effective", func(p *NewPolicy) { p.EffectiveAt = time.Time{} }, errs.CodeValidationFailed},
		{"expires before effective", func(p *NewPolicy) { e := p.EffectiveAt.Add(-time.Second); p.ExpiresAt = &e }, errs.CodeValidationFailed},
		{"blank actor id", func(p *NewPolicy) { p.ActorID = "" }, errs.CodeValidationFailed},
		{"blank reason", func(p *NewPolicy) { p.Reason = "" }, errs.CodeValidationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validNewPolicy()
			tc.mutate(&p)
			err := p.Validate()
			require.Error(t, err)
			assert.Equal(t, tc.code, errs.CodeOf(err))
		})
	}
}

func TestFailClosedPolicy(t *testing.T) {
	p := FailClosedPolicy(solID)
	assert.True(t, p.PolicyMissing)
	assert.Equal(t, assets.StatusRestricted, p.Status)
	assert.Zero(t, p.CollateralFactor)
	assert.Zero(t, p.MaxPriceAge)
	assert.Equal(t, MissingPolicyVersion, p.PolicyVersion)
	assert.Equal(t, solID, p.AssetID)
}

func TestStablecoinStatus_Valid(t *testing.T) {
	for _, s := range []StablecoinStatus{StablecoinNormal, StablecoinDegraded, StablecoinRestricted, StablecoinHalted} {
		assert.True(t, s.Valid(), s)
	}
	assert.False(t, StablecoinStatus("").Valid())
	assert.False(t, StablecoinStatus("normal").Valid())
}
