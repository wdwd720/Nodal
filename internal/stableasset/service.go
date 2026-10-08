package stableasset

import (
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/valuation"
)

// Params are the service's tunables. Zero values take the defaults.
type Params struct {
	// GasMinimum and GasTopUp keep the fee payers (the mint authority and the
	// holder wallets) able to pay devnet fees: below the minimum, the host's
	// faucet tops the account up.
	GasMinimum uint64
	GasTopUp   uint64
	// ReservationTTL is the capital reservation's TTL. The reservation is
	// also locked to its request, so expiry never takes it while the chain
	// outcome is unknown.
	ReservationTTL time.Duration
	// UnknownGrace is how old a SUBMITTING request must be before
	// reconciliation treats it as an unknown submission (the process that
	// recorded it may still be about to submit).
	UnknownGrace time.Duration
}

func (p Params) withDefaults() Params {
	if p.GasMinimum == 0 {
		p.GasMinimum = 1_000_000
	}
	if p.GasTopUp < p.GasMinimum {
		p.GasTopUp = 100 * p.GasMinimum
	}
	if p.ReservationTTL <= 0 || p.ReservationTTL > capital.MaxReservationTTL {
		p.ReservationTTL = capital.MaxReservationTTL
	}
	if p.UnknownGrace <= 0 {
		p.UnknownGrace = time.Minute
	}
	return p
}

// Deps are the Service's collaborators. Every one is required; the provider
// is given once, as Provider or as its four parts.
type Deps struct {
	DB    *db.DB
	Clock clock.Clock
	// Provider is the chain the stable asset is minted and burned on and the
	// custody of the keys that sign there (StableAssetProvider). The
	// binaries build it with NewProvider.
	Provider StableAssetProvider
	// Host, Issuer, Custody and Snapshots are the sandbox provider's parts,
	// for a caller that holds them rather than a provider: New wraps them
	// exactly as NewProvider does in sandbox mode -- the mode every
	// environment builds -- so they are never a way around its refusals. Set
	// Provider or all four, never both.
	Host           *devnethost.Host
	Intents        *econ.IntentStore
	Effects        *econ.EffectStore
	Authorizer     *econ.Authorizer
	Facilities     *econ.Registry
	Ledger         *ledger.Service
	Capital        *capital.Service
	Risk           *risk.Store
	RiskPolicy     risk.AdvancedPolicy
	Router         *legalrouter.Router
	Prices         *valuation.PriceStore
	Kills          econ.KillSwitchReader
	Reconciliation *reconciliation.Repository
	Audit          audit.Writer
	Accounts       *accounts.Repository
	Assets         *assets.Repository
	Issuer         *IssuerSigner
	Custody        *CustodialWalletSigner
	Snapshots      *SnapshotSigner
	Params         Params
}

// Service is the stable asset controller (the StableAssetController of goal
// §5): registry, issuance, burns and redemptions, the reserve ledger, peg
// monitoring with its circuit breaker, supply reconciliation and reserve
// snapshots. It holds no mutable state; the database and the devnet host
// hold it all.
type Service struct {
	db         *db.DB
	clk        clock.Clock
	provider   StableAssetProvider
	intents    *econ.IntentStore
	effects    *econ.EffectStore
	authorizer *econ.Authorizer
	facilities *econ.Registry
	ledger     *ledger.Service
	capital    *capital.Service
	risk       *risk.Store
	policy     risk.AdvancedPolicy
	router     *legalrouter.Router
	prices     *valuation.PriceStore
	kills      econ.KillSwitchReader
	recon      *reconciliation.Repository
	audit      audit.Writer
	accounts   *accounts.Repository
	assets     *assets.Repository
	params     Params
}

// StableAssetController is the name the goal gives this component.
type StableAssetController = Service

// New wires a Service. A missing dependency is an error, never a silently
// weaker service.
func New(d Deps) (*Service, error) {
	missing := []string{}
	check := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	check(d.DB != nil, "DB")
	check(d.Clock != nil, "Clock")
	parts := ProviderConfig{Host: d.Host, Issuer: d.Issuer, Custody: d.Custody, Snapshots: d.Snapshots}
	switch {
	case d.Provider != nil && !parts.isZero():
		return nil, errs.New(errs.CodeInternal, "stableasset: the provider is given twice, as Provider and as parts")
	case d.Provider == nil:
		missing = append(missing, parts.missing()...)
	}
	check(d.Intents != nil, "Intents")
	check(d.Effects != nil, "Effects")
	check(d.Authorizer != nil, "Authorizer")
	check(d.Facilities != nil, "Facilities")
	check(d.Ledger != nil, "Ledger")
	check(d.Capital != nil, "Capital")
	check(d.Risk != nil, "Risk")
	check(!d.RiskPolicy.Missing(), "RiskPolicy")
	check(d.Router != nil, "Router")
	check(d.Prices != nil, "Prices")
	check(d.Kills != nil, "Kills")
	check(d.Reconciliation != nil, "Reconciliation")
	check(d.Audit != nil, "Audit")
	check(d.Accounts != nil, "Accounts")
	check(d.Assets != nil, "Assets")
	if len(missing) > 0 {
		return nil, errs.New(errs.CodeInternal, "stableasset: missing dependencies").WithField("missing", missing)
	}
	provider := d.Provider
	if provider == nil {
		p, err := newSandboxProvider(parts)
		if err != nil {
			return nil, err
		}
		provider = p
	}
	if err := checkProvider(provider); err != nil {
		return nil, err
	}
	return &Service{
		db: d.DB, clk: d.Clock, provider: provider, intents: d.Intents, effects: d.Effects, authorizer: d.Authorizer,
		facilities: d.Facilities, ledger: d.Ledger, capital: d.Capital, risk: d.Risk, policy: d.RiskPolicy,
		router: d.Router, prices: d.Prices, kills: d.Kills, recon: d.Reconciliation, audit: d.Audit,
		accounts: d.Accounts, assets: d.Assets, params: d.Params.withDefaults(),
	}, nil
}

// Provider returns the chain and custody the service issues through.
func (s *Service) Provider() StableAssetProvider { return s.provider }

// Network is the devnet network this service's host carries: the one its
// stable assets are minted, burned, observed and reconciled on.
func (s *Service) Network() string { return s.provider.Network() }

// onHost refuses, without changing anything, a stable asset of another
// network: this host would read its mints and burns as never seen, and could
// fail one as expired while it stands final on its own chain.
func (s *Service) onHost(a PlatformStableAsset) error {
	if host := s.Network(); a.Network != host {
		return errs.Newf(errs.CodeConflict, "stableasset: %s is on devnet network %s; this service carries %s", a.Name, a.Network, host).
			WithField("stable_asset_id", a.ID.String()).WithField("asset_network", a.Network).WithField("host_network", host)
	}
	return nil
}

// txOptions is every transaction this package runs: READ COMMITTED (row
// locks serialise what must be serialised) with a small deadlock budget.
// Nothing outside the database happens inside a transaction function, so a
// retry is always safe.
var txOptions = db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 3}
