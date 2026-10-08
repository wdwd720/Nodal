//go:build integration

package margin

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/econ/econtest"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// supply registers a stable asset reserved in the sandbox dollar and a twin,
// and issues units of the stable asset to acct (who pays sandbox dollars for
// them), so that a refusal of it is never for want of a balance.
func (f *fixture) supply(acct accounts.AccountID, issued money.Quantity) econtest.Supply {
	f.t.Helper()
	var s econtest.Supply
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, err = econtest.RegisterSupply(ctx, tx)
		return err
	}))
	f.fund(acct, s.Dollar.ID, issued)
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.ledger.Post(ctx, tx, s.Stable.Issuance(acct, issued, uuid.NewString(), f.clk.Now()))
		return err
	}))
	return s
}

// TestIntegration_AMarginAccountNeverBooksASupplyControlledAsset (migration
// 00763): a platform stable asset or a bridged twin passes every check of a
// numeraire (a USD-pegged SIMULATED asset) and of collateral (its risk class
// is mapped), but of margin's kinds the database allows only the collateral
// move. Its loans, interest, deficits and liquidations would be refused at
// COMMIT -- a liquidation of such an account could never complete. So it is
// refused, by name and before anything is written, as a numeraire, as an
// ISOLATED account's asset, in a policy's asset rules, as collateral, as a
// leveraged buy and as facility funding. Its supply still reconciles.
func TestIntegration_AMarginAccountNeverBooksASupplyControlledAsset(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	s := f.supply(acct, usd(500))
	user := econ.Actor{Type: security.ActorUser, ID: "margin-itest-user"}
	openWith := func(svc *Service, r OpenRequest) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := svc.OpenAccount(ctx, tx, r)
			return err
		})
	}

	for _, c := range s.Controlled() {
		err := openWith(f.svc, OpenRequest{AccountID: acct, MarginMode: ModeCross, Mode: gates.ModePaper, NumeraireAssetID: c.Asset, Actor: user})
		econtest.RequireRefusal(t, err, c.Asset, c.Controller)
		iso := c.Asset
		err = openWith(f.svc, OpenRequest{AccountID: acct, MarginMode: ModeIsolated, Mode: gates.ModePaper, NumeraireAssetID: f.usd,
			IsolatedAssetID: &iso, Actor: user})
		econtest.RequireRefusal(t, err, c.Asset, c.Controller)
	}
	assert.Zero(t, f.countRows(`SELECT count(*) FROM margin_accounts WHERE account_id = $1`, acct), "no margin account was opened")
	assert.Zero(t, f.countRows(`SELECT count(*) FROM margin_policies WHERE version = $1`, f.cfg.Policy.Version), "the policy was not registered")

	// A policy whose rules name one is refused when it would govern an
	// account, before it is registered.
	pol := f.cfg.Policy
	pol.Version = "margin-itest-supply-" + uuid.NewString()
	pol.AssetRules = append(append([]AssetRule(nil), pol.AssetRules...), AssetRule{AssetID: s.Stable.Asset.ID.String(), RiskWeightBPS: 10_000})
	require.NoError(t, pol.Validate())
	cfg := f.cfg
	cfg.Policy = pol
	listing, err := NewService(cfg, f.deps)
	require.NoError(t, err)
	err = openWith(listing, OpenRequest{AccountID: acct, MarginMode: ModeCross, Mode: gates.ModePaper, NumeraireAssetID: f.usd, Actor: user})
	econtest.RequireRefusal(t, err, s.Stable.Asset.ID, econ.ControllerStableIssuer)
	assert.Zero(t, f.countRows(`SELECT count(*) FROM margin_policies WHERE version = $1`, pol.Version), "the policy was not registered")
	assert.Zero(t, f.countRows(`SELECT count(*) FROM margin_accounts WHERE account_id = $1`, acct))

	// An account in a plain numeraire opens; posting either asset as
	// collateral and buying either on margin are refused before the intent.
	f.open(acct, ModeCross)
	for _, c := range s.Controlled() {
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.PostCollateral(ctx, tx, CollateralRequest{AccountID: acct, AssetID: c.Asset, Quantity: usd(100),
				IdempotencyKey: "post-" + uuid.NewString(), Requester: userRequester})
			return err
		})
		econtest.RequireRefusal(t, err, c.Asset, c.Controller)
		err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.OpenLeveragedSpot(ctx, tx, SpotRequest{AccountID: acct, AssetID: c.Asset, Quantity: usd(100),
				IdempotencyKey: "spot-" + uuid.NewString(), Requester: userRequester})
			return err
		})
		econtest.RequireRefusal(t, err, c.Asset, c.Controller)
	}
	assert.Zero(t, f.countRows(`SELECT count(*) FROM economic_intents WHERE account_id = $1`, acct), "no intent was written")
	assert.Zero(t, f.countJournals(acct, ledger.KindMarginCollateralMove))
	assert.Equal(t, "500000000", f.customer(acct, ledger.CodeWallet, s.Stable.Asset.ID).String(), "the units stayed in the wallet")

	// The lending pool and the insurance fund are never funded in one.
	for _, c := range s.Controlled() {
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.SeedFacilities(ctx, tx, SeedRequest{Asset: c.Asset, PoolAmount: usd(1_000), FundAmount: usd(1_000),
				Key: "seed-" + uuid.NewString(), Actor: f.actor})
			return err
		})
		econtest.RequireRefusal(t, err, c.Asset, c.Controller)
		assert.Zero(t, f.countRows(`SELECT count(*) FROM journal_entries e JOIN journal_transactions t ON t.id = e.transaction_id
			WHERE t.kind = 'FACILITY_FUNDING' AND e.asset_id = $1`, c.Asset))
	}

	// The supply reconciles: what is held is what was issued.
	held, err := econtest.Holdings(f.ctx, testDB, s.Stable.Asset.ID)
	require.NoError(t, err)
	outstanding, err := s.Stable.Outstanding(f.ctx, testDB, f.ledger)
	require.NoError(t, err)
	assert.Equal(t, "500000000", outstanding.String())
	assert.True(t, held.Equal(outstanding), "holdings %s, outstanding %s", held, outstanding)
}
