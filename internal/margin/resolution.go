package margin

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// A case in MANUAL_REVIEW leaves it in exactly one way: one operator proposes
// a resolution (ProposeCaseResolution) and a second, distinct operator
// approves it (DecideCaseResolution). The approval applies the outcome in the
// same transaction:
//
//   - RESUME returns the case to IN_PROGRESS with a fresh budget window of
//     trading steps, and the liquidation queue takes it up again;
//   - DECLARE_BANKRUPT records the loans the books carry as the case's
//     deficit and makes the case and the account BANKRUPT, ready for
//     CoverDeficit. It is refused while anything is left to sell, apply or
//     sweep, or derivative exposure remains: a bankruptcy is what is left
//     after everything else, never a way to leave a customer their collateral.
//
// Interest charged up to the moment the case entered review stays charged;
// from then until the resolution it is waived (recorded with a zero amount),
// so a person's deliberation never costs the customer interest.
//
// The database holds the same line (migration 00754): a transition out of
// MANUAL_REVIEW must name an APPROVED resolution of that case and of the
// review it is in now, whose outcome is that transition, approved by an
// OPERATOR other than the OPERATOR who proposed it (MG009); a resolution
// licenses one exit.

// Resolution is a margin_case_resolutions row with its decision, if any.
type Resolution struct {
	ID     ResolutionID
	CaseID CaseID
	// ReviewTransitionID names the transition that put the case in the review
	// this resolution answers.
	ReviewTransitionID string
	Outcome            ResolutionOutcome
	Reason             string
	ProposedBy         econ.Actor
	ProposedAt         time.Time
	// Decision, DecidedBy, DecidedAt and DecisionReason are set once a second
	// operator approved or anyone rejected the proposal.
	Decision       *ResolutionDecision
	DecidedBy      *econ.Actor
	DecidedAt      *time.Time
	DecisionReason string
}

// ProposeResolutionRequest proposes how a MANUAL_REVIEW case ends.
type ProposeResolutionRequest struct {
	CaseID   CaseID
	Outcome  ResolutionOutcome
	Reason   string
	Proposer econ.Actor
}

// DecideResolutionRequest approves or rejects a proposed resolution.
type DecideResolutionRequest struct {
	ResolutionID ResolutionID
	Decision     ResolutionDecision
	Reason       string
	Decider      econ.Actor
}

// ResolutionResult is the outcome of a decision.
type ResolutionResult struct {
	Resolution    Resolution
	Case          Case
	AccountStatus AccountStatus
	// Waived are the interest periods of the review, recorded at zero.
	Waived   []Accrual
	Replayed bool
}

const maxResolutionReason = 1000

func validOperator(a econ.Actor) error {
	if a.Type != security.ActorOperator || strings.TrimSpace(a.ID) == "" || len(a.ID) > 200 {
		return errs.New(errs.CodeForbidden, "margin: a manual review is resolved by individually identified operators only")
	}
	return nil
}

func validReason(r string) error {
	if t := strings.TrimSpace(r); t == "" || len(r) > maxResolutionReason {
		return errs.New(errs.CodeValidationFailed, "margin: a resolution states its reason in 1..1000 bytes")
	}
	return nil
}

// ProposeCaseResolution records an operator's proposal for a case in
// MANUAL_REVIEW. Nothing moves until a second operator approves it.
func (s *Service) ProposeCaseResolution(ctx context.Context, tx pgx.Tx, r ProposeResolutionRequest) (Resolution, error) {
	if !r.Outcome.Valid() {
		return Resolution{}, errs.Newf(errs.CodeValidationFailed, "margin: unknown resolution outcome %q", r.Outcome)
	}
	if err := validReason(r.Reason); err != nil {
		return Resolution{}, err
	}
	if err := validOperator(r.Proposer); err != nil {
		return Resolution{}, err
	}
	a, c, err := s.lockCase(ctx, tx, r.CaseID)
	if err != nil {
		return Resolution{}, err
	}
	if c.Status != CaseManualReview {
		return Resolution{}, errs.Newf(errs.CodeInvalidStateTransition, "margin: case %s is %s; only a case in MANUAL_REVIEW is resolved", c.ID, c.Status)
	}
	review, err := s.st.currentReview(ctx, tx, c.ID)
	if err != nil {
		return Resolution{}, err
	}
	res := Resolution{ID: newResolutionID(), CaseID: c.ID, ReviewTransitionID: review, Outcome: r.Outcome, Reason: r.Reason,
		ProposedBy: r.Proposer, ProposedAt: s.now()}
	if err := s.st.insertResolution(ctx, tx, res); err != nil {
		return Resolution{}, err
	}
	if err := s.st.auditTransition(ctx, tx, a.AccountID, r.Proposer, "case_resolution", res.ID.String(), "", "PROPOSED",
		string(r.Outcome)+": "+r.Reason, "", res.ProposedAt); err != nil {
		return Resolution{}, err
	}
	return res, nil
}

// DecideCaseResolution approves or rejects a proposal. Approval needs an
// operator other than the proposer and applies the outcome at once; anyone
// may reject (the proposer withdrawing it, say). A decision is final: the
// same decision by the same operator again is a replay, any other refused.
func (s *Service) DecideCaseResolution(ctx context.Context, tx pgx.Tx, r DecideResolutionRequest) (ResolutionResult, error) {
	if r.Decision != ResolutionApproved && r.Decision != ResolutionRejected {
		return ResolutionResult{}, errs.Newf(errs.CodeValidationFailed, "margin: unknown resolution decision %q", r.Decision)
	}
	if err := validReason(r.Reason); err != nil {
		return ResolutionResult{}, err
	}
	if err := validOperator(r.Decider); err != nil {
		return ResolutionResult{}, err
	}
	res, err := s.st.resolution(ctx, tx, r.ResolutionID)
	if err != nil {
		return ResolutionResult{}, err
	}
	a, c, err := s.lockCase(ctx, tx, res.CaseID)
	if err != nil {
		return ResolutionResult{}, err
	}
	// Read again under the case lock: a concurrent decision is now visible.
	if res, err = s.st.resolution(ctx, tx, r.ResolutionID); err != nil {
		return ResolutionResult{}, err
	}
	if res.Decision != nil {
		if *res.Decision == r.Decision && res.DecidedBy != nil && res.DecidedBy.ID == r.Decider.ID {
			return ResolutionResult{Resolution: res, Case: c, AccountStatus: a.Status, Replayed: true}, nil
		}
		return ResolutionResult{}, errs.Newf(errs.CodeConflict, "margin: resolution %s was already %s", res.ID, *res.Decision)
	}
	if r.Decision == ResolutionApproved && r.Decider.ID == res.ProposedBy.ID {
		return ResolutionResult{}, errs.Wrap(security.ErrSelfApproval, errs.CodeForbidden,
			"margin: the operator who proposed a resolution cannot approve it")
	}
	now := s.now()
	if r.Decision == ResolutionRejected {
		if err := s.st.insertDecision(ctx, tx, res.ID, r, now); err != nil {
			return ResolutionResult{}, err
		}
		if err := s.st.auditTransition(ctx, tx, a.AccountID, r.Decider, "case_resolution", res.ID.String(), "PROPOSED",
			string(ResolutionRejected), r.Reason, "", now); err != nil {
			return ResolutionResult{}, err
		}
		if res, err = s.st.resolution(ctx, tx, res.ID); err != nil {
			return ResolutionResult{}, err
		}
		return ResolutionResult{Resolution: res, Case: c, AccountStatus: a.Status}, nil
	}

	if c.Status != CaseManualReview {
		return ResolutionResult{}, errs.Newf(errs.CodeInvalidStateTransition, "margin: case %s is %s and no longer in review", c.ID, c.Status)
	}
	review, err := s.st.currentReview(ctx, tx, c.ID)
	if err != nil {
		return ResolutionResult{}, err
	}
	if review != res.ReviewTransitionID {
		return ResolutionResult{}, errs.New(errs.CodeConflict, "margin: this resolution answers an earlier review of the case").
			WithField("resolution_id", res.ID.String())
	}
	if err := s.authorize(ctx, tx, a.Mode, true, gates.Margin); err != nil {
		return ResolutionResult{}, err
	}
	pool, _, err := s.accountFacilities(ctx, tx, a)
	if err != nil {
		return ResolutionResult{}, err
	}
	if err := s.st.insertDecision(ctx, tx, res.ID, r, now); err != nil {
		return ResolutionResult{}, err
	}
	// The review's interest is waived up to now; the case is still in review,
	// so accrue records the period at zero.
	waived, err := s.accrue(ctx, tx, a, pool, now, true)
	if err != nil {
		return ResolutionResult{}, err
	}
	reason := "resolution " + res.ID.String() + " (" + string(res.Outcome) + ") approved: " + r.Reason
	switch res.Outcome {
	case ResolveResume:
		if err := s.st.resolveCase(ctx, tx, a, &c, res, reason, nil, r.Decider, now); err != nil {
			return ResolutionResult{}, err
		}
	case ResolveDeclareBankrupt:
		deficit, err := s.bankruptcyDeficit(ctx, tx, a, now)
		if err != nil {
			return ResolutionResult{}, err
		}
		if err := s.st.resolveCase(ctx, tx, a, &c, res, reason, &deficit, r.Decider, now); err != nil {
			return ResolutionResult{}, err
		}
		if err := s.st.setAccountStatus(ctx, tx, &a, StatusBankrupt, r.Decider, "declared bankrupt by "+res.ID.String(), "", now); err != nil {
			return ResolutionResult{}, err
		}
	default:
		return ResolutionResult{}, errs.Newf(errs.CodeInternal, "margin: unknown resolution outcome %s", res.Outcome)
	}
	if res, err = s.st.resolution(ctx, tx, res.ID); err != nil {
		return ResolutionResult{}, err
	}
	return ResolutionResult{Resolution: res, Case: c, AccountStatus: a.Status, Waived: waived}, nil
}

// bankruptcyDeficit is the deficit an operator-declared bankruptcy records:
// the loans the books carry, once nothing is left to sell, apply or sweep
// and no derivative exposure remains. Anything else is refused: the case
// should be resumed so the planner takes what is left.
func (s *Service) bankruptcyDeficit(ctx context.Context, tx pgx.Tx, a Account, now time.Time) (money.Quantity, error) {
	state, lots, err := s.loadState(ctx, tx, a, now)
	if err != nil {
		return money.Quantity{}, err
	}
	for _, h := range state.Holdings {
		if h.Quantity.IsPositive() {
			return money.Quantity{}, errs.New(errs.CodeInvalidStateTransition,
				"margin: the account still holds collateral to sell or apply; resume the case instead").WithField("asset_id", h.AssetID)
		}
	}
	if len(state.Derivatives) > 0 {
		return money.Quantity{}, errs.New(errs.CodeInvalidStateTransition,
			"margin: derivative exposure remains; the derivatives lane closes it before a bankruptcy")
	}
	free, err := s.freeNumeraire(ctx, tx, a)
	if err != nil {
		return money.Quantity{}, err
	}
	if free.IsPositive() {
		return money.Quantity{}, errs.New(errs.CodeInvalidStateTransition,
			"margin: the customer holds a free balance that repays the loans first; resume the case so it is swept").
			WithField("free_numeraire", free.String())
	}
	deficit := state.LoanTotal()
	owed := money.Quantity{}
	for _, l := range lots {
		owed = owed.Add(l.Outstanding())
	}
	if !deficit.IsPositive() || !owed.Equal(deficit) {
		return money.Quantity{}, errs.New(errs.CodeInvalidStateTransition, "margin: nothing is owed; there is no deficit to declare").
			WithField("loans", deficit.String())
	}
	return deficit, nil
}

// lockCase locks the margin account a case belongs to (making the status
// bindings immediate), then the case: the same order every margin operation
// takes them in.
func (s *Service) lockCase(ctx context.Context, tx pgx.Tx, cid CaseID) (Account, Case, error) {
	if err := bindingsImmediate(ctx, tx); err != nil {
		return Account{}, Case{}, err
	}
	c, err := s.st.caseByID(ctx, tx, cid, false)
	if err != nil {
		return Account{}, Case{}, err
	}
	a, err := s.st.accountByID(ctx, tx, c.MarginAccountID, true)
	if err != nil {
		return Account{}, Case{}, err
	}
	if err := s.requireOwnPolicy(a); err != nil {
		return Account{}, Case{}, err
	}
	if c, err = s.st.caseByID(ctx, tx, cid, true); err != nil {
		return Account{}, Case{}, err
	}
	return a, c, nil
}

// Resolutions lists a case's resolutions, oldest first.
func (s *Service) Resolutions(ctx context.Context, q db.Querier, cid CaseID) ([]Resolution, error) {
	return s.st.resolutions(ctx, q, cid)
}

// --- SQL ---------------------------------------------------------------------

const resolutionColumns = `r.id, r.case_id, r.review_transition_id::text, r.outcome, r.reason, r.proposed_by_actor_type,
	r.proposed_by_actor_id, r.proposed_at, d.decision, d.decided_by_actor_type, d.decided_by_actor_id, d.decided_at,
	coalesce(d.reason, '')`

func scanResolution(row pgx.Row) (Resolution, error) {
	var r Resolution
	var decision, decidedType, decidedID *string
	if err := row.Scan(&r.ID, &r.CaseID, &r.ReviewTransitionID, &r.Outcome, &r.Reason, &r.ProposedBy.Type, &r.ProposedBy.ID,
		&r.ProposedAt, &decision, &decidedType, &decidedID, &r.DecidedAt, &r.DecisionReason); err != nil {
		return Resolution{}, err
	}
	r.ProposedAt = r.ProposedAt.UTC()
	if decision != nil {
		d := ResolutionDecision(*decision)
		r.Decision = &d
		r.DecidedBy = &econ.Actor{Type: security.ActorType(deref(decidedType)), ID: deref(decidedID)}
	}
	if r.DecidedAt != nil {
		t := r.DecidedAt.UTC()
		r.DecidedAt = &t
	}
	return r, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (st *store) resolution(ctx context.Context, q db.Querier, rid ResolutionID) (Resolution, error) {
	r, err := scanResolution(q.QueryRow(ctx, `SELECT `+resolutionColumns+` FROM margin_case_resolutions r
		LEFT JOIN margin_case_resolution_decisions d ON d.resolution_id = r.id WHERE r.id = $1`, rid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Resolution{}, errs.New(errs.CodeNotFound, "case resolution not found").WithField("resolution_id", rid.String())
	}
	if err != nil {
		return Resolution{}, dbErr("load case resolution", err)
	}
	return r, nil
}

func (st *store) resolutions(ctx context.Context, q db.Querier, cid CaseID) ([]Resolution, error) {
	rows, err := q.Query(ctx, `SELECT `+resolutionColumns+` FROM margin_case_resolutions r
		LEFT JOIN margin_case_resolution_decisions d ON d.resolution_id = r.id WHERE r.case_id = $1
		ORDER BY r.proposed_at, r.id`, cid)
	if err != nil {
		return nil, dbErr("list case resolutions", err)
	}
	defer rows.Close()
	out := []Resolution{}
	for rows.Next() {
		r, err := scanResolution(rows)
		if err != nil {
			return nil, dbErr("scan case resolution", err)
		}
		out = append(out, r)
	}
	return out, dbErr("list case resolutions", rows.Err())
}

func (st *store) insertResolution(ctx context.Context, tx pgx.Tx, r Resolution) error {
	_, err := tx.Exec(ctx, `INSERT INTO margin_case_resolutions (id, case_id, review_transition_id, outcome, reason,
		proposed_by_actor_type, proposed_by_actor_id, proposed_at) VALUES ($1,$2,$3::uuid,$4,$5,$6,$7,$8)`,
		r.ID, r.CaseID, r.ReviewTransitionID, r.Outcome, r.Reason, r.ProposedBy.Type, r.ProposedBy.ID, r.ProposedAt)
	return dbErr("record case resolution", err)
}

func (st *store) insertDecision(ctx context.Context, tx pgx.Tx, rid ResolutionID, r DecideResolutionRequest, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO margin_case_resolution_decisions (id, resolution_id, decision, reason,
		decided_by_actor_type, decided_by_actor_id, decided_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id.New[id.Any](), rid, r.Decision, r.Reason, r.Decider.Type, r.Decider.ID, at)
	return dbErr("record case resolution decision", err)
}

// currentReview returns the transition that put the case in the review it is
// in now: the latest entry into MANUAL_REVIEW that no resolution has left.
func (st *store) currentReview(ctx context.Context, q db.Querier, cid CaseID) (string, error) {
	var tid string
	err := q.QueryRow(ctx, `SELECT t.id::text FROM margin_liquidation_case_transitions t
		WHERE t.case_id = $1 AND t.to_status = 'MANUAL_REVIEW' AND NOT cp_margin_review_left(t.id)
		ORDER BY t.occurred_at DESC, t.id DESC LIMIT 1`, cid).Scan(&tid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errs.New(errs.CodeInvalidStateTransition, "margin: the case is not in a review").WithField("case_id", cid.String())
	}
	return tid, dbErr("load current review", err)
}

// resolveCase moves a case out of MANUAL_REVIEW on an approved resolution:
// its transition row names the resolution (the database refuses an exit
// without one); a resumed case is reopened for automation with a fresh budget
// window, a bankrupt one records its deficit.
func (st *store) resolveCase(ctx context.Context, tx pgx.Tx, a Account, c *Case, r Resolution, reason string, deficit *money.Quantity,
	actor econ.Actor, at time.Time) error {
	to := r.Outcome.CaseStatus()
	if c.Status != CaseManualReview || !CanTransitionCase(c.Status, to) || r.CaseID != c.ID {
		return errs.Newf(errs.CodeInvalidStateTransition, "margin: case %s cannot be resolved %s from %s", c.ID, r.Outcome, c.Status)
	}
	if (to == CaseBankrupt) != (deficit != nil) {
		return errs.New(errs.CodeInternal, "margin: a bankrupt resolution records its deficit, and only it does")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO margin_liquidation_case_transitions (id, case_id, from_status, to_status, reason, actor_type,
		actor_id, occurred_at, resolution_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		id.New[id.Any](), c.ID, c.Status, to, reason, actor.Type, actor.ID, at, r.ID); err != nil {
		return dbErr("record liquidation case resolution", err)
	}
	switch to {
	case CaseInProgress:
		if _, err := tx.Exec(ctx, `UPDATE margin_liquidation_cases SET status = 'IN_PROGRESS', last_reason = $2, closed_at = NULL,
			budget_from_step = steps_executed WHERE id = $1`, c.ID, reason); err != nil {
			return dbErr("resume liquidation case", err)
		}
		c.ClosedAt, c.BudgetFromStep = nil, c.StepsExecuted
	case CaseBankrupt:
		if _, err := tx.Exec(ctx, `UPDATE margin_liquidation_cases SET status = 'BANKRUPT', last_reason = $2, closed_at = $3,
			deficit_units = $4::numeric WHERE id = $1`, c.ID, reason, at, deficit.String()); err != nil {
			return dbErr("declare liquidation case bankrupt", err)
		}
		d := *deficit
		c.ClosedAt, c.DeficitUnits = &at, &d
	}
	if err := st.auditTransition(ctx, tx, a.AccountID, actor, "case", c.ID.String(), string(c.Status), string(to), reason, "", at); err != nil {
		return err
	}
	c.Status, c.LastReason = to, reason
	return nil
}
