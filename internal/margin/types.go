package margin

import (
	"github.com/nodal/controlplane/internal/id"
)

// MarginMode is how a margin account margins its positions.
type MarginMode string

// Margin modes.
const (
	// ModeCash is a fully paid account: no borrowing and no leverage. Its
	// collateral backs only derivative exposures the derivatives lane reports.
	ModeCash MarginMode = "CASH"
	// ModeIsolated margins exactly one risky position: the account's
	// collateral backs that position and nothing else, so a loss on it can
	// never consume collateral the customer holds anywhere else.
	ModeIsolated MarginMode = "ISOLATED"
	// ModeCross shares the account's collateral across every position.
	ModeCross MarginMode = "CROSS"
)

// AllMarginModes returns every margin mode; it is the Go twin of
// margin_accounts_margin_mode_check.
func AllMarginModes() []MarginMode { return []MarginMode{ModeCash, ModeIsolated, ModeCross} }

// Valid reports whether m is a declared margin mode.
func (m MarginMode) Valid() bool {
	for _, x := range AllMarginModes() {
		if x == m {
			return true
		}
	}
	return false
}

// AccountStatus is the lifecycle status of a margin account.
type AccountStatus string

// Account statuses.
const (
	// StatusNormal: equity is at or above maintenance plus the warning
	// buffer, or the account carries no liability at all.
	StatusNormal AccountStatus = "NORMAL"
	// StatusWarning: equity is below maintenance plus the warning buffer and
	// at or above maintenance. Nothing is sold; the customer is told.
	StatusWarning AccountStatus = "WARNING"
	// StatusMarginCall: equity is below maintenance. The account is eligible
	// for liquidation and a margin call is open.
	StatusMarginCall AccountStatus = "MARGIN_CALL"
	// StatusLiquidating: a liquidation case is open.
	StatusLiquidating AccountStatus = "LIQUIDATING"
	// StatusBankrupt: everything was sold and liabilities remain. The deficit
	// is the insurance fund's to cover and the customer's to owe.
	StatusBankrupt AccountStatus = "BANKRUPT"
	// StatusClosed: the account holds nothing and owes nothing to the pool.
	StatusClosed AccountStatus = "CLOSED"
)

// AllAccountStatuses returns every account status; it is the Go twin of
// margin_accounts_status_check.
func AllAccountStatuses() []AccountStatus {
	return []AccountStatus{StatusNormal, StatusWarning, StatusMarginCall, StatusLiquidating, StatusBankrupt, StatusClosed}
}

// Valid reports whether s is a declared account status.
func (s AccountStatus) Valid() bool {
	for _, x := range AllAccountStatuses() {
		if x == s {
			return true
		}
	}
	return false
}

// AllowsNewRisk reports whether an account in this status may borrow, buy
// on margin or release collateral.
func (s AccountStatus) AllowsNewRisk() bool { return s == StatusNormal || s == StatusWarning }

// AllowsRiskReduction reports whether collateral may be posted and loans
// repaid in this status.
func (s AccountStatus) AllowsRiskReduction() bool {
	switch s {
	case StatusNormal, StatusWarning, StatusMarginCall, StatusLiquidating:
		return true
	}
	return false
}

// accountEdges is the legal transition table. It mirrors
// cp_margin_account_transition_legal in migration 00740; a parity test
// compares the two.
func accountEdges(from AccountStatus) []AccountStatus {
	switch from {
	case StatusNormal:
		return []AccountStatus{StatusWarning, StatusMarginCall, StatusLiquidating, StatusClosed}
	case StatusWarning:
		return []AccountStatus{StatusNormal, StatusMarginCall, StatusLiquidating}
	case StatusMarginCall:
		return []AccountStatus{StatusNormal, StatusWarning, StatusLiquidating}
	case StatusLiquidating:
		return []AccountStatus{StatusNormal, StatusWarning, StatusBankrupt}
	case StatusBankrupt:
		return []AccountStatus{StatusClosed}
	}
	return nil
}

// CanTransitionAccount reports whether from -> to is legal.
func CanTransitionAccount(from, to AccountStatus) bool {
	for _, t := range accountEdges(from) {
		if t == to {
			return true
		}
	}
	return false
}

// CaseStatus is the status of a liquidation case.
type CaseStatus string

// Liquidation case statuses.
const (
	// CaseAwaitingPrice: a mark the case needs is stale or missing. Nothing
	// is sold on a price nobody can vouch for; the case waits.
	CaseAwaitingPrice CaseStatus = "AWAITING_PRICE"
	// CaseAwaitingVenue: the paper venue cannot execute; the case waits.
	CaseAwaitingVenue CaseStatus = "AWAITING_VENUE"
	// CaseInProgress: steps are being executed.
	CaseInProgress CaseStatus = "IN_PROGRESS"
	// CaseCompleted: the account is restored to the case's target.
	CaseCompleted CaseStatus = "COMPLETED"
	// CaseBankrupt: everything was sold and a deficit remains.
	CaseBankrupt CaseStatus = "BANKRUPT"
	// CaseManualReview: the trading budget is spent or the planner can make
	// no further progress. Automation stops and a person takes over; the case
	// never loops. It leaves review only through a resolution two distinct
	// operators agree on (ProposeCaseResolution, DecideCaseResolution).
	CaseManualReview CaseStatus = "MANUAL_REVIEW"
)

// AllCaseStatuses returns every case status; it is the Go twin of
// margin_liquidation_cases_status_check.
func AllCaseStatuses() []CaseStatus {
	return []CaseStatus{CaseAwaitingPrice, CaseAwaitingVenue, CaseInProgress, CaseCompleted, CaseBankrupt, CaseManualReview}
}

// Valid reports whether s is a declared case status.
func (s CaseStatus) Valid() bool {
	for _, x := range AllCaseStatuses() {
		if x == s {
			return true
		}
	}
	return false
}

// Terminal reports whether the case is finished for good: COMPLETED or
// BANKRUPT. Nothing moves a terminal case again.
func (s CaseStatus) Terminal() bool { return s == CaseCompleted || s == CaseBankrupt }

// Automated reports whether the liquidation engine moves the case: it is
// waiting for a price or a venue, or in progress.
func (s CaseStatus) Automated() bool {
	return s == CaseAwaitingPrice || s == CaseAwaitingVenue || s == CaseInProgress
}

// Closed reports whether the case carries closed_at: it is terminal, or held
// for a person in MANUAL_REVIEW (closed for automation). It is the Go twin of
// margin_liquidation_cases_closed.
func (s CaseStatus) Closed() bool { return s.Terminal() || s == CaseManualReview }

// BirthCaseStatuses are the statuses a case may be created in.
func BirthCaseStatuses() []CaseStatus {
	return []CaseStatus{CaseInProgress, CaseAwaitingPrice, CaseAwaitingVenue}
}

// CanTransitionCase reports whether from -> to is legal: an automated case
// (waiting or in progress) may move to any other status; a MANUAL_REVIEW case
// may move only to IN_PROGRESS (resumed with a fresh trading budget) or
// BANKRUPT, and only through an approved resolution (ResolutionRequired); a
// terminal case never moves. It mirrors cp_margin_case_transition_legal.
func CanTransitionCase(from, to CaseStatus) bool {
	if !from.Valid() || !to.Valid() || from == to {
		return false
	}
	switch {
	case from.Automated():
		return true
	case from == CaseManualReview:
		return to == CaseInProgress || to == CaseBankrupt
	}
	return false
}

// ResolutionRequired reports whether leaving from needs an approved
// resolution: every exit from MANUAL_REVIEW does, in Go and in SQL
// (margin_liquidation_case_transitions_review_exit_resolved).
func ResolutionRequired(from CaseStatus) bool { return from == CaseManualReview }

// ResolutionOutcome is what a MANUAL_REVIEW resolution does with the case.
type ResolutionOutcome string

// Resolution outcomes.
const (
	// ResolveResume returns the case to IN_PROGRESS with a fresh trading
	// budget window: automation continues where it stopped.
	ResolveResume ResolutionOutcome = "RESUME"
	// ResolveDeclareBankrupt declares the case BANKRUPT with the loans the
	// books carry as its deficit. It is refused while anything is left to
	// sell, apply or sweep.
	ResolveDeclareBankrupt ResolutionOutcome = "DECLARE_BANKRUPT"
)

// AllResolutionOutcomes returns every outcome; it is the Go twin of
// margin_case_resolutions_outcome_check.
func AllResolutionOutcomes() []ResolutionOutcome {
	return []ResolutionOutcome{ResolveResume, ResolveDeclareBankrupt}
}

// Valid reports whether o is a declared outcome.
func (o ResolutionOutcome) Valid() bool { return o == ResolveResume || o == ResolveDeclareBankrupt }

// CaseStatus is the status a case moves to when a resolution with this
// outcome is approved.
func (o ResolutionOutcome) CaseStatus() CaseStatus {
	if o == ResolveDeclareBankrupt {
		return CaseBankrupt
	}
	return CaseInProgress
}

// ResolutionDecision is a second operator's answer to a proposed resolution.
type ResolutionDecision string

// Resolution decisions.
const (
	// ResolutionApproved applies the resolution. The approver must be an
	// operator other than the proposer.
	ResolutionApproved ResolutionDecision = "APPROVED"
	// ResolutionRejected withdraws it; any operator, the proposer included, may
	// reject a proposal.
	ResolutionRejected ResolutionDecision = "REJECTED"
)

// AllResolutionDecisions returns every decision; it is the Go twin of
// margin_case_resolution_decisions_decision_check.
func AllResolutionDecisions() []ResolutionDecision {
	return []ResolutionDecision{ResolutionApproved, ResolutionRejected}
}

// CallStatus is the status of a margin call.
type CallStatus string

// Margin call statuses.
const (
	CallOpen       CallStatus = "OPEN"
	CallMet        CallStatus = "MET"
	CallLiquidated CallStatus = "LIQUIDATED"
)

// AllCallStatuses returns every margin call status; it is the Go twin of
// margin_calls_status_check.
func AllCallStatuses() []CallStatus { return []CallStatus{CallOpen, CallMet, CallLiquidated} }

// CanTransitionCall reports whether from -> to is legal. It mirrors
// cp_margin_call_transition_legal.
func CanTransitionCall(from, to CallStatus) bool {
	return from == CallOpen && (to == CallMet || to == CallLiquidated)
}

// LiquidationTarget is what a partial liquidation restores.
type LiquidationTarget string

// Liquidation targets.
const (
	// TargetInitialMargin restores equity to the initial requirement.
	TargetInitialMargin LiquidationTarget = "INITIAL_MARGIN"
	// TargetMaintenancePlusBuffer restores equity to maintenance plus the
	// warning buffer.
	TargetMaintenancePlusBuffer LiquidationTarget = "MAINTENANCE_PLUS_BUFFER"
)

// AllLiquidationTargets returns every target; it is the Go twin of
// margin_liquidation_cases_target_check.
func AllLiquidationTargets() []LiquidationTarget {
	return []LiquidationTarget{TargetInitialMargin, TargetMaintenancePlusBuffer}
}

// Valid reports whether t is a declared target.
func (t LiquidationTarget) Valid() bool {
	return t == TargetInitialMargin || t == TargetMaintenancePlusBuffer
}

// VolatilityClass is the risk class of an asset as the margin policy sees it.
// Classes are ordered: a later class is never treated as less risky.
type VolatilityClass string

// Volatility classes in ascending order of risk.
const (
	VolStable  VolatilityClass = "STABLE"
	VolLow     VolatilityClass = "LOW"
	VolMedium  VolatilityClass = "MEDIUM"
	VolHigh    VolatilityClass = "HIGH"
	VolExtreme VolatilityClass = "EXTREME"
)

// AllVolatilityClasses returns every class in ascending order of risk.
func AllVolatilityClasses() []VolatilityClass {
	return []VolatilityClass{VolStable, VolLow, VolMedium, VolHigh, VolExtreme}
}

// Rank is the class's position in ascending order of risk, or -1.
func (c VolatilityClass) Rank() int {
	for i, x := range AllVolatilityClasses() {
		if x == c {
			return i
		}
	}
	return -1
}

// LiquidityClass is how easily an asset can be sold without moving its price.
type LiquidityClass string

// Liquidity classes in ascending order of risk.
const (
	LiqDeep     LiquidityClass = "DEEP"
	LiqNormal   LiquidityClass = "NORMAL"
	LiqThin     LiquidityClass = "THIN"
	LiqIlliquid LiquidityClass = "ILLIQUID"
)

// AllLiquidityClasses returns every class in ascending order of risk.
func AllLiquidityClasses() []LiquidityClass {
	return []LiquidityClass{LiqDeep, LiqNormal, LiqThin, LiqIlliquid}
}

// Rank is the class's position in ascending order of risk, or -1.
func (c LiquidityClass) Rank() int {
	for i, x := range AllLiquidityClasses() {
		if x == c {
			return i
		}
	}
	return -1
}

// VenueTier is the chain and venue risk of where an asset lives.
type VenueTier string

// Venue tiers in ascending order of risk.
const (
	VenueTier1 VenueTier = "TIER_1"
	VenueTier2 VenueTier = "TIER_2"
	VenueTier3 VenueTier = "TIER_3"
)

// AllVenueTiers returns every tier in ascending order of risk.
func AllVenueTiers() []VenueTier { return []VenueTier{VenueTier1, VenueTier2, VenueTier3} }

// Rank is the tier's position in ascending order of risk, or -1.
func (t VenueTier) Rank() int {
	for i, x := range AllVenueTiers() {
		if x == t {
			return i
		}
	}
	return -1
}

// PlanDecision is what the liquidation planner decided for one step.
type PlanDecision string

// Planner decisions.
const (
	// DecisionComplete: the account needs no (further) liquidation.
	DecisionComplete PlanDecision = "COMPLETE"
	// DecisionAwaitPrice: a mark is stale or missing; nothing is sold.
	DecisionAwaitPrice PlanDecision = "AWAIT_PRICE"
	// DecisionAwaitVenue: the venue cannot execute; nothing is sold.
	DecisionAwaitVenue PlanDecision = "AWAIT_VENUE"
	// DecisionSell: a partial liquidation restores the target.
	DecisionSell PlanDecision = "SELL"
	// DecisionSellAll: no partial liquidation restores the target; every
	// risky holding is sold.
	DecisionSellAll PlanDecision = "SELL_ALL"
	// DecisionApplyCollateral: nothing risky is left; numeraire collateral
	// repays the loans.
	DecisionApplyCollateral PlanDecision = "APPLY_COLLATERAL"
	// DecisionSweepWallet: nothing is left in the margin account, loans
	// remain, and the customer's free numeraire cash (borrowed cash that
	// never entered the account, say) repays them before any bankruptcy.
	DecisionSweepWallet PlanDecision = "SWEEP_WALLET"
	// DecisionBankrupt: nothing is left to sell, apply or sweep and
	// liabilities remain.
	DecisionBankrupt PlanDecision = "BANKRUPT"
	// DecisionManualReview: the trading budget is spent or the remaining
	// risk is not something this planner can reduce.
	DecisionManualReview PlanDecision = "MANUAL_REVIEW"
)

// StepAction is what one executed liquidation step did.
type StepAction string

// Step actions.
const (
	// ActionSell is a reduce-only sale: a TRADING step, counted against the
	// case's step budget.
	ActionSell StepAction = "SELL"
	// ActionApplyCollateral applies numeraire collateral to the loans:
	// terminal accounting, not counted against the budget.
	ActionApplyCollateral StepAction = "APPLY_COLLATERAL"
	// ActionSweepWallet repays the loans from the customer's free numeraire
	// wallet cash before a bankruptcy: terminal accounting, not counted
	// against the budget.
	ActionSweepWallet StepAction = "SWEEP_WALLET"
)

// AllStepActions returns every step action; it is the Go twin of
// margin_liquidation_steps_action_check.
func AllStepActions() []StepAction {
	return []StepAction{ActionSell, ActionApplyCollateral, ActionSweepWallet}
}

// Trading reports whether a step of this action counts against the case's
// step budget. Only a sale trades.
func (a StepAction) Trading() bool { return a == ActionSell }

// decision is the planner decision a recorded step of this action executed.
func (a StepAction) decision() PlanDecision {
	switch a {
	case ActionApplyCollateral:
		return DecisionApplyCollateral
	case ActionSweepWallet:
		return DecisionSweepWallet
	}
	return DecisionSell
}

// MoveDirection is the direction of a collateral move.
type MoveDirection string

// Collateral move directions.
const (
	MovePost    MoveDirection = "POST"
	MoveRelease MoveDirection = "RELEASE"
)

// AllMoveDirections returns every direction; it is the Go twin of
// margin_collateral_moves_direction_check.
func AllMoveDirections() []MoveDirection { return []MoveDirection{MovePost, MoveRelease} }

// LotCloseReason is why a borrow lot closed.
type LotCloseReason string

// Lot close reasons.
const (
	CloseRepaid         LotCloseReason = "REPAID"
	CloseDeficitCovered LotCloseReason = "DEFICIT_COVERED"
)

// AllLotCloseReasons returns every close reason; it is the Go twin of
// margin_borrow_lots_close_reason_check.
func AllLotCloseReasons() []LotCloseReason { return []LotCloseReason{CloseRepaid, CloseDeficitCovered} }

// StressScenario names one deterministic stress scenario.
type StressScenario string

// Stress scenarios.
const (
	StressGapDown5          StressScenario = "GAP_DOWN_5"
	StressGapDown10         StressScenario = "GAP_DOWN_10"
	StressGapDown25         StressScenario = "GAP_DOWN_25"
	StressGapDown50         StressScenario = "GAP_DOWN_50"
	StressGapUp5            StressScenario = "GAP_UP_5"
	StressGapUp10           StressScenario = "GAP_UP_10"
	StressGapUp25           StressScenario = "GAP_UP_25"
	StressGapUp50           StressScenario = "GAP_UP_50"
	StressOracleUnavailable StressScenario = "ORACLE_UNAVAILABLE"
	StressVenueUnavailable  StressScenario = "VENUE_UNAVAILABLE"
	StressPartialFill       StressScenario = "PARTIAL_FILL"
	StressExtremeSpread     StressScenario = "EXTREME_SPREAD"
	StressBridgeDelayed     StressScenario = "BRIDGE_DELAYED"
	StressCollateralDepeg   StressScenario = "COLLATERAL_DEPEG"
)

// AllStressScenarios returns every scenario in the order RunStress reports
// them.
func AllStressScenarios() []StressScenario {
	return []StressScenario{
		StressGapDown5, StressGapDown10, StressGapDown25, StressGapDown50,
		StressGapUp5, StressGapUp10, StressGapUp25, StressGapUp50,
		StressOracleUnavailable, StressVenueUnavailable, StressPartialFill, StressExtremeSpread,
		StressBridgeDelayed, StressCollateralDepeg,
	}
}

// RunModes are the modes a margin account may run in: the simulated modes
// gates.AdvancedSpecs lists for MARGIN and LEVERAGE. It is the Go twin of
// margin_accounts_mode_check.
func RunModes() []string { return []string{"PAPER", "SANDBOX"} }

type (
	accountKind    struct{}
	lotKind        struct{}
	callKind       struct{}
	caseKind       struct{}
	stepKind       struct{}
	stressKind     struct{}
	moveKind       struct{}
	accrualKind    struct{}
	resolutionKind struct{}
	repaymentKind  struct{}
)

// AccountID identifies a margin account (margin_accounts.id), which is not
// the customer account it belongs to.
type AccountID = id.ID[accountKind]

// LotID identifies a borrow lot.
type LotID = id.ID[lotKind]

// CallID identifies a margin call.
type CallID = id.ID[callKind]

// CaseID identifies a liquidation case.
type CaseID = id.ID[caseKind]

// StepID identifies a liquidation step.
type StepID = id.ID[stepKind]

// StressRunID identifies a stored stress run.
type StressRunID = id.ID[stressKind]

// MoveID identifies a collateral move.
type MoveID = id.ID[moveKind]

// AccrualID identifies an interest accrual.
type AccrualID = id.ID[accrualKind]

// ResolutionID identifies a MANUAL_REVIEW resolution.
type ResolutionID = id.ID[resolutionKind]

// RepaymentID identifies a repayment of a covered deficit.
type RepaymentID = id.ID[repaymentKind]

func newAccountID() AccountID     { return id.New[accountKind]() }
func newLotID() LotID             { return id.New[lotKind]() }
func newCallID() CallID           { return id.New[callKind]() }
func newCaseID() CaseID           { return id.New[caseKind]() }
func newStepID() StepID           { return id.New[stepKind]() }
func newStressRunID() StressRunID { return id.New[stressKind]() }
func newMoveID() MoveID           { return id.New[moveKind]() }
func newAccrualID() AccrualID     { return id.New[accrualKind]() }
func newResolutionID() ResolutionID {
	return id.New[resolutionKind]()
}
func newRepaymentID() RepaymentID { return id.New[repaymentKind]() }

// ParseAccountID parses the canonical form of a margin account id.
func ParseAccountID(s string) (AccountID, error) { return id.Parse[accountKind](s) }

// ParseCaseID parses the canonical form of a liquidation case id.
func ParseCaseID(s string) (CaseID, error) { return id.Parse[caseKind](s) }

// ParseResolutionID parses the canonical form of a resolution id.
func ParseResolutionID(s string) (ResolutionID, error) { return id.Parse[resolutionKind](s) }
