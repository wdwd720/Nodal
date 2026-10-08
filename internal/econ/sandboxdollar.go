package econ

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The sandbox dollar is the SIMULATED asset every advanced figure is quoted
// and settled in: buying power V2 marks in it, fees and premiums are charged
// in it, a stable asset's reserve holds it.
//
// It has a well-known identifier, so a second one cannot be provisioned under
// it: the primary key and the (chain, mint) key both refuse the row. It is
// registered as a USD-pegged unit (is_stablecoin, peg_currency USD), which is
// the declaration valuation and buying power read to value it at par -- a
// statement about how the sandbox prices it, not a claim that it is money.
// internal/sandboxfund and every other caller go through EnsureSandboxDollar,
// so there is one sandbox dollar, never one per subsystem.
const (
	SandboxDollarSymbol   = "SBX-USD"
	SandboxDollarName     = "Sandbox US dollar (simulated, no economic value)"
	SandboxDollarDecimals = 6
)

// sandboxDollarID is a valid UUIDv7 whose node bytes spell "SBX-US".
var sandboxDollarID = func() assets.AssetID {
	a, err := assets.ParseAssetID("01920000-0000-7000-8000-5342582d5553")
	if err != nil {
		panic("econ: the sandbox dollar id is not a canonical UUIDv7: " + err.Error())
	}
	return a
}()

// SandboxDollarID is the sandbox dollar's well-known asset id.
func SandboxDollarID() assets.AssetID { return sandboxDollarID }

// SandboxDollar returns the provisioned sandbox dollar. It answers NOT_FOUND
// when this deployment has not provisioned one, and INTERNAL when
// the row under the well-known id is anything other than an ACTIVE simulated
// sandbox asset -- a mark quoted in a real asset would not be a sandbox figure.
func SandboxDollar(ctx context.Context, q db.Querier) (assets.Asset, error) {
	a, err := assets.NewRepository().Get(ctx, q, sandboxDollarID)
	if err != nil {
		if errs.HasCode(err, errs.CodeNotFound) {
			return assets.Asset{}, errs.New(errs.CodeNotFound, "the sandbox dollar is not provisioned in this deployment").
				WithField("asset_id", sandboxDollarID.String())
		}
		return assets.Asset{}, err
	}
	if err := checkSandboxDollar(a); err != nil {
		return assets.Asset{}, err
	}
	return a, nil
}

// EnsureSandboxDollar provisions the sandbox dollar if it is absent and
// returns it. Concurrent callers converge on the one row. Another simulated
// sandbox asset already claiming the symbol is a CONFLICT: two assets both
// called SBX-USD would split every sandbox balance between them.
func EnsureSandboxDollar(ctx context.Context, q db.Querier) (assets.Asset, error) {
	a, err := SandboxDollar(ctx, q)
	if err == nil || !errs.HasCode(err, errs.CodeNotFound) {
		return a, err
	}
	var squatter string
	switch err := q.QueryRow(ctx, `SELECT id::text FROM assets WHERE chain = $1 AND symbol = $2 AND id <> $3 LIMIT 1`,
		assets.SandboxChain, SandboxDollarSymbol, sandboxDollarID).Scan(&squatter); {
	case err == nil:
		return assets.Asset{}, errs.New(errs.CodeConflict, "another sandbox asset already claims the sandbox dollar's symbol").
			WithField("asset_id", squatter).WithField("symbol", SandboxDollarSymbol)
	case !errors.Is(err, pgx.ErrNoRows):
		return assets.Asset{}, errs.Wrap(err, errs.CodeInternal, "econ: look up the sandbox dollar's symbol")
	}
	stored, _, err := assets.NewRepository().CreateIfAbsent(ctx, q, assets.Asset{
		ID: sandboxDollarID, Chain: assets.SandboxChain, MintAddress: sandboxDollarID.String(),
		Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: SandboxDollarSymbol, Name: SandboxDollarName, Decimals: SandboxDollarDecimals,
		IsStablecoin: true, PegCurrency: valuation.USDPeg,
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	if err != nil {
		return assets.Asset{}, err
	}
	if err := checkSandboxDollar(stored); err != nil {
		return assets.Asset{}, err
	}
	return stored, nil
}

func checkSandboxDollar(a assets.Asset) error {
	ok := a.Kind == assets.KindSimulated && a.ValueDomain == valuedomain.Simulated &&
		a.Chain == assets.SandboxChain && a.Symbol == SandboxDollarSymbol &&
		a.Decimals == SandboxDollarDecimals && a.Status == assets.StatusActive &&
		a.IsStablecoin && a.PegCurrency == valuation.USDPeg
	if !ok {
		return errs.New(errs.CodeInternal, "the asset under the sandbox dollar's id is not an active simulated sandbox dollar").
			WithField("asset_id", a.ID.String()).WithField("symbol", a.Symbol).WithField("kind", string(a.Kind)).
			WithField("status", string(a.Status))
	}
	return nil
}
