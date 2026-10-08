package delegation

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
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
// ends pending swaps and withdrawals that can no longer be signed, and
// observes every signed, submitted or UNKNOWN one by transaction id --
// settling, failing or leaving it -- without ever resubmitting anything.
// When produce is set (a SANDBOX driver) it produces the blocks a submitted
// transaction's finality needs. It returns how many it looked at.
func (s *Service) ReconcilePending(ctx context.Context, limit int, produce bool) (int, error) {
	if err := requireExecutor(ctx); err != nil {
		return 0, err
	}
	ctx = serviceContext(ctx)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	seen := 0
	pendingSwaps, err := listSwapIDs(ctx, s.db, `status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED') ORDER BY created_at, id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	for _, sid := range pendingSwaps {
		if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := s.ExpireStaleSwap(ctx, tx, sid); return err }); err != nil {
			return seen, err
		}
		seen++
	}
	pendingWithdrawals, err := listWithdrawalIDs(ctx, s.db, `status IN ('AWAITING_APPROVAL','APPROVED') ORDER BY created_at, id LIMIT $1`, limit)
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
