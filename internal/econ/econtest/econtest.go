// Package econtest registers supply-controlled assets for integration tests
// (migration 00763; econ.SupplyController): a platform stable asset with its
// STABLE_ISSUER, a bridged twin with its BRIDGE, the STABLE_ISSUANCE and
// STABLE_REDEMPTION postings that move a stable asset's supply, and the
// holdings a supply reconciliation counts.
//
// It writes the registry rows internal/stableasset and internal/bridge write,
// without their devnet mints, so that a test of any subsystem can put a
// supply-controlled asset in front of its own entry points without standing
// up the simulated chains.
//
// What this package must never do:
//
//   - Be imported by a production file (scripts/lintfin, rule 2).
//   - Register an asset that already has ledger accounts as supply-controlled:
//     every asset it registers is created in the same transaction, which is
//     what the database requires anyway (SU003).
package econtest

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Decimals is the decimals of every asset this package creates: those of the
// sandbox dollar, so that a stable asset reserved in it issues at par unit for
// unit.
const Decimals = 6

// actor registers what this package registers.
var actor = econ.SystemActor("econtest")

// devnetMint is a fresh devnet address (a base58 public key), the form a
// mint and an authority take on the simulated chains.
func devnetMint() string {
	return devnet.NewKeypairFromSeed(sha256.Sum256([]byte(uuid.NewString()))).PublicKey().String()
}

// label is a fresh registry name.
func label(prefix string) string {
	return prefix + "-" + strings.ToLower(strings.ReplaceAll(uuid.NewString(), "-", "")[:16])
}

// simulatedAsset creates a SIMULATED, USD-pegged asset on a simulated chain
// under a fresh mint -- the shape of a platform stable asset, and of a twin
// of one.
func simulatedAsset(ctx context.Context, tx pgx.Tx, chain, prefix string) (assets.Asset, error) {
	sym := prefix + strings.ToUpper(uuid.NewString()[:8])
	return assets.NewRepository().Create(ctx, tx, assets.Asset{
		Chain: chain, MintAddress: devnetMint(), Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: sym, Name: sym + " (supply-controlled, test)", Decimals: Decimals, IsStablecoin: true,
		PegCurrency: valuation.USDPeg, RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
}

// Stable is a platform stable asset registered for a test.
type Stable struct {
	// ID is the stable_assets row.
	ID      uuid.UUID
	Asset   assets.Asset
	Issuer  econ.Facility
	Reserve assets.AssetID
}

// RegisterStable creates a fresh SIMULATED, USD-pegged asset on the simulated
// chain sbx-solana and registers it as a platform stable asset of a new
// STABLE_ISSUER facility, fully reserved at par in reserve (normally the
// sandbox dollar, which must have Decimals): the rows
// stableasset.Service.Register writes. The asset is supply-controlled from its
// first moment.
func RegisterStable(ctx context.Context, tx pgx.Tx, reserve assets.AssetID) (Stable, error) {
	a, err := simulatedAsset(ctx, tx, "sbx-solana", "PSA")
	if err != nil {
		return Stable{}, err
	}
	name := label("psa")
	issuer, err := econ.NewRegistry().EnsureFacility(ctx, tx, ledger.FacilityStableIssuer, "stable issuer "+name, actor)
	if err != nil {
		return Stable{}, err
	}
	sid := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO stable_assets (id, name, asset_id, facility_id, network, chain_id, devnet_mint, decimals,
		status, issuance_cap, mint_authority_mode, mint_authority, burn_authority_mode, redemption_mode, peg_quote_asset_id,
		peg_mantissa, peg_scale, oracle_source, max_price_age_ms, peg_deviation_threshold_bps, attestable, created_by_actor_type,
		created_by_actor_id)
		VALUES ($1,$2,$3,$4,'econtest',$5,$6,$7,'ACTIVE',1000000000000000,'PLATFORM_SIGNER',$8,'HOLDER_SIGNED','INSTANT',$9,1,0,
		        'econtest',60000,50,true,'SYSTEM','econtest')`,
		sid, name, a.ID, issuer.ID, a.Chain, a.MintAddress, int16(a.Decimals), devnetMint(), reserve); err != nil {
		return Stable{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO stable_reserve_assets (id, stable_asset_id, reserve_asset_id, reserve_kind,
		eligibility_factor_bps, accepted_for_mint) VALUES ($1,$2,$3,'CASH_EQUIVALENT',10000,true)`, uuid.New(), sid, reserve); err != nil {
		return Stable{}, err
	}
	return Stable{ID: sid, Asset: a, Issuer: issuer, Reserve: reserve}, nil
}

// Issuance is the STABLE_ISSUANCE template of ADVANCED_FINANCIAL_MODEL §5 at
// par: the account pays q of the reserve into the issuer's reserve and
// receives q units, which the issuer issues.
func (s Stable) Issuance(account accounts.AccountID, q money.Quantity, key string, at time.Time) ledger.Posting {
	c := func(code ledger.Code, a assets.AssetID) ledger.AccountRef { return ledger.CustomerAccount(account, code, a) }
	return ledger.Posting{
		Kind: ledger.KindStableIssuance, IdempotencyKey: "econtest-issue:" + key,
		Reference:   ledger.FinancialEventReference{Type: "econtest_issuance", ID: key},
		EffectiveAt: at, Description: "test issuance of a platform stable asset",
		Entries: []ledger.Entry{
			{Account: c(ledger.CodeWallet, s.Reserve), Side: ledger.Credit, Quantity: q},
			{Account: c(ledger.CodeTradingOutflow, s.Reserve), Side: ledger.Debit, Quantity: q},
			{Account: c(ledger.CodeWallet, s.Asset.ID), Side: ledger.Debit, Quantity: q},
			{Account: c(ledger.CodeTradingInflow, s.Asset.ID), Side: ledger.Credit, Quantity: q},
			{Account: s.Issuer.Account(ledger.CodeStableReserve, s.Reserve), Side: ledger.Debit, Quantity: q},
			{Account: s.Issuer.Account(ledger.CodeStableReserveFunding, s.Reserve), Side: ledger.Credit, Quantity: q},
			{Account: s.Issuer.Account(ledger.CodeStableIssued, s.Asset.ID), Side: ledger.Debit, Quantity: q},
			{Account: s.Issuer.Account(ledger.CodeStableOutstanding, s.Asset.ID), Side: ledger.Credit, Quantity: q},
		},
	}
}

// Redemption is the exact inverse of Issuance: the account retires q units
// and is paid q of the reserve.
func (s Stable) Redemption(account accounts.AccountID, q money.Quantity, key string, at time.Time) ledger.Posting {
	p := s.Issuance(account, q, key, at)
	p.Kind, p.IdempotencyKey = ledger.KindStableRedemption, "econtest-redeem:"+key
	p.Reference.Type, p.Description = "econtest_redemption", "test redemption of a platform stable asset"
	for i := range p.Entries {
		if p.Entries[i].Side == ledger.Debit {
			p.Entries[i].Side = ledger.Credit
		} else {
			p.Entries[i].Side = ledger.Debit
		}
	}
	return p
}

// Outstanding is what the issuer issued and has not retired: its
// STABLE_OUTSTANDING balance.
func (s Stable) Outstanding(ctx context.Context, q db.Querier, l *ledger.Service) (money.Quantity, error) {
	return l.Balance(ctx, q, s.Issuer.Account(ledger.CodeStableOutstanding, s.Asset.ID))
}

// Twin is a bridged twin registered for a test.
type Twin struct {
	// RouteID is the bridge_routes row.
	RouteID uuid.UUID
	Asset   assets.Asset
	Bridge  econ.Facility
	Source  assets.AssetID
}

// RegisterTwin opens a bridge route from source -- a SIMULATED asset of
// Decimals on sbx-solana or sbx-ethereum -- to a fresh twin on the other of
// the two chains, through a new BRIDGE facility: the rows bridge.CreateRoute
// writes. The twin is USD-pegged when its source is, and supply-controlled
// from its first moment.
func RegisterTwin(ctx context.Context, tx pgx.Tx, source assets.Asset) (Twin, error) {
	var chain string
	switch source.Chain {
	case "sbx-solana":
		chain = "sbx-ethereum"
	case "sbx-ethereum":
		chain = "sbx-solana"
	}
	if chain == "" || source.Decimals != Decimals {
		return Twin{}, errs.New(errs.CodeValidationFailed, "econtest: a twin's source is an asset of 6 decimals on sbx-solana or sbx-ethereum")
	}
	twin, err := simulatedAsset(ctx, tx, chain, "TW")
	if err != nil {
		return Twin{}, err
	}
	name := label("route")
	bridge, err := econ.NewRegistry().EnsureFacility(ctx, tx, ledger.FacilityBridge, "bridge "+name, actor)
	if err != nil {
		return Twin{}, err
	}
	rid := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO bridge_routes (id, name, facility_id, network, source_chain, source_asset_id, source_mint,
		destination_chain, destination_asset_id, destination_mint, decimals, attester, unit_price_mantissa, unit_price_scale,
		max_release_attempts, status, created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,'econtest',$4,$5,$6,$7,$8,$9,$10,$11,1,0,3,'ACTIVE','SYSTEM','econtest')`,
		rid, name, bridge.ID, source.Chain, source.ID, source.MintAddress, twin.Chain, twin.ID, twin.MintAddress,
		int16(twin.Decimals), devnetMint()); err != nil {
		return Twin{}, err
	}
	return Twin{RouteID: rid, Asset: twin, Bridge: bridge, Source: source.ID}, nil
}

// SourceAsset creates a SIMULATED, USD-pegged asset on sbx-solana that is no
// controller's: a route's source asset.
func SourceAsset(ctx context.Context, tx pgx.Tx) (assets.Asset, error) {
	return SourceAssetOn(ctx, tx, "sbx-solana")
}

// SourceAssetOn is SourceAsset on another simulated chain.
func SourceAssetOn(ctx context.Context, tx pgx.Tx, chain string) (assets.Asset, error) {
	return simulatedAsset(ctx, tx, chain, "SRC")
}

// Supply is the usual pair of supply-controlled assets of a test: a platform
// stable asset reserved in the sandbox dollar, and a bridged twin of a plain
// source asset.
type Supply struct {
	Dollar assets.Asset
	Stable Stable
	Twin   Twin
}

// Controlled is a supply-controlled asset and its controller.
type Controlled struct {
	Asset      assets.AssetID
	Controller string
}

// RegisterSupply ensures the sandbox dollar and registers a stable asset
// reserved in it and a twin.
func RegisterSupply(ctx context.Context, tx pgx.Tx) (Supply, error) {
	var s Supply
	var err error
	if s.Dollar, err = econ.EnsureSandboxDollar(ctx, tx); err != nil {
		return Supply{}, err
	}
	if s.Stable, err = RegisterStable(ctx, tx, s.Dollar.ID); err != nil {
		return Supply{}, err
	}
	source, err := SourceAsset(ctx, tx)
	if err != nil {
		return Supply{}, err
	}
	if s.Twin, err = RegisterTwin(ctx, tx, source); err != nil {
		return Supply{}, err
	}
	return s, nil
}

// Controlled lists the stable asset and the twin, each with its controller.
func (s Supply) Controlled() []Controlled {
	return []Controlled{
		{Asset: s.Stable.Asset.ID, Controller: econ.ControllerStableIssuer},
		{Asset: s.Twin.Asset.ID, Controller: econ.ControllerBridge},
	}
}

// RequireRefusal fails the test unless err is the shared check's refusal of
// asset under controller: ASSET_RESTRICTED, with the fields refusal
// (econ.RefusalSupplyControlled), controller and asset_id.
func RequireRefusal(t testing.TB, err error, asset assets.AssetID, controller string) {
	t.Helper()
	require.Error(t, err, "a supply-controlled asset (%s) must be refused", asset)
	e, ok := errs.As(err)
	require.True(t, ok, "not a coded error: %v", err)
	require.Equal(t, errs.CodeAssetRestricted, e.Code, "%v", err)
	require.Equal(t, econ.RefusalSupplyControlled, e.Fields["refusal"], "%v", err)
	require.Equal(t, controller, e.Fields["controller"], "%v", err)
	require.Equal(t, asset.String(), e.Fields["asset_id"], "%v", err)
}

// Holdings is what a supply reconciliation counts as an asset's units in
// existence: the sum, over every owner, of every account of it under a
// holding code (ledger.HoldingCodes). For a supply-controlled asset the
// database keeps it equal to its controller's outstanding (SU002).
func Holdings(ctx context.Context, q db.Querier, asset assets.AssetID) (money.Quantity, error) {
	codes := make([]string, 0, len(ledger.HoldingCodes()))
	for _, c := range ledger.HoldingCodes() {
		codes = append(codes, string(c))
	}
	var s string
	if err := q.QueryRow(ctx, `SELECT coalesce(sum(b.balance), 0)::text FROM ledger_accounts la
		JOIN ledger_balances b ON b.ledger_account_id = la.id WHERE la.asset_id = $1 AND la.code = ANY($2)`, asset, codes).Scan(&s); err != nil {
		return money.Quantity{}, err
	}
	return money.ScanQuantity(s)
}
