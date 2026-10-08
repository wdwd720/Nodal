package margin

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
)

// EvaluateResult is an account's margin state after a mark-to-market.
type EvaluateResult struct {
	MarginAccountID AccountID
	AccountID       accounts.AccountID
	Previous        AccountStatus
	Status          AccountStatus
	Evaluation      Evaluation
	// Call is the open (or just resolved) margin call, if any.
	Call *Call
}

// Evaluate marks an account to market and moves it between NORMAL, WARNING
// and MARGIN_CALL: WARNING when equity falls below maintenance plus the
// warning buffer, MARGIN_CALL -- with a margin call issued -- when it falls
// below maintenance, which also makes it eligible for liquidation; back to
// WARNING or NORMAL, with the call MET, when it recovers. An account whose
// case is open keeps LIQUIDATING: the case decides. Evaluating reduces no
// risk and adds none, so a kill switch does not stop it.
func (s *Service) Evaluate(ctx context.Context, tx pgx.Tx, acct accounts.AccountID) (EvaluateResult, error) {
	a, err := s.lockAccount(ctx, tx, acct)
	if err != nil {
		return EvaluateResult{}, err
	}
	if err := s.authorize(ctx, tx, a.Mode, true, gates.Margin); err != nil {
		return EvaluateResult{}, err
	}
	prev := a.Status
	ev, call, err := s.refreshHealth(ctx, tx, &a, "marked to market")
	if err != nil {
		return EvaluateResult{}, err
	}
	return EvaluateResult{MarginAccountID: a.ID, AccountID: a.AccountID, Previous: prev, Status: a.Status, Evaluation: ev, Call: call}, nil
}

// MarkToMarket evaluates up to limit accounts in NORMAL, WARNING or
// MARGIN_CALL, in id order, each in its own savepoint so one account's
// failure does not stop the others. It is what feeds the liquidation queue.
func (s *Service) MarkToMarket(ctx context.Context, tx pgx.Tx, limit int) ([]EvaluateResult, []error, error) {
	if limit <= 0 || limit > s.cfg.MaxQueueScan {
		limit = s.cfg.MaxQueueScan
	}
	if err := bindingsImmediate(ctx, tx); err != nil {
		return nil, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT account_id FROM margin_accounts WHERE status IN ('NORMAL','WARNING','MARGIN_CALL')
		AND policy_version = $1 ORDER BY id LIMIT $2`, s.policy.Version, limit)
	if err != nil {
		return nil, nil, dbErr("list accounts to mark", err)
	}
	var ids []accounts.AccountID
	for rows.Next() {
		var id accounts.AccountID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, dbErr("scan account to mark", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, dbErr("list accounts to mark", err)
	}
	var out []EvaluateResult
	var failures []error
	for _, id := range ids {
		r, err := withSavepoint(ctx, tx, func(sp pgx.Tx) (EvaluateResult, error) { return s.Evaluate(ctx, sp, id) })
		if err != nil {
			failures = append(failures, errs.Wrap(err, errs.CodeOf(err), "margin: mark "+id.String()))
			continue
		}
		out = append(out, r)
	}
	return out, failures, nil
}

// withSavepoint runs fn in a savepoint of tx, rolling it back on error.
func withSavepoint[T any](ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) (T, error)) (T, error) {
	var zero T
	sp, err := tx.Begin(ctx)
	if err != nil {
		return zero, dbErr("open savepoint", err)
	}
	v, err := fn(sp)
	if err != nil {
		_ = sp.Rollback(ctx)
		return zero, err
	}
	if err := sp.Commit(ctx); err != nil {
		return zero, dbErr("release savepoint", err)
	}
	return v, nil
}
