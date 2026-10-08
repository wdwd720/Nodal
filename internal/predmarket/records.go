package predmarket

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// Persistence of the hardening records: void proposals and their approvals,
// held payouts and their claims. Every table is append-only; what a record
// "is now" (approved, claimed) is the presence of its second row.

// --- void proposals --------------------------------------------------------------

const voidProposalColumns = `p.id, p.market_id, p.market_status, p.resolution_id, p.reason, p.evidence_ref, p.evidence_sha256,
	p.proposed_by_actor_type, p.proposed_by_actor_id, p.proposed_at,
	a.id, a.reason, a.approved_by_actor_type, a.approved_by_actor_id, a.approved_at`

const voidProposalFrom = ` FROM prediction_void_proposals p LEFT JOIN prediction_void_approvals a ON a.proposal_id = p.id`

func scanVoidProposal(row pgx.Row) (VoidProposal, error) {
	var (
		p          VoidProposal
		approvalID *VoidApprovalID
		note       *string
		approver   *security.ActorType
		approverID *string
		approvedAt *time.Time
	)
	if err := row.Scan(&p.ID, &p.MarketID, &p.MarketStatus, &p.ResolutionID, &p.Reason, &p.EvidenceRef, &p.EvidenceSHA256,
		&p.ProposedBy.Type, &p.ProposedBy.ID, &p.ProposedAt,
		&approvalID, &note, &approver, &approverID, &approvedAt); err != nil {
		return VoidProposal{}, err
	}
	p.ProposedAt = p.ProposedAt.UTC()
	if approvalID == nil || note == nil || approver == nil || approverID == nil || approvedAt == nil {
		return p, nil
	}
	p.Approval = &VoidApproval{
		ID: *approvalID, ProposalID: p.ID, MarketID: p.MarketID, Reason: *note,
		ApprovedBy: econ.Actor{Type: *approver, ID: *approverID}, ApprovedAt: approvedAt.UTC(),
	}
	return p, nil
}

func insertVoidProposal(ctx context.Context, tx pgx.Tx, p VoidProposal) error {
	if _, err := tx.Exec(ctx, `INSERT INTO prediction_void_proposals
		(id, market_id, market_status, resolution_id, reason, evidence_ref, evidence_sha256, proposed_by_actor_type,
		 proposed_by_actor_id, proposed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		p.ID, p.MarketID, p.MarketStatus, p.ResolutionID, p.Reason, p.EvidenceRef, p.EvidenceSHA256, p.ProposedBy.Type,
		p.ProposedBy.ID, p.ProposedAt); err != nil {
		return dbErr("record void proposal", err)
	}
	return nil
}

func insertVoidApproval(ctx context.Context, tx pgx.Tx, a VoidApproval) error {
	if _, err := tx.Exec(ctx, `INSERT INTO prediction_void_approvals
		(id, proposal_id, market_id, reason, approved_by_actor_type, approved_by_actor_id, approved_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		a.ID, a.ProposalID, a.MarketID, a.Reason, a.ApprovedBy.Type, a.ApprovedBy.ID, a.ApprovedAt); err != nil {
		return dbErr("record void approval", err)
	}
	return nil
}

// VoidProposal returns one void proposal with its approval, if any.
func (s *Service) VoidProposal(ctx context.Context, q db.Querier, pid VoidProposalID) (VoidProposal, error) {
	p, err := scanVoidProposal(q.QueryRow(ctx, `SELECT `+voidProposalColumns+voidProposalFrom+` WHERE p.id = $1`, pid))
	if errors.Is(err, pgx.ErrNoRows) {
		return VoidProposal{}, errs.New(errs.CodeNotFound, "predmarket: void proposal not found")
	}
	if err != nil {
		return VoidProposal{}, dbErr("load void proposal", err)
	}
	return p, nil
}

// VoidProposals returns every void proposal made on a market, oldest first,
// each with its approval if it was approved.
func (s *Service) VoidProposals(ctx context.Context, q db.Querier, marketID MarketID) ([]VoidProposal, error) {
	rows, err := q.Query(ctx, `SELECT `+voidProposalColumns+voidProposalFrom+` WHERE p.market_id = $1
		ORDER BY p.proposed_at, p.id`, marketID)
	if err != nil {
		return nil, dbErr("list void proposals", err)
	}
	defer rows.Close()
	var out []VoidProposal
	for rows.Next() {
		p, err := scanVoidProposal(rows)
		if err != nil {
			return nil, dbErr("scan void proposal", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- held payouts ----------------------------------------------------------------

const heldColumns = `h.id, h.market_id, h.settlement_id, h.account_id, h.kind, h.collateral_asset_id, h.amount::text,
	h.reason_codes, h.refused_intent_id, h.effect_id, h.journal_transaction_id, h.evidence_ref, h.evidence_sha256, h.held_at,
	c.id, c.amount::text, c.intent_id, c.effect_id, c.journal_transaction_id, c.claimed_by_actor_type, c.claimed_by_actor_id,
	c.evidence_ref, c.evidence_sha256, c.claimed_at`

const heldFrom = ` FROM prediction_held_payouts h LEFT JOIN prediction_held_payout_claims c ON c.held_payout_id = h.id`

func scanHeldPayout(row pgx.Row) (HeldPayout, error) {
	var (
		h           HeldPayout
		amount      string
		claimID     *HeldPayoutClaimID
		claimAmount *string
		intentID    *econ.IntentID
		effectID    *econ.EffectID
		journalID   *ledger.TransactionID
		claimer     *security.ActorType
		claimerID   *string
		claimRef    *string
		claimSum    []byte
		claimedAt   *time.Time
	)
	if err := row.Scan(&h.ID, &h.MarketID, &h.SettlementID, &h.AccountID, &h.Kind, &h.CollateralAssetID, &amount,
		&h.ReasonCodes, &h.RefusedIntentID, &h.EffectID, &h.JournalTransactionID, &h.EvidenceRef, &h.EvidenceSHA256, &h.HeldAt,
		&claimID, &claimAmount, &intentID, &effectID, &journalID, &claimer, &claimerID, &claimRef, &claimSum, &claimedAt); err != nil {
		return HeldPayout{}, err
	}
	q, err := money.ScanQuantity(amount)
	if err != nil {
		return HeldPayout{}, errs.Wrap(err, errs.CodeInternal, "predmarket: a held payout is not an integer")
	}
	h.Amount = q
	h.HeldAt = h.HeldAt.UTC()
	if claimID == nil {
		return h, nil
	}
	if claimAmount == nil || intentID == nil || effectID == nil || journalID == nil || claimer == nil || claimerID == nil ||
		claimRef == nil || claimedAt == nil {
		return HeldPayout{}, errs.New(errs.CodeInternal, "predmarket: a stored claim is incomplete")
	}
	claimed, err := money.ScanQuantity(*claimAmount)
	if err != nil {
		return HeldPayout{}, errs.Wrap(err, errs.CodeInternal, "predmarket: a claimed amount is not an integer")
	}
	h.Claim = &HeldPayoutClaim{
		ID: *claimID, HeldPayoutID: h.ID, MarketID: h.MarketID, AccountID: h.AccountID, Amount: claimed,
		IntentID: *intentID, EffectID: *effectID, JournalTransactionID: *journalID,
		ClaimedBy: econ.Actor{Type: *claimer, ID: *claimerID}, EvidenceRef: *claimRef, EvidenceSHA256: claimSum,
		ClaimedAt: claimedAt.UTC(),
	}
	return h, nil
}

func insertHeldPayout(ctx context.Context, tx pgx.Tx, h HeldPayout) error {
	if _, err := tx.Exec(ctx, `INSERT INTO prediction_held_payouts
		(id, market_id, settlement_id, account_id, kind, collateral_asset_id, amount, reason_codes, refused_intent_id, effect_id,
		 journal_transaction_id, evidence_ref, evidence_sha256, held_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8,$9,$10,$11,$12,$13,$14)`,
		h.ID, h.MarketID, h.SettlementID, h.AccountID, h.Kind, h.CollateralAssetID, h.Amount.String(), h.ReasonCodes,
		h.RefusedIntentID, h.EffectID, h.JournalTransactionID, h.EvidenceRef, h.EvidenceSHA256, h.HeldAt); err != nil {
		return dbErr("record held payout", err)
	}
	return nil
}

func insertHeldClaim(ctx context.Context, tx pgx.Tx, c HeldPayoutClaim) error {
	if _, err := tx.Exec(ctx, `INSERT INTO prediction_held_payout_claims
		(id, held_payout_id, market_id, account_id, amount, intent_id, effect_id, journal_transaction_id,
		 claimed_by_actor_type, claimed_by_actor_id, evidence_ref, evidence_sha256, claimed_at)
		VALUES ($1,$2,$3,$4,$5::numeric,$6,$7,$8,$9,$10,$11,$12,$13)`,
		c.ID, c.HeldPayoutID, c.MarketID, c.AccountID, c.Amount.String(), c.IntentID, c.EffectID, c.JournalTransactionID,
		c.ClaimedBy.Type, c.ClaimedBy.ID, c.EvidenceRef, c.EvidenceSHA256, c.ClaimedAt); err != nil {
		return dbErr("record held payout claim", err)
	}
	return nil
}

// HeldPayoutOf returns the payout a market holds for one account, if any.
func (s *Service) HeldPayoutOf(ctx context.Context, q db.Querier, marketID MarketID, account accounts.AccountID) (HeldPayout, bool, error) {
	h, err := scanHeldPayout(q.QueryRow(ctx, `SELECT `+heldColumns+heldFrom+` WHERE h.market_id = $1 AND h.account_id = $2`,
		marketID, account))
	if errors.Is(err, pgx.ErrNoRows) {
		return HeldPayout{}, false, nil
	}
	if err != nil {
		return HeldPayout{}, false, dbErr("load held payout", err)
	}
	return h, true, nil
}

// HeldPayouts returns every payout a market held, by account, each with its
// claim if it was claimed.
func (s *Service) HeldPayouts(ctx context.Context, q db.Querier, marketID MarketID) ([]HeldPayout, error) {
	rows, err := q.Query(ctx, `SELECT `+heldColumns+heldFrom+` WHERE h.market_id = $1 ORDER BY h.account_id`, marketID)
	if err != nil {
		return nil, dbErr("list held payouts", err)
	}
	defer rows.Close()
	var out []HeldPayout
	for rows.Next() {
		h, err := scanHeldPayout(rows)
		if err != nil {
			return nil, dbErr("scan held payout", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// unclaimedHeld counts a market's held payouts not yet claimed.
func unclaimedHeld(ctx context.Context, q db.Querier, marketID MarketID) (int, error) {
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM prediction_held_payouts h WHERE h.market_id = $1
		AND NOT EXISTS (SELECT 1 FROM prediction_held_payout_claims c WHERE c.held_payout_id = h.id)`, marketID).Scan(&n); err != nil {
		return 0, dbErr("count unclaimed held payouts", err)
	}
	return n, nil
}
