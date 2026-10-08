package delegation

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// ScopeDelegatedTransaction is the reconciliation scope type of one
// delegated swap's or withdrawal's chain transaction.
const ScopeDelegatedTransaction = "delegated_transaction"

// openChainMismatch records, as a material DEVNET_CHAIN reconciliation that
// blocks new risk, that the chain finalized something other than the signed
// transaction under its id. Nothing is posted for it.
func (s *Service) openChainMismatch(ctx context.Context, tx pgx.Tx, kind SubjectKind, subject string, account accounts.AccountID,
	chain devnet.ChainID, txID devnet.TxID) error {
	if _, found, err := s.recon.FindUnresolvedByScopeForUpdate(ctx, tx, reconciliation.KindDevnetChain, ScopeDelegatedTransaction, subject); err != nil {
		return err
	} else if found {
		return nil
	}
	_, err := s.recon.Open(ctx, tx, reconciliation.OpenRequest{
		Kind: reconciliation.KindDevnetChain, Mode: reconciliation.ModeEventDriven, ScopeType: ScopeDelegatedTransaction,
		ScopeID: subject, AccountID: account,
		Expected:   map[string]string{"kind": string(kind), "chain": string(chain), "tx_id": string(txID), "transaction": "the signed transaction"},
		Observed:   map[string]string{"transaction": "a different or non-final transaction under the same id"},
		Difference: map[string]string{"posted": "nothing"},
		Status:     reconciliation.StatusMismatch, Material: true, BlocksNewRisk: true,
		Actor:  reconciliation.Actor{Type: serviceActor.Type, ID: serviceActor.ID},
		Reason: "the finalized transaction does not match the signed one",
	})
	return err
}

// ExpireStaleSwap ends a swap that was admitted but can no longer be signed:
// its quote expired, or its capability, account or agent no longer stands.
// The funds return and the intent is REJECTED. Anything else is unchanged.
func (s *Service) ExpireStaleSwap(ctx context.Context, tx pgx.Tx, sid SwapID) (Swap, error) {
	if err := requireExecutor(ctx); err != nil {
		return Swap{}, err
	}
	sw, err := getSwap(ctx, tx, sid, true)
	if err != nil {
		return Swap{}, err
	}
	if sw.Status != SwapAdmitted && sw.Status != SwapAwaitingApproval && sw.Status != SwapApproved {
		return sw, nil
	}
	now := dbTime(s.clk.Now())
	code, detail, err := s.swapStanding(ctx, tx, sw, now)
	if err != nil {
		return Swap{}, err
	}
	if code == "" && !now.Before(sw.QuoteExpiresAt) {
		code, detail = ReasonQuoteExpired, "the venue's quote expired before the swap was signed"
	}
	if code == "" {
		return sw, nil
	}
	return s.rejectSwap(ctx, tx, sw, code, detail)
}

// ExpireStaleWithdrawal ends a withdrawal that was proposed but can no
// longer be signed: its approval deadline passed while it waited, or its
// capability, account, agent or destination no longer stands.
func (s *Service) ExpireStaleWithdrawal(ctx context.Context, tx pgx.Tx, wid WithdrawalID) (Withdrawal, error) {
	if err := requireExecutor(ctx); err != nil {
		return Withdrawal{}, err
	}
	w, err := getWithdrawal(ctx, tx, wid, true)
	if err != nil {
		return Withdrawal{}, err
	}
	if w.Status != WithdrawalAwaitingApproval && w.Status != WithdrawalApproved {
		return w, nil
	}
	now := dbTime(s.clk.Now())
	code, detail, err := s.withdrawalStanding(ctx, tx, w, now)
	if err != nil {
		return Withdrawal{}, err
	}
	if code == "" && w.Status == WithdrawalAwaitingApproval && !now.Before(w.ApprovalExpiresAt) {
		code, detail = ReasonApprovalExpired, "nobody decided the withdrawal before its deadline"
	}
	if code == "" {
		return w, nil
	}
	return s.rejectWithdrawal(ctx, tx, w, code, detail)
}

// ReconcilePending is the reconciliation sweep of delegated authority: it
// ends the pending swaps and withdrawals past their deadline (DueSwaps,
// DueWithdrawals: a swap's venue quote, a withdrawal's approval window),
// oldest deadline first, and observes every signed, submitted or UNKNOWN one
// by transaction id -- settling, failing or leaving it -- without ever
// resubmitting anything. A pending subject whose standing lapsed before its
// deadline is ended by the upkeep's sweep (PendingSwaps, PendingWithdrawals
// with ExpireStaleSwap / ExpireStaleWithdrawal) or refused when it is
// executed; taking the oldest pending subjects instead would let ones that
// stand -- a withdrawal cooling for days -- fill every pass. When produce is
// set (a SANDBOX driver) it produces the blocks a submitted transaction's
// finality needs. It returns how many it looked at.
func (s *Service) ReconcilePending(ctx context.Context, limit int, produce bool) (int, error) {
	if err := requireExecutor(ctx); err != nil {
		return 0, err
	}
	ctx = serviceContext(ctx)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	seen := 0
	now := dbTime(s.clk.Now())
	pendingSwaps, err := s.DueSwaps(ctx, s.db, now, limit)
	if err != nil {
		return 0, err
	}
	for _, sid := range pendingSwaps {
		if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := s.ExpireStaleSwap(ctx, tx, sid); return err }); err != nil {
			return seen, err
		}
		seen++
	}
	pendingWithdrawals, err := s.DueWithdrawals(ctx, s.db, now, limit)
	if err != nil {
		return seen, err
	}
	for _, wid := range pendingWithdrawals {
		if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := s.ExpireStaleWithdrawal(ctx, tx, wid)
			return err
		}); err != nil {
			return seen, err
		}
		seen++
	}
	signedSwaps, err := listSwapIDs(ctx, s.db, `status IN ('SIGNED','SUBMITTED','UNKNOWN') ORDER BY created_at, id LIMIT $1`, limit)
	if err != nil {
		return seen, err
	}
	for _, sid := range signedSwaps {
		sw, err := getSwap(ctx, s.db, sid, false)
		if err != nil {
			return seen, err
		}
		stale := sw.SignedAt != nil && !s.clk.Now().Before(sw.SignedAt.Add(s.params.QuoteTTL))
		if _, err := s.driveSwap(ctx, sid, produce && sw.Mode == gates.ModeSandbox, stale); err != nil && !errs.HasCode(err, errs.CodeReconciliationRequired) {
			return seen, err
		}
		seen++
	}
	signedWithdrawals, err := listWithdrawalIDs(ctx, s.db, `status IN ('SIGNED','SUBMITTED','UNKNOWN') ORDER BY created_at, id LIMIT $1`, limit)
	if err != nil {
		return seen, err
	}
	for _, wid := range signedWithdrawals {
		w, err := getWithdrawal(ctx, s.db, wid, false)
		if err != nil {
			return seen, err
		}
		stale := w.SignedAt != nil && !s.clk.Now().Before(w.SignedAt.Add(s.params.QuoteTTL))
		if _, err := s.driveWithdrawal(ctx, wid, produce && w.Mode == gates.ModeSandbox, stale); err != nil && !errs.HasCode(err, errs.CodeReconciliationRequired) {
			return seen, err
		}
		seen++
	}
	return seen, nil
}

// --- Selection -----------------------------------------------------------------------

// maxSelection bounds one selection of subjects.
const maxSelection = 500

func selectionLimit(limit, def int) int {
	if limit <= 0 || limit > maxSelection {
		return def
	}
	return limit
}

// ExecutableSubject is a swap or a withdrawal an execution run may carry out
// now (ExecutableSubjects).
type ExecutableSubject struct {
	Kind SubjectKind
	// SwapID is set for a SWAP, WithdrawalID for a WITHDRAWAL.
	SwapID       SwapID
	WithdrawalID WithdrawalID
	AccountID    accounts.AccountID
	Status       string
	// Due bounds when the subject may be carried out: a swap must be signed
	// before its venue quote expires at Due; a withdrawal may be signed from
	// Due, the end of its cooling period.
	Due time.Time
}

// ID is the subject's id in canonical form.
func (e ExecutableSubject) ID() string {
	if e.Kind == SubjectSwap {
		return e.SwapID.String()
	}
	return e.WithdrawalID.String()
}

// executableSQL is ExecutableSubjects: $1 now, $2 the per-account cap, $3 the
// limit. A subject's turn is its place among its own account's, most urgent
// first (urgency 0: a swap, whose quote expires; 1: a withdrawal, which does
// not), so ordering by turn first serves every account's first subject
// before any account's second.
const executableSQL = `SELECT kind, id::text, account_id, status, due FROM (
	SELECT kind, id, account_id, status, due, urgency,
	       row_number() OVER (PARTITION BY account_id ORDER BY urgency, due, id) AS turn
	  FROM (SELECT 'SWAP' AS kind, id, account_id, status, quote_expires_at AS due, 0 AS urgency
	          FROM delegated_swaps WHERE status IN ('ADMITTED','APPROVED') AND quote_expires_at > $1
	        UNION ALL
	        SELECT 'WITHDRAWAL', id, account_id, status, executable_at, 1
	          FROM delegated_withdrawals WHERE status = 'APPROVED' AND executable_at <= $1) AS ready) AS ranked
	WHERE turn <= $2 ORDER BY turn, urgency, due, id LIMIT $3`

// ExecutableSubjects selects at most limit (1..500; 50 otherwise) subjects an
// execution run may carry out now -- the swaps that need no approval
// (ADMITTED) or have it (APPROVED) and the APPROVED withdrawals -- fairly
// across accounts:
//
//   - Only what can be signed now. A swap whose venue quote has expired and a
//     withdrawal still in its cooling period are never taken: neither can be
//     signed, so neither may hold a place in a run (the upkeep ends the swap;
//     the withdrawal waits for its time).
//   - Fairly. The accounts take turns -- every account's first subject before
//     any account's second -- and one run takes at most
//     Params.ExecutablePerAccount subjects of one account, so no account can
//     fill a run, however many subjects it has.
//   - Most urgent first within a turn: swaps before withdrawals, the quote
//     that expires soonest first, so an admitted swap is carried out before
//     its quote expires; then withdrawals, the longest due first.
//
// It decides nothing: the service decides each subject again when it
// executes it.
func (s *Service) ExecutableSubjects(ctx context.Context, q db.Querier, limit int) ([]ExecutableSubject, error) {
	limit = selectionLimit(limit, 50)
	rows, err := q.Query(ctx, executableSQL, dbTime(s.clk.Now()), s.params.ExecutablePerAccount, limit)
	if err != nil {
		return nil, dbErr("select executable subjects", err)
	}
	defer rows.Close()
	var out []ExecutableSubject
	for rows.Next() {
		var e ExecutableSubject
		var subject string
		if err := rows.Scan(&e.Kind, &subject, &e.AccountID, &e.Status, &e.Due); err != nil {
			return nil, dbErr("scan executable subject", err)
		}
		switch e.Kind {
		case SubjectSwap:
			e.SwapID, err = ParseSwapID(subject)
		case SubjectWithdrawal:
			e.WithdrawalID, err = ParseWithdrawalID(subject)
		default:
			err = errs.New(errs.CodeInternal, "delegation: an executable subject of unknown kind "+string(e.Kind))
		}
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "delegation: a stored subject id does not parse")
		}
		e.Due = e.Due.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// The expiry candidates. An expiry pass looks at two kinds of pending
// subject: those past their deadline, which ExpireStaleSwap and
// ExpireStaleWithdrawal certainly end, oldest deadline first (every one it
// takes leaves the set, so none waits behind another for long); and every
// other pending subject in turn, a paged sweep in id order, which finds those
// whose capability, account, agent, destination, gate or kill switch no
// longer stands. The status sets are exactly the ones those two act on. Each
// is the WHERE clause of listSwapIDs / listWithdrawalIDs.
const (
	dueSwapsWhere = `status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED') AND quote_expires_at <= $1
		ORDER BY quote_expires_at, id LIMIT $2`
	pendingSwapsWhere = `status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED') AND ($1::uuid IS NULL OR id > $1::uuid)
		ORDER BY id LIMIT $2`
	dueWithdrawalsWhere = `status = 'AWAITING_APPROVAL' AND approval_expires_at <= $1
		ORDER BY approval_expires_at, id LIMIT $2`
	pendingWithdrawalsWhere = `status IN ('AWAITING_APPROVAL','APPROVED') AND ($1::uuid IS NULL OR id > $1::uuid)
		ORDER BY id LIMIT $2`
)

// DueSwaps returns at most limit (1..500; 100 otherwise) pending swaps
// (ADMITTED, AWAITING_APPROVAL, APPROVED) whose venue quote expired at or
// before now, oldest deadline first.
func (s *Service) DueSwaps(ctx context.Context, q db.Querier, now time.Time, limit int) ([]SwapID, error) {
	return listSwapIDs(ctx, q, dueSwapsWhere, dbTime(now), selectionLimit(limit, 100))
}

// PendingSwaps returns at most limit (1..500; 100 otherwise) pending swaps
// after the paging key after (the zero id: from the first), in id order.
func (s *Service) PendingSwaps(ctx context.Context, q db.Querier, after SwapID, limit int) ([]SwapID, error) {
	return listSwapIDs(ctx, q, pendingSwapsWhere, after, selectionLimit(limit, 100))
}

// DueWithdrawals returns at most limit (1..500; 100 otherwise) withdrawals
// still waiting for the owner's decision when their approval window closed
// at or before now, oldest deadline first.
func (s *Service) DueWithdrawals(ctx context.Context, q db.Querier, now time.Time, limit int) ([]WithdrawalID, error) {
	return listWithdrawalIDs(ctx, q, dueWithdrawalsWhere, dbTime(now), selectionLimit(limit, 100))
}

// PendingWithdrawals returns at most limit (1..500; 100 otherwise) pending
// withdrawals (AWAITING_APPROVAL, APPROVED) after the paging key after (the
// zero id: from the first), in id order.
func (s *Service) PendingWithdrawals(ctx context.Context, q db.Querier, after WithdrawalID, limit int) ([]WithdrawalID, error) {
	return listWithdrawalIDs(ctx, q, pendingWithdrawalsWhere, after, selectionLimit(limit, 100))
}
