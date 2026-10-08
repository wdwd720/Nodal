package sandboxfund

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

func TestPolicy_DefaultIsValidVersionedAndHashed(t *testing.T) {
	p := DefaultPolicy()
	require.NoError(t, p.Validate())
	assert.Equal(t, 24*time.Hour, p.Window())
	h, err := p.Hash()
	require.NoError(t, err)
	assert.Len(t, h, 64)
	q := p
	q.CapWholeUnits++
	h2, err := q.Hash()
	require.NoError(t, err)
	assert.NotEqual(t, h, h2)

	for name, mutate := range map[string]func(*Policy){
		"no version": func(p *Policy) { p.Version = "" },
		"no window":  func(p *Policy) { p.WindowMS = 0 },
		"no cap":     func(p *Policy) { p.CapWholeUnits = 0 },
		"minus cap":  func(p *Policy) { p.CapWholeUnits = -1 },
	} {
		bad := DefaultPolicy()
		mutate(&bad)
		assert.Error(t, bad.Validate(), name)
	}
}

func TestPolicy_TheCapIsExactInEveryAssetsBaseUnits(t *testing.T) {
	p := Policy{Version: "v", WindowMS: 1, CapWholeUnits: 100_000}
	assert.Equal(t, "100000", p.Cap(0).String())
	assert.Equal(t, "100000000000", p.Cap(6).String())
	assert.Equal(t, "100000000000000000000000", p.Cap(18).String(), "beyond int64, still exact")

	require.NoError(t, p.Admit(money.QuantityFromInt64(0), p.Cap(6), 6), "exactly the cap fits")
	err := p.Admit(money.QuantityFromInt64(1), p.Cap(6), 6)
	require.Error(t, err)
	assert.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(p.Admit(money.Quantity{}, money.Quantity{}, 6)))
}

// TestProp_GrantsNeverExceedTheCap: whatever sequence of requests arrives,
// the sum of the admitted ones never exceeds the cap.
func TestProp_GrantsNeverExceedTheCap(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		p := Policy{Version: "v", WindowMS: 1, CapWholeUnits: rapid.Int64Range(1, 1_000).Draw(rt, "cap")}
		decimals := uint8(rapid.IntRange(0, 9).Draw(rt, "decimals"))
		used := money.Quantity{}
		for range rapid.IntRange(1, 50).Draw(rt, "requests") {
			req := money.QuantityFromInt64(rapid.Int64Range(-5, 400_000_000_000).Draw(rt, "request"))
			if p.Admit(used, req, decimals) == nil {
				if !req.IsPositive() {
					rt.Fatalf("admitted a non-positive grant %s", req)
				}
				used = used.Add(req)
			}
			if used.Cmp(p.Cap(decimals)) > 0 {
				rt.Fatalf("granted %s over a cap of %s", used, p.Cap(decimals))
			}
		}
	})
}

func TestAllowed_TheFaucetIsGatedOnTheEnvironment(t *testing.T) {
	for _, c := range []struct {
		env     config.Environment
		enabled bool
		want    bool
	}{
		{config.EnvLocal, false, true}, {config.EnvTest, false, true}, {config.EnvDev, false, true},
		{config.EnvStaging, false, false}, {config.EnvStaging, true, true},
		{config.EnvProd, false, false}, {config.EnvProd, true, true},
		{"MOON", true, false}, {"", true, false},
	} {
		assert.Equal(t, c.want, Allowed(c.env, c.enabled), "%s enabled=%t", c.env, c.enabled)
	}
	assert.Equal(t, config.Environments(), AllEnvironments())
	assert.Equal(t, []security.ActorType{security.ActorUser, security.ActorService, security.ActorSystem}, AllowedActorTypes())
}

func TestAssetSpec_TheSandboxDollarAndValidation(t *testing.T) {
	d := SandboxDollarSpec()
	require.NoError(t, d.Validate())
	assert.Equal(t, "SBX-USD", d.Symbol)
	assert.Equal(t, uint8(6), d.Decimals)
	assert.True(t, d.USDPegged)
	for _, bad := range []AssetSpec{
		{Symbol: "sbx-usd", Name: "x", Decimals: 6},
		{Symbol: "S", Name: "x", Decimals: 6},
		{Symbol: "SBX USD", Name: "x", Decimals: 6},
		{Symbol: "SBX", Name: "", Decimals: 6},
		{Symbol: "SBX", Name: "x", Decimals: 19},
	} {
		assert.Error(t, bad.Validate(), "%+v", bad)
	}
	_, err := New(Deps{})
	require.Error(t, err)
}
