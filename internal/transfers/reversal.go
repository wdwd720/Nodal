package transfers

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

// A completed transfer is reversed under dual control (review finding F3):
//
//  1. one OPERATOR holding ledger:post_correction, with a recent strong
//     authentication, PROPOSES the reversal of a COMPLETED transfer with a
//     stable reason code and a reason (ProposeReversal); nothing moves;
//  2. a SECOND, different operator holding the same permission, with a
//     recent strong authentication, APPROVES it (ApproveReversal), and the
//     approval executes the reversal in the same transaction: the decision,
//     the COMPENSATION journal, the compensated effect and the REVERSED
//     transfer are one fact;
//  3. or anyone holding the permission -- the proposer withdrawing it, say --
//     REJECTS it (RejectReversal), and it can never execute.
//
// A proposal is decided once. At most one proposal of a transfer is open at a
// time, so two operators can never race two reversals of one transfer, and an
// approved proposal is consumed by the reversal it executed. The database
// holds the same line (migration 00758): an approval by the proposer is
// refused (PT006); a reversal that does not name an APPROVED proposal of that
// transfer by a second operator who is the operator recorded as reversing it,
// or an approval whose reversal did not happen in its transaction, is refused
// at COMMIT (PT007); a proposal for a transfer that is not COMPLETED, while
// another one is open, or out of sequence, is refused (PT008).

type reversalProposalKind struct{}

// ReversalProposalID identifies a proposal to reverse a transfer.
type ReversalProposalID = id.ID[reversalProposalKind]

// NewReversalProposalID returns a fresh proposal id.
func NewReversalProposalID() ReversalProposalID { return id.New[reversalProposalKind]() }

// ParseReversalProposalID parses the canonical form.
func ParseReversalProposalID(s string) (ReversalProposalID, error) {
	return id.Parse[reversalProposalKind](s)
}

// ReversalDecision is the one decision a proposal receives.
type ReversalDecision string

// Decisions.
const (
	ReversalApproved ReversalDecision = "APPROVED"
	ReversalRejected ReversalDecision = "REJECTED"
)

// AllReversalDecisions returns every decision (a copy). It is the Go twin of
// the CHECK transfer_reversal_decisions_decision_check.
func AllReversalDecisions() []ReversalDecision {
	return []ReversalDecision{ReversalApproved, ReversalRejected}
}

// Valid reports whether d is a declared decision.
func (d ReversalDecision) Valid() bool { return d == ReversalApproved || d == ReversalRejected }

// ReversalProposalStatus is where a proposal stands. It is derived from the
// proposal's decision, never stored: PENDING has no decision yet, APPROVED
// was decided and executed in the same transaction, REJECTED never executes.
type ReversalProposalStatus string

// Proposal statuses.
const (
	ProposalPending  ReversalProposalStatus = "PENDING"
	ProposalApproved ReversalProposalStatus = "APPROVED"
	ProposalRejected ReversalProposalStatus = "REJECTED"
)

// AllReversalProposalStatuses returns every status (a copy).
func AllReversalProposalStatuses() []ReversalProposalStatus {
	return []ReversalProposalStatus{ProposalPending, ProposalApproved, ProposalRejected}
}

// Valid reports whether s is a declared status.
func (s ReversalProposalStatus) Valid() bool {
	for _, x := range AllReversalProposalStatuses() {
		if x == s {
			return true
		}
	}
	return false
}

// ReversalProposal is a transfer_reversal_proposals row with its decision, if
// any.
type ReversalProposal struct {
	ID         ReversalProposalID
	TransferID TransferID
	// Seq numbers the proposals of one transfer: 1, 2, 3 ...
	Seq           int
	ReasonCode    string
	Reason        string
	ProposedBy    econ.Actor
	ProposedAt    time.Time
	CorrelationID string
	// Decision, DecisionReason, DecidedBy and DecidedAt are set once the
	// proposal is decided; Decision is nil while it is PENDING.
	Decision       *ReversalDecision
	DecisionReason string
	DecidedBy      *econ.Actor
	DecidedAt      *time.Time
}

// Status is where the proposal stands.
func (p ReversalProposal) Status() ReversalProposalStatus {
	switch {
	case p.Decision == nil:
		return ProposalPending
	case *p.Decision == ReversalApproved:
		return ProposalApproved
	default:
		return ProposalRejected
	}
}

// ProposeReversalRequest proposes the reversal of a COMPLETED transfer.
type ProposeReversalRequest struct {
	TransferID TransferID
	// ReasonCode is a stable code (e.g. FRAUD_CONFIRMED); it becomes the
	// compensation journal's reason_code if the proposal is approved.
	ReasonCode    string
	Reason        string
	CorrelationID string
}

// DecideReversalRequest approves or rejects a proposal. Reason is optional
// for an approval and required for a rejection.
type DecideReversalRequest struct {
	ProposalID    ReversalProposalID
	Reason        string
	CorrelationID string
}

// ReversalProposalResult is the outcome of ProposeReversal.
type ReversalProposalResult struct {
	Proposal ReversalProposal
	// Replayed is true when the same operator had already made the same
	// proposal and it is still open: nothing new was written.
	Replayed bool
}

// ReversalDecisionResult is the outcome of ApproveReversal or RejectReversal.
type ReversalDecisionResult struct {
	Proposal ReversalProposal
	// Transfer is REVERSED after an approval and unchanged after a rejection.
	Transfer Transfer
	// Replayed is true when the same operator had already made the same
	// decision: nothing new was written.
	Replayed bool
}

// ReversalProposalFilter selects proposals for ListReversalProposals.
type ReversalProposalFilter struct {
	// Status, when set, lists only proposals in that status.
	Status ReversalProposalStatus
	// TransferID, when set, lists only the proposals of that transfer.
	TransferID *TransferID
	// Cursor is the id of the last proposal of the previous page.
	Cursor string
	// Limit bounds the page; <= 0 or > 200 means 50.
	Limit int
}

// maxReversalReason bounds a proposal's or a decision's reason, in bytes.
// The columns bound characters to 500, which a 500-byte reason never exceeds.
const maxReversalReason = 500

// rejectionSelfApproval is the code an approval by the proposer is refused
// with.
const rejectionSelfApproval = "TRANSFER_REVERSAL_SELF_APPROVAL"

func validReversalReason(r string) bool {
	return strings.TrimSpace(r) != "" && len(r) <= maxReversalReason && utf8.ValidString(r)
}

func (r ProposeReversalRequest) validate() error {
	var problems []string
	if r.TransferID.IsZero() {
		problems = append(problems, "transfer_id required")
	}
	if !validReasonCode(r.ReasonCode) {
		problems = append(problems, "reason_code must be a stable code (^[A-Z][A-Z0-9_]{2,63}$)")
	}
	if !validReversalReason(r.Reason) {
		problems = append(problems, "reason must be 1..500 bytes of UTF-8 and not blank")
	}
	if len(r.CorrelationID) > 200 {
		problems = append(problems, "correlation id is at most 200 bytes")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid reversal proposal").WithField("problems", problems)
	}
	return nil
}

func (r DecideReversalRequest) validate(decision ReversalDecision) error {
	var problems []string
	if r.ProposalID.IsZero() {
		problems = append(problems, "proposal_id required")
	}
	switch {
	case decision == ReversalRejected && !validReversalReason(r.Reason):
		problems = append(problems, "a rejection states its reason in 1..500 bytes of UTF-8")
	case r.Reason != "" && !validReversalReason(r.Reason):
		problems = append(problems, "reason, when given, must be 1..500 bytes of UTF-8 and not blank")
	}
	if len(r.CorrelationID) > 200 {
		problems = append(problems, "correlation id is at most 200 bytes")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid reversal decision").WithField("problems", problems)
	}
	return nil
}

// correctionOperator admits the principal of a proposal or a decision: a
// valid OPERATOR (never an agent, never a customer) holding
// ledger:post_correction who authenticated strongly within the policy's
// step-up age. It returns ctx carrying the principal and the actor to record.
func (s *Service) correctionOperator(ctx context.Context, p security.Principal) (context.Context, econ.Actor, error) {
	if p.IsAgent() || p.ActorType != security.ActorOperator {
		return ctx, econ.Actor{}, errs.New(errs.CodeForbidden, "only an operator proposes, approves or rejects a reversal").
			WithField("actor_type", string(p.ActorType))
	}
	if err := p.Validate(); err != nil {
		return ctx, econ.Actor{}, errs.Wrap(err, errs.CodeForbidden, "invalid principal")
	}
	ctx = security.WithPrincipal(ctx, p)
	if err := security.RequireAt(ctx, security.PermLedgerPostCorrection, s.clk.Now); err != nil {
		return ctx, econ.Actor{}, securityErr(err)
	}
	if err := security.RequireStepUp(ctx, s.policy.StepUpMaxAge(), s.clk.Now); err != nil {
		if errors.Is(err, security.ErrStepUpRequired) {
			return ctx, econ.Actor{}, errs.Wrap(err, errs.CodeStepUpRequired,
				"a reversal is proposed and decided only after a recent strong authentication").
				WithField("step_up_max_age_ms", s.policy.StepUpMaxAgeMS)
		}
		return ctx, econ.Actor{}, securityErr(err)
	}
	return ctx, econ.Actor{Type: security.ActorOperator, ID: p.SubjectID}, nil
}

// mayReadReversals reports whether p may read reversal proposals: an
// operator who may read any account (a proposal names two customers' accounts
// and the reason one of them is being corrected).
func (s *Service) mayReadReversals(p security.Principal) bool {
	return p.Validate() == nil && !p.IsAgent() && p.ActorType == security.ActorOperator &&
		p.Has(security.PermAccountReadAny, s.clk.Now())
}

// lockForReversal takes, in the order every reversal path uses, the
// recipient's transfer lock and then the transfer row, and returns the
// transfer as it is under them.
func lockForReversal(ctx context.Context, tx pgx.Tx, tid TransferID) (Transfer, error) {
	peek, err := getTransfer(ctx, tx, tid, false)
	if err != nil {
		return Transfer{}, err
	}
	// The reversal debits the recipient, so every step of it takes the
	// recipient's transfer lock: it cannot interleave with a transfer the
	// recipient is sending, nor with another decision about this transfer.
	if err := lockAccount(ctx, tx, peek.RecipientAccountID); err != nil {
		return Transfer{}, err
	}
	return getTransfer(ctx, tx, tid, true)
}

// ProposeReversal records one operator's proposal to reverse a COMPLETED
// transfer. Nothing moves until a second operator approves it.
//
// At most one proposal of a transfer is open at a time: the same operator
// proposing the same reversal again gets the open proposal back (Replayed);
// any other proposal while one is open is a CONFLICT naming it. A transfer
// that is not COMPLETED (REVERSED, REJECTED) is INVALID_STATE_TRANSITION.
func (s *Service) ProposeReversal(ctx context.Context, tx pgx.Tx, p security.Principal, r ProposeReversalRequest) (ReversalProposalResult, error) {
	if tx == nil {
		return ReversalProposalResult{}, errs.New(errs.CodeInternal, "transfers: ProposeReversal requires a transaction")
	}
	ctx, actor, err := s.correctionOperator(ctx, p)
	if err != nil {
		return ReversalProposalResult{}, err
	}
	if err := r.validate(); err != nil {
		return ReversalProposalResult{}, err
	}
	now := s.clk.Now().UTC()
	t, err := lockForReversal(ctx, tx, r.TransferID)
	if err != nil {
		return ReversalProposalResult{}, err
	}
	if t.Status != StatusCompleted {
		return ReversalProposalResult{}, errs.Newf(errs.CodeInvalidStateTransition,
			"a %s transfer cannot be proposed for reversal; only a COMPLETED one can", t.Status).
			WithField("transfer_id", t.ID.String()).WithField("status", string(t.Status))
	}
	existing, err := proposalsOfTransfer(ctx, tx, t.ID)
	if err != nil {
		return ReversalProposalResult{}, err
	}
	for _, x := range existing {
		if x.Status() != ProposalPending {
			continue
		}
		if x.ProposedBy.ID == actor.ID && x.ReasonCode == r.ReasonCode && x.Reason == r.Reason {
			return ReversalProposalResult{Proposal: x, Replayed: true}, nil
		}
		return ReversalProposalResult{}, errs.New(errs.CodeConflict,
			"a reversal of this transfer is already proposed and awaits a decision").
			WithField("proposal_id", x.ID.String()).WithField("transfer_id", t.ID.String())
	}
	prop := ReversalProposal{
		ID: NewReversalProposalID(), TransferID: t.ID, Seq: len(existing) + 1, ReasonCode: r.ReasonCode, Reason: r.Reason,
		ProposedBy: actor, ProposedAt: now, CorrelationID: r.CorrelationID,
	}
	if err := insertProposal(ctx, tx, prop); err != nil {
		return ReversalProposalResult{}, err
	}
	if err := s.auditReversal(ctx, tx, t, prop, nil, actor, prop.Reason, r.CorrelationID, now); err != nil {
		return ReversalProposalResult{}, err
	}
	// The row as stored (timestamps at the database's precision), so the
	// answer is exactly what every later read returns.
	stored, err := proposalByID(ctx, tx, prop.ID)
	if err != nil {
		return ReversalProposalResult{}, err
	}
	return ReversalProposalResult{Proposal: stored}, nil
}

// ApproveReversal is the second operator's approval of a proposal, and it
// executes the reversal in the same transaction: the decision, a
// COMPENSATION journal mirroring the transfer (reversal_of = its journal,
// reason code = the proposal's), the consumed reservation of the recipient's
// funds, the evidence, the COMPENSATED effect, the REVERSED transfer and the
// recipient's notification. The operator who proposed it can never approve it
// (FORBIDDEN, nothing written).
//
// A decided proposal is final: the same operator approving it again gets the
// same outcome (Replayed); any other decision is a CONFLICT. A recipient who
// no longer holds the funds makes the reversal a CONFLICT
// (TRANSFER_REVERSAL_FUNDS_UNAVAILABLE): roll tx back and the proposal stays
// open, to be approved once the funds are back or rejected.
func (s *Service) ApproveReversal(ctx context.Context, tx pgx.Tx, p security.Principal, r DecideReversalRequest) (ReversalDecisionResult, error) {
	return s.decideReversal(ctx, tx, p, r, ReversalApproved)
}

// RejectReversal rejects a proposal, with a reason; it never executes. Any
// operator holding ledger:post_correction may reject one, the proposer
// withdrawing it included. The same operator rejecting it again is a replay;
// any other decision of a decided proposal is a CONFLICT.
func (s *Service) RejectReversal(ctx context.Context, tx pgx.Tx, p security.Principal, r DecideReversalRequest) (ReversalDecisionResult, error) {
	return s.decideReversal(ctx, tx, p, r, ReversalRejected)
}

func (s *Service) decideReversal(ctx context.Context, tx pgx.Tx, p security.Principal, r DecideReversalRequest, decision ReversalDecision) (ReversalDecisionResult, error) {
	if tx == nil {
		return ReversalDecisionResult{}, errs.New(errs.CodeInternal, "transfers: a reversal decision requires a transaction")
	}
	ctx, actor, err := s.correctionOperator(ctx, p)
	if err != nil {
		return ReversalDecisionResult{}, err
	}
	if err := r.validate(decision); err != nil {
		return ReversalDecisionResult{}, err
	}
	now := s.clk.Now().UTC()
	peek, err := proposalByID(ctx, tx, r.ProposalID)
	if err != nil {
		return ReversalDecisionResult{}, err
	}
	t, err := lockForReversal(ctx, tx, peek.TransferID)
	if err != nil {
		return ReversalDecisionResult{}, err
	}
	// Read again under the locks: a concurrent decision is now visible.
	prop, err := proposalByID(ctx, tx, r.ProposalID)
	if err != nil {
		return ReversalDecisionResult{}, err
	}
	if prop.Decision != nil {
		if *prop.Decision == decision && prop.DecidedBy != nil && prop.DecidedBy.ID == actor.ID {
			return ReversalDecisionResult{Proposal: prop, Transfer: t, Replayed: true}, nil
		}
		return ReversalDecisionResult{}, errs.Newf(errs.CodeConflict, "reversal proposal %s was already %s", prop.ID, *prop.Decision).
			WithField("proposal_id", prop.ID.String()).WithField("decision", string(*prop.Decision))
	}
	if decision == ReversalApproved && actor.ID == prop.ProposedBy.ID {
		return ReversalDecisionResult{}, errs.Wrap(security.ErrSelfApproval, errs.CodeForbidden,
			"the operator who proposed a reversal cannot approve it; a second operator must").
			WithField("proposal_id", prop.ID.String()).WithField("rejection_code", rejectionSelfApproval)
	}
	if decision == ReversalApproved && t.Status != StatusCompleted {
		// Unreachable while the database holds 00758: an open proposal exists
		// only for a COMPLETED transfer, and nothing else reverses one.
		return ReversalDecisionResult{}, errs.Newf(errs.CodeInvalidStateTransition, "a %s transfer cannot be reversed", t.Status).
			WithField("transfer_id", t.ID.String()).WithField("proposal_id", prop.ID.String())
	}
	corr := r.CorrelationID
	if corr == "" {
		corr = prop.CorrelationID
	}
	if err := insertDecision(ctx, tx, prop.ID, decision, r.Reason, actor, corr, now); err != nil {
		return ReversalDecisionResult{}, err
	}
	reason := r.Reason
	if reason == "" {
		reason = string(decision)
	}
	// Recorded first, which also takes the sender's audit stream before any
	// ledger row: every transaction touching this transfer takes them in the
	// same order.
	if err := s.auditReversal(ctx, tx, t, prop, &decision, actor, reason, corr, now); err != nil {
		return ReversalDecisionResult{}, err
	}
	out := t
	if decision == ReversalApproved {
		if out, err = s.executeReversal(ctx, tx, actor, t, prop, r.Reason, corr, now); err != nil {
			return ReversalDecisionResult{}, err
		}
	}
	if prop, err = proposalByID(ctx, tx, prop.ID); err != nil {
		return ReversalDecisionResult{}, err
	}
	return ReversalDecisionResult{Proposal: prop, Transfer: out}, nil
}

// ReversalProposal returns one proposal to an operator who may read any
// account. Anyone else is told it does not exist.
func (s *Service) ReversalProposal(ctx context.Context, q db.Querier, p security.Principal, pid ReversalProposalID) (ReversalProposal, error) {
	if !s.mayReadReversals(p) {
		return ReversalProposal{}, proposalNotFound(pid)
	}
	return proposalByID(ctx, q, pid)
}

// ListReversalProposals pages proposals, newest first, to an operator who may
// read any account. The cursor is the id of the last proposal of the previous
// page; the returned cursor is empty on the last page.
func (s *Service) ListReversalProposals(ctx context.Context, q db.Querier, p security.Principal, f ReversalProposalFilter) ([]ReversalProposal, string, error) {
	if !s.mayReadReversals(p) {
		return nil, "", errs.New(errs.CodeForbidden, "not permitted to read reversal proposals")
	}
	if f.Status != "" && !f.Status.Valid() {
		return nil, "", errs.Newf(errs.CodeValidationFailed, "unknown proposal status %q", f.Status)
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var cursor *ReversalProposalID
	if f.Cursor != "" {
		cid, err := ParseReversalProposalID(f.Cursor)
		if err != nil || cid.IsZero() {
			return nil, "", errs.New(errs.CodeValidationFailed, "invalid cursor")
		}
		cursor = &cid
	}
	rows, err := q.Query(ctx, `SELECT `+proposalColumns+` FROM transfer_reversal_proposals p
		LEFT JOIN transfer_reversal_decisions d ON d.proposal_id = p.id
		WHERE ($1::text = '' OR ($1::text = 'PENDING' AND d.id IS NULL) OR d.decision = $1::text)
		  AND ($2::uuid IS NULL OR p.transfer_id = $2::uuid)
		  AND ($3::uuid IS NULL OR (p.proposed_at, p.id) < (SELECT c.proposed_at, c.id FROM transfer_reversal_proposals c WHERE c.id = $3::uuid))
		ORDER BY p.proposed_at DESC, p.id DESC LIMIT $4`, string(f.Status), f.TransferID, cursor, limit+1)
	if err != nil {
		return nil, "", dbErr("list reversal proposals", err)
	}
	out, err := scanProposals(rows)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID.String()
	}
	return out, next, nil
}

// reversalEvent is the payload of every reversal audit record: ids, codes
// and actors only.
type reversalEvent struct {
	ProposalID string `json:"proposal_id"`
	TransferID string `json:"transfer_id"`
	Seq        int    `json:"seq"`
	ReasonCode string `json:"reason_code"`
	ProposedBy string `json:"proposed_by"`
	Decision   string `json:"decision,omitempty"`
	DecidedBy  string `json:"decided_by,omitempty"`
}

// auditReversal appends the audit record of a proposal (decision nil) or of
// its decision to the sender account's stream, the stream of every record
// about the transfer.
func (s *Service) auditReversal(ctx context.Context, tx pgx.Tx, t Transfer, prop ReversalProposal, decision *ReversalDecision, actor econ.Actor, reason, correlationID string, now time.Time) error {
	ev := reversalEvent{
		ProposalID: prop.ID.String(), TransferID: t.ID.String(), Seq: prop.Seq, ReasonCode: prop.ReasonCode,
		ProposedBy: prop.ProposedBy.ID,
	}
	action := "transfers.reversal.proposed"
	if decision != nil {
		action = "transfers.reversal." + strings.ToLower(string(*decision))
		ev.Decision, ev.DecidedBy = string(*decision), actor.ID
	}
	if correlationID == "" {
		correlationID = t.CorrelationID
	}
	return s.appendAudit(ctx, tx, t, auditSubject{
		resourceType: "transfer_reversal_proposal", resourceID: prop.ID.String(), correlationID: correlationID,
	}, action, actor, reason, "", ev, now)
}

// --- SQL ---------------------------------------------------------------------

const proposalColumns = `p.id, p.transfer_id, p.seq, p.reason_code, p.reason, p.proposed_by_actor_type, p.proposed_by_actor_id,
	coalesce(p.correlation_id,''), p.proposed_at, d.decision, coalesce(d.reason,''), d.decided_by_actor_type,
	d.decided_by_actor_id, d.decided_at`

func scanProposal(row pgx.Row) (ReversalProposal, error) {
	var r ReversalProposal
	var decision, decidedType, decidedID *string
	if err := row.Scan(&r.ID, &r.TransferID, &r.Seq, &r.ReasonCode, &r.Reason, &r.ProposedBy.Type, &r.ProposedBy.ID,
		&r.CorrelationID, &r.ProposedAt, &decision, &r.DecisionReason, &decidedType, &decidedID, &r.DecidedAt); err != nil {
		return ReversalProposal{}, err
	}
	r.ProposedAt = r.ProposedAt.UTC()
	if decision != nil {
		d := ReversalDecision(*decision)
		r.Decision = &d
		actor := econ.Actor{}
		if decidedType != nil {
			actor.Type = security.ActorType(*decidedType)
		}
		if decidedID != nil {
			actor.ID = *decidedID
		}
		r.DecidedBy = &actor
	}
	if r.DecidedAt != nil {
		at := r.DecidedAt.UTC()
		r.DecidedAt = &at
	}
	return r, nil
}

func scanProposals(rows pgx.Rows) ([]ReversalProposal, error) {
	defer rows.Close()
	out := []ReversalProposal{}
	for rows.Next() {
		r, err := scanProposal(rows)
		if err != nil {
			return nil, dbErr("scan reversal proposal", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("list reversal proposals", err)
	}
	return out, nil
}

func proposalNotFound(pid ReversalProposalID) *errs.Error {
	return errs.New(errs.CodeNotFound, "reversal proposal not found").WithField("proposal_id", pid.String())
}

func proposalByID(ctx context.Context, q db.Querier, pid ReversalProposalID) (ReversalProposal, error) {
	r, err := scanProposal(q.QueryRow(ctx, `SELECT `+proposalColumns+` FROM transfer_reversal_proposals p
		LEFT JOIN transfer_reversal_decisions d ON d.proposal_id = p.id WHERE p.id = $1`, pid))
	if errors.Is(err, pgx.ErrNoRows) {
		return ReversalProposal{}, proposalNotFound(pid)
	}
	if err != nil {
		return ReversalProposal{}, dbErr("load reversal proposal", err)
	}
	return r, nil
}

// proposalsOfTransfer lists a transfer's proposals in sequence order.
func proposalsOfTransfer(ctx context.Context, q db.Querier, tid TransferID) ([]ReversalProposal, error) {
	rows, err := q.Query(ctx, `SELECT `+proposalColumns+` FROM transfer_reversal_proposals p
		LEFT JOIN transfer_reversal_decisions d ON d.proposal_id = p.id WHERE p.transfer_id = $1 ORDER BY p.seq`, tid)
	if err != nil {
		return nil, dbErr("list reversal proposals of transfer", err)
	}
	return scanProposals(rows)
}

func insertProposal(ctx context.Context, tx pgx.Tx, r ReversalProposal) error {
	_, err := tx.Exec(ctx, `INSERT INTO transfer_reversal_proposals (id, transfer_id, seq, reason_code, reason,
		proposed_by_actor_type, proposed_by_actor_id, correlation_id, proposed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9)`,
		r.ID, r.TransferID, r.Seq, r.ReasonCode, r.Reason, r.ProposedBy.Type, r.ProposedBy.ID, r.CorrelationID, r.ProposedAt)
	if err != nil {
		return reversalDBErr("record reversal proposal", err)
	}
	return nil
}

func insertDecision(ctx context.Context, tx pgx.Tx, pid ReversalProposalID, decision ReversalDecision, reason string, actor econ.Actor, correlationID string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO transfer_reversal_decisions (id, proposal_id, decision, reason, decided_by_actor_type,
		decided_by_actor_id, correlation_id, decided_at) VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,NULLIF($7,''),$8)`,
		id.New[id.Any](), pid, decision, reason, actor.Type, actor.ID, correlationID, at)
	if err != nil {
		return reversalDBErr("record reversal decision", err)
	}
	return nil
}

// reversalDBErr maps the database's own refusals of a proposal or a decision
// onto stable codes. The Go checks run first under the same locks, so these
// fire only on a race the locks did not cover or on a defect: still a
// refusal, never a silent success. A retryable failure stays retryable.
func reversalDBErr(op string, err error) error {
	switch {
	case db.IsRetryable(err):
		return err
	case db.IsUniqueViolation(err):
		return errs.Wrap(err, errs.CodeConflict, "transfers: "+op+": a concurrent proposal or decision got there first")
	case db.SQLState(err) == "PT006":
		return errs.Wrap(err, errs.CodeForbidden, "transfers: "+op+": the proposer cannot approve their own reversal").
			WithField("rejection_code", rejectionSelfApproval)
	case db.SQLState(err) == "PT008":
		return errs.Wrap(err, errs.CodeConflict, "transfers: "+op+": the transfer is not open to a new reversal proposal")
	}
	return dbErr(op, err)
}
