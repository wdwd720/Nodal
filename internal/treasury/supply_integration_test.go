//go:build integration

package treasury

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/econ/econtest"
	"github.com/nodal/controlplane/internal/orders"
)

// written counts what a treasury command of a house book writes: its
// actions, fundings, paper fills, risk decisions and position events, and the
// effects of the treasury subsystem.
func (f *fixture) written(b Book) map[string]int {
	f.t.Helper()
	out := map[string]int{}
	for _, table := range []string{"treasury_actions", "treasury_fundings", "treasury_paper_fills", "treasury_risk_decisions",
		"treasury_position_events"} {
		out[table] = f.count(`SELECT count(*) FROM `+table+` WHERE house_account_id = $1`, b.HouseAccountID)
	}
	out["house journal entries"] = f.count(`SELECT count(*) FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		WHERE la.owner_type = 'HOUSE' AND la.owner_id = $1`, b.HouseAccountID.String())
	got, err := f.svc.GetBook(f.ctx, testDB, b.HouseAccountID)
	require.NoError(f.t, err)
	out["book version"] = int(got.Version)
	return out
}

func (f *fixture) supply() econtest.Supply {
	f.t.Helper()
	s, err := do(f, func(ctx context.Context, tx pgx.Tx) (econtest.Supply, error) { return econtest.RegisterSupply(ctx, tx) })
	require.NoError(f.t, err)
	return s
}

// TestIntegration_AHouseBookIsNeverFundedInASupplyControlledAsset (migration
// 00763): HOUSE_FUNDING of a platform stable asset or a bridged twin is
// refused by the database (SU001). Fund refuses it by name before the book is
// even locked: no funding, action, effect or journal, and the book's version
// does not move.
func TestIntegration_AHouseBookIsNeverFundedInASupplyControlledAsset(t *testing.T) {
	f := newFixture(t)
	s := f.supply()
	b := f.register(econ.HouseHedging, false)
	before := f.written(b)
	for _, c := range s.Controlled() {
		_, err := do(f, func(ctx context.Context, tx pgx.Tx) (Funding, error) {
			return f.svc.Fund(ctx, tx, FundRequest{HouseAccountID: b.HouseAccountID, Asset: c.Asset, Quantity: units(10),
				IdempotencyKey: "fund-" + uuid.NewString(), Reason: "seed the house book", Actor: operator})
		})
		econtest.RequireRefusal(t, err, c.Asset, c.Controller)
	}
	assert.Equal(t, before, f.written(b), "nothing was written")
	assert.Zero(t, f.count(`SELECT count(*) FROM economic_effects WHERE effect_key LIKE $1`, "treasury:fund:"+b.HouseAccountID.String()+":%"),
		"no funding effect was claimed")
	f.fund(b, f.usd.ID, units(10)) // the sandbox dollar is no controller's
}

// TestIntegration_TheHousePaperVenueNeverFillsASupplyControlledAsset: a fill
// at the house paper venue is external on one side, so it would create or
// destroy units of a stable asset or a twin against no counterparty (SU002),
// as its asset or as its quote, the book's valuation asset. ExecutePaper --
// Hedge, Rebalance and Convert through it -- refuses either before the book is
// locked. A book may be valued in a stable asset; it just never fills on the
// paper venue.
func TestIntegration_TheHousePaperVenueNeverFillsASupplyControlledAsset(t *testing.T) {
	f := newFixture(t)
	s := f.supply()
	b := f.register(econ.HouseHedging, false)
	f.fund(b, f.usd.ID, units(1_000))
	before := f.written(b)
	for _, c := range s.Controlled() {
		_, err := do(f, func(ctx context.Context, tx pgx.Tx) (PaperOutcome, error) {
			return f.svc.Hedge(ctx, tx, PaperRequest{HouseAccountID: b.HouseAccountID, Asset: c.Asset, Side: orders.SideBuy,
				Quantity: units(1), IdempotencyKey: "hedge-" + uuid.NewString(), Actor: houseRobot})
		})
		econtest.RequireRefusal(t, err, c.Asset, c.Controller)
	}
	assert.Equal(t, before, f.written(b), "nothing was written")

	rebalancing := f.register(econ.HouseInventoryRebalancing, false)
	before = f.written(rebalancing)
	_, _, err := func() (PaperOutcome, bool, error) {
		var out PaperOutcome
		var noop bool
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, noop, err = f.svc.Rebalance(ctx, tx, RebalanceRequest{HouseAccountID: rebalancing.HouseAccountID, Asset: s.Stable.Asset.ID,
				Target: units(5), IdempotencyKey: "rebalance-" + uuid.NewString(), Actor: houseRobot})
			return err
		})
		return out, noop, err
	}()
	econtest.RequireRefusal(t, err, s.Stable.Asset.ID, econ.ControllerStableIssuer)
	assert.Equal(t, before, f.written(rebalancing), "nothing was written")

	// A book valued in the stable asset registers, and refuses every paper
	// fill for its quote.
	valued, err := do(f, func(ctx context.Context, tx pgx.Tx) (Book, error) {
		return f.svc.Register(ctx, tx, RegisterRequest{
			Name: "itest stable-valued " + uuid.NewString(), Strategy: econ.HouseHedging, ValuationAsset: s.Stable.Asset.ID,
			Signing:        SigningPolicy{ServiceActorID: houseRobot.ID},
			IdempotencyKey: "register-" + uuid.NewString(), Reason: "a book valued in a platform stable asset", Actor: operator,
		})
	})
	require.NoError(t, err, "a house book may be valued in a stable asset")
	before = f.written(valued)
	plain := f.simAsset("TRP")
	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (PaperOutcome, error) {
		return f.svc.Hedge(ctx, tx, PaperRequest{HouseAccountID: valued.HouseAccountID, Asset: plain.ID, Side: orders.SideBuy,
			Quantity: units(1), IdempotencyKey: "hedge-" + uuid.NewString(), Actor: houseRobot})
	})
	econtest.RequireRefusal(t, err, s.Stable.Asset.ID, econ.ControllerStableIssuer)
	assert.Equal(t, before, f.written(valued), "nothing was written")
}
