package delegation

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// ScopeDelegatedTransaction is the reconciliation scope type of one
// delegated swap's or withdrawal's chain transaction.
const ScopeDelegatedTransaction = "delegated_transaction"

// ReasonNetworkUnrecorded ends a pending swap or withdrawal admitted before
// the network of a delegated subject was recorded (migration 00760) and that
// the migration could not attribute: no service can tell which chain it
// would be signed for, so none signs it, and its funds are released.
const ReasonNetworkUnrecorded = "DELEGATION_NETWORK_UNRECORDED"

// Network is the devnet network this service's host carries: the one its
// swaps and withdrawals are admitted, signed, submitted and observed on.
func (s *admitter) Network() string { return s.chain.Config().Network }

// requireNetwork refuses, without changing anything, to sign, submit or
// observe a subject of another network -- or one whose network was never
// recorded. This host would read that subject's transaction as one it has
// never seen, and could fail it as expired while it stands final on its own
// chain.
func (s *Service) requireNetwork(kind SubjectKind, subject, network string) error {
	host := s.Network()
	switch network {
	case host:
		return nil
	case "":
		return errs.Newf(errs.CodeConflict, "delegation: %s %s was admitted before its network was recorded; it is signed and observed on no network",
			kind, subject).WithField("reason", ReasonNetworkUnrecorded).WithField("host_network", host)
	}
	return errs.Newf(errs.CodeConflict, "delegation: %s %s belongs to devnet network %s; this service carries %s", kind, subject, network, host).
		WithField("subject_network", network).WithField("host_network", host)
}

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
// its quote expired, or its capability, account or agent no longer stands,
// or its network was never recorded. The funds return and the intent is
// REJECTED. Anything else -- including a pending swap of another network,
// which that network's service decides -- is unchanged.
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
	switch sw.Network {
	case s.Network():
	case "":
		return s.rejectSwap(ctx, tx, sw, ReasonNetworkUnrecorded,
			"the swap was admitted before its devnet network was recorded; no service can sign it")
	default:
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
// capability, account, agent or destination no longer stands, or its network
// was never recorded. A pending withdrawal of another network is unchanged.
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
	switch w.Network {
	case s.Network():
	case "":
		return s.rejectWithdrawal(ctx, tx, w, ReasonNetworkUnrecorded,
			"the withdrawal was proposed before its devnet network was recorded; no service can sign it")
	default:
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

// The operations of ReconcilePending in the upkeep failure ledger (migration
// 00760); their items are swaps and withdrawals.
const (
	UpkeepExpireSwap        = "delegation.expire_swap"
	UpkeepExpireWithdrawal  = "delegation.expire_withdrawal"
	UpkeepObserveSwap       = "delegation.observe_swap"
	UpkeepObserveWithdrawal = "delegation.observe_withdrawal"
)

// PendingFailure is a swap or withdrawal ReconcilePending could not handle.
type PendingFailure struct {
	Kind      SubjectKind
	SubjectID string
	// Operation is the step that failed (UpkeepExpireSwap ...).
	Operation string
	// Attempts counts its consecutive failures of that step; it is not taken
	// again before RetryAfter.
	Attempts   int
	RetryAfter time.Time
	Err        error
}

// PendingReport is what one ReconcilePending pass did.
type PendingReport struct {
	// Pending counts the pending swaps and withdrawals the pass checked;
	// Ended, those of them it ended.
	Pending, Ended int
	// InFlight counts the signed, submitted and UNKNOWN ones it observed;
	// Moved, those whose status changed; Waiting, those still waiting on the
	// chain.
	InFlight, Moved, Waiting int
	// Failures are the subjects it could not handle. Every other subject was
	// handled: one subject's failure never stops the pass.
	Failures []PendingFailure
}

// The subjects ReconcilePending takes: its own network's (and, for expiry, a
// pending one whose network was never recorded), leaving out one whose last
// attempt at the step failed and whose backoff is still running (unless the
// clock has gone back past that failure), never-failed ones first, then the
// oldest. $1 limit, $2 network, $3 operation, $4 now.
const (
	pendingSwapsWhere = `status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED') AND (network = $2 OR network IS NULL)
		AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = delegated_swaps.id
		                  AND uf.cleared_at IS NULL AND uf.retry_after > $4 AND uf.last_failed_at <= $4)
		ORDER BY EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = delegated_swaps.id AND uf.cleared_at IS NULL),
		         created_at, id LIMIT $1`
	inFlightSwapsWhere = `status IN ('SIGNED','SUBMITTED','UNKNOWN') AND network = $2
		AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = delegated_swaps.id
		                  AND uf.cleared_at IS NULL AND uf.retry_after > $4 AND uf.last_failed_at <= $4)
		ORDER BY EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = delegated_swaps.id AND uf.cleared_at IS NULL),
		         created_at, id LIMIT $1`
	pendingWithdrawalsWhere = `status IN ('AWAITING_APPROVAL','APPROVED') AND (network = $2 OR network IS NULL)
		AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = delegated_withdrawals.id
		                  AND uf.cleared_at IS NULL AND uf.retry_after > $4 AND uf.last_failed_at <= $4)
		ORDER BY EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = delegated_withdrawals.id AND uf.cleared_at IS NULL),
		         created_at, id LIMIT $1`
	inFlightWithdrawalsWhere = `status IN ('SIGNED','SUBMITTED','UNKNOWN') AND network = $2
		AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = delegated_withdrawals.id
		                  AND uf.cleared_at IS NULL AND uf.retry_after > $4 AND uf.last_failed_at <= $4)
		ORDER BY EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $3 AND uf.item_id = delegated_withdrawals.id AND uf.cleared_at IS NULL),
		         created_at, id LIMIT $1`
)

// Statements of the upkeep failure ledger (migration 00760).
const (
	upkeepSucceededSQL = `SELECT cp_upkeep_succeeded($1, $2, $3)`
	upkeepFailedSQL    = `SELECT failures, retry_after FROM cp_upkeep_failed($1, $2, $3, $4, $5)`
)

// ReconcilePending is the reconciliation sweep of delegated authority over
// this service's network: it ends pending swaps and withdrawals that can no
// longer be signed (ExpireStale, over the ones past their deadline first --
// DueSwaps, DueWithdrawals -- and then the oldest pending ones), and observes
// every signed, submitted or UNKNOWN one by transaction id -- settling,
// failing or leaving it -- without ever resubmitting anything
// (ObserveInFlight). A subject of another network is never touched: this host
// would read its transaction as never seen. When produce is set (a SANDBOX
// driver) it produces the blocks a submitted transaction's finality needs.
// Taking the oldest pending subjects alone would let ones that still stand --
// a withdrawal cooling for days -- hold every place; the ones past their
// deadline, which the pass certainly ends, are therefore taken first.
//
// Each subject is handled in its own transactions. One that cannot be
// handled is recorded in the upkeep failure ledger with a backoff and
// reported in Failures, and the pass goes on; until its backoff has passed it
// is not taken again, and never-failed subjects are taken first, so a subject
// that keeps failing never stands in front of the others. An error means the
// pass as a whole could not proceed (a subject list could not be read, the
// context ended, a failure could not be recorded); what it did before that is
// committed and in the report.
func (s *Service) ReconcilePending(ctx context.Context, limit int, produce bool) (PendingReport, error) {
	if err := requireExecutor(ctx); err != nil {
		return PendingReport{}, err
	}
	limit = boundPendingLimit(limit)
	network := s.Network()
	now := s.clk.Now().UTC()
	swaps, err := s.DueSwaps(ctx, s.db, now, limit)
	if err != nil {
		return PendingReport{}, err
	}
	if left := limit - len(swaps); left > 0 {
		oldest, err := listSwapIDs(ctx, s.db, pendingSwapsWhere, left, network, UpkeepExpireSwap, now)
		if err != nil {
			return PendingReport{}, err
		}
		swaps = appendUnseen(swaps, oldest)
	}
	withdrawals, err := s.DueWithdrawals(ctx, s.db, now, limit)
	if err != nil {
		return PendingReport{}, err
	}
	if left := limit - len(withdrawals); left > 0 {
		oldest, err := listWithdrawalIDs(ctx, s.db, pendingWithdrawalsWhere, left, network, UpkeepExpireWithdrawal, now)
		if err != nil {
			return PendingReport{}, err
		}
		withdrawals = appendUnseen(withdrawals, oldest)
	}
	rep, err := s.ExpireStale(ctx, swaps, withdrawals)
	if err != nil {
		return rep, err
	}
	observed, err := s.ObserveInFlight(ctx, limit, produce)
	rep.InFlight, rep.Moved, rep.Waiting = observed.InFlight, observed.Moved, observed.Waiting
	rep.Failures = append(rep.Failures, observed.Failures...)
	return rep, err
}

func boundPendingLimit(limit int) int {
	if limit <= 0 || limit > 500 {
		return 100
	}
	return limit
}

// appendUnseen appends to list the ids of more it does not hold yet.
func appendUnseen[T comparable](list, more []T) []T {
	seen := make(map[T]bool, len(list))
	for _, v := range list {
		seen[v] = true
	}
	for _, v := range more {
		if !seen[v] {
			seen[v] = true
			list = append(list, v)
		}
	}
	return list
}

// ExpireStale runs ExpireStaleSwap and ExpireStaleWithdrawal over the given
// candidates, each in its own transaction: a pending subject that can no
// longer be signed is ended and its funds released, anything else is left
// exactly as it is (one of another network included). A candidate that cannot
// be checked is recorded in the upkeep failure ledger with a backoff and
// reported in Failures, and the others are checked. Pending and Ended are
// filled in; an error means the pass could not go on.
func (s *Service) ExpireStale(ctx context.Context, swaps []SwapID, withdrawals []WithdrawalID) (PendingReport, error) {
	var rep PendingReport
	if err := requireExecutor(ctx); err != nil {
		return rep, err
	}
	ctx = serviceContext(ctx)
	for _, sid := range swaps {
		rep.Pending++
		var before, after Swap
		err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			if before, err = getSwap(ctx, tx, sid, true); err != nil {
				return err
			}
			if after, err = s.ExpireStaleSwap(ctx, tx, sid); err != nil {
				return err
			}
			return clearUpkeepFailure(ctx, tx, UpkeepExpireSwap, sid, s.clk.Now().UTC())
		})
		if err != nil {
			if ferr := s.pendingFailed(ctx, &rep, SubjectSwap, sid.String(), UpkeepExpireSwap, sid, err); ferr != nil {
				return rep, ferr
			}
			continue
		}
		if after.Status == SwapRejected && before.Status != SwapRejected {
			rep.Ended++
		}
	}
	for _, wid := range withdrawals {
		rep.Pending++
		var before, after Withdrawal
		err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			if before, err = getWithdrawal(ctx, tx, wid, true); err != nil {
				return err
			}
			if after, err = s.ExpireStaleWithdrawal(ctx, tx, wid); err != nil {
				return err
			}
			return clearUpkeepFailure(ctx, tx, UpkeepExpireWithdrawal, wid, s.clk.Now().UTC())
		})
		if err != nil {
			if ferr := s.pendingFailed(ctx, &rep, SubjectWithdrawal, wid.String(), UpkeepExpireWithdrawal, wid, err); ferr != nil {
				return rep, ferr
			}
			continue
		}
		if after.Status == WithdrawalRejected && before.Status != WithdrawalRejected {
			rep.Ended++
		}
	}
	return rep, nil
}

// ObserveInFlight observes up to limit signed, submitted or UNKNOWN swaps and
// as many withdrawals of this service's network by transaction id, each in
// its own transactions, settling, failing or leaving each, never resubmitting
// anything; with produce (a SANDBOX driver) it produces the blocks finality
// needs. A subject that cannot be observed is recorded with a backoff and
// reported in Failures, and the others are observed. InFlight, Moved and
// Waiting are filled in; an error means the pass could not go on.
func (s *Service) ObserveInFlight(ctx context.Context, limit int, produce bool) (PendingReport, error) {
	var rep PendingReport
	if err := requireExecutor(ctx); err != nil {
		return rep, err
	}
	ctx = serviceContext(ctx)
	limit = boundPendingLimit(limit)
	network := s.Network()
	now := s.clk.Now().UTC()
	signedSwaps, err := listSwapIDs(ctx, s.db, inFlightSwapsWhere, limit, network, UpkeepObserveSwap, now)
	if err != nil {
		return rep, err
	}
	for _, sid := range signedSwaps {
		rep.InFlight++
		before, after, err := s.observeSwap(ctx, sid, produce)
		if err != nil {
			if ferr := s.pendingFailed(ctx, &rep, SubjectSwap, sid.String(), UpkeepObserveSwap, sid, err); ferr != nil {
				return rep, ferr
			}
			continue
		}
		rep.tally(before == after, after == SwapSigned || after == SwapSubmitted || after == SwapUnknown)
	}
	signedWithdrawals, err := listWithdrawalIDs(ctx, s.db, inFlightWithdrawalsWhere, limit, network, UpkeepObserveWithdrawal, now)
	if err != nil {
		return rep, err
	}
	for _, wid := range signedWithdrawals {
		rep.InFlight++
		before, after, err := s.observeWithdrawal(ctx, wid, produce)
		if err != nil {
			if ferr := s.pendingFailed(ctx, &rep, SubjectWithdrawal, wid.String(), UpkeepObserveWithdrawal, wid, err); ferr != nil {
				return rep, ferr
			}
			continue
		}
		rep.tally(before == after,
			after == WithdrawalSigned || after == WithdrawalSubmitted || after == WithdrawalUnknown)
	}
	return rep, nil
}

func (r *PendingReport) tally(unchanged, waiting bool) {
	if !unchanged {
		r.Moved++
	}
	if waiting {
		r.Waiting++
	}
}

// observeSwap drives one signed, submitted or UNKNOWN swap by observation and
// returns its status before and after. A chain that finalized something other
// than the signed transaction is an outcome, not a failure: the mismatch is
// recorded as a reconciliation record and the swap waits for a person.
func (s *Service) observeSwap(ctx context.Context, sid SwapID, produce bool) (SwapStatus, SwapStatus, error) {
	sw, err := getSwap(ctx, s.db, sid, false)
	if err != nil {
		return "", "", err
	}
	stale := sw.SignedAt != nil && !s.clk.Now().Before(sw.SignedAt.Add(s.params.QuoteTTL))
	res, err := s.driveSwap(ctx, sid, produce && sw.Mode == gates.ModeSandbox, stale)
	if err != nil && !errs.HasCode(err, errs.CodeReconciliationRequired) {
		return "", "", err
	}
	if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return clearUpkeepFailure(ctx, tx, UpkeepObserveSwap, sid, s.clk.Now().UTC())
	}); err != nil {
		return "", "", err
	}
	after := res.Swap.Status
	if after == "" {
		after = sw.Status
	}
	return sw.Status, after, nil
}

// observeWithdrawal is observeSwap for a withdrawal.
func (s *Service) observeWithdrawal(ctx context.Context, wid WithdrawalID, produce bool) (WithdrawalStatus, WithdrawalStatus, error) {
	w, err := getWithdrawal(ctx, s.db, wid, false)
	if err != nil {
		return "", "", err
	}
	stale := w.SignedAt != nil && !s.clk.Now().Before(w.SignedAt.Add(s.params.QuoteTTL))
	res, err := s.driveWithdrawal(ctx, wid, produce && w.Mode == gates.ModeSandbox, stale)
	if err != nil && !errs.HasCode(err, errs.CodeReconciliationRequired) {
		return "", "", err
	}
	if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return clearUpkeepFailure(ctx, tx, UpkeepObserveWithdrawal, wid, s.clk.Now().UTC())
	}); err != nil {
		return "", "", err
	}
	after := res.Withdrawal.Status
	if after == "" {
		after = w.Status
	}
	return w.Status, after, nil
}

// pendingFailed records, in a transaction of its own (the subject's own has
// rolled back), that the step op could not handle the subject, and adds it to
// the report. A cancelled context, or a failure that cannot be recorded, ends
// the pass instead.
func (s *Service) pendingFailed(ctx context.Context, rep *PendingReport, kind SubjectKind, subject, op string, item any, cause error) error {
	if ctx.Err() != nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	f := PendingFailure{Kind: kind, SubjectID: subject, Operation: op, Err: cause}
	if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, upkeepFailedSQL, id.New[id.Any](), op, item, s.clk.Now().UTC(), string(errs.CodeOf(cause))).
			Scan(&f.Attempts, &f.RetryAfter)
	}); err != nil {
		return errors.Join(cause, dbErr("record the subject's upkeep failure", err))
	}
	f.RetryAfter = f.RetryAfter.UTC()
	rep.Failures = append(rep.Failures, f)
	return nil
}

// clearUpkeepFailure ends the subject's run of failures of op, if it had one.
func clearUpkeepFailure(ctx context.Context, tx pgx.Tx, op string, item any, now time.Time) error {
	if _, err := tx.Exec(ctx, upkeepSucceededSQL, op, item, now); err != nil {
		return dbErr("clear the subject's upkeep failures", err)
	}
	return nil
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
// limit, $4 the network. A subject's turn is its place among its own
// account's, most urgent first (urgency 0: a swap, whose quote expires; 1: a
// withdrawal, which does not), so ordering by turn first serves every
// account's first subject before any account's second.
const executableSQL = `SELECT kind, id::text, account_id, status, due FROM (
	SELECT kind, id, account_id, status, due, urgency,
	       row_number() OVER (PARTITION BY account_id ORDER BY urgency, due, id) AS turn
	  FROM (SELECT 'SWAP' AS kind, id, account_id, status, quote_expires_at AS due, 0 AS urgency
	          FROM delegated_swaps WHERE status IN ('ADMITTED','APPROVED') AND quote_expires_at > $1 AND network = $4
	        UNION ALL
	        SELECT 'WITHDRAWAL', id, account_id, status, executable_at, 1
	          FROM delegated_withdrawals WHERE status = 'APPROVED' AND executable_at <= $1 AND network = $4) AS ready) AS ranked
	WHERE turn <= $2 ORDER BY turn, urgency, due, id LIMIT $3`

// ExecutableSubjects selects at most limit (1..500; 50 otherwise) subjects of
// this service's network that an execution run may carry out now -- the
// swaps that need no approval (ADMITTED) or have it (APPROVED) and the
// APPROVED withdrawals -- fairly across accounts:
//
//   - Only what can be signed now. A swap whose venue quote has expired, a
//     withdrawal still in its cooling period and a subject of another network
//     (or of none) are never taken: none of them can be signed here, so none
//     may hold a place in a run (the upkeep ends the swap; the withdrawal
//     waits for its time; another network's service runs its own).
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
	rows, err := q.Query(ctx, executableSQL, dbTime(s.clk.Now()), s.params.ExecutablePerAccount, limit, s.Network())
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

// The expiry candidates. An expiry pass looks at two kinds of pending subject
// of this service's network (and pending ones whose network was never
// recorded, which ExpireStale ends): those past their deadline, which
// ExpireStaleSwap and ExpireStaleWithdrawal certainly end, oldest deadline
// first (every one taken leaves the set, so none waits behind another for
// long); and every other pending subject in turn, a paged sweep in id order,
// which finds those whose capability, account, agent, destination, gate or
// kill switch no longer stands. One whose last expiry failed is left out
// while its backoff runs (the upkeep failure ledger). The status sets are
// exactly the ones ExpireStale acts on. Each is the WHERE clause of
// listSwapIDs / listWithdrawalIDs.
const (
	// $1 now, $2 limit, $3 network, $4 operation.
	dueSwapsWhere = `status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED') AND quote_expires_at <= $1
		AND (network = $3 OR network IS NULL)
		AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $4 AND uf.item_id = delegated_swaps.id
		                  AND uf.cleared_at IS NULL AND uf.retry_after > $1 AND uf.last_failed_at <= $1)
		ORDER BY quote_expires_at, id LIMIT $2`
	// $1 paging key (NULL: from the first), $2 limit, $3 network, $4 operation, $5 now.
	sweepSwapsWhere = `status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED') AND ($1::uuid IS NULL OR id > $1::uuid)
		AND (network = $3 OR network IS NULL)
		AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $4 AND uf.item_id = delegated_swaps.id
		                  AND uf.cleared_at IS NULL AND uf.retry_after > $5 AND uf.last_failed_at <= $5)
		ORDER BY id LIMIT $2`
	// $1 now, $2 limit, $3 network, $4 operation.
	dueWithdrawalsWhere = `status = 'AWAITING_APPROVAL' AND approval_expires_at <= $1
		AND (network = $3 OR network IS NULL)
		AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $4 AND uf.item_id = delegated_withdrawals.id
		                  AND uf.cleared_at IS NULL AND uf.retry_after > $1 AND uf.last_failed_at <= $1)
		ORDER BY approval_expires_at, id LIMIT $2`
	// $1 paging key (NULL: from the first), $2 limit, $3 network, $4 operation, $5 now.
	sweepWithdrawalsWhere = `status IN ('AWAITING_APPROVAL','APPROVED') AND ($1::uuid IS NULL OR id > $1::uuid)
		AND (network = $3 OR network IS NULL)
		AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $4 AND uf.item_id = delegated_withdrawals.id
		                  AND uf.cleared_at IS NULL AND uf.retry_after > $5 AND uf.last_failed_at <= $5)
		ORDER BY id LIMIT $2`
)

// DueSwaps returns at most limit (1..500; 100 otherwise) pending swaps
// (ADMITTED, AWAITING_APPROVAL, APPROVED) whose venue quote expired at or
// before now, oldest deadline first: expiry candidates (see above).
func (s *Service) DueSwaps(ctx context.Context, q db.Querier, now time.Time, limit int) ([]SwapID, error) {
	return listSwapIDs(ctx, q, dueSwapsWhere, now.UTC(), selectionLimit(limit, 100), s.Network(), UpkeepExpireSwap)
}

// PendingSwaps returns at most limit (1..500; 100 otherwise) pending swaps
// after the paging key after (the zero id: from the first), in id order:
// the expiry sweep's candidates (see above).
func (s *Service) PendingSwaps(ctx context.Context, q db.Querier, after SwapID, now time.Time, limit int) ([]SwapID, error) {
	return listSwapIDs(ctx, q, sweepSwapsWhere, after, selectionLimit(limit, 100), s.Network(), UpkeepExpireSwap, now.UTC())
}

// DueWithdrawals returns at most limit (1..500; 100 otherwise) withdrawals
// still waiting for the owner's decision when their approval window closed
// at or before now, oldest deadline first: expiry candidates (see above).
func (s *Service) DueWithdrawals(ctx context.Context, q db.Querier, now time.Time, limit int) ([]WithdrawalID, error) {
	return listWithdrawalIDs(ctx, q, dueWithdrawalsWhere, now.UTC(), selectionLimit(limit, 100), s.Network(), UpkeepExpireWithdrawal)
}

// PendingWithdrawals returns at most limit (1..500; 100 otherwise) pending
// withdrawals (AWAITING_APPROVAL, APPROVED) after the paging key after (the
// zero id: from the first), in id order: the expiry sweep's candidates (see
// above).
func (s *Service) PendingWithdrawals(ctx context.Context, q db.Querier, after WithdrawalID, now time.Time, limit int) ([]WithdrawalID, error) {
	return listWithdrawalIDs(ctx, q, sweepWithdrawalsWhere, after, selectionLimit(limit, 100), s.Network(), UpkeepExpireWithdrawal,
		now.UTC())
}
