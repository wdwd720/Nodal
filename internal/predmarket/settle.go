package predmarket

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// SettleRequest settles a RESOLVED market.
type SettleRequest struct {
	MarketID MarketID
	Actor    econ.Actor
}

// SettleResult is a market after its settlement or refund.
type SettleResult struct {
	Market     Market
	Settlement Settlement
	// Plan and Lines describe what this call allotted, one line per holder in
	// account order. A line's intent is COMPLETED when the amount reached the
	// holder's wallet, or REJECTED when the route or Risk V2 refused the
	// holder's own payout: its shares were retired all the same, its Effect
	// and JournalTransactionID are the ones that retired them, and the amount
	// stays in the escrow as a HeldPayout. All three are empty on a replay.
	Plan  SettlementPlan
	Lines []ActionResult
	// Held lists the payouts this call held, one per held holder with an
	// amount; a holder of only losing shares has nothing to hold.
	Held []HeldPayout
	// Replay is true when the market had already settled (or been voided):
	// nothing was paid and the stored record is returned.
	Replay bool
}

// Settle pays a RESOLVED market out of its escrow: every share of the
// winning outcome pays exactly one collateral unit, every share of every
// outcome is retired, Σ payouts equals the escrow exactly, and every
// outcome's outstanding shares are zero afterwards -- checked from the books
// before the market is allowed to become SETTLED.
//
// One holder can never block it for everyone. A holder whose own payout the
// route or Risk V2 refuses for the holder's own reasons (a frozen or closed
// account, an unverified identity) has their shares retired like everyone
// else, and their amount is HELD: it stays in the escrow, recorded per holder,
// and they claim it with ClaimHeldPayout once eligible. The escrow then holds
// exactly the unclaimed held payouts, and the market instrument stays
// CLOSE_ONLY (admitting only those claims) until the last one is claimed. A
// refusal that is not the holder's own -- a kill switch, the instrument, the
// books -- refuses the whole settlement.
//
// It refuses a market that is not RESOLVED (no settlement before resolution),
// that is DISPUTED, whose dispute window is still open, whose books do not
// reconcile, or whose shares an open reservation or hold still encumbers.
// With OutcomeBooks configured, every resting order on the outcome
// instruments is cancelled first; without it the refusal names every
// reservation. It is all or nothing. Settling a SETTLED market returns its
// record and pays nothing, which is how concurrent attempts settle once: the
// market row is locked, and every attempt after the first finds it SETTLED.
func (s *Service) Settle(ctx context.Context, tx pgx.Tx, req SettleRequest) (SettleResult, error) {
	by, err := canonicalOperator(req.Actor)
	if err != nil {
		return SettleResult{}, err
	}
	var out SettleResult
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		m, err := s.lockMarket(ctx, tx, req.MarketID, lockUpdate)
		if err != nil {
			return err
		}
		switch m.Status {
		case StatusSettled:
			st, found, err := s.SettlementOf(ctx, tx, m.ID)
			if err != nil {
				return err
			}
			if !found {
				return errs.New(errs.CodeInternal, "predmarket: a SETTLED market has no settlement record")
			}
			out = SettleResult{Market: m, Settlement: st, Replay: true}
			return nil
		case StatusResolved:
		case StatusDisputed:
			return errs.Newf(errs.CodeInvalidStateTransition,
				"predmarket: market %s is DISPUTED; it settles only once the dispute is closed", m.Symbol).WithField("market_id", m.ID.String())
		default:
			return errs.Newf(errs.CodeInvalidStateTransition,
				"predmarket: market %s is %s; there is no settlement before resolution", m.Symbol, m.Status).WithField("market_id", m.ID.String())
		}
		if err := s.authorize(ctx, tx, m.Mode); err != nil {
			return err
		}
		res, err := s.Resolution(ctx, tx, *m.CurrentResolutionID)
		if err != nil {
			return err
		}
		now := s.now()
		if until := res.AcceptedAt.Add(m.DisputeWindow); now.Before(until) {
			return errs.Newf(errs.CodeInvalidStateTransition,
				"predmarket: market %s is in its dispute window until %s", m.Symbol, until.Format(time.RFC3339)).
				WithField("market_id", m.ID.String())
		}
		if _, err := s.cancelResting(ctx, tx, m, by, "prediction market "+m.Symbol+" settling: no order outlives its market"); err != nil {
			return err
		}
		if err := s.requireUnencumbered(ctx, tx, m); err != nil {
			return err
		}
		books, err := s.requireReconciled(ctx, tx, m)
		if err != nil {
			return err
		}
		holdings, err := s.holdings(ctx, tx, m)
		if err != nil {
			return err
		}
		plan, err := PlanPayout(books.Escrow, books.SharesOutstanding, holdings, res.OutcomeIndex, m.UnitQuantity)
		if err != nil {
			return err
		}
		inst, err := s.instr.Get(ctx, tx, m.InstrumentID)
		if err != nil {
			return dbErr("load market instrument", err)
		}
		attempt := id.New[settlementKind]()
		lines, held, err := s.pay(ctx, tx, m, plan, by, inst.Status, &res, attempt)
		if err != nil {
			return err
		}
		st, err := s.recordSettlement(ctx, tx, settlementRecord{
			attempt: attempt, market: m, plan: plan, resolution: &res, lines: lines, held: held, by: by,
		})
		if err != nil {
			return err
		}
		heldPayouts, err := s.recordHeld(ctx, tx, m, st, held)
		if err != nil {
			return err
		}
		if err := s.closeOutInstrument(ctx, tx, m, len(heldPayouts) > 0, by, "prediction market settled"); err != nil {
			return err
		}
		next, err := s.marketTransition(ctx, tx, m, StatusSettled, by, "settled: "+m.Outcomes[plan.WinningOutcome].Symbol+" won",
			st.EvidenceRef, nil, map[string]any{
				"settlement_id": st.ID.String(), "resolution_id": res.ID.String(),
				"escrow_before": plan.EscrowBefore.String(), "total_paid": plan.TotalPaid.String(),
				"total_held": st.TotalHeld.String(), "holders": len(plan.Lines), "held_holders": len(held),
			})
		if err != nil {
			return err
		}
		if err := s.requireEmptied(ctx, tx, next, money.Quantity{}); err != nil {
			return err
		}
		out = SettleResult{Market: next, Settlement: st, Plan: plan, Lines: lines, Held: heldPayouts}
		return nil
	})
	return out, err
}

// --- voids: proposed by one operator, approved by another -----------------------

// VoidProposalRequest proposes voiding a market and refunding its shares.
type VoidProposalRequest struct {
	MarketID MarketID
	Actor    econ.Actor
	// Reason says why, in 8..2000 characters.
	Reason string
	// Evidence is the document the proposal rests on. It is required, and it
	// is archived with the proposal and again with the refund record.
	Evidence any
}

// VoidApprovalRequest approves a void proposal, which voids the market.
type VoidApprovalRequest struct {
	MarketID   MarketID
	ProposalID VoidProposalID
	Actor      econ.Actor
	// Reason is the approver's own note; the proposal's reason when empty.
	Reason string
}

// voidTime is the time a void proposal or approval is recorded at, at the
// precision the database keeps, so the dispute-window comparison made here is
// the one the database repeats on the stored timestamp.
func (s *Service) voidTime() time.Time { return s.now().Truncate(time.Microsecond) }

// voidable checks what both halves of a void require of the market as it
// stands and of the operator taking part: the market has neither settled nor
// been voided; the operator is neither its creator nor the author of the
// resolution in force; and a RESOLVED market is past its dispute window --
// inside it, a resolution is disputed, and the void then rules on the
// dispute. It returns the resolution in force, if any.
func (s *Service) voidable(ctx context.Context, tx pgx.Tx, m Market, by econ.Actor, at time.Time) (*Resolution, error) {
	switch m.Status {
	case StatusVoided:
		return nil, errs.Newf(errs.CodeConflict, "predmarket: market %s is already voided", m.Symbol).WithField("market_id", m.ID.String())
	case StatusSettled:
		return nil, errs.Newf(errs.CodeInvalidStateTransition, "predmarket: market %s has settled and cannot be voided", m.Symbol).
			WithField("market_id", m.ID.String())
	}
	if err := requireOtherThan(by, m.CreatedBy, "the creator of a market takes no part in voiding it"); err != nil {
		return nil, err
	}
	if m.CurrentResolutionID == nil {
		return nil, nil
	}
	res, err := s.Resolution(ctx, tx, *m.CurrentResolutionID)
	if err != nil {
		return nil, err
	}
	if err := requireOtherThan(by, res.AcceptedBy, "the author of a market's resolution in force takes no part in voiding it"); err != nil {
		return nil, err
	}
	if m.Status == StatusResolved {
		if until := res.AcceptedAt.Add(m.DisputeWindow); at.Before(until) {
			return nil, errs.Newf(errs.CodeInvalidStateTransition,
				"predmarket: market %s is RESOLVED and inside its dispute window until %s; a resolution is disputed, not voided, and the void then rules on the dispute",
				m.Symbol, until.Format(time.RFC3339)).
				WithField("market_id", m.ID.String()).WithField("void_refusal", "DISPUTE_WINDOW_OPEN")
		}
	}
	return &res, nil
}

// ProposeVoid records one operator's proposal to void a market, with its
// reason and archived evidence, against the market as it stands. Nothing is
// refunded until a different operator approves it (ApproveVoid). The proposer
// may be neither the market's creator nor the author of its resolution in
// force, and a RESOLVED market inside its dispute window is disputed, not
// voided.
func (s *Service) ProposeVoid(ctx context.Context, tx pgx.Tx, req VoidProposalRequest) (VoidProposal, error) {
	by, err := canonicalOperator(req.Actor)
	if err != nil {
		return VoidProposal{}, err
	}
	reason := strings.TrimSpace(req.Reason)
	if len(reason) < 8 || len(reason) > 2000 {
		return VoidProposal{}, errs.New(errs.CodeValidationFailed, "predmarket: a void proposal states its reason in 8..2000 characters")
	}
	if req.Evidence == nil {
		return VoidProposal{}, errs.New(errs.CodeValidationFailed, "predmarket: a void proposal carries its evidence")
	}
	var out VoidProposal
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		m, err := s.lockMarket(ctx, tx, req.MarketID, lockUpdate)
		if err != nil {
			return err
		}
		at := s.voidTime()
		if _, err := s.voidable(ctx, tx, m, by, at); err != nil {
			return err
		}
		p := VoidProposal{
			ID: id.New[voidProposalKind](), MarketID: m.ID, MarketStatus: m.Status, ResolutionID: m.CurrentResolutionID,
			Reason: reason, ProposedBy: by, ProposedAt: at,
		}
		doc := map[string]any{
			"kind": "PREDICTION_VOID_PROPOSAL", "proposal_id": p.ID.String(), "market_id": m.ID.String(), "market_symbol": m.Symbol,
			"market_status": string(m.Status), "proposed_by": map[string]any{"type": string(by.Type), "id": by.ID},
			"reason": reason, "evidence": req.Evidence, "proposed_at": at,
		}
		if m.CurrentResolutionID != nil {
			doc["resolution_id"] = m.CurrentResolutionID.String()
		}
		ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "predmarket/void-proposal/" + p.ID.String(), Document: doc})
		if err != nil {
			return err
		}
		p.EvidenceRef, p.EvidenceSHA256 = ref.URI, ref.SHA256
		if err := insertVoidProposal(ctx, tx, p); err != nil {
			return err
		}
		if err := s.auditVoid(ctx, tx, m, "predmarket.void.proposed", by, reason, ref.URI, map[string]any{
			"proposal_id": p.ID.String(), "market_status": string(m.Status),
		}, at); err != nil {
			return err
		}
		out, err = s.VoidProposal(ctx, tx, p.ID)
		return err
	})
	return out, err
}

// ApproveVoid approves a void proposal and voids the market in the same
// transaction: dual control, enforced here and bound in the database. The
// approver is not the proposer, not the market's creator, not the author of
// its resolution in force and, for a DISPUTED market, not the operator who
// raised the dispute (the void rules on it). The proposal must still describe
// the market -- no other resolution has come into force since it was made --
// and a RESOLVED market inside its dispute window is refused.
//
// The void refunds every share under RoundingFloorPerHolder: each whole share
// of any outcome refunds floor(unit / N) collateral base units, a holder's
// refund is rounded down once, and what is not paid stays on the facility's
// books as the residual the REFUND record discloses -- escrow before = Σ
// refunds + residual, checked from the books before the market may become
// VOIDED. Trading halts on every outcome instrument first and, with
// OutcomeBooks, every resting order is cancelled. A holder whose refund
// cannot be delivered is held exactly as in Settle. Approving the proposal
// that voided the market again returns the refund record and pays nothing.
func (s *Service) ApproveVoid(ctx context.Context, tx pgx.Tx, req VoidApprovalRequest) (SettleResult, error) {
	by, err := canonicalOperator(req.Actor)
	if err != nil {
		return SettleResult{}, err
	}
	note := strings.TrimSpace(req.Reason)
	if len(note) > 2000 {
		return SettleResult{}, errs.New(errs.CodeValidationFailed, "predmarket: an approval's note is at most 2000 characters")
	}
	var out SettleResult
	err = atomically(ctx, tx, func(tx pgx.Tx) error {
		m, err := s.lockMarket(ctx, tx, req.MarketID, lockUpdate)
		if err != nil {
			return err
		}
		p, err := s.VoidProposal(ctx, tx, req.ProposalID)
		if err != nil {
			return err
		}
		if p.MarketID != m.ID {
			return errs.New(errs.CodeNotFound, "predmarket: void proposal not found")
		}
		if p.Approval != nil {
			if m.Status != StatusVoided {
				return errs.New(errs.CodeInternal, "predmarket: an approved void proposal names a market that is not VOIDED")
			}
			st, found, err := s.SettlementOf(ctx, tx, m.ID)
			if err != nil {
				return err
			}
			if !found {
				return errs.New(errs.CodeInternal, "predmarket: a VOIDED market has no refund record")
			}
			out = SettleResult{Market: m, Settlement: st, Replay: true}
			return nil
		}
		if err := requireOtherThan(by, p.ProposedBy, "the operator who proposed a void cannot approve it"); err != nil {
			return err
		}
		at := s.voidTime()
		res, err := s.voidable(ctx, tx, m, by, at)
		if err != nil {
			return err
		}
		if !sameResolution(p.ResolutionID, m.CurrentResolutionID) {
			return errs.Newf(errs.CodeConflict,
				"predmarket: market %s has another resolution in force than when this void was proposed; propose it again", m.Symbol).
				WithField("market_id", m.ID.String()).WithField("proposal_id", p.ID.String())
		}
		if res != nil {
			if err := requireOtherThan(p.ProposedBy, res.AcceptedBy, "the author of a market's resolution in force takes no part in voiding it"); err != nil {
				return err
			}
		}
		if m.Status == StatusDisputed {
			_, dispute, err := s.openDispute(ctx, tx, m)
			if err != nil {
				return err
			}
			if err := requireOtherThan(by, dispute.RaisedBy, "the operator who raised a dispute cannot approve the void that rules on it"); err != nil {
				return err
			}
		}
		if err := s.authorize(ctx, tx, m.Mode); err != nil {
			return err
		}
		a := VoidApproval{
			ID: id.New[voidApprovalKind](), ProposalID: p.ID, MarketID: m.ID, Reason: reasonOr(note, p.Reason), ApprovedBy: by, ApprovedAt: at,
		}
		if err := insertVoidApproval(ctx, tx, a); err != nil {
			return err
		}
		if err := s.auditVoid(ctx, tx, m, "predmarket.void.approved", by, a.Reason, p.EvidenceRef, map[string]any{
			"proposal_id": p.ID.String(), "approval_id": a.ID.String(), "proposed_by": p.ProposedBy.ID,
		}, at); err != nil {
			return err
		}
		out, err = s.executeVoid(ctx, tx, m, p, a)
		return err
	})
	return out, err
}

func sameResolution(a, b *ResolutionID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// executeVoid refunds an approved void. The caller holds the market's lock
// and has recorded the approval.
func (s *Service) executeVoid(ctx context.Context, tx pgx.Tx, m Market, p VoidProposal, a VoidApproval) (SettleResult, error) {
	by := a.ApprovedBy
	// Trading halts on every outcome first. The market instrument -- the
	// contract, which no book can trade -- keeps its status while the refunds
	// are evaluated (ACTIVE for a market voided while OPEN, CLOSE_ONLY after
	// close) and moves once, at the end: no instrument changes status twice
	// in one transaction.
	for _, o := range m.Outcomes {
		if err := s.setInstrumentStatus(ctx, tx, o.InstrumentID, assets.StatusHalted, by, "prediction market voided: no trading"); err != nil {
			return SettleResult{}, err
		}
	}
	if _, err := s.cancelResting(ctx, tx, m, by, "prediction market "+m.Symbol+" voided: no order outlives its market"); err != nil {
		return SettleResult{}, err
	}
	if err := s.requireUnencumbered(ctx, tx, m); err != nil {
		return SettleResult{}, err
	}
	books, err := s.requireReconciled(ctx, tx, m)
	if err != nil {
		return SettleResult{}, err
	}
	holdings, err := s.holdings(ctx, tx, m)
	if err != nil {
		return SettleResult{}, err
	}
	plan, err := PlanRefund(books.Escrow, books.SharesOutstanding, holdings, m.UnitQuantity)
	if err != nil {
		return SettleResult{}, err
	}
	inst, err := s.instr.Get(ctx, tx, m.InstrumentID)
	if err != nil {
		return SettleResult{}, dbErr("load market instrument", err)
	}
	attempt := id.New[settlementKind]()
	lines, held, err := s.pay(ctx, tx, m, plan, by, inst.Status, nil, attempt)
	if err != nil {
		return SettleResult{}, err
	}
	st, err := s.recordSettlement(ctx, tx, settlementRecord{
		attempt: attempt, market: m, plan: plan, lines: lines, held: held, by: by, reason: p.Reason,
		evidence: map[string]any{
			"proposal_id": p.ID.String(), "proposal_evidence_ref": p.EvidenceRef,
			"proposed_by": map[string]any{"type": string(p.ProposedBy.Type), "id": p.ProposedBy.ID}, "proposed_at": p.ProposedAt,
			"approval_id": a.ID.String(), "approved_by": map[string]any{"type": string(a.ApprovedBy.Type), "id": a.ApprovedBy.ID},
			"approved_at": a.ApprovedAt, "approval_reason": a.Reason,
		},
	})
	if err != nil {
		return SettleResult{}, err
	}
	heldPayouts, err := s.recordHeld(ctx, tx, m, st, held)
	if err != nil {
		return SettleResult{}, err
	}
	if err := s.closeOutInstrument(ctx, tx, m, len(heldPayouts) > 0, by, "prediction market voided"); err != nil {
		return SettleResult{}, err
	}
	next, err := s.marketTransition(ctx, tx, m, StatusVoided, by, p.Reason, st.EvidenceRef, nil, map[string]any{
		"settlement_id": st.ID.String(), "proposal_id": p.ID.String(), "approval_id": a.ID.String(),
		"proposed_by": p.ProposedBy.ID, "escrow_before": plan.EscrowBefore.String(), "total_refunded": plan.TotalPaid.String(),
		"total_held": st.TotalHeld.String(), "residual": plan.Residual.String(), "refund_rate_per_share": plan.RefundRate.String(),
		"holders": len(plan.Lines), "held_holders": len(held),
	})
	if err != nil {
		return SettleResult{}, err
	}
	if err := s.requireEmptied(ctx, tx, next, plan.Residual); err != nil {
		return SettleResult{}, err
	}
	return SettleResult{Market: next, Settlement: st, Plan: plan, Lines: lines, Held: heldPayouts}, nil
}

// auditVoid appends a void step to the admin audit stream.
func (s *Service) auditVoid(ctx context.Context, tx pgx.Tx, m Market, action string, by econ.Actor, reason, evidenceRef string,
	payload map[string]any, at time.Time) error {
	doc := map[string]any{"market_id": m.ID.String(), "symbol": m.Symbol, "status": string(m.Status), "mode": string(m.Mode)}
	for k, v := range payload {
		doc[k] = v
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "predmarket: encode audit payload")
	}
	_, err = s.audit.Append(ctx, tx, auditEvent(by, action, m, reason, evidenceRef, body, at))
	return err
}

// closeOutInstrument moves the market instrument to its status after a
// settlement or a void: HALTED once nothing is owed, CLOSE_ONLY while a held
// payout awaits its claim (a claim is a risk-reducing intent on the market
// instrument, which CLOSE_ONLY admits and HALTED would refuse). The last
// claim halts it.
func (s *Service) closeOutInstrument(ctx context.Context, tx pgx.Tx, m Market, owed bool, by econ.Actor, reason string) error {
	to := assets.StatusHalted
	if owed {
		to, reason = assets.StatusCloseOnly, reason+": held payouts await their claims"
	}
	return s.setInstrumentStatus(ctx, tx, m.InstrumentID, to, by, reason)
}

// requireUnencumbered refuses to settle or refund while an open reservation
// or hold backs any outcome share of the market: an order the book has not
// cancelled would otherwise be left reserving shares that no longer exist.
// The refusal names every reservation and hold (up to a bound), so an
// operator knows exactly which orders to cancel.
func (s *Service) requireUnencumbered(ctx context.Context, tx pgx.Tx, m Market) error {
	const bound = 100
	shares := make([]string, 0, len(m.Outcomes))
	for _, o := range m.Outcomes {
		shares = append(shares, o.ShareAssetID.String())
	}
	collect := func(sql string, args ...any) ([]string, error) {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var rid, acct, asset, qty string
			if err := rows.Scan(&rid, &acct, &asset, &qty); err != nil {
				return nil, err
			}
			out = append(out, rid+" (account "+acct+", share "+asset+", "+qty+" base units)")
		}
		return out, rows.Err()
	}
	reservations, err := collect(`SELECT id::text, account_id::text, asset_id::text, (quantity - consumed_quantity)::text
		FROM asset_reservations WHERE asset_id = ANY($1::uuid[]) AND status = 'ACTIVE'
		ORDER BY created_at, id LIMIT $2`, shares, bound+1)
	if err != nil {
		return dbErr("list share reservations", err)
	}
	holds, err := collect(`SELECT id::text, account_id::text, asset_id::text, quantity::text
		FROM withdrawal_holds WHERE asset_id = ANY($1::uuid[]) AND released_at IS NULL AND (expires_at IS NULL OR expires_at > $2)
		ORDER BY created_at, id LIMIT $3`, shares, s.now(), bound+1)
	if err != nil {
		return dbErr("list share holds", err)
	}
	if len(reservations) == 0 && len(holds) == 0 {
		return nil
	}
	truncated := len(reservations) > bound || len(holds) > bound
	if len(reservations) > bound {
		reservations = reservations[:bound]
	}
	if len(holds) > bound {
		holds = holds[:bound]
	}
	books := "no order book is configured to cancel them: cancel the orders that hold them"
	if s.books != nil {
		books = "the order book cancelled every resting order, so something else holds them: release them"
	}
	return errs.New(errs.CodeConflict, fmt.Sprintf(
		"predmarket: %d open reservation(s) and %d hold(s) still encumber the outcome shares of market %s; %s, then settle again",
		len(reservations), len(holds), m.Symbol, books)).
		WithField("market_id", m.ID.String()).WithField("refusal", "SHARES_RESERVED").
		WithField("reservations", reservations).WithField("holds", holds).WithField("truncated", truncated).
		WithField("outcome_books_configured", s.books != nil)
}

// requireEmptied is the post-condition of a settlement or refund, read from
// the books: no outcome share is outstanding or held by a customer, and the
// escrow holds exactly the disclosed residual plus the payouts it holds for
// their holders (zero after a payout with nothing held).
func (s *Service) requireEmptied(ctx context.Context, tx pgx.Tx, m Market, residual money.Quantity) error {
	b, err := s.Books(ctx, tx, m)
	if err != nil {
		return err
	}
	violations := b.Violations(residual)
	if b.Escrow.Cmp(residual.Add(b.Held)) != 0 {
		violations = append(violations, "the escrow does not hold exactly the residual and the held payouts")
	}
	for i := range b.SharesOutstanding {
		if !b.SharesOutstanding[i].IsZero() || !b.HeldByCustomers[i].IsZero() {
			violations = append(violations, "outcome "+m.Outcomes[i].Symbol+" still has shares outstanding")
		}
	}
	if len(violations) > 0 {
		return errs.New(errs.CodeReconciliationRequired, "predmarket: the settlement did not empty the market's books").
			WithField("market_id", m.ID.String()).WithField("violations", violations)
	}
	return nil
}

// heldBooking is one holder's payout or refund that could not be delivered:
// the refused intent and why, and the effect and journal that retired the
// holder's shares while the amount stayed in the escrow.
type heldBooking struct {
	line     PlanLine
	refused  ActionResult
	reasons  []string
	effect   econ.Effect
	journal  ledger.TransactionID
	evidence econ.EvidenceRef
	at       time.Time
}

// holderRiskReasons and holderRouteReasons are the refusals that are a
// holder's own: they depend only on the holder's account, identity,
// eligibility or jurisdiction. A payout refused for these alone is held; any
// other refusal stops the whole settlement.
var (
	holderRiskReasons = map[string]bool{
		risk.ReasonAccountStatus: true, risk.ReasonIdentityUnverified: true,
		risk.ReasonEligibilityDenied: true, risk.ReasonJurisdictionBlocked: true,
	}
	holderRouteReasons = map[string]bool{
		settlement.ReasonVerificationRequired: true, settlement.ReasonLegalRouterDenied: true,
	}
)

// holderScopedRefusal reports whether a payout's refusal names only the
// holder's own reasons, and returns them.
func holderScopedRefusal(err error) ([]string, bool) {
	e, ok := errs.As(err)
	if !ok {
		return nil, false
	}
	var (
		reasons []string
		allowed map[string]bool
	)
	switch {
	case e.Code == errs.CodeRiskRejected:
		reasons, _ = e.Fields["reason_codes"].([]string)
		allowed = holderRiskReasons
	case e.Code == errs.CodeForbidden && e.Fields["refusal_stage"] == "route":
		reasons, _ = e.Fields["reasons"].([]string)
		allowed = holderRouteReasons
	default:
		return nil, false
	}
	if len(reasons) == 0 {
		return nil, false
	}
	for _, r := range reasons {
		if !allowed[r] {
			return nil, false
		}
	}
	return append([]string(nil), reasons...), true
}

// pay runs one RESOLVE_PREDICTION_POSITION action per plan line, in account
// order, tracking the escrow each payout draws on. A line refused for the
// holder's own reasons is held (bookHeld) instead of stopping the others.
// attempt is the id the settlement record will carry.
func (s *Service) pay(ctx context.Context, tx pgx.Tx, m Market, plan SettlementPlan, by econ.Actor, instrumentStatus assets.Status,
	res *Resolution, attempt SettlementID) ([]ActionResult, []heldBooking, error) {
	remaining := plan.EscrowBefore
	out := make([]ActionResult, 0, len(plan.Lines))
	var held []heldBooking
	for _, line := range plan.Lines {
		a, err := s.settlementAction(ctx, tx, m, plan, line, by, instrumentStatus, remaining, res, attempt)
		if err != nil {
			return nil, nil, err
		}
		r, err := s.run(ctx, tx, a, m.Mode)
		switch {
		case err == nil && r.Replay:
			return nil, nil, errs.New(errs.CodeReconciliationRequired, "predmarket: a holder was already paid by an earlier settlement attempt").
				WithField("account_id", line.AccountID.String())
		case err == nil:
			out = append(out, r)
		default:
			reasons, scoped := holderScopedRefusal(err)
			if !scoped || r.Intent.Status != econ.StatusRejected {
				return nil, nil, err
			}
			hb, err := s.bookHeld(ctx, tx, m, plan, line, r, reasons)
			if err != nil {
				return nil, nil, err
			}
			r.Effect, r.JournalTransactionID = hb.effect, hb.journal
			out = append(out, r)
			held = append(held, hb)
		}
		remaining = remaining.Sub(line.Amount)
	}
	return out, held, nil
}

// settlementEffectKey is the deterministic key of one holder's settlement
// effect: delivered or held, a holder is settled once per market.
func settlementEffectKey(verb string, m Market, account fmt.Stringer) string {
	return "predmarket:" + verb + ":" + m.ID.String() + ":" + account.String()
}

func shareMap(m Market, line PlanLine) map[string]string {
	shares := make(map[string]string, len(m.Outcomes))
	for i, o := range m.Outcomes {
		shares[o.Symbol] = line.Shares[i].String()
	}
	return shares
}

// bookHeld settles a holder whose own payout was refused: their shares are
// retired exactly as a delivered payout retires them, under the same effect
// key (so a holder is settled once, delivered or held, never both), and the
// amount moves nowhere: it stays in the market's escrow, owed to the holder.
// The caller records the HeldPayout once the settlement record exists.
func (s *Service) bookHeld(ctx context.Context, tx pgx.Tx, m Market, plan SettlementPlan, line PlanLine, refused ActionResult, reasons []string) (heldBooking, error) {
	verb, kind := "payout", "PREDICTION_PAYOUT_HELD"
	if plan.Kind == SettlementRefund {
		verb, kind = "refund", "PREDICTION_REFUND_HELD"
	}
	key := settlementEffectKey(verb, m, line.AccountID)
	shares := shareMap(m, line)
	var hb heldBooking
	err := atomically(ctx, tx, func(sp pgx.Tx) error {
		eff, replayed, err := s.effects.Begin(ctx, sp, econ.BeginRequest{
			Key: key, Subsystem: econ.SubsystemPredictionMarkets, Kind: kind, AccountID: line.AccountID.String(),
			Request: map[string]any{
				"market_id": m.ID.String(), "account_id": line.AccountID.String(), "settlement": string(plan.Kind),
				"shares": shares, "amount": line.Amount.String(), "held": true,
				"refused_intent_id": refused.Intent.ID.String(), "reason_codes": reasons,
			},
			PostsLedger: true, Actor: serviceActor(), CorrelationID: refused.Intent.CorrelationID,
		})
		if err != nil {
			return err
		}
		if replayed {
			return errs.New(errs.CodeReconciliationRequired, "predmarket: this holder was already settled by an earlier attempt").
				WithField("effect_key", key).WithField("effect_status", string(eff.Status))
		}
		retire := line
		retire.Amount = money.Quantity{}
		p, err := SettlementPosting(m, plan.Kind, retire, postingMeta{
			Key: key, Reference: ledger.FinancialEventReference{Type: "prediction_" + verb + "_held", ID: refused.Intent.ID.String()},
			At: s.now(), Description: "prediction market " + m.Symbol + " " + verb + " held: shares retired, amount kept in escrow",
		})
		if err != nil {
			return err
		}
		posted, err := s.ledger.Post(ctx, sp, p)
		if err != nil {
			return err
		}
		if posted.Existing {
			return errs.New(errs.CodeReconciliationRequired, "predmarket: the journal transaction of this held settlement already exists").
				WithField("journal_transaction_id", posted.TransactionID.String())
		}
		ref, err := econ.RecordEvidence(ctx, sp, econ.Evidence{
			Key: "predmarket/" + strings.ToLower(kind) + "/" + refused.Intent.ID.String(),
			Document: map[string]any{
				"kind": kind, "market_id": m.ID.String(), "market_symbol": m.Symbol, "account_id": line.AccountID.String(),
				"settlement": string(plan.Kind), "shares_retired": shares, "amount_held": line.Amount.String(),
				"collateral_asset_id": m.CollateralAssetID.String(), "refused_intent_id": refused.Intent.ID.String(),
				"refusal_reason_codes": reasons, "risk_decision_hash": refused.DecisionHash,
				"journal_transaction_id": posted.TransactionID.String(),
				"held_in":                "PREDICTION_ESCROW of facility " + m.FacilityID.String(),
			},
		})
		if err != nil {
			return err
		}
		applied, err := s.effects.Apply(ctx, sp, eff.ID, econ.ApplyRequest{
			JournalTransactionID: &posted.TransactionID, Evidence: &ref, Actor: serviceActor(),
			Reason: kind + " posted: the holder's shares are retired and the amount stays in escrow until claimed",
		})
		if err != nil {
			return err
		}
		hb = heldBooking{line: line, refused: refused, reasons: reasons, effect: applied, journal: posted.TransactionID, evidence: ref, at: s.now()}
		return nil
	})
	return hb, err
}

// recordHeld writes one HeldPayout per held holder with an amount, once the
// settlement record exists. The database checks at COMMIT that they sum to
// the record's total_held.
func (s *Service) recordHeld(ctx context.Context, tx pgx.Tx, m Market, st Settlement, held []heldBooking) ([]HeldPayout, error) {
	out := make([]HeldPayout, 0, len(held))
	for _, hb := range held {
		if !hb.line.Amount.IsPositive() {
			continue
		}
		h := HeldPayout{
			ID: id.New[heldPayoutKind](), MarketID: m.ID, SettlementID: st.ID, AccountID: hb.line.AccountID, Kind: st.Kind,
			CollateralAssetID: m.CollateralAssetID, Amount: hb.line.Amount, ReasonCodes: hb.reasons,
			RefusedIntentID: hb.refused.Intent.ID, EffectID: hb.effect.ID, JournalTransactionID: hb.journal,
			EvidenceRef: hb.evidence.URI, EvidenceSHA256: hb.evidence.SHA256, HeldAt: hb.at,
		}
		if err := insertHeldPayout(ctx, tx, h); err != nil {
			return nil, err
		}
		stored, found, err := s.HeldPayoutOf(ctx, tx, m.ID, h.AccountID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errs.New(errs.CodeInternal, "predmarket: the held payout vanished")
		}
		out = append(out, stored)
	}
	return out, nil
}

// settlementAction is one holder's payout or refund as an economic action.
//
// Exactly once is the effect's business: its key is deterministic per market
// and holder, so no market pays a holder twice. The intent's idempotency key
// is scoped to the settlement attempt instead, because economic intents are
// unique per (account, key) across every subsystem: a deterministic key a
// customer could claim first, through any service that accepts client keys,
// would block the whole settlement.
func (s *Service) settlementAction(ctx context.Context, tx pgx.Tx, m Market, plan SettlementPlan, line PlanLine, by econ.Actor,
	instrumentStatus assets.Status, escrowRemaining money.Quantity, res *Resolution, attempt SettlementID) (action, error) {
	verb := "payout"
	effectKind := "PREDICTION_PAYOUT"
	if plan.Kind == SettlementRefund {
		verb, effectKind = "refund", "PREDICTION_REFUND"
	}
	facts, err := s.facts.Facts(ctx, tx, line.AccountID)
	if err != nil {
		return action{}, err
	}
	shares := shareMap(m, line)
	terms := map[string]any{
		"market_id": m.ID.String(), "market_symbol": m.Symbol, "settlement": string(plan.Kind), "shares": shares,
		"amount": line.Amount.String(), "rounding_policy": string(plan.Policy), "settlement_id": attempt.String(),
	}
	if plan.Kind == SettlementPayout {
		terms["winning_outcome"] = m.Outcomes[plan.WinningOutcome].Symbol
		if res != nil {
			terms["resolution_id"] = res.ID.String()
		}
	} else {
		terms["refund_rate_per_share"] = plan.RefundRate.String()
	}
	body, err := json.Marshal(terms)
	if err != nil {
		return action{}, errs.Wrap(err, errs.CodeInternal, "predmarket: encode settlement terms")
	}
	var qty *money.Quantity
	if line.Amount.IsPositive() {
		amt := line.Amount
		qty = &amt
	}
	collateral := m.CollateralAssetID
	key := settlementEffectKey(verb, m, line.AccountID)
	intentKey := "predmarket:" + verb + ":" + attempt.String() + ":" + line.AccountID.String()
	notional, err := usd(line.Amount, money.RoundUp)
	if err != nil {
		return action{}, err
	}
	in := econ.Intent{
		AccountID: line.AccountID, Kind: econ.KindResolvePredictionPosition, Mode: m.Mode, Capability: gates.PredictionMarkets,
		ActorType: by.Type, ActorID: by.ID, InstrumentID: m.InstrumentID.String(), AssetID: &collateral, Quantity: qty,
		Terms: body, IdempotencyKey: intentKey, CorrelationID: "predmarket:" + m.ID.String(),
	}
	return action{
		intent: in, facts: facts, mover: by,
		subject:    settlement.Subject{Type: settlement.SubjectInstrument, ID: m.InstrumentID.String(), AssetID: collateral.String()},
		effectKey:  func(econ.Intent) string { return key },
		effectKind: effectKind,
		riskInput: func(it econ.Intent) (risk.AdvancedInput, error) {
			kills, err := s.killSwitches(ctx, tx)
			if err != nil {
				return risk.AdvancedInput{}, err
			}
			// Both sides are converted the same way, so the comparison never
			// refuses what the exact amounts allow; the ledger refuses an
			// escrow that would go negative.
			have, err := usd(escrowRemaining, money.RoundDown)
			if err != nil {
				return risk.AdvancedInput{}, err
			}
			need, err := usd(line.Amount, money.RoundDown)
			if err != nil {
				return risk.AdvancedInput{}, err
			}
			now := s.now()
			return risk.AdvancedInput{
				Stage: risk.StageFinal,
				Intent: risk.AdvancedIntent{
					ID: it.ID.String(), Kind: string(it.Kind), AccountID: it.AccountID.String(), ActorType: string(it.ActorType),
					Capability: string(it.Capability), Mode: string(it.Mode), InstrumentID: m.InstrumentID.String(),
					InstrumentKind: "EVENT_OUTCOME", NotionalUSD: notional, RiskIncreasing: false,
				},
				Identity: &risk.IdentitySnapshot{Verified: facts.Verification.AtLeast(valuedomain.VerificationNodalIdentity)},
				Account:  &risk.AdvancedAccountSnapshot{Status: string(facts.Status)},
				// The session of a settlement intent is the settlement window,
				// which the caller has just established is open: the market is
				// RESOLVED with no dispute pending and its dispute window has
				// passed, or an approved void has opened its refund window.
				Instrument:   &risk.InstrumentSnapshot{Status: string(instrumentStatus), SessionOpen: true, Expired: !now.Before(m.CloseAt)},
				Balance:      &risk.BalanceSnapshot{Available: have, Required: need},
				KillSwitches: kills,
				Now:          now,
			}, nil
		},
		request: func(it econ.Intent) any {
			return map[string]any{
				"market_id": m.ID.String(), "account_id": it.AccountID.String(), "settlement": string(plan.Kind),
				"shares": shares, "amount": line.Amount.String(),
			}
		},
		posting: func(it econ.Intent) (ledger.Posting, error) {
			return SettlementPosting(m, plan.Kind, line, postingMeta{
				Key: key, Reference: ledger.FinancialEventReference{Type: "prediction_" + verb, ID: it.ID.String()},
				At: s.now(), Description: "prediction market " + m.Symbol + " " + verb,
			})
		},
		evidence: func(it econ.Intent, d risk.AdvancedDecision, r settlement.Route, _ []string, journal ledger.TransactionID) any {
			doc := map[string]any{
				"kind": effectKind, "market_id": m.ID.String(), "market_symbol": m.Symbol, "intent_id": it.ID.String(),
				"account_id": it.AccountID.String(), "shares_retired": shares, "amount": line.Amount.String(),
				"collateral_asset_id": collateral.String(), "rounding_policy": string(plan.Policy),
				"settled_by":             map[string]any{"type": string(by.Type), "id": by.ID},
				"route":                  map[string]any{"permitted": r.Permitted, "product": r.Product, "executor": string(r.Executor)},
				"risk_decision_hash":     d.Hash,
				"journal_transaction_id": journal.String(),
			}
			if plan.Kind == SettlementPayout {
				doc["winning_outcome"] = m.Outcomes[plan.WinningOutcome].Symbol
			} else {
				doc["refund_rate_per_share"] = plan.RefundRate.String()
			}
			if res != nil {
				doc["resolution_id"] = res.ID.String()
				doc["report_hash"] = hex.EncodeToString(res.ReportHash)
			}
			return doc
		},
	}, nil
}

// settlementRecord is what recordSettlement writes down.
type settlementRecord struct {
	attempt    SettlementID
	market     Market
	plan       SettlementPlan
	resolution *Resolution
	lines      []ActionResult
	held       []heldBooking
	by         econ.Actor
	reason     string
	evidence   any
}

// recordSettlement archives the settlement's evidence and writes its record.
func (s *Service) recordSettlement(ctx context.Context, tx pgx.Tx, r settlementRecord) (Settlement, error) {
	m, plan, res := r.market, r.plan, r.resolution
	heldBy := make(map[string]heldBooking, len(r.held))
	totalHeld := money.Quantity{}
	for _, hb := range r.held {
		heldBy[hb.line.AccountID.String()] = hb
		totalHeld = totalHeld.Add(hb.line.Amount)
	}
	st := Settlement{
		ID: r.attempt, MarketID: m.ID, Kind: plan.Kind, OutcomeCount: plan.OutcomeCount,
		UnitQuantity: plan.Unit, RoundingPolicy: plan.Policy, EscrowBefore: plan.EscrowBefore, TotalPaid: plan.TotalPaid,
		TotalHeld: totalHeld, Residual: plan.Residual, HolderCount: len(plan.Lines), SettledBy: r.by, SettledAt: s.now(),
	}
	if plan.Kind == SettlementPayout {
		if res == nil {
			return Settlement{}, errs.New(errs.CodeInternal, "predmarket: a payout needs its resolution")
		}
		w := plan.WinningOutcome
		st.ResolutionID, st.WinningOutcomeIndex = &res.ID, &w
	} else {
		rate := plan.RefundRate
		st.RefundRatePerShare = &rate
	}
	rows := make([]map[string]any, 0, len(plan.Lines))
	for i, line := range plan.Lines {
		row := map[string]any{"account_id": line.AccountID.String(), "shares": shareMap(m, line), "amount": line.Amount.String()}
		if i < len(r.lines) {
			row["intent_id"] = r.lines[i].Intent.ID.String()
			row["journal_transaction_id"] = r.lines[i].JournalTransactionID.String()
		}
		if hb, ok := heldBy[line.AccountID.String()]; ok {
			row["held"] = true
			row["held_reason_codes"] = hb.reasons
		}
		rows = append(rows, row)
	}
	doc := map[string]any{
		"kind": "PREDICTION_SETTLEMENT", "settlement_kind": string(plan.Kind), "settlement_id": st.ID.String(),
		"market_id": m.ID.String(), "market_symbol": m.Symbol, "outcome_count": plan.OutcomeCount,
		"unit_quantity": plan.Unit.String(), "rounding_policy": string(plan.Policy),
		"escrow_before": plan.EscrowBefore.String(), "total_paid": plan.TotalPaid.String(), "total_held": totalHeld.String(),
		"residual":     plan.Residual.String(),
		"conservation": "escrow_before = total_paid + residual; total_held of total_paid stays in escrow until claimed",
		"holders":      rows,
		"settled_by":   map[string]any{"type": string(r.by.Type), "id": r.by.ID}, "settled_at": st.SettledAt,
	}
	if res != nil {
		doc["resolution_id"] = res.ID.String()
		doc["winning_outcome"] = m.Outcomes[plan.WinningOutcome].Symbol
	} else {
		doc["refund_rate_per_share"] = plan.RefundRate.String()
		doc["residual_per_complete_set"] = plan.Unit.Sub(plan.RefundRate.Mul(money.QuantityFromInt64(int64(plan.OutcomeCount)))).String()
	}
	if r.reason != "" {
		doc["reason"] = r.reason
	}
	if r.evidence != nil {
		doc["evidence"] = r.evidence
	}
	ref, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "predmarket/settlement/" + m.ID.String(), Document: doc})
	if err != nil {
		return Settlement{}, err
	}
	st.EvidenceRef, st.EvidenceSHA256 = ref.URI, ref.SHA256
	if err := insertSettlement(ctx, tx, st); err != nil {
		return Settlement{}, err
	}
	stored, found, err := s.SettlementOf(ctx, tx, m.ID)
	if err != nil {
		return Settlement{}, err
	}
	if !found {
		return Settlement{}, errs.New(errs.CodeInternal, "predmarket: the settlement record vanished")
	}
	return stored, nil
}
