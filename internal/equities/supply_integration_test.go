//go:build integration

package equities

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/econ/econtest"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// listAround makes an EQUITY listing of share against cash the way a caller
// that bypasses ListEquity would (instruments.Repository.CreateAdvanced),
// and loads it.
func (w *world) listAround(cash, share assets.AssetID) Listing {
	w.t.Helper()
	symbol := uniqueSymbol("EQAR")
	var l Listing
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		terms := instruments.Terms{
			Kind: instruments.TypeEquity, Symbol: symbol, VenueCode: w.venue.Name(), ExchangeMIC: "XNAS",
			BaseAssetID: &share, QuoteAssetID: &cash, SettlementAssetID: cash, ContractMultiplier: money.QuantityFromInt64(1),
			PriceScale: 2, TickSize: q(1), LotSize: q(1), MinQuantity: q(1), QuantityDecimals: 6,
			SettlementStyle: instruments.SettlePhysical, TradingCalendar: CalendarXNYS, OracleMaxAgeMS: 60_000,
			OracleSources: []string{"itest-feed"}, MarginModel: instruments.MarginNone,
			CorporateActionBehavior: instruments.CorporateActionsAdjust, Transferability: instruments.Transferable,
		}
		inst, _, err := instruments.NewRepository().CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
			Terms: terms, CanonicalName: symbol + " XNAS " + share.String(), RiskClass: assets.RiskStandard,
			Status: assets.StatusActive, ActiveFrom: w.clk.Now().Add(-time.Hour), ActorType: "OPERATOR", ActorID: "equities-itest",
		})
		if err != nil {
			return err
		}
		l, err = w.svc.LoadListing(ctx, tx, inst.ID)
		return err
	}))
	return l
}

// TestIntegration_NoListingBooksASupplyControlledAsset (migration 00763): an
// equity trade is a paper fill at an external venue that is never booked, so
// its trade-date SIM_TRADE would change a platform stable asset's or a twin's
// holdings by what no controller issued (SU002), and its settlement and
// corporate actions are kinds the database refuses for such an asset (SU001).
// ListEquity refuses one as the settlement asset before the share asset is
// created; a listing that books one in either role anyway -- made around
// ListEquity -- has its orders and corporate actions refused before anything
// is written.
func TestIntegration_NoListingBooksASupplyControlledAsset(t *testing.T) {
	w := newWorld(t)
	var s econtest.Supply
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, err = econtest.RegisterSupply(ctx, tx)
		return err
	}))
	for _, c := range s.Controlled() {
		symbol := uniqueSymbol("EQSC")
		err := w.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := w.svc.ListEquity(ctx, tx, ListingSpec{
				Kind: instruments.TypeEquity, Symbol: symbol, Name: "Example Corp", ExchangeMIC: "XNAS", CashAssetID: c.Asset,
				ShareDecimals: 6, PriceScale: 2, TickSize: q(1), LotSize: q(1), MinQuantity: q(1), OracleMaxAgeMS: 60_000,
				OracleSources: []string{"itest-feed"}, RiskClass: assets.RiskStandard, Actor: w.sys,
			})
			return err
		})
		econtest.RequireRefusal(t, err, c.Asset, c.Controller)
		assert.Zero(t, w.count(`SELECT count(*) FROM assets WHERE symbol = $1`, symbol), "no share asset was created")
		assert.Zero(t, w.count(`SELECT count(*) FROM instrument_terms WHERE symbol = $1`, symbol), "no instrument was created")
	}

	plainShare, err := assets.NewRepository().Create(w.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated, Symbol: uniqueSymbol("SHR"),
		Name: "around share", Decimals: 6, RiskClass: assets.RiskStandard, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	acct := w.account()
	for _, tc := range []struct {
		name        string
		cash, share assets.AssetID
		refused     econtest.Controlled
	}{
		{"cash in the stable asset", s.Stable.Asset.ID, plainShare.ID, econtest.Controlled{Asset: s.Stable.Asset.ID, Controller: econ.ControllerStableIssuer}},
		{"shares in the twin", w.cash.ID, s.Twin.Asset.ID, econtest.Controlled{Asset: s.Twin.Asset.ID, Controller: econ.ControllerBridge}},
	} {
		l := w.listAround(tc.cash, tc.share)
		err := w.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := w.svc.SubmitOrderIntent(ctx, tx, w.request(acct, l, market(SideBuy, 1)))
			return err
		})
		econtest.RequireRefusal(t, err, tc.refused.Asset, tc.refused.Controller)
		assert.Zero(t, w.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1`, acct), "%s: no intent was written", tc.name)

		_, err = w.announce(CorporateActionSpec{InstrumentID: l.Instrument.ID, Type: ActionSplit, EffectiveDate: NewDate(2026, 10, 8),
			Ratio: &Ratio{Numerator: 2, Denominator: 1}, Reason: "2-for-1 split"})
		econtest.RequireRefusal(t, err, tc.refused.Asset, tc.refused.Controller)
		assert.Zero(t, w.count(`SELECT count(*) FROM equity_corporate_actions WHERE instrument_id = $1`, l.Instrument.ID),
			"%s: no action was recorded", tc.name)
	}
}
