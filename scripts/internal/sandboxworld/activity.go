package sandboxworld

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/transfers"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The functions below each drive one piece of advanced activity the way the
// demo scenarios (scripts/advanceddemo) drive it: through the service, as the
// customer or operator it belongs to, in the transactions the API would use.
// They exist so that a command which needs an economy rather than a
// demonstration -- the restore drill (scripts/restoredrill) seeds one -- calls
// the same code instead of copying scenario bodies.

// Transfer runs one transfer in its own transaction. A refusal the service
// records (REJECTED) is committed -- that is what makes it a record -- and
// still returned as the error.
func (w *World) Transfer(ctx context.Context, p security.Principal, req transfers.SendRequest) (transfers.Result, error) {
	var res transfers.Result
	var sendErr error
	err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		res, sendErr = w.Transfers.Send(ctx, tx, p, req)
		if sendErr != nil && res.Transfer.Status != transfers.StatusRejected {
			return sendErr
		}
		return nil
	})
	if err != nil {
		return transfers.Result{}, err
	}
	return res, sendErr
}

// BookSchedule is a book fee schedule charging bps basis points of notional
// in the sandbox dollar, and nothing else.
func (w *World) BookSchedule(version string, bps money.BPS) fees.Schedule {
	s := fees.ZeroSchedule(w.USD.ID, w.USD.Decimals)
	s.Version = version
	s.PercentageFeeBPS = bps
	return s
}

// SpotBook is an internal book for a SIMULATED asset quoted and settled in
// the sandbox dollar. Prices carry two decimals: a tick is 0.01 of the
// sandbox dollar.
type SpotBook struct {
	Base assets.Asset
	// Mode is PAPER or SANDBOX.
	Mode gates.Mode
	// Lot is the quantity step, and the smallest order, in base units of
	// Base. A lot of 10^(Base.Decimals-4) base units makes one tick-lot
	// exactly one base unit of the sandbox dollar, so no notional has
	// anything to round.
	Lot          money.Quantity
	Maker, Taker fees.Schedule
	// SnapshotInterval is how many commands pass between snapshots.
	SnapshotInterval int
}

// OpenSpotBook lists Base against the sandbox dollar -- the SPOT_PAIR
// instrument, active from an hour before the world's clock -- and opens its
// internal book, in one transaction, as the world.
func (w *World) OpenSpotBook(ctx context.Context, spec SpotBook) (orders.Book, error) {
	symbol := spec.Base.Symbol + "/" + w.USD.Symbol
	var book orders.Book
	err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		inst, err := w.Instruments.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: spec.Base.ID, Quote: w.USD.ID, Settlement: w.USD.ID, CanonicalName: symbol,
			RiskClass: spec.Base.RiskClass, Status: assets.StatusActive, ActiveFrom: w.Clock.Now().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		base, quote := spec.Base.ID, w.USD.ID
		terms := instruments.Terms{
			Kind: instruments.TypeSpotPair, Symbol: symbol, VenueCode: "NODAL-SIM", BaseAssetID: &base,
			QuoteAssetID: &quote, SettlementAssetID: quote, ContractMultiplier: money.QuantityFromInt64(1), PriceScale: 2,
			TickSize: money.QuantityFromInt64(1), LotSize: spec.Lot, MinQuantity: spec.Lot, QuantityDecimals: spec.Base.Decimals,
			SettlementStyle: instruments.SettlePhysical, TradingCalendar: "24x7", OracleMaxAgeMS: 30_000,
			MarginModel: instruments.MarginNone, CorporateActionBehavior: instruments.CorporateActionsNone,
			Transferability: instruments.Transferable,
		}
		book, err = w.Orders.OpenBook(ctx, tx, orders.OpenBookRequest{
			InstrumentID: inst.ID, Mode: spec.Mode, SpotTerms: &terms, MakerSchedule: spec.Maker, TakerSchedule: spec.Taker,
			SnapshotInterval: spec.SnapshotInterval, Actor: w.System,
		})
		return err
	})
	return book, err
}

// LimitOrder is a GTC limit order the customer places as itself.
func LimitOrder(c Customer, inst instruments.InstrumentID, side orders.Side, price money.Price, qty money.Quantity, key string) orders.OrderRequest {
	return orders.OrderRequest{
		InstrumentID: inst, AccountID: c.Account, Side: side, Type: orders.TypeLimit, TimeInForce: orders.GTC,
		Price: &price, Quantity: qty, IdempotencyKey: key, ActorType: security.ActorUser, ActorID: c.User.String(),
	}
}

// OpenMarginAccount opens a CROSS margin account for the customer, as the
// customer, with the sandbox dollar as its numeraire, in one transaction.
// Opening it creates the lending pool and the insurance fund it is wired to
// when they do not exist yet; holding collateral needs neither to be funded.
func (w *World) OpenMarginAccount(ctx context.Context, c Customer, mode gates.Mode) (margin.Account, error) {
	var acct margin.Account
	err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		acct, err = w.Margin.OpenAccount(ctx, tx, margin.OpenRequest{AccountID: c.Account, MarginMode: margin.ModeCross,
			Mode: mode, NumeraireAssetID: w.USD.ID, Actor: econ.Actor{Type: security.ActorUser, ID: c.User.String()}})
		return err
	})
	return acct, err
}

// PostCollateral moves qty of an asset from the customer's wallet into their
// margin account, as the customer, in one transaction.
func (w *World) PostCollateral(ctx context.Context, c Customer, asset assets.AssetID, qty money.Quantity, key string) (margin.CollateralResult, error) {
	var res margin.CollateralResult
	err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = w.Margin.PostCollateral(ctx, tx, margin.CollateralRequest{AccountID: c.Account, AssetID: asset,
			Quantity: qty, IdempotencyKey: key, Requester: margin.Requester{Type: security.ActorUser, ID: c.User.String()}})
		return err
	})
	return res, err
}

// StableFacts are the identity facts of a customer as a deployment resolves
// them, never as a request states them: the production identity reader (a
// verified email address, D-055) and the verification level that implies.
func (w *World) StableFacts(ctx context.Context, c Customer) (stableasset.AccountFacts, error) {
	verified, err := w.Identity.IdentityVerified(ctx, w.DB, c.Account)
	if err != nil {
		return stableasset.AccountFacts{}, err
	}
	return stableasset.AccountFacts{Verification: valuedomain.VerificationNodalIdentity, IdentityVerified: verified}, nil
}

// MintStable mints qty base units of a platform stable asset to the
// customer against the same value of the sandbox dollar paid into its
// reserve, as the customer, in SANDBOX mode: the service submits the mint
// once, produces blocks until it is final on the simulated chain, and posts
// the issuance only then.
func (w *World) MintStable(ctx context.Context, c Customer, a stableasset.PlatformStableAsset, qty money.Quantity, key string) (stableasset.MintResult, error) {
	facts, err := w.StableFacts(ctx, c)
	if err != nil {
		return stableasset.MintResult{}, err
	}
	return w.Stable.Mint(security.WithPrincipal(ctx, c.P), stableasset.MintRequest{
		StableAssetID: a.ID, AccountID: c.Account, Quantity: qty, ReserveAssetID: w.USD.ID, IdempotencyKey: key,
		Mode: gates.ModeSandbox, Actor: econ.Actor{Type: security.ActorUser, ID: c.User.String()}, Facts: facts,
	})
}

// OpenPredictionMarket defines a market and opens it, as its creator, each
// in its own transaction.
func (w *World) OpenPredictionMarket(ctx context.Context, spec predmarket.MarketSpec, creator econ.Actor) (predmarket.Market, error) {
	var m predmarket.Market
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		m, _, err = w.Predictions.CreateMarket(ctx, tx, spec, creator)
		return err
	}); err != nil {
		return predmarket.Market{}, err
	}
	err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		m, err = w.Predictions.Open(ctx, tx, m.ID, creator)
		return err
	})
	return m, err
}

// IssueCompleteSets buys sets complete sets of a market for the customer, as
// the customer, in one transaction: each set costs one unit of the market's
// collateral, paid into its escrow, and is one share of every outcome.
func (w *World) IssueCompleteSets(ctx context.Context, m predmarket.Market, c Customer, sets int64, key string) (predmarket.ActionResult, error) {
	var res predmarket.ActionResult
	err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = w.Predictions.Issue(ctx, tx, predmarket.PositionRequest{
			MarketID: m.ID, AccountID: c.Account, Quantity: predmarket.ShareUnit().Mul(money.QuantityFromInt64(sets)),
			IdempotencyKey: key, ActorType: security.ActorUser, ActorID: c.User.String(),
		})
		return err
	})
	return res, err
}
