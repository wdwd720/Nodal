package orders

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// PipelineMaxRetries is the deadlock and serialization retry budget of each
// stage transaction. A retried stage is safe: every stage is idempotent, and
// an engine that applied a command in a transaction that then rolled back is
// rebuilt from the journal by the next command.
const PipelineMaxRetries = 5

// Pipeline drives the stages of a customer submission, each in its own READ
// COMMITTED transaction, the way ledger.Service.PostInTx drives a posting. Its
// methods return a recorded refusal as their error only after the refusal
// was committed, so the refusal is durable whatever the caller does next.
type Pipeline struct {
	svc *Service
	db  *db.DB
}

// NewPipeline returns a Pipeline over svc and d.
func NewPipeline(svc *Service, d *db.DB) (*Pipeline, error) {
	if svc == nil || d == nil {
		return nil, errs.New(errs.CodeValidationFailed, "orders: a pipeline needs the service and the database")
	}
	return &Pipeline{svc: svc, db: d}, nil
}

// Service returns the service the pipeline drives.
func (p *Pipeline) Service() *Service { return p.svc }

func (p *Pipeline) inTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return p.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: PipelineMaxRetries}, fn)
}

// Submit runs Accept, Approve, Reserve, Execute and Complete. It returns the
// order as the last stage left it and, if a stage refused it, that refusal.
func (p *Pipeline) Submit(ctx context.Context, r OrderRequest) (StageOutcome, error) {
	var out StageOutcome
	if err := p.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		o, err := p.svc.Accept(ctx, tx, r)
		out = o
		return err
	}); err != nil {
		return StageOutcome{}, err
	}
	if out.Rejection != nil {
		return out, out.Rejection
	}
	return p.Resume(ctx, out.Order.ID)
}

// Resume continues a submission from whichever stage it stopped at. Stages
// already passed are no-ops.
func (p *Pipeline) Resume(ctx context.Context, oid OrderID) (StageOutcome, error) {
	stages := []func(context.Context, pgx.Tx, OrderID) (StageOutcome, error){
		p.svc.Approve, p.svc.Reserve, p.svc.Execute, p.svc.Complete,
	}
	var out, executed StageOutcome
	for i, stage := range stages {
		if err := p.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			o, err := stage(ctx, tx, oid)
			out = o
			return err
		}); err != nil {
			return StageOutcome{}, err
		}
		if i == 2 {
			executed = out
		}
		if out.Rejection != nil {
			return out, out.Rejection
		}
	}
	out.CommandSeq, out.Trades, out.StateHash = executed.CommandSeq, executed.Trades, executed.StateHash
	return out, nil
}

// Cancel cancels a customer order in its own transaction.
func (p *Pipeline) Cancel(ctx context.Context, r CancelRequest) (CommandOutcome, error) {
	return p.command(ctx, func(ctx context.Context, tx pgx.Tx) (CommandOutcome, error) { return p.svc.Cancel(ctx, tx, r) })
}

// Replace replaces a customer order in its own transaction.
func (p *Pipeline) Replace(ctx context.Context, r ReplaceRequest) (CommandOutcome, error) {
	return p.command(ctx, func(ctx context.Context, tx pgx.Tx) (CommandOutcome, error) { return p.svc.Replace(ctx, tx, r) })
}

// SubmitHouse places a house order in its own transaction.
func (p *Pipeline) SubmitHouse(ctx context.Context, r HouseOrderRequest) (CommandOutcome, error) {
	return p.command(ctx, func(ctx context.Context, tx pgx.Tx) (CommandOutcome, error) {
		return p.svc.SubmitHouseOrder(ctx, tx, r)
	})
}

func (p *Pipeline) command(ctx context.Context, fn func(context.Context, pgx.Tx) (CommandOutcome, error)) (CommandOutcome, error) {
	var out CommandOutcome
	if err := p.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		o, err := fn(ctx, tx)
		out = o
		return err
	}); err != nil {
		return CommandOutcome{}, err
	}
	if out.Rejection != nil {
		return out, out.Rejection
	}
	return out, nil
}
