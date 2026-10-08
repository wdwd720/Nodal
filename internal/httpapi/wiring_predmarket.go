package httpapi

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// PredictionMarketService and PredictionSandboxOracle name the prediction
// market types AdvancedDeps carries, so the shared wiring file needs no import
// of its own for them.
type (
	PredictionMarketService = predmarket.Service
	PredictionSandboxOracle = predmarket.SandboxOracle
)

// newPredmarketAdapter serves PredictionMarketsPort over the real service.
// The oracle is optional: a deployment without one answers
// PROVIDER_UNAVAILABLE wherever an oracle is needed. The service holds the
// same sandbox oracle and stores what it observes; the adapter only names it.
func newPredmarketAdapter(d WireDeps) PredictionMarketsPort {
	repo := d.Assets
	if repo == nil {
		repo = assets.NewRepository()
	}
	return predmarketAdapter{svc: d.Advanced.PredictionMarkets, oracle: d.Advanced.PredictionOracle, assets: repo, db: d.DB}
}

type predmarketAdapter struct {
	svc    *predmarket.Service
	oracle *predmarket.SandboxOracle
	assets *assets.Repository
	db     *db.DB
}

var _ PredictionMarketsPort = predmarketAdapter{}

// snapshot runs a multi-statement read against one consistent snapshot, so a
// response never mixes two versions of a market and its books.
func (a predmarketAdapter) snapshot(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, fn)
}

// write runs one service call in its own transaction. The service locks the
// market row it changes and is written for READ COMMITTED; every lifecycle
// operation is all or nothing inside it, and an error rolls everything back.
func (a predmarketAdapter) write(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

// keepingRefusal runs an issuance or a redemption and commits what it left
// behind even when it refuses: the service records a refused request as a
// REJECTED economic intent (and a risk refusal as its decision) in the
// caller's transaction, and documents that the caller commits to keep that
// record. Execution itself is all or nothing inside the service, so nothing
// else can survive a refusal. A retryable failure is rolled back and retried
// like any other.
func (a predmarketAdapter) keepingRefusal(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	var opErr error
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		opErr = fn(ctx, tx)
		if opErr != nil && db.IsRetryable(opErr) {
			return opErr
		}
		return nil
	})
	if err != nil && (opErr == nil || db.IsRetryable(opErr)) {
		return err
	}
	// The refusal is the answer, whether or not its record could be kept.
	return opErr
}

// collaterals resolves each market's collateral asset once per call.
type collaterals struct {
	repo *assets.Repository
	seen map[assets.AssetID]assets.Asset
}

func (a predmarketAdapter) collaterals() *collaterals {
	return &collaterals{repo: a.assets, seen: map[assets.AssetID]assets.Asset{}}
}

func (c *collaterals) record(ctx context.Context, q db.Querier, m predmarket.Market) (PredictionMarketRecord, error) {
	asset, ok := c.seen[m.CollateralAssetID]
	if !ok {
		var err error
		if asset, err = c.repo.Get(ctx, q, m.CollateralAssetID); err != nil {
			return PredictionMarketRecord{}, err
		}
		c.seen[m.CollateralAssetID] = asset
	}
	return PredictionMarketRecord{Market: m, Collateral: asset}, nil
}

func (a predmarketAdapter) record(ctx context.Context, q db.Querier, m predmarket.Market) (PredictionMarketRecord, error) {
	return a.collaterals().record(ctx, q, m)
}

// --- reads -------------------------------------------------------------------

func (a predmarketAdapter) Markets(ctx context.Context, status predmarket.MarketStatus, limit int) ([]PredictionMarketRecord, error) {
	var out []PredictionMarketRecord
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		markets, err := a.svc.ListByStatus(ctx, tx, status, limit)
		if err != nil {
			return err
		}
		c := a.collaterals()
		out = make([]PredictionMarketRecord, 0, len(markets))
		for _, m := range markets {
			rec, err := c.record(ctx, tx, m)
			if err != nil {
				return err
			}
			out = append(out, rec)
		}
		return nil
	})
	return out, err
}

func (a predmarketAdapter) Market(ctx context.Context, id predmarket.MarketID) (PredictionMarketDetailView, error) {
	var v PredictionMarketDetailView
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		m, err := a.svc.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		if v.Record, err = a.record(ctx, tx, m); err != nil {
			return err
		}
		if v.Resolutions, err = a.svc.Resolutions(ctx, tx, id); err != nil {
			return err
		}
		if v.Disputes, err = a.svc.Disputes(ctx, tx, id); err != nil {
			return err
		}
		if v.VoidProposals, err = a.svc.VoidProposals(ctx, tx, id); err != nil {
			return err
		}
		st, found, err := a.svc.SettlementOf(ctx, tx, id)
		if err != nil {
			return err
		}
		if found {
			v.Settlement = &st
		}
		return nil
	})
	return v, err
}

func (a predmarketAdapter) Books(ctx context.Context, id predmarket.MarketID) (PredictionBooksView, error) {
	var v PredictionBooksView
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		m, err := a.svc.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		if v.Record, err = a.record(ctx, tx, m); err != nil {
			return err
		}
		if v.Books, err = a.svc.Books(ctx, tx, m); err != nil {
			return err
		}
		// The residual is the one the market's settlement record discloses:
		// a refund's, and zero after a payout (which leaves none) or before
		// any settlement.
		st, found, err := a.svc.SettlementOf(ctx, tx, id)
		if err != nil {
			return err
		}
		v.Residual = money.Quantity{}
		if found {
			v.Residual = st.Residual
		}
		v.Violations = v.Books.Violations(v.Residual)
		return nil
	})
	return v, err
}

func (a predmarketAdapter) Positions(ctx context.Context, id predmarket.MarketID, account accounts.AccountID) (PredictionPositionsView, error) {
	var v PredictionPositionsView
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		m, err := a.svc.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		if v.Record, err = a.record(ctx, tx, m); err != nil {
			return err
		}
		all, err := a.svc.Positions(ctx, tx, id)
		if err != nil {
			return err
		}
		// The projection is the market's; this read is one account's.
		v.Positions = make([]predmarket.Position, 0, len(m.Outcomes))
		for _, p := range all {
			if p.AccountID == account {
				v.Positions = append(v.Positions, p)
			}
		}
		held, found, err := a.svc.HeldPayoutOf(ctx, tx, id, account)
		if err != nil {
			return err
		}
		if found {
			v.HeldPayout = &held
		}
		return nil
	})
	return v, err
}

func (a predmarketAdapter) Oracle(context.Context) (PredictionOracleView, error) {
	if a.oracle == nil {
		return PredictionOracleView{}, errNoOracle()
	}
	return PredictionOracleView{Source: a.oracle.Source(), PublicKey: a.oracle.PublicKey()}, nil
}

// errNoOracle is the deployment's answer wherever an oracle is needed and none
// is configured: the provider is not there, which is not a refusal of the
// request.
func errNoOracle() error {
	return errs.New(errs.CodeProviderUnavailable, "no prediction market oracle is configured in this deployment")
}

// --- the customer's complete sets -----------------------------------------------

func (a predmarketAdapter) Issue(ctx context.Context, req predmarket.PositionRequest) (predmarket.ActionResult, error) {
	var out predmarket.ActionResult
	err := a.keepingRefusal(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.svc.Issue(ctx, tx, req)
		return err
	})
	return out, err
}

func (a predmarketAdapter) Redeem(ctx context.Context, req predmarket.PositionRequest) (predmarket.ActionResult, error) {
	var out predmarket.ActionResult
	err := a.keepingRefusal(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.svc.Redeem(ctx, tx, req)
		return err
	})
	return out, err
}

// --- the operator's lifecycle ---------------------------------------------------

func (a predmarketAdapter) Create(ctx context.Context, spec predmarket.MarketSpec, by econ.Actor) (PredictionMarketRecord, bool, error) {
	var (
		out      PredictionMarketRecord
		existing bool
	)
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// The sandbox dollar is provisioned the one way every caller
		// provisions it; a refused creation rolls the provisioning back too.
		if spec.CollateralAssetID == econ.SandboxDollarID() {
			if _, err := econ.EnsureSandboxDollar(ctx, tx); err != nil {
				return err
			}
		}
		m, ex, err := a.svc.CreateMarket(ctx, tx, spec, by)
		if err != nil {
			return err
		}
		existing = ex
		out, err = a.record(ctx, tx, m)
		return err
	})
	return out, existing, err
}

// step runs one lifecycle step and reports the market after it.
func (a predmarketAdapter) step(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) (predmarket.Market, error)) (PredictionMarketRecord, error) {
	var out PredictionMarketRecord
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		m, err := fn(ctx, tx)
		if err != nil {
			return err
		}
		out, err = a.record(ctx, tx, m)
		return err
	})
	return out, err
}

func (a predmarketAdapter) Open(ctx context.Context, id predmarket.MarketID, by econ.Actor) (PredictionMarketRecord, error) {
	return a.step(ctx, func(ctx context.Context, tx pgx.Tx) (predmarket.Market, error) { return a.svc.Open(ctx, tx, id, by) })
}

func (a predmarketAdapter) Close(ctx context.Context, id predmarket.MarketID, by econ.Actor) (PredictionMarketRecord, error) {
	return a.step(ctx, func(ctx context.Context, tx pgx.Tx) (predmarket.Market, error) { return a.svc.Close(ctx, tx, id, by) })
}

func (a predmarketAdapter) RequestResolution(ctx context.Context, id predmarket.MarketID, by econ.Actor) (PredictionMarketRecord, error) {
	return a.step(ctx, func(ctx context.Context, tx pgx.Tx) (predmarket.Market, error) {
		return a.svc.RequestResolution(ctx, tx, id, by)
	})
}

// Observe records, as the requesting operator, what the sandbox oracle saw
// for a market: the service signs it and stores it, with its audit event, in
// this transaction, so every replica resolves from the same observation.
func (a predmarketAdapter) Observe(ctx context.Context, req predmarket.ObservationRequest) (predmarket.ObservationResult, error) {
	var out predmarket.ObservationResult
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.svc.RecordObservation(ctx, tx, req)
		return err
	})
	return out, err
}

// Resolve fetches the market's report -- the latest stored sandbox observation,
// verified, or an external oracle's, which is asked outside any transaction --
// and hands it to the service, which verifies it against the market's
// registered key again before accepting it.
func (a predmarketAdapter) Resolve(ctx context.Context, cmd PredictionResolveCommand) (PredictionResolveView, error) {
	report, err := a.svc.FetchReport(ctx, a.db, cmd.MarketID)
	if err != nil {
		return PredictionResolveView{}, err
	}
	var v PredictionResolveView
	err = a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		res, err := a.svc.Resolve(ctx, tx, predmarket.ResolveRequest{
			MarketID: cmd.MarketID, Report: report, Actor: cmd.Actor, Reason: cmd.Reason,
		})
		if err != nil {
			return err
		}
		v.Resolution, v.Replay = res.Resolution, res.Replay
		v.Record, err = a.record(ctx, tx, res.Market)
		return err
	})
	return v, err
}

func (a predmarketAdapter) Dispute(ctx context.Context, req predmarket.DisputeRequest) (PredictionDisputeView, error) {
	var v PredictionDisputeView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d, m, err := a.svc.Dispute(ctx, tx, req)
		if err != nil {
			return err
		}
		v.Dispute = d
		v.Record, err = a.record(ctx, tx, m)
		return err
	})
	return v, err
}

func (a predmarketAdapter) Uphold(ctx context.Context, req predmarket.RulingRequest) (PredictionResolveView, error) {
	var v PredictionResolveView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		res, err := a.svc.UpholdResolution(ctx, tx, req)
		if err != nil {
			return err
		}
		v.Resolution, v.Replay = res.Resolution, res.Replay
		v.Record, err = a.record(ctx, tx, res.Market)
		return err
	})
	return v, err
}

// ProposeVoid records one operator's proposal to void a market; nothing is
// refunded until another operator approves it.
func (a predmarketAdapter) ProposeVoid(ctx context.Context, req predmarket.VoidProposalRequest) (PredictionVoidProposalView, error) {
	var v PredictionVoidProposalView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		p, err := a.svc.ProposeVoid(ctx, tx, req)
		if err != nil {
			return err
		}
		v.Proposal = p
		m, err := a.svc.Get(ctx, tx, req.MarketID)
		if err != nil {
			return err
		}
		v.Record, err = a.record(ctx, tx, m)
		return err
	})
	return v, err
}

// ApproveVoid approves a void proposal, which voids the market and refunds
// its shares in the same transaction.
func (a predmarketAdapter) ApproveVoid(ctx context.Context, req predmarket.VoidApprovalRequest) (PredictionSettleView, error) {
	return a.settlement(ctx, func(ctx context.Context, tx pgx.Tx) (predmarket.SettleResult, error) {
		return a.svc.ApproveVoid(ctx, tx, req)
	})
}

// Claim delivers a held payout to its holder. Like an issuance, a refused
// claim keeps its REJECTED intent (and risk decision) on record.
func (a predmarketAdapter) Claim(ctx context.Context, req predmarket.ClaimRequest) (predmarket.ClaimResult, error) {
	var out predmarket.ClaimResult
	err := a.keepingRefusal(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.svc.ClaimHeldPayout(ctx, tx, req)
		return err
	})
	return out, err
}

func (a predmarketAdapter) Settle(ctx context.Context, req predmarket.SettleRequest) (PredictionSettleView, error) {
	return a.settlement(ctx, func(ctx context.Context, tx pgx.Tx) (predmarket.SettleResult, error) {
		return a.svc.Settle(ctx, tx, req)
	})
}

func (a predmarketAdapter) settlement(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) (predmarket.SettleResult, error)) (PredictionSettleView, error) {
	var v PredictionSettleView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		res, err := fn(ctx, tx)
		if err != nil {
			return err
		}
		v.Settlement, v.Plan, v.Lines, v.Held, v.Replay = res.Settlement, res.Plan, res.Lines, res.Held, res.Replay
		v.Record, err = a.record(ctx, tx, res.Market)
		return err
	})
	return v, err
}

// Reconcile runs the PREDICTION_ESCROW comparison on an operator's request.
// It compares the books and records what it found; it repairs nothing.
func (a predmarketAdapter) Reconcile(ctx context.Context, id predmarket.MarketID, by reconciliation.Actor) (reconciliation.Record, error) {
	var out reconciliation.Record
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.svc.ReconcileEscrow(ctx, tx, predmarket.ReconcileRequest{
			MarketID: id, Mode: reconciliation.ModeFull, Actor: by,
			Reason: "operator-requested prediction market escrow reconciliation",
		})
		return err
	})
	return out, err
}
