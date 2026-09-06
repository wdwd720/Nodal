package reality_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/reality"
)

func validSource() reality.DataSource {
	return reality.DefaultChainDataSource("helius.wallet_events", "helius", 90, 2*time.Minute, "market-ingest-worker")
}

func TestDataSource_DefaultChainRegistrationIsBlockedUntilLicensed(t *testing.T) {
	t.Parallel()
	d := validSource()
	require.NoError(t, d.Validate())
	require.Equal(t, reality.HistoricalUseUnknown, d.HistoricalUsePermitted)
	require.Equal(t, reality.PersistenceBlocked, d.PersistenceCapability)
	require.False(t, d.PersistenceAllowed(), "unknown legal rights never build permanent historical dependence")
	require.Equal(t, string(reality.RetentionRawMarketData), d.RetentionClass)
	require.Equal(t, 90, d.RetentionDays)
	require.True(t, d.AllowedIn("PROD"), "empty environments means every environment")
	d.Environments = []string{"LOCAL", "TEST"}
	require.False(t, d.AllowedIn("PROD"))
}

func TestDataSource_ValidateMirrorsTheDatabaseChecks(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*reality.DataSource){
		"persistence allowed with unknown rights": func(d *reality.DataSource) { d.PersistenceCapability = reality.PersistenceAllowed },
		"persistence allowed with rights NO": func(d *reality.DataSource) {
			d.PersistenceCapability, d.HistoricalUsePermitted = reality.PersistenceAllowed, reality.HistoricalUseNo
		},
		"agent actor":         func(d *reality.DataSource) { d.CreatedByActorType = "AGENT" },
		"zero retention":      func(d *reality.DataSource) { d.RetentionDays = 0 },
		"bad class":           func(d *reality.DataSource) { d.RetentionClass = "FOREVER" },
		"bad kind":            func(d *reality.DataSource) { d.Kind = "BLOG" },
		"bad dedup":           func(d *reality.DataSource) { d.DedupStrategy = "GUESS" },
		"zero heartbeat":      func(d *reality.DataSource) { d.HeartbeatTimeout = 0 },
		"bad status":          func(d *reality.DataSource) { d.Status = "SLEEPING" },
		"bad code":            func(d *reality.DataSource) { d.Code = "Helius Events" },
		"bad redistribution":  func(d *reality.DataSource) { d.RedistributionPolicy = "SELL" },
		"missing actor id":    func(d *reality.DataSource) { d.CreatedByActorID = "" },
		"missing provider":    func(d *reality.DataSource) { d.Provider = "" },
		"bad historical use":  func(d *reality.DataSource) { d.HistoricalUsePermitted = "MAYBE" },
		"bad persistence":     func(d *reality.DataSource) { d.PersistenceCapability = "SOMETIMES" },
		"missing actor type ": func(d *reality.DataSource) { d.CreatedByActorType = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := validSource()
			mutate(&d)
			err := d.Validate()
			require.Error(t, err)
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
	licensed := validSource()
	licensed.HistoricalUsePermitted, licensed.PersistenceCapability = reality.HistoricalUseYes, reality.PersistenceAllowed
	require.NoError(t, licensed.Validate())
	require.True(t, licensed.PersistenceAllowed())
}
