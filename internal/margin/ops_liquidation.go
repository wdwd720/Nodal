package margin

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
)

// LiquidateRequest asks for one step of an account's liquidation.
type LiquidateRequest struct {
	AccountID accounts.AccountID
	// ExpectedStep is the step the caller means to execute (1 for the
	// first). When the case has already executed it, the recorded step is
	// returned and nothing is posted: a replayed step is exactly once. Zero
	// means "whatever step is next".
	ExpectedStep int
}

// LiquidationResult is the outcome of one Liquidate call.
type LiquidationResult struct {
	MarginAccountID AccountID
	// Eligible is false when the account needed no liquidation and no case
	// was opened.
	Eligible      bool
	Case          *Case
	AccountStatus AccountStatus
	Decision      PlanDecision
	Reason        string
	Step          *Step
	Plan          *Plan
	Replayed      bool
}

// Liquidate executes at most one deterministic step of an account's
// liquidation, opening the case when an eligible account has none:
//
//   - the account must be below maintenance (eligible); a margin call is
//     issued if none is open, and is LIQUIDATED when the case opens;
//   - interest is accrued to the instant, so the loans the step repays are
//     exact;
//   - PlanLiquidation decides. On a stale or missing mark the case waits in
//     AWAITING_PRICE and nothing is posted; with no venue it waits in
//     AWAITING_VENUE; with its step budget spent it goes to MANUAL_REVIEW;
//     with nothing left and liabilities remaining it is BANKRUPT with the
//     deficit recorded;
//   - otherwise the first order executes: a reduce-only paper sale posted as
//     LIQUIDATION (the fee goes to the insurance fund), MARGIN_REPAY of the
//     net proceeds, and MARGIN_COLLATERAL_MOVE of any excess -- or, with
//     nothing risky left, numeraire collateral applied to the loans. Each
//     posting is its own exactly-once effect keyed on the case and step.
//
// Liquidation reduces risk: a kill switch never stops it.
func (s *Service) Liquidate(ctx context.Context, tx pgx.Tx, r LiquidateRequest) (LiquidationResult, error) {
	if r.AccountID.IsZero() || r.ExpectedStep < 0 {
		return LiquidationResult{}, errs.New(errs.CodeValidationFailed, "margin: account required and expected step not negative")
	}
	a, err := s.lockAccount(ctx, tx, r.AccountID)
	if err != nil {
		return LiquidationResult{}, err
	}
	out := LiquidationResult{MarginAccountID: a.ID}
	c, found, err := s.st.latestCase(ctx, tx, a.ID, true)
	if err != nil {
		return LiquidationResult{}, err
	}
	if found && r.ExpectedStep > 0 && c.StepsExecuted >= r.ExpectedStep {
		step, err := s.st.step(ctx, tx, c.ID, r.ExpectedStep)
		if err != nil {
			return LiquidationResult{}, err
		}
		cc := c
		decision := DecisionSell
		if step.Action == ActionApplyCollateral {
			decision = DecisionApplyCollateral
		}
		return LiquidationResult{MarginAccountID: a.ID, Eligible: true, Case: &cc, AccountStatus: a.Status, Decision: decision,
			Reason: "step already executed", Step: &step, Replayed: true}, nil
	}
	if err := s.authorize(ctx, tx, a.Mode, true, gates.Margin); err != nil {
		return LiquidationResult{}, err
	}
	pool, fund, err := s.accountFacilities(ctx, tx, a)
	if err != nil {
		return LiquidationResult{}, err
	}
	now := s.now()
	active := found && !c.Status.Terminal()
	if found && c.Status == CaseManualReview {
		cc := c
		return LiquidationResult{MarginAccountID: a.ID, Eligible: true, Case: &cc, AccountStatus: a.Status,
			Decision: DecisionManualReview, Reason: c.LastReason}, nil
	}
	if !active {
		switch a.Status {
		case StatusNormal, StatusWarning, StatusMarginCall:
		default:
			return LiquidationResult{}, errs.Newf(errs.CodeInvalidStateTransition, "margin: a %s account has no liquidation to run", a.Status)
		}
		ev, call, err := s.refreshHealth(ctx, tx, &a, "evaluated for liquidation")
		if err != nil {
			return LiquidationResult{}, err
		}
		if !ev.LiquidationEligible {
			out.AccountStatus, out.Decision, out.Reason = a.Status, DecisionComplete, "the account is not below maintenance"
			return out, nil
		}
		if call == nil {
			return LiquidationResult{}, errs.New(errs.CodeInternal, "margin: an eligible account has no margin call")
		}
		if r.ExpectedStep > 1 {
			return LiquidationResult{}, errs.Newf(errs.CodeConflict, "margin: no case is open; step %d cannot be next", r.ExpectedStep)
		}
		c, err = s.st.openCase(ctx, tx, a, *call, CaseInProgress, s.policy, "equity below maintenance", s.actor, now)
		if err != nil {
			return LiquidationResult{}, err
		}
		if err := s.st.resolveCall(ctx, tx, a, call, CallLiquidated, s.actor, "liquidation case opened", now); err != nil {
			return LiquidationResult{}, err
		}
		if err := s.st.setAccountStatus(ctx, tx, &a, StatusLiquidating, s.actor, "liquidation case opened", ev.Hash, now); err != nil {
			return LiquidationResult{}, err
		}
	}
	if r.ExpectedStep > 0 && r.ExpectedStep != c.StepsExecuted+1 {
		return LiquidationResult{}, errs.Newf(errs.CodeConflict, "margin: the case is at step %d; step %d cannot be next",
			c.StepsExecuted, r.ExpectedStep).WithField("case_id", c.ID.String())
	}
	out.Eligible = true
	plan, state, lots, err := s.planStep(ctx, tx, a, pool, c, now)
	if err != nil {
		return LiquidationResult{}, err
	}
	out.Plan, out.Decision, out.Reason = &plan, plan.Decision, plan.Reason

	switch plan.Decision {
	case DecisionComplete:
		if err := s.st.setCaseStatus(ctx, tx, a, &c, CaseCompleted, plan.Reason, nil, s.actor, now); err != nil {
			return LiquidationResult{}, err
		}
		if err := s.st.setAccountStatus(ctx, tx, &a, plan.Before.Health, s.actor, "liquidation completed", plan.Before.Hash, now); err != nil {
			return LiquidationResult{}, err
		}
	case DecisionAwaitPrice:
		if err := s.st.setCaseStatus(ctx, tx, a, &c, CaseAwaitingPrice, plan.Reason, nil, s.actor, now); err != nil {
			return LiquidationResult{}, err
		}
	case DecisionAwaitVenue:
		if err := s.st.setCaseStatus(ctx, tx, a, &c, CaseAwaitingVenue, plan.Reason, nil, s.actor, now); err != nil {
			return LiquidationResult{}, err
		}
	case DecisionManualReview:
		if err := s.st.setCaseStatus(ctx, tx, a, &c, CaseManualReview, plan.Reason, nil, s.actor, now); err != nil {
			return LiquidationResult{}, err
		}
	case DecisionBankrupt:
		owed := state.LoanTotal()
		if !owed.Equal(plan.DeficitUnits) {
			return LiquidationResult{}, errs.New(errs.CodeReconciliationRequired, "margin: the deficit disagrees with the loans the books carry").
				WithField("deficit", plan.DeficitUnits.String()).WithField("loans", owed.String())
		}
		d := plan.DeficitUnits
		if err := s.st.setCaseStatus(ctx, tx, a, &c, CaseBankrupt, plan.Reason, &d, s.actor, now); err != nil {
			return LiquidationResult{}, err
		}
		if err := s.st.setAccountStatus(ctx, tx, &a, StatusBankrupt, s.actor, "liquidated to a deficit", plan.Before.Hash, now); err != nil {
			return LiquidationResult{}, err
		}
	case DecisionSell, DecisionSellAll, DecisionApplyCollateral:
		step, err := s.executeStep(ctx, tx, a, &c, pool, fund, state, lots, plan, now)
		if err != nil {
			return LiquidationResult{}, err
		}
		out.Step = &step
		after, err := s.loadAfterStep(ctx, tx, a, now)
		if err != nil {
			return LiquidationResult{}, err
		}
		if after.Restored(s.policy) {
			if err := s.st.setCaseStatus(ctx, tx, a, &c, CaseCompleted, "the step restored the target", nil, s.actor, now); err != nil {
				return LiquidationResult{}, err
			}
			if err := s.st.setAccountStatus(ctx, tx, &a, after.Health, s.actor, "liquidation completed", after.Hash, now); err != nil {
				return LiquidationResult{}, err
			}
		} else if err := s.st.setCaseStatus(ctx, tx, a, &c, CaseInProgress, "step "+strconv.Itoa(step.StepNo)+" executed", nil, s.actor, now); err != nil {
			return LiquidationResult{}, err
		}
	default:
		return LiquidationResult{}, errs.Newf(errs.CodeInternal, "margin: unknown plan decision %s", plan.Decision)
	}
	cc := c
	out.Case, out.AccountStatus = &cc, a.Status
	return out, nil
}
