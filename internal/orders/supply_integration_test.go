//go:build integration

package orders

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/econ/econtest"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

func (f *fixture) supply() econtest.Supply {
	f.t.Helper()
	var s econtest.Supply
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, err = econtest.RegisterSupply(ctx, tx)
		return err
	}))
	return s
}

// marketOf opens a paper-mode internal book on a new spot pair of base in
// quote (both of 6 decimals), with the given maker and taker fee, and marks
// it at 100.00.
func (f *fixture) marketOf(base, quote assets.Asset, bps money.BPS) spotMarket {
	f.t.Helper()
	m := spotMarket{base: base, quote: quote}
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		inst, err := f.instr.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: base.ID, Quote: quote.ID, Settlement: quote.ID, CanonicalName: base.Symbol + "/" + quote.Symbol,
			RiskClass: assets.RiskSettlement, Status: assets.StatusActive, ActiveFrom: f.clk.Now().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		terms := spotTerms(base.ID, quote.ID)
		m.inst = inst.ID
		m.book, err = f.svc.OpenBook(ctx, tx, OpenBookRequest{
			InstrumentID: inst.ID, Mode: gates.ModePaper, SpotTerms: &terms,
			MakerSchedule: bookSchedule(quote.ID, bps), TakerSchedule: bookSchedule(quote.ID, bps), SnapshotInterval: 8,
			Actor: systemActor,
		})
		return err
	}))
	f.mark(m, "100.00")
	return m
}

// TestIntegration_ThePaperVenueRefusesASupplyControlledInstrument (migration
// 00763): the paper venue's side of a fill is external and never booked, so a
// fill of an instrument whose base or quote is a platform stable asset or a
// bridged twin would create or destroy units of it against no counterparty
// (SU002). The venue refuses such an instrument by name when it resolves it:
// before a paper order's intent is written, and before ExecuteIntent -- the
// marketplace's path -- reserves anything.
func TestIntegration_ThePaperVenueRefusesASupplyControlledInstrument(t *testing.T) {
	f := newFixture(t)
	s := f.supply()
	plain := f.simAsset("PVB", 6, false)
	quoted := f.marketOf(plain, s.Stable.Asset, 0) // its quote is the stable asset
	twinned := f.marketOf(s.Twin.Asset, f.simAsset("PVQ", 6, true), 0)
	v := f.paperVenue(map[instruments.InstrumentID]fees.Schedule{
		quoted.inst: paperSchedule(quoted), twinned.inst: paperSchedule(twinned),
	})
	x := f.account()
	for _, tc := range []struct {
		m       spotMarket
		refused econtest.Controlled
	}{
		{quoted, econtest.Controlled{Asset: s.Stable.Asset.ID, Controller: econ.ControllerStableIssuer}},
		{twinned, econtest.Controlled{Asset: s.Twin.Asset.ID, Controller: econ.ControllerBridge}},
	} {
		for _, side := range []Side{SideBuy, SideSell} {
			_, err := v.Submit(f.ctx, testDB, paperRequest(tc.m, x, side, units(1)))
			econtest.RequireRefusal(t, err, tc.refused.Asset, tc.refused.Controller)
		}
	}
	assert.Zero(t, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1`, x), "no intent was written")

	// ExecuteIntent, given an intent some other subsystem approved, refuses
	// before it reserves or posts anything.
	qty := units(1)
	terms, err := json.Marshal(map[string]string{"source": "itest"})
	require.NoError(t, err)
	var in econ.Intent
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		in, _, err = f.intents.Submit(ctx, tx, econ.Intent{
			AccountID: x, Kind: econ.KindAcquireExposure, Mode: gates.ModePaper, Capability: gates.SpreadEngine,
			ActorType: security.ActorService, ActorID: "itest-strategy", InstrumentID: quoted.inst.String(), AssetID: &plain.ID,
			Quantity: &qty, Terms: terms, IdempotencyKey: "approved-" + uuid.NewString(),
		})
		if err != nil {
			return err
		}
		for _, to := range []econ.IntentStatus{econ.StatusRouted, econ.StatusRiskApproved} {
			if in, err = f.intents.Transition(ctx, tx, in.ID, to, systemActor, "approved elsewhere", ""); err != nil {
				return err
			}
		}
		return nil
	}))
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := v.ExecuteIntent(ctx, tx, in)
		return err
	})
	econtest.RequireRefusal(t, err, s.Stable.Asset.ID, econ.ControllerStableIssuer)
	assert.Zero(t, f.count(`SELECT count(*) FROM asset_reservations WHERE account_id = $1`, x), "nothing was reserved")
	assert.Zero(t, f.count(`SELECT count(*) FROM paper_fills WHERE account_id = $1`, x), "nothing was filled")
}

// TestIntegration_AnInternalBookQuotedInAStableAssetTradesAndSupplyReconciles:
// the flow 00763 allows and the guards must leave alone. On the internal book
// both sides of a trade are booked, so a trade quoted in a platform stable
// asset -- its notional and the fees charged in it -- moves units between
// holders and creates none: the trade posts, and the asset's holdings still
// equal what its issuer issued.
func TestIntegration_AnInternalBookQuotedInAStableAssetTradesAndSupplyReconciles(t *testing.T) {
	f := newFixture(t)
	s := f.supply()
	m := f.marketOf(f.simAsset("IBB", 6, false), s.Stable.Asset, 10)
	buyer, seller := f.account(), f.account()
	f.fund(buyer, s.Dollar.ID, 1_000_000_000)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.ledger.Post(ctx, tx, s.Stable.Issuance(buyer, units(1_000), uuid.NewString(), f.clk.Now()))
		return err
	}))
	f.fund(seller, m.base.ID, 5_000_000)

	rest := f.submit(f.limit(m, seller, SideSell, "100.00", units(2)))
	require.NoError(t, rest.Rejection)
	take := f.submit(f.limit(m, buyer, SideBuy, "100.00", units(2)))
	require.NoError(t, take.Rejection)
	require.Len(t, take.Trades, 1, "the orders crossed")

	assert.Equal(t, "2000000", f.wallet(buyer, m.base.ID).String())
	assert.True(t, f.wallet(seller, s.Stable.Asset.ID).IsPositive(), "the seller was paid in the stable asset")
	assert.True(t, f.wallet(buyer, s.Stable.Asset.ID).Cmp(units(800)) < 0, "the buyer paid 200 and its fee")
	assert.True(t, f.balance(ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, s.Stable.Asset.ID)).IsPositive(),
		"the fees were charged in the stable asset")

	held, err := econtest.Holdings(f.ctx, testDB, s.Stable.Asset.ID)
	require.NoError(t, err)
	outstanding, err := s.Stable.Outstanding(f.ctx, testDB, f.ledger)
	require.NoError(t, err)
	assert.Equal(t, units(1_000).String(), outstanding.String())
	assert.True(t, held.Equal(outstanding), "holdings %s, outstanding %s", held, outstanding)
}
