package httpapi

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Adapters for the Nodal-native economy.
//
// They are where transaction boundaries live: the domain services take a
// pgx.Tx because a Credit movement, its provenance and its journal posting are
// one atomic act, and the HTTP layer above them knows nothing about that.
//
// They are also where deployment facts are resolved. A handler is given an
// account id and an amount; the payout policy, the caller's verification level
// and the active capability set are properties of the deployment, and asking
// the client for them would let the client choose them.

// NativeEconomyDeps are the domain services behind the internal economy. Each
// is optional: a deployment that has not provisioned the internal economy
// leaves them nil and the corresponding routes answer UNSUPPORTED.
type NativeEconomyDeps struct {
	Credits       *credit.Service
	NativeAssets  *nativeasset.Service
	NativeMarkets *nativemarket.Service
	Payouts       *payout.Service
	PayoutEngine  *payout.Engine

	// PayoutPolicy is the deployment's current payout policy. Nil means the
	// fail-closed default, under which no origin may be withdrawn.
	PayoutPolicy *valuedomain.Policy
	// Capabilities reports which conversion and payout capabilities are
	// ACTIVE. Nil means none are, which is the correct reading for a fresh
	// deployment.
	Capabilities CapabilityResolver
	// Verification reports an account's financial verification level. Nil
	// means NONE: a deployment that cannot establish identity has not
	// established it.
	Verification VerificationResolver
}

// CapabilityResolver reports the currently ACTIVE capabilities.
type CapabilityResolver interface {
	Active(ctx context.Context) (map[valuedomain.CapabilityKey]bool, error)
}

// VerificationResolver reports an account's financial verification level,
// which is deliberately distinct from having a Nodal session (PART XLVII).
type VerificationResolver interface {
	Level(ctx context.Context, accountID accounts.AccountID) (valuedomain.VerificationLevel, error)
}

// policyOf returns the deployment's payout policy, defaulting to fail-closed.
func (d NativeEconomyDeps) policyOf() valuedomain.Policy {
	if d.PayoutPolicy != nil {
		return *d.PayoutPolicy
	}
	return valuedomain.DefaultPolicy()
}

func (d NativeEconomyDeps) capsOf(ctx context.Context) (map[valuedomain.CapabilityKey]bool, error) {
	if d.Capabilities == nil {
		return nil, nil
	}
	return d.Capabilities.Active(ctx)
}

func (d NativeEconomyDeps) verificationOf(ctx context.Context, accountID accounts.AccountID) (valuedomain.VerificationLevel, error) {
	if d.Verification == nil {
		return valuedomain.VerificationNone, nil
	}
	return d.Verification.Level(ctx, accountID)
}

// --- credits ---------------------------------------------------------------

type creditsAdapter struct {
	deps NativeEconomyDeps
	db   *db.DB
	clk  clock.Clock
}

func (a creditsAdapter) Balance(ctx context.Context, accountID accounts.AccountID) (credit.Balances, error) {
	caps, err := a.deps.capsOf(ctx)
	if err != nil {
		return credit.Balances{}, err
	}
	level, err := a.deps.verificationOf(ctx, accountID)
	if err != nil {
		return credit.Balances{}, err
	}
	return a.deps.Credits.Balances(ctx, a.db, credit.BalanceRequest{
		AccountID:  accountID,
		Policy:     a.deps.policyOf(),
		Verified:   level,
		ActiveCaps: caps,
		Now:        a.clk.Now(),
	})
}

// --- native assets ---------------------------------------------------------

type nativeAssetsAdapter struct {
	deps NativeEconomyDeps
	db   *db.DB
}

func (a nativeAssetsAdapter) Create(ctx context.Context, r CreateNativeAsset) (nativeasset.Asset, nativeasset.Verdict, error) {
	var (
		asset   nativeasset.Asset
		verdict nativeasset.Verdict
	)
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var cerr error
		asset, verdict, cerr = a.deps.NativeAssets.CreateDraft(ctx, tx, nativeasset.CreateRequest{
			CreatorAccountID: r.AccountID,
			Name:             r.Name,
			Symbol:           r.Symbol,
			Description:      r.Description,
			ImageURL:         r.ImageURL,
			Decimals:         r.Decimals,
			Supply: nativeasset.SupplyModel{
				MaxSupply:         r.MaxSupply,
				CreatorAllocation: r.CreatorAllocation,
			},
		})
		return cerr
	})
	return asset, verdict, err
}

func (a nativeAssetsAdapter) Get(ctx context.Context, assetID assets.AssetID) (nativeasset.Asset, error) {
	return a.deps.NativeAssets.Get(ctx, a.db, assetID)
}

func (a nativeAssetsAdapter) ListTradable(ctx context.Context, limit int) ([]nativeasset.Asset, error) {
	return a.deps.NativeAssets.ListTradable(ctx, a.db, limit)
}

// --- native markets --------------------------------------------------------

type nativeMarketsAdapter struct {
	deps NativeEconomyDeps
	db   *db.DB
}

// TopHolderLimit is how many holders the market view returns. It is the
// concentration disclosure of PART LIV, and a short list is the honest one: a
// buyer needs to see whether one account holds most of it, not a directory.
const TopHolderLimit = 10

func (a nativeMarketsAdapter) Market(ctx context.Context, marketID nativemarket.MarketID) (MarketView, error) {
	m, err := a.deps.NativeMarkets.Market(ctx, a.db, marketID)
	if err != nil {
		return MarketView{}, err
	}
	st, err := a.deps.NativeMarkets.State(ctx, a.db, marketID)
	if err != nil {
		return MarketView{}, err
	}
	holders, err := a.deps.NativeMarkets.Holders(ctx, a.db, m.AssetID, TopHolderLimit)
	if err != nil {
		return MarketView{}, err
	}
	return MarketView{Market: m, State: st, Holders: holders}, nil
}

func (a nativeMarketsAdapter) Quote(ctx context.Context, r nativemarket.QuoteRequest) (nativemarket.Quote, error) {
	var q nativemarket.Quote
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var qerr error
		q, qerr = a.deps.NativeMarkets.Quote(ctx, tx, r)
		return qerr
	})
	return q, err
}

func (a nativeMarketsAdapter) Execute(ctx context.Context, r nativemarket.ExecuteRequest) (nativemarket.ExecuteResult, error) {
	var res nativemarket.ExecuteResult
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var eerr error
		res, eerr = a.deps.NativeMarkets.Execute(ctx, tx, r)
		return eerr
	})
	return res, err
}

// --- payouts ---------------------------------------------------------------

type payoutsAdapter struct {
	deps NativeEconomyDeps
	db   *db.DB
	clk  clock.Clock
}

func (a payoutsAdapter) Create(ctx context.Context, r CreatePayout) (payout.Request, payout.Decision, error) {
	caps, err := a.deps.capsOf(ctx)
	if err != nil {
		return payout.Request{}, payout.Decision{}, err
	}
	level, err := a.deps.verificationOf(ctx, r.AccountID)
	if err != nil {
		return payout.Request{}, payout.Decision{}, err
	}

	// Destination and provider facts. A payout with no destination cannot be
	// eligible, and saying so here means the decision records WHY rather than
	// failing later with a foreign-key error.
	destinationVerified := false
	providerSupports := false
	if r.DestinationID != nil {
		dest, derr := a.deps.Payouts.Destination(ctx, a.db, *r.DestinationID)
		if derr != nil {
			return payout.Request{}, payout.Decision{}, derr
		}
		if dest.AccountID != r.AccountID {
			return payout.Request{}, payout.Decision{}, errs.New(errs.CodeForbidden,
				"that payout destination belongs to another account")
		}
		destinationVerified = dest.Status.Usable()
		providerSupports = a.providerSupports(dest)
	}

	in := payout.EligibilityInput{
		Policy:              a.deps.policyOf(),
		Verified:            level,
		ActiveCaps:          caps,
		Now:                 a.clk.Now(),
		DestinationVerified: destinationVerified,
		ProviderSupports:    providerSupports,
	}

	var (
		req      payout.Request
		decision payout.Decision
	)
	err = a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var cerr error
		req, decision, cerr = a.deps.Payouts.Create(ctx, tx, payout.CreateRequest{
			AccountID:      r.AccountID,
			DestinationID:  r.DestinationID,
			Quantity:       r.Amount,
			IdempotencyKey: r.IdempotencyKey,
			EffectiveAt:    a.clk.Now(),
			CorrelationID:  r.CorrelationID,
		}, in)
		return cerr
	})
	return req, decision, err
}

// providerSupports asks the configured provider whether it can actually pay
// this destination. PART LXXVI: never infer a capability from marketing copy,
// and never from the fact that a destination row exists.
func (a payoutsAdapter) providerSupports(d payout.Destination) bool {
	p, err := a.deps.Payouts.Provider(d.Provider)
	if err != nil {
		return false
	}
	caps := p.Capabilities()
	if !caps.Supports(d.Kind) {
		return false
	}
	if d.Currency != "" && !caps.SupportsCurrency(d.Currency) {
		return false
	}
	return true
}

func (a payoutsAdapter) Get(ctx context.Context, id payout.RequestID) (payout.Request, error) {
	return a.deps.Payouts.Get(ctx, a.db, id)
}

func (a payoutsAdapter) ListByAccount(ctx context.Context, accountID accounts.AccountID, limit int) ([]payout.Request, error) {
	return a.deps.Payouts.ListByAccount(ctx, a.db, accountID, limit)
}

// wireNativeEconomy attaches the internal-economy ports that have services
// behind them and leaves the rest nil.
func wireNativeEconomy(p *Ports, d WireDeps) {
	n := d.NativeEconomy
	if n.Credits != nil {
		p.Credits = creditsAdapter{deps: n, db: d.DB, clk: d.Clock}
	}
	if n.NativeAssets != nil {
		p.NativeAssets = nativeAssetsAdapter{deps: n, db: d.DB}
	}
	if n.NativeMarkets != nil {
		p.NativeMarkets = nativeMarketsAdapter{deps: n, db: d.DB}
	}
	if n.Payouts != nil {
		p.Payouts = payoutsAdapter{deps: n, db: d.DB, clk: d.Clock}
	}
}
