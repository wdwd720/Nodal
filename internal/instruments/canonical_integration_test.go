//go:build integration

package instruments_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

func simAsset(t *testing.T, d *db.DB, symbol string) assets.Asset {
	t.Helper()
	a, err := assets.NewRepository().Create(context.Background(), d.Pool(), assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: symbol, Name: symbol, Decimals: 6, RiskClass: assets.RiskStandard, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	return a
}

func perpTerms(settle assets.AssetID, under instruments.InstrumentID, symbol string) instruments.Terms {
	return instruments.Terms{
		Kind: instruments.TypePerpetual, Symbol: symbol, VenueCode: "NODAL-SANDBOX", SettlementAssetID: settle,
		UnderlyingInstrumentID: &under, ContractMultiplier: money.QuantityFromInt64(1), PriceScale: 2,
		TickSize: money.QuantityFromInt64(1), LotSize: money.QuantityFromInt64(1), MinQuantity: money.QuantityFromInt64(1),
		SettlementStyle: instruments.SettleCash, FundingIntervalMS: 8 * 3600 * 1000, TradingCalendar: "24x7",
		OracleMaxAgeMS: 30_000, OracleSources: []string{"sandbox-index"}, MarginModel: instruments.MarginLinear,
		CorporateActionBehavior: instruments.CorporateActionsNone, Transferability: instruments.NonTransferable,
	}
}

func TestIntegration_CanonicalInstrumentsAreVersionedAndSimulatedOnly(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	repo := instruments.NewRepository()
	usd := simAsset(t, d, "SBXUSD")
	btc := simAsset(t, d, "SBXBTC")
	tag := strings.ToUpper(uuid.NewString()[:8])

	// An underlying spot index to reference.
	spot, err := createSpot(ctx, t, d, repo, btc.ID, usd.ID, "SBXBTC/SBXUSD-"+tag)
	require.NoError(t, err)

	var perp instruments.Instrument
	var v1 instruments.Version
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		perp, v1, err = repo.CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
			Terms: perpTerms(usd.ID, spot.ID, "SBXBTC-PERP-"+tag), CanonicalName: "BTC perpetual " + tag,
			RiskClass: assets.RiskSpeculative, Status: assets.StatusActive, ActiveFrom: time.Now(),
			ActorType: "OPERATOR", ActorID: "op-1",
		})
		return err
	}))
	assert.Equal(t, instruments.TypePerpetual, perp.Type)
	assert.Equal(t, 1, v1.Version)

	// Amend: a new oracle source is version 2; the history keeps version 1.
	next := v1.Terms
	next.OracleSources = []string{"sandbox-index", "sandbox-index-b"}
	var v2 instruments.Version
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v2, err = repo.AmendTerms(ctx, tx, perp.ID, next, "second oracle source", "OPERATOR", "op-1")
		return err
	}))
	assert.Equal(t, 2, v2.Version)
	cur, err := repo.CurrentTerms(ctx, d.Pool(), perp.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, cur.Version)
	hist, err := repo.TermsHistory(ctx, d.Pool(), perp.ID)
	require.NoError(t, err)
	require.Len(t, hist, 2)
	assert.NotEqual(t, hist[0].ContentHash, hist[1].ContentHash)

	// The kind never changes.
	bad := next
	bad.Kind = instruments.TypeFuture
	exp := time.Now().Add(24 * time.Hour)
	bad.Expiry, bad.FundingIntervalMS = &exp, 0
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.AmendTerms(ctx, tx, perp.ID, bad, "turn it into a future", "OPERATOR", "op-1")
		return err
	})
	require.Error(t, err)

	// History is immutable and consecutive.
	_, err = d.Pool().Exec(ctx, `UPDATE instrument_versions SET reason = 'rewritten' WHERE instrument_id = $1`, perp.ID)
	require.Error(t, err)
	_, err = d.Pool().Exec(ctx, `INSERT INTO instrument_versions (id, instrument_id, version, terms, content_hash, reason, actor_type, actor_id)
		VALUES (gen_random_uuid(), $1, 9, '{}', decode(repeat('00', 32), 'hex'), 'gap', 'OPERATOR', 'x')`, perp.ID)
	require.Error(t, err)
	assert.Equal(t, "IN002", db.SQLState(err))

	// An advanced instrument cannot settle in real value.
	sol, real := seedAssets(t, d)
	_ = sol
	err = d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := repo.CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
			Terms: perpTerms(real.ID, spot.ID, "REAL-PERP-"+tag), CanonicalName: "real perpetual " + tag,
			RiskClass: assets.RiskSpeculative, Status: assets.StatusActive, ActiveFrom: time.Now(),
			ActorType: "OPERATOR", ActorID: "op-1",
		})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	list, err := repo.ListByType(ctx, d.Pool(), instruments.TypePerpetual, 100)
	require.NoError(t, err)
	found := false
	for _, i := range list {
		if i.ID == perp.ID {
			found = true
		}
	}
	assert.True(t, found)
}

func createSpot(ctx context.Context, t *testing.T, d *db.DB, repo *instruments.Repository, base, quote assets.AssetID, name string) (instruments.Instrument, error) {
	t.Helper()
	var ins instruments.Instrument
	err := d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ins, err = repo.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: base, Quote: quote, Settlement: quote, CanonicalName: name,
			RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: time.Now(),
		})
		return err
	})
	return ins, err
}

// ListWithTerms returns each instrument with its CURRENT terms in one read,
// filters by kind, and leaves out instruments that carry no canonical terms.
func TestIntegration_ListWithTermsReturnsCurrentTermsAndFiltersByKind(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	repo := instruments.NewRepository()
	usd := simAsset(t, d, "SBXUSDL")
	eth := simAsset(t, d, "SBXETHL")
	tag := strings.ToUpper(uuid.NewString()[:8])
	spot, err := createSpot(ctx, t, d, repo, eth.ID, usd.ID, "SBXETH/SBXUSD-"+tag)
	require.NoError(t, err)

	var perp instruments.Instrument
	var v1 instruments.Version
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		perp, v1, err = repo.CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
			Terms: perpTerms(usd.ID, spot.ID, "SBXETH-PERP-"+tag), CanonicalName: "ETH perpetual " + tag,
			RiskClass: assets.RiskSpeculative, Status: assets.StatusActive, ActiveFrom: time.Now(),
			ActorType: "OPERATOR", ActorID: "op-1",
		})
		return err
	}))
	next := v1.Terms
	next.OracleMaxAgeMS = 10_000
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := repo.AmendTerms(ctx, tx, perp.ID, next, "tighter oracle bound", "OPERATOR", "op-1")
		return err
	}))

	find := func(list []instruments.Listed, id instruments.InstrumentID) (instruments.Listed, bool) {
		for _, l := range list {
			if l.Instrument.ID == id {
				return l, true
			}
		}
		return instruments.Listed{}, false
	}

	all, err := repo.ListWithTerms(ctx, d.Pool(), "", 1000)
	require.NoError(t, err)
	got, ok := find(all, perp.ID)
	require.True(t, ok, "the perpetual is listed")
	assert.Equal(t, 2, got.Current.Version, "the current version, not the first")
	assert.Equal(t, int64(10_000), got.Current.Terms.OracleMaxAgeMS)
	_, ok = find(all, spot.ID)
	assert.False(t, ok, "a spot pair created without canonical terms is not an advanced listing")

	perps, err := repo.ListWithTerms(ctx, d.Pool(), instruments.TypePerpetual, 1000)
	require.NoError(t, err)
	_, ok = find(perps, perp.ID)
	assert.True(t, ok)
	futures, err := repo.ListWithTerms(ctx, d.Pool(), instruments.TypeFuture, 1000)
	require.NoError(t, err)
	_, ok = find(futures, perp.ID)
	assert.False(t, ok, "the kind filter applies")
}
