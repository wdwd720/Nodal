package httpapi

import (
	"context"
	"crypto/ed25519"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// Prediction markets (internal/predmarket, ADR-0020). Every figure behind this
// port is value domain SIMULATED: the collateral is the sandbox dollar and the
// shares are sandbox assets. The port translates nothing and decides nothing;
// the service does both, and the handlers only change shapes.

// PredictionMarketRecord is a market as the API reports it: the service's
// market and the collateral asset its amounts are counted in.
type PredictionMarketRecord struct {
	Market     predmarket.Market
	Collateral assets.Asset
}

// PredictionMarketDetailView is one market with its resolution history, the
// disputes raised on it and its settlement record, read from one snapshot.
type PredictionMarketDetailView struct {
	Record      PredictionMarketRecord
	Resolutions []predmarket.Resolution
	Disputes    []predmarket.Dispute
	// Settlement is nil until the market settles or is voided.
	Settlement *predmarket.Settlement
}

// PredictionBooksView is a market's escrow books with the residual its
// settlement record discloses and every way the books break the escrow
// identity (none when they reconcile), as the service states it.
type PredictionBooksView struct {
	Record     PredictionMarketRecord
	Books      predmarket.EscrowBooks
	Residual   money.Quantity
	Violations []string
}

// PredictionPositionsView is one account's outcome shares in one market.
type PredictionPositionsView struct {
	Record    PredictionMarketRecord
	Positions []predmarket.Position
}

// PredictionOracleView is the oracle a deployment resolves markets from.
type PredictionOracleView struct {
	Source    string
	PublicKey ed25519.PublicKey
}

// PredictionResolveCommand asks for a market to be resolved from the
// configured oracle's report.
type PredictionResolveCommand struct {
	MarketID predmarket.MarketID
	Actor    econ.Actor
	Reason   string
}

// PredictionResolveView is a market after a resolution step.
type PredictionResolveView struct {
	Record     PredictionMarketRecord
	Resolution predmarket.Resolution
	Replay     bool
}

// PredictionDisputeView is a market after a dispute was raised on it.
type PredictionDisputeView struct {
	Record  PredictionMarketRecord
	Dispute predmarket.Dispute
}

// PredictionSettleView is a market after its settlement or refund, with what
// the call paid (nothing on a replay).
type PredictionSettleView struct {
	Record     PredictionMarketRecord
	Settlement predmarket.Settlement
	Plan       predmarket.SettlementPlan
	Lines      []predmarket.ActionResult
	Replay     bool
}

// PredictionMarketsPort serves prediction markets: the public reads of a
// market and its escrow, an account's shares, the customer's complete-set
// issuance and redemption, and the operator's lifecycle, resolution and
// settlement actions. Each method is one service call in its own
// transaction; the service enforces every rule, dual control included.
type PredictionMarketsPort interface {
	Markets(ctx context.Context, status predmarket.MarketStatus, limit int) ([]PredictionMarketRecord, error)
	Market(ctx context.Context, id predmarket.MarketID) (PredictionMarketDetailView, error)
	Books(ctx context.Context, id predmarket.MarketID) (PredictionBooksView, error)
	Positions(ctx context.Context, id predmarket.MarketID, account accounts.AccountID) (PredictionPositionsView, error)
	Oracle(ctx context.Context) (PredictionOracleView, error)

	Issue(ctx context.Context, req predmarket.PositionRequest) (predmarket.ActionResult, error)
	Redeem(ctx context.Context, req predmarket.PositionRequest) (predmarket.ActionResult, error)

	Create(ctx context.Context, spec predmarket.MarketSpec, by econ.Actor) (PredictionMarketRecord, bool, error)
	Open(ctx context.Context, id predmarket.MarketID, by econ.Actor) (PredictionMarketRecord, error)
	Close(ctx context.Context, id predmarket.MarketID, by econ.Actor) (PredictionMarketRecord, error)
	RequestResolution(ctx context.Context, id predmarket.MarketID, by econ.Actor) (PredictionMarketRecord, error)
	Observe(ctx context.Context, id predmarket.MarketID, outcome string, observedAt time.Time, evidence []byte) (predmarket.OracleReport, error)
	Resolve(ctx context.Context, cmd PredictionResolveCommand) (PredictionResolveView, error)
	Dispute(ctx context.Context, req predmarket.DisputeRequest) (PredictionDisputeView, error)
	Uphold(ctx context.Context, req predmarket.RulingRequest) (PredictionResolveView, error)
	Void(ctx context.Context, req predmarket.VoidRequest) (PredictionSettleView, error)
	Settle(ctx context.Context, req predmarket.SettleRequest) (PredictionSettleView, error)
	Reconcile(ctx context.Context, id predmarket.MarketID, by reconciliation.Actor) (reconciliation.Record, error)
}
