//go:build integration

package feeledger

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ/econtest"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_APerformancePeriodIsNeverAccountedInASupplyControlledAsset
// (migration 00763): a period's accruals and crystallization are
// PERFORMANCE_FEE_ACCRUAL and PERFORMANCE_FEE_CRYSTALLIZATION journals in its
// fee asset, which the database refuses for a platform stable asset or a
// bridged twin. OpenPeriod refuses such a fee asset by name, before the period
// is written; the same request in the sandbox dollar opens.
func TestIntegration_APerformancePeriodIsNeverAccountedInASupplyControlledAsset(t *testing.T) {
	f := newFixture(t)
	supply, err := do(f, func(ctx context.Context, tx pgx.Tx) (econtest.Supply, error) { return econtest.RegisterSupply(ctx, tx) })
	require.NoError(t, err)
	acct := f.customer(supply.Dollar.ID, money.Quantity{})
	strategy := "strat-" + uuid.NewString()[:8]
	open := func(fee econtest.Controlled) error {
		_, err := do(f, func(ctx context.Context, tx pgx.Tx) (Period, error) {
			return f.svc.OpenPeriod(ctx, tx, OpenPeriodRequest{AccountID: acct, StrategyRef: strategy, FeeAsset: fee.Asset,
				TermsVersion: "perf-v1", PeriodStart: periodStart, PeriodEnd: periodStart.AddDate(0, 3, 0), OpeningNAV: usd("10000.00"),
				FeeRateBPS: 2_000, IdempotencyKey: "open-" + fee.Asset.String(), Actor: service})
		})
		return err
	}
	for _, c := range supply.Controlled() {
		econtest.RequireRefusal(t, open(c), c.Asset, c.Controller)
	}
	assert.Zero(t, f.count(`SELECT count(*) FROM performance_fee_periods WHERE account_id = $1`, acct), "no period was written")
	require.NoError(t, open(econtest.Controlled{Asset: supply.Dollar.ID}), "the sandbox dollar is no controller's")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM performance_fee_periods WHERE account_id = $1`, acct))
}
