//go:build integration

package sandboxfund

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// --- registry rows, as internal/stableasset and internal/bridge write them ----

// devnetMint is a fresh mint address of the length the registries accept.
func devnetMint() string {
	sum := sha256.Sum256([]byte(uuid.NewString()))
	return hex.EncodeToString(sum[:])[:40]
}

// devnetAsset creates a SIMULATED asset on a simulated chain under its mint.
func (f *fixture) devnetAsset(chain string) assets.Asset {
	f.t.Helper()
	sym := "SU" + strings.ToUpper(uuid.NewString()[:8])
	a, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{Chain: chain, MintAddress: devnetMint(), Kind: assets.KindSimulated,
		ValueDomain: valuedomain.Simulated, Symbol: sym, Name: sym + " (supply itest)", Decimals: 6, RiskClass: assets.RiskSettlement,
		Status: assets.StatusActive})
	require.NoError(f.t, err)
	return a
}

var itestActor = econ.SystemActor("sandboxfund-itest")

// insertStable registers asset as a platform stable asset of issuer, fully
// reserved in the sandbox dollar at par, the way Register writes the rows.
func (f *fixture) insertStable(ctx context.Context, tx pgx.Tx, asset assets.Asset, issuer econ.Facility) error {
	sid := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO stable_assets (id, name, asset_id, facility_id, network, chain_id, devnet_mint, decimals,
		status, issuance_cap, mint_authority_mode, mint_authority, burn_authority_mode, redemption_mode, peg_quote_asset_id,
		peg_mantissa, peg_scale, oracle_source, max_price_age_ms, peg_deviation_threshold_bps, attestable, created_by_actor_type,
		created_by_actor_id)
		VALUES ($1,$2,$3,$4,'supply-itest',$5,$6,$7,'ACTIVE',1000000000000,'PLATFORM_SIGNER',$8,'HOLDER_SIGNED','INSTANT',$9,1,0,
		        'supply-itest',60000,50,true,'SYSTEM','supply-itest')`,
		sid, "supply-"+strings.ToLower(uuid.NewString()[:13]), asset.ID, issuer.ID, asset.Chain, asset.MintAddress, int16(asset.Decimals),
		devnetMint(), f.dollar.ID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO stable_reserve_assets (id, stable_asset_id, reserve_asset_id, reserve_kind, eligibility_factor_bps,
		accepted_for_mint) VALUES ($1,$2,$3,'CASH_EQUIVALENT',10000,true)`, uuid.New(), sid, f.dollar.ID)
	return err
}

// stableAsset registers a new platform stable asset and returns it with its
// issuer.
func (f *fixture) stableAsset() (assets.Asset, econ.Facility) {
	f.t.Helper()
	a := f.devnetAsset("sbx-solana")
	var issuer econ.Facility
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if issuer, err = econ.NewRegistry().EnsureFacility(ctx, tx, ledger.FacilityStableIssuer, "supply itest issuer "+uuid.NewString()[:8], itestActor); err != nil {
			return err
		}
		return f.insertStable(ctx, tx, a, issuer)
	}))
	return a, issuer
}

// insertRoute opens a bridge route from source to twin through bridge.
func insertRoute(ctx context.Context, tx pgx.Tx, source, twin assets.Asset, bridge econ.Facility) error {
	_, err := tx.Exec(ctx, `INSERT INTO bridge_routes (id, name, facility_id, network, source_chain, source_asset_id, source_mint,
		destination_chain, destination_asset_id, destination_mint, decimals, attester, unit_price_mantissa, unit_price_scale,
		max_release_attempts, status, created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,'supply-itest',$4,$5,$6,$7,$8,$9,$10,$11,1,0,3,'ACTIVE','SYSTEM','supply-itest')`,
		uuid.New(), "supply-"+strings.ToLower(uuid.NewString()[:13]), bridge.ID, source.Chain, source.ID, source.MintAddress, twin.Chain,
		twin.ID, twin.MintAddress, int16(twin.Decimals), devnetMint())
	return err
}

// bridgedTwin opens a route and returns its source asset, its twin and the
// bridge.
func (f *fixture) bridgedTwin() (source, twin assets.Asset, bridge econ.Facility) {
	f.t.Helper()
	source, twin = f.devnetAsset("sbx-solana"), f.devnetAsset("sbx-ethereum")
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if bridge, err = econ.NewRegistry().EnsureFacility(ctx, tx, ledger.FacilityBridge, "supply itest bridge "+uuid.NewString()[:8], itestActor); err != nil {
			return err
		}
		return insertRoute(ctx, tx, source, twin, bridge)
	}))
	return source, twin, bridge
}

// --- raw journals ---------------------------------------------------------------

type rawEntry struct {
	ref  ledger.AccountRef
	side ledger.Side
	qty  int64
}

func dr(ref ledger.AccountRef, qty int64) rawEntry {
	return rawEntry{ref: ref, side: ledger.Debit, qty: qty}
}
func cr(ref ledger.AccountRef, qty int64) rawEntry {
	return rawEntry{ref: ref, side: ledger.Credit, qty: qty}
}

// rawJournal writes a journal transaction and its entries with plain SQL --
// past internal/ledger and every service -- in its own transaction, and
// returns what the database said, at COMMIT included.
func (f *fixture) rawJournal(kind ledger.Kind, entries ...rawEntry) error {
	return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		txID := uuid.New()
		sum := sha256.Sum256([]byte(txID.String()))
		if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id,
			effective_at, posted_by_actor_type, posted_by_actor_id, content_hash) VALUES ($1,$2,$3,'raw',$4,now(),'SYSTEM','raw',$5)`,
			txID, kind, "raw:"+txID.String(), txID.String(), sum[:]); err != nil {
			return err
		}
		for i, e := range entries {
			acct, err := f.ledger.EnsureAccount(ctx, tx, e.ref)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`, uuid.New(), txID, i, acct.ID, e.ref.AssetID, e.side, e.qty); err != nil {
				return err
			}
		}
		return nil
	})
}

func wallet(acct customer, asset assets.AssetID) ledger.AccountRef {
	return ledger.CustomerAccount(acct.account, ledger.CodeWallet, asset)
}

func custodian(acct customer, code ledger.Code, asset assets.AssetID) ledger.AccountRef {
	return ledger.CustomerAccount(acct.account, code, asset)
}

// issuance is the STABLE_ISSUANCE template of ADVANCED_FINANCIAL_MODEL §5:
// the customer pays q sandbox dollars into the issuer's reserve and receives
// credited units of the stable asset, of which the issuer issues issued.
func (f *fixture) issuance(c customer, stable assets.Asset, issuer econ.Facility, q, credited, issued int64) []rawEntry {
	return []rawEntry{
		cr(wallet(c, f.dollar.ID), q), dr(custodian(c, ledger.CodeTradingOutflow, f.dollar.ID), q),
		dr(wallet(c, stable.ID), credited), cr(custodian(c, ledger.CodeTradingInflow, stable.ID), credited),
		dr(issuer.Account(ledger.CodeStableReserve, f.dollar.ID), q), cr(issuer.Account(ledger.CodeStableReserveFunding, f.dollar.ID), q),
		dr(issuer.Account(ledger.CodeStableIssued, stable.ID), issued), cr(issuer.Account(ledger.CodeStableOutstanding, stable.ID), issued),
	}
}

// holdings is the sum of every holding balance of an asset, across owners.
func (f *fixture) holdings(asset assets.AssetID) money.Quantity {
	f.t.Helper()
	codes := make([]string, 0, len(ledger.HoldingCodes()))
	for _, c := range ledger.HoldingCodes() {
		codes = append(codes, string(c))
	}
	var s string
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT coalesce(sum(b.balance), 0)::text FROM ledger_accounts la
		JOIN ledger_balances b ON b.ledger_account_id = la.id WHERE la.asset_id = $1 AND la.code = ANY($2)`, asset, codes).Scan(&s))
	q, err := money.ScanQuantity(s)
	require.NoError(f.t, err)
	return q
}

func (f *fixture) balanceOf(ref ledger.AccountRef) money.Quantity {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ref)
	require.NoError(f.t, err)
	return b
}

func requireState(t *testing.T, err error, state string, msgAndArgs ...any) {
	t.Helper()
	require.Error(t, err, msgAndArgs...)
	require.Equal(t, state, db.SQLState(err), "%v", err)
}

// --- the faucet -------------------------------------------------------------------

// TestIntegration_TheFaucetNeverGrantsAnAssetWhoseSupplyAControllerOwns
// (review 3, observation): a platform stable asset exists only as its issuer
// mints it against reserve, a bridged twin only as its bridge releases it
// against a lock. The faucet refuses both by name, before anything is
// written; around the faucet the database refuses the grant's journal
// (SU001). A route's source asset is no controller's, and is still granted.
func TestIntegration_TheFaucetNeverGrantsAnAssetWhoseSupplyAControllerOwns(t *testing.T) {
	f := newFixture(t)
	c := f.customer()
	stable, _ := f.stableAsset()
	source, twin, _ := f.bridgedTwin()

	for _, tc := range []struct {
		asset      assets.AssetID
		controller string
	}{{stable.ID, econ.ControllerStableIssuer}, {twin.ID, econ.ControllerBridge}} {
		_, err := f.fund(f.faucet, c.p, c.account, tc.asset, 1_000_000, "controlled-"+tc.controller)
		requireCode(t, err, errs.CodeAssetRestricted)
		e, ok := errs.As(err)
		require.True(t, ok)
		assert.Equal(t, econ.RefusalSupplyControlled, e.Fields["refusal"])
		assert.Equal(t, tc.controller, e.Fields["controller"])
		assert.Equal(t, tc.asset.String(), e.Fields["asset_id"])
		assert.True(t, f.wallet(c.account, tc.asset).IsZero())

		err = f.rawJournal(ledger.KindSandboxFunding,
			dr(wallet(c, tc.asset), 1_000_000), cr(custodian(c, ledger.CodeCapital, tc.asset), 1_000_000))
		requireState(t, err, "SU001", "a grant posted around the faucet is refused by the database")
	}
	assert.Zero(t, f.count(`SELECT count(*) FROM sandbox_fundings WHERE account_id = $1`, c.account))
	assert.Zero(t, f.count(`SELECT count(*) FROM economic_effects WHERE account_id = $1`, c.account), "no effect was claimed")

	_, err := f.fund(f.faucet, c.p, c.account, source.ID, 1_000_000, "route-source")
	require.NoError(t, err, "a route's source asset is not supply-controlled")
}

// TestIntegration_TheSupplyRuleCountsTheLedgersHoldingCodes: the database's
// list of holding codes (cp_supply_holding_codes, 00763) is exactly
// ledger.HoldingCodes, which the stable asset's supply reconciliation sums.
func TestIntegration_TheSupplyRuleCountsTheLedgersHoldingCodes(t *testing.T) {
	f := newFixture(t)
	var inDB []string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT cp_supply_holding_codes()`).Scan(&inDB))
	var inGo []string
	for _, c := range ledger.HoldingCodes() {
		inGo = append(inGo, string(c))
	}
	sort.Strings(inDB)
	sort.Strings(inGo)
	assert.Equal(t, inGo, inDB)
}

// --- the database's rules, bypassing every service ------------------------------------

// TestIntegration_AStableAssetsUnitsExistOnlyAsItsIssuerIssuesThem: with plain
// SQL, past internal/ledger and every service. An issuance by the asset's
// issuer, a trade between two customers, a transfer and a collateral move
// are written; a faucet grant, a house book's funding, a withdrawal, a paper
// fill against no one, an issuance that credits more than it issues, and the
// issuer's accounts booked outside an issuance or by another issuer are each
// refused -- and the holdings always equal what was issued.
func TestIntegration_AStableAssetsUnitsExistOnlyAsItsIssuerIssuesThem(t *testing.T) {
	f := newFixture(t)
	stable, issuer := f.stableAsset()
	a, b := f.customer(), f.customer()
	for _, c := range []customer{a, b} {
		_, err := f.fund(f.faucet, c.p, c.account, f.dollar.ID, 1_000_000_000, "dollars-"+c.account.String())
		require.NoError(t, err)
	}
	outstanding := issuer.Account(ledger.CodeStableOutstanding, stable.ID)

	// What the rules allow.
	require.NoError(t, f.rawJournal(ledger.KindStableIssuance, f.issuance(a, stable, issuer, 500_000_000, 500_000_000, 500_000_000)...),
		"the issuer's issuance")
	require.NoError(t, f.rawJournal(ledger.KindSimTrade,
		cr(wallet(a, stable.ID), 100_000_000), dr(custodian(a, ledger.CodeTradingOutflow, stable.ID), 100_000_000),
		dr(wallet(a, f.dollar.ID), 100_000_000), cr(custodian(a, ledger.CodeTradingInflow, f.dollar.ID), 100_000_000),
		dr(wallet(b, stable.ID), 100_000_000), cr(custodian(b, ledger.CodeTradingInflow, stable.ID), 100_000_000),
		cr(wallet(b, f.dollar.ID), 100_000_000), dr(custodian(b, ledger.CodeTradingOutflow, f.dollar.ID), 100_000_000)),
		"a trade between two customers")
	require.NoError(t, f.rawJournal(ledger.KindP2PTransfer,
		dr(custodian(b, ledger.CodeTransferOut, stable.ID), 40_000_000), cr(wallet(b, stable.ID), 40_000_000),
		dr(wallet(a, stable.ID), 40_000_000), cr(custodian(a, ledger.CodeTransferIn, stable.ID), 40_000_000)),
		"a customer-to-customer transfer")
	require.NoError(t, f.rawJournal(ledger.KindMarginCollateralMove,
		dr(custodian(a, ledger.CodeMarginCollateral, stable.ID), 10_000_000), cr(wallet(a, stable.ID), 10_000_000)),
		"a customer's own collateral move")
	assert.Equal(t, "500000000", f.holdings(stable.ID).String())
	assert.Equal(t, "500000000", f.balanceOf(outstanding).String())

	// What they refuse.
	house := f.houseBook()
	other, err := func() (econ.Facility, error) {
		var fac econ.Facility
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			fac, err = econ.NewRegistry().EnsureFacility(ctx, tx, ledger.FacilityStableIssuer, "supply itest other issuer "+uuid.NewString()[:8], itestActor)
			return err
		})
		return fac, err
	}()
	require.NoError(t, err)
	for _, c := range []struct {
		name    string
		kind    ledger.Kind
		entries []rawEntry
		state   string
	}{
		{"a faucet grant", ledger.KindSandboxFunding,
			[]rawEntry{dr(wallet(a, stable.ID), 1), cr(custodian(a, ledger.CodeCapital, stable.ID), 1)}, "SU001"},
		{"a house book funded in it", ledger.KindHouseFunding,
			[]rawEntry{dr(house.Account(ledger.CodeHouseInventory, stable.ID), 1), cr(house.Account(ledger.CodeHouseCapital, stable.ID), 1)}, "SU001"},
		{"a withdrawal out of the books", ledger.KindSandboxWithdrawal,
			[]rawEntry{dr(custodian(a, ledger.CodeWithdrawn, stable.ID), 1), cr(wallet(a, stable.ID), 1)}, "SU001"},
		{"a paper fill against no one", ledger.KindSimTrade, []rawEntry{
			cr(wallet(a, f.dollar.ID), 7_000_000), dr(custodian(a, ledger.CodeTradingOutflow, f.dollar.ID), 7_000_000),
			dr(wallet(a, stable.ID), 7_000_000), cr(custodian(a, ledger.CodeTradingInflow, stable.ID), 7_000_000)}, "SU002"},
		{"a trade that books a unit to a flow account", ledger.KindSimTrade, []rawEntry{
			cr(wallet(a, stable.ID), 3_000_000), dr(custodian(a, ledger.CodeTradingOutflow, stable.ID), 3_000_000)}, "SU002"},
		{"an issuance that credits more than it issues", ledger.KindStableIssuance,
			f.issuance(a, stable, issuer, 5_000_000, 10_000_000, 5_000_000), "SU002"},
		{"the issuer's accounts in a trade", ledger.KindSimTrade, []rawEntry{
			dr(wallet(b, stable.ID), 2_000_000), cr(custodian(b, ledger.CodeTradingInflow, stable.ID), 2_000_000),
			dr(issuer.Account(ledger.CodeStableIssued, stable.ID), 2_000_000), cr(outstanding, 2_000_000)}, "SU001"},
		{"another issuer's issuance of it", ledger.KindStableIssuance, []rawEntry{
			cr(wallet(b, f.dollar.ID), 2_000_000), dr(custodian(b, ledger.CodeTradingOutflow, f.dollar.ID), 2_000_000),
			dr(wallet(b, stable.ID), 2_000_000), cr(custodian(b, ledger.CodeTradingInflow, stable.ID), 2_000_000),
			dr(other.Account(ledger.CodeStableReserve, f.dollar.ID), 2_000_000), cr(other.Account(ledger.CodeStableReserveFunding, f.dollar.ID), 2_000_000),
			dr(other.Account(ledger.CodeStableIssued, stable.ID), 2_000_000), cr(other.Account(ledger.CodeStableOutstanding, stable.ID), 2_000_000)}, "SU001"},
	} {
		requireState(t, f.rawJournal(c.kind, c.entries...), c.state, c.name)
	}
	assert.Equal(t, "500000000", f.holdings(stable.ID).String(), "nothing refused moved a unit")
	assert.Equal(t, "500000000", f.balanceOf(outstanding).String())
}

// TestIntegration_ABridgedTwinsUnitsExistOnlyAsItsBridgeReleasesThem: the
// same, for the twin a bridge route mints: a paper fill against no one is
// refused at COMMIT (SU002), and the bridge's minting accounts booked outside
// its release are refused outright (SU001).
func TestIntegration_ABridgedTwinsUnitsExistOnlyAsItsBridgeReleasesThem(t *testing.T) {
	f := newFixture(t)
	_, twin, bridge := f.bridgedTwin()
	c := f.customer()
	_, err := f.fund(f.faucet, c.p, c.account, f.dollar.ID, 100_000_000, "dollars")
	require.NoError(t, err)
	requireState(t, f.rawJournal(ledger.KindSimTrade,
		cr(wallet(c, f.dollar.ID), 1_000_000), dr(custodian(c, ledger.CodeTradingOutflow, f.dollar.ID), 1_000_000),
		dr(wallet(c, twin.ID), 1_000_000), cr(custodian(c, ledger.CodeTradingInflow, twin.ID), 1_000_000)), "SU002", "a paper fill of the twin")
	requireState(t, f.rawJournal(ledger.KindSimTrade,
		dr(wallet(c, twin.ID), 1_000_000), cr(custodian(c, ledger.CodeTradingInflow, twin.ID), 1_000_000),
		dr(bridge.Account(ledger.CodeBridgeMinted, twin.ID), 1_000_000), cr(bridge.Account(ledger.CodeBridgeOutstanding, twin.ID), 1_000_000)),
		"SU001", "the bridge's minting accounts outside its release")
	assert.True(t, f.holdings(twin.ID).IsZero())
}

// TestIntegration_AnAssetWithLedgerAccountsNeverBecomesSupplyControlled: an
// asset in which value already moved can never be registered as a stable
// asset or as a route's twin -- its holdings would start above what its
// controller issued (SU003).
func TestIntegration_AnAssetWithLedgerAccountsNeverBecomesSupplyControlled(t *testing.T) {
	f := newFixture(t)
	c := f.customer()
	held, heldTwin := f.devnetAsset("sbx-solana"), f.devnetAsset("sbx-ethereum")
	for _, a := range []assets.Asset{held, heldTwin} {
		_, err := f.fund(f.faucet, c.p, c.account, a.ID, 1_000_000, "before-"+a.ID.String())
		require.NoError(t, err, "not supply-controlled yet")
	}
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		issuer, err := econ.NewRegistry().EnsureFacility(ctx, tx, ledger.FacilityStableIssuer, "supply itest late issuer "+uuid.NewString()[:8], itestActor)
		if err != nil {
			return err
		}
		return f.insertStable(ctx, tx, held, issuer)
	})
	requireState(t, err, "SU003", "a stable asset over existing holdings")
	source := f.devnetAsset("sbx-solana")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		bridge, err := econ.NewRegistry().EnsureFacility(ctx, tx, ledger.FacilityBridge, "supply itest late bridge "+uuid.NewString()[:8], itestActor)
		if err != nil {
			return err
		}
		return insertRoute(ctx, tx, source, heldTwin, bridge)
	})
	requireState(t, err, "SU003", "a twin over existing holdings")
}

// houseBook creates a house book.
func (f *fixture) houseBook() econ.HouseAccount {
	f.t.Helper()
	var h econ.HouseAccount
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		h, err = econ.NewRegistry().CreateHouseAccount(ctx, tx, "supply itest house "+uuid.NewString()[:8], econ.HouseMarketMaking, itestActor)
		return err
	}))
	return h
}
