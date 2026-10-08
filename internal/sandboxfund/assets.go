package sandboxfund

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// SandboxDollarSymbol is the symbol of the standard sandbox dollar, owned by
// internal/econ.
const SandboxDollarSymbol = econ.SandboxDollarSymbol

// AssetSpec describes a standard sandbox asset. The symbol is how
// EnsureSandboxAsset finds an existing one; it is never the asset's identity
// anywhere else.
type AssetSpec struct {
	Symbol   string
	Name     string
	Decimals uint8
	// USDPegged declares a simulated dollar: the registry marks it a
	// USD-pegged stable unit (is_stablecoin, peg_currency USD), the
	// declaration buying power and valuation read, so it is valued at par.
	USDPegged bool
}

// SandboxDollarSpec is the standard sandbox dollar: SBX-USD, 6 decimals,
// SIMULATED, on chain nodal-sandbox, valued at par.
func SandboxDollarSpec() AssetSpec {
	return AssetSpec{Symbol: SandboxDollarSymbol, Name: econ.SandboxDollarName, Decimals: econ.SandboxDollarDecimals, USDPegged: true}
}

var symbolRE = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.-]{1,31}$`)

// Validate checks a spec before anything is written.
func (s AssetSpec) Validate() error {
	var problems []string
	if !symbolRE.MatchString(s.Symbol) {
		problems = append(problems, "symbol must match ^[A-Z0-9][A-Z0-9.-]{1,31}$")
	}
	if n := strings.TrimSpace(s.Name); n == "" || n != s.Name || len(n) > 100 {
		problems = append(problems, "name must be 1..100 bytes without surrounding whitespace")
	}
	if s.Decimals > 18 {
		problems = append(problems, "decimals must be <= 18")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid sandbox asset").WithField("problems", problems)
	}
	return nil
}

// EnsureSandboxDollar returns the standard sandbox dollar, creating it on
// first use. It is econ's: one well-known asset every subsystem shares.
func EnsureSandboxDollar(ctx context.Context, tx pgx.Tx) (assets.Asset, error) {
	if tx == nil {
		return assets.Asset{}, errs.New(errs.CodeInternal, "sandboxfund: EnsureSandboxDollar requires a transaction")
	}
	return econ.EnsureSandboxDollar(ctx, tx)
}

// EnsureSandboxAsset returns the SIMULATED asset on chain nodal-sandbox with
// the spec's symbol, creating it ACTIVE if there is none. Concurrent callers
// serialize on a transaction lock keyed by the symbol, so two transactions
// can never both create it. An existing asset whose decimals or peg differ
// from the spec is a CONFLICT, never silently reused.
func EnsureSandboxAsset(ctx context.Context, tx pgx.Tx, spec AssetSpec) (assets.Asset, error) {
	if tx == nil {
		return assets.Asset{}, errs.New(errs.CodeInternal, "sandboxfund: EnsureSandboxAsset requires a transaction")
	}
	if err := spec.Validate(); err != nil {
		return assets.Asset{}, err
	}
	// The sandbox dollar is never created here under a random id: it has one
	// well-known identity (econ). A spec that disagrees with it is a CONFLICT.
	if spec.Symbol == SandboxDollarSymbol {
		if spec.Decimals != econ.SandboxDollarDecimals || !spec.USDPegged {
			return assets.Asset{}, errs.Newf(errs.CodeConflict,
				"sandbox asset %s is the sandbox dollar (%d decimals, USD peg), not %d decimals and USD peg %t",
				spec.Symbol, econ.SandboxDollarDecimals, spec.Decimals, spec.USDPegged)
		}
		return econ.EnsureSandboxDollar(ctx, tx)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"sandboxfund.asset:"+assets.SandboxChain+":"+spec.Symbol); err != nil {
		return assets.Asset{}, dbErr("lock sandbox asset", err)
	}
	repo := assets.NewRepository()
	var existing assets.AssetID
	err := tx.QueryRow(ctx, `SELECT id FROM assets WHERE chain = $1 AND kind = 'SIMULATED' AND symbol = $2
		ORDER BY created_at, id LIMIT 1`, assets.SandboxChain, spec.Symbol).Scan(&existing)
	switch {
	case err == nil:
		a, err := repo.Get(ctx, tx, existing)
		if err != nil {
			return assets.Asset{}, err
		}
		pegged := a.IsStablecoin && a.PegCurrency == valuation.USDPeg
		if a.Decimals != spec.Decimals || pegged != spec.USDPegged {
			return assets.Asset{}, errs.Newf(errs.CodeConflict,
				"sandbox asset %s exists with decimals %d and USD peg %t, not %d and %t",
				spec.Symbol, a.Decimals, pegged, spec.Decimals, spec.USDPegged).WithField("asset_id", a.ID.String())
		}
		return a, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return assets.Asset{}, dbErr("look up sandbox asset", err)
	}
	a := assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: spec.Symbol, Name: spec.Name, Decimals: spec.Decimals,
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	}
	if spec.USDPegged {
		a.IsStablecoin, a.PegCurrency = true, valuation.USDPeg
	}
	return repo.Create(ctx, tx, a)
}
