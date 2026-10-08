package derivatives

import (
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

type (
	positionKind   struct{}
	fillKind       struct{}
	markKind       struct{}
	cashFlowKind   struct{}
	vmRunKind      struct{}
	settlementKind struct{}
	fundingKind    struct{}
	accrualKind    struct{}
	exerciseKind   struct{}
	assignmentKind struct{}
	overrideKind   struct{}
)

// Typed identifiers.
type (
	PositionID    = id.ID[positionKind]
	FillID        = id.ID[fillKind]
	MarkID        = id.ID[markKind]
	CashFlowID    = id.ID[cashFlowKind]
	VMRunID       = id.ID[vmRunKind]
	SettlementID  = id.ID[settlementKind]
	FundingRateID = id.ID[fundingKind]
	AccrualID     = id.ID[accrualKind]
	ExerciseID    = id.ID[exerciseKind]
	AssignmentID  = id.ID[assignmentKind]
	// SettlementOverrideID identifies a proposed operator override of a final
	// settlement price.
	SettlementOverrideID = id.ID[overrideKind]
)

// ParsePositionID parses the canonical form.
func ParsePositionID(s string) (PositionID, error) { return id.Parse[positionKind](s) }

// ParseSettlementOverrideID parses the canonical form.
func ParseSettlementOverrideID(s string) (SettlementOverrideID, error) {
	return id.Parse[overrideKind](s)
}

func newSettlementOverrideID() SettlementOverrideID { return id.New[overrideKind]() }

func newPositionID() PositionID       { return id.New[positionKind]() }
func newFillID() FillID               { return id.New[fillKind]() }
func newMarkID() MarkID               { return id.New[markKind]() }
func newCashFlowID() CashFlowID       { return id.New[cashFlowKind]() }
func newVMRunID() VMRunID             { return id.New[vmRunKind]() }
func newSettlementID() SettlementID   { return id.New[settlementKind]() }
func newFundingRateID() FundingRateID { return id.New[fundingKind]() }
func newAccrualID() AccrualID         { return id.New[accrualKind]() }
func newExerciseID() ExerciseID       { return id.New[exerciseKind]() }
func newAssignmentID() AssignmentID   { return id.New[assignmentKind]() }

// PositionStatus is the lifecycle of a position.
type PositionStatus string

// Position statuses.
const (
	// PositionOpen holds contracts.
	PositionOpen PositionStatus = "OPEN"
	// PositionClosed was flattened by trades, exercise or assignment.
	PositionClosed PositionStatus = "CLOSED"
	// PositionSettled was closed by the instrument's final settlement.
	PositionSettled PositionStatus = "SETTLED"
)

// AllPositionStatuses returns exactly the list of the CHECK constraints
// derivative_positions_status_check, derivative_position_transitions_from_status_check
// and derivative_position_transitions_to_status_check.
func AllPositionStatuses() []PositionStatus {
	return []PositionStatus{PositionOpen, PositionClosed, PositionSettled}
}

// CanTransitionPosition mirrors cp_derivative_position_transition_legal
// (migration 00741); an integration test compares the two.
func CanTransitionPosition(from, to PositionStatus) bool {
	return from == PositionOpen && (to == PositionClosed || to == PositionSettled)
}

// FillSide is the side of a fill.
type FillSide string

// Fill sides.
const (
	Buy  FillSide = "BUY"
	Sell FillSide = "SELL"
)

// AllFillSides returns exactly the list of derivative_fills_side_check.
func AllFillSides() []FillSide { return []FillSide{Buy, Sell} }

// Valid reports whether s is BUY or SELL.
func (s FillSide) Valid() bool { return s == Buy || s == Sell }

// sign is +1 for a buy and -1 for a sell.
func (s FillSide) sign() int64 {
	if s == Buy {
		return 1
	}
	return -1
}

// FillSource says what produced a quantity change.
type FillSource string

// Fill sources.
const (
	// SourcePaper is a simulated fill at the mark plus disclosed slippage.
	SourcePaper FillSource = "PAPER"
	// SourceBook is a fill matched in an internal order book (internal/orders).
	SourceBook FillSource = "BOOK"
	// SourceExercise is a holder's exercise of long options.
	SourceExercise FillSource = "EXERCISE"
	// SourceAssignment is the assignment of exercised contracts to a writer.
	SourceAssignment FillSource = "ASSIGNMENT"
	// SourceExpiry is the closing of a position by final settlement.
	SourceExpiry FillSource = "EXPIRY"
)

// AllFillSources returns exactly the list of derivative_fills_source_check.
func AllFillSources() []FillSource {
	return []FillSource{SourcePaper, SourceBook, SourceExercise, SourceAssignment, SourceExpiry}
}

// isTrade reports whether the source is a trade that needs a risk decision.
func (s FillSource) isTrade() bool { return s == SourcePaper || s == SourceBook }

// CashFlowKind classifies a customer <-> clearing house cash flow.
type CashFlowKind string

// Cash flow kinds.
const (
	// FlowRealized is the cash of a reducing or closing futures trade.
	FlowRealized CashFlowKind = "REALIZED"
	// FlowVariationMargin is a periodic mark-to-market settlement.
	FlowVariationMargin CashFlowKind = "VARIATION_MARGIN"
	// FlowFunding is a perpetual funding payment.
	FlowFunding CashFlowKind = "FUNDING"
	// FlowFutureSettlement is a dated future's final settlement.
	FlowFutureSettlement CashFlowKind = "FUTURE_SETTLEMENT"
	// FlowOptionPremium is premium paid or received on an option trade.
	FlowOptionPremium CashFlowKind = "OPTION_PREMIUM"
	// FlowOptionExercise is the intrinsic value a long receives on exercise.
	FlowOptionExercise CashFlowKind = "OPTION_EXERCISE"
	// FlowOptionAssignment is the intrinsic value an assigned writer pays.
	FlowOptionAssignment CashFlowKind = "OPTION_ASSIGNMENT"
)

// AllCashFlowKinds returns exactly the list of derivative_cash_flows_kind_check.
func AllCashFlowKinds() []CashFlowKind {
	return []CashFlowKind{FlowRealized, FlowVariationMargin, FlowFunding, FlowFutureSettlement,
		FlowOptionPremium, FlowOptionExercise, FlowOptionAssignment}
}

// JournalKindFor returns the journal kind a cash flow of kind k is posted
// with. It mirrors cp_derivative_cash_flow_journal_kind (migration 00741),
// which the database applies to every cash flow (DV004).
func JournalKindFor(k CashFlowKind) (ledger.Kind, bool) {
	switch k {
	case FlowRealized, FlowVariationMargin:
		return ledger.KindVariationMargin, true
	case FlowFunding:
		return ledger.KindFundingPayment, true
	case FlowFutureSettlement:
		return ledger.KindFutureSettlement, true
	case FlowOptionPremium:
		return ledger.KindOptionPremium, true
	case FlowOptionExercise, FlowOptionAssignment:
		return ledger.KindOptionSettlement, true
	}
	return "", false
}

// SettlementKind is what a final settlement settled.
type SettlementKind string

// Settlement kinds.
const (
	SettleFuture SettlementKind = "FUTURE"
	SettleOption SettlementKind = "OPTION"
)

// AllSettlementKinds returns exactly the list of derivative_settlements_kind_check.
func AllSettlementKinds() []SettlementKind { return []SettlementKind{SettleFuture, SettleOption} }

// FundingDirection says whether a position pays or receives funding.
type FundingDirection string

// Funding directions.
const (
	FundingPay     FundingDirection = "PAY"
	FundingReceive FundingDirection = "RECEIVE"
)

// AllFundingDirections returns exactly the list of funding_accruals_direction_check.
func AllFundingDirections() []FundingDirection { return []FundingDirection{FundingPay, FundingReceive} }

// ExerciseKind says who exercised.
type ExerciseKind string

// Exercise kinds.
const (
	// ExerciseHolder is a holder-initiated exercise (American options).
	ExerciseHolder ExerciseKind = "HOLDER"
	// ExerciseAutomatic is the automatic exercise of in-the-money contracts at
	// expiry.
	ExerciseAutomatic ExerciseKind = "AUTOMATIC"
)

// AllExerciseKinds returns exactly the list of option_exercises_kind_check.
func AllExerciseKinds() []ExerciseKind { return []ExerciseKind{ExerciseHolder, ExerciseAutomatic} }

// Side is the direction of an opening order.
type Side string

// Sides.
const (
	Long  Side = "LONG"
	Short Side = "SHORT"
)

// AllSides returns every side.
func AllSides() []Side { return []Side{Long, Short} }

// Valid reports whether s is LONG or SHORT.
func (s Side) Valid() bool { return s == Long || s == Short }

// fillSide is the fill side that opens or increases a position on this side.
func (s Side) fillSide() FillSide {
	if s == Long {
		return Buy
	}
	return Sell
}

// FundingRateScale is the denominator of every funding rate: a rate of r
// means r / 10^8 per interval, so one basis point is 10 000 and the fixed
// precision is a ten-thousandth of a basis point.
const FundingRateScale int64 = 100_000_000

// Mark is one derivative_marks row.
type Mark struct {
	ID           MarkID
	InstrumentID instruments.InstrumentID
	// MarkMantissa is the mark at MarkScale (the instrument's price scale),
	// on the instrument's tick grid.
	MarkMantissa money.Quantity
	MarkScale    int32
	// HasIndex reports whether the index (the underlying's spot) is known.
	HasIndex      bool
	IndexMantissa money.Quantity
	IndexScale    int32
	IndexSource   string
	Source        string
	ObservedAt    time.Time
	RecordedAt    time.Time
}

// Position is one derivative_positions row. Values are in base units of the
// settlement asset.
type Position struct {
	ID                PositionID
	AccountID         accounts.AccountID
	InstrumentID      instruments.InstrumentID
	SettlementAssetID assets.AssetID
	Status            PositionStatus
	// Quantity is signed contracts: long > 0, short < 0.
	Quantity     money.Quantity
	CostValue    money.Quantity
	SettledValue money.Quantity
	RealizedPnL  money.Quantity
	FundingPnL   money.Quantity
	MarginPosted money.Quantity
	Shortfall    money.Quantity
	Version      int64
	OpenedAt     time.Time
	ClosedAt     *time.Time
}

// FillRecord is one derivative_fills row.
type FillRecord struct {
	ID             FillID
	Key            string
	PositionID     PositionID
	AccountID      accounts.AccountID
	InstrumentID   instruments.InstrumentID
	Source         FillSource
	Side           FillSide
	Quantity       money.Quantity
	PriceMantissa  money.Quantity
	PriceScale     int32
	Value          money.Quantity
	RealizedPnL    money.Quantity
	CashSettled    money.Quantity
	MarkID         *MarkID
	SlippageBPS    money.BPS
	IntentID       string
	RiskDecisionID string
	OccurredAt     time.Time
}

// CashFlow is one derivative_cash_flows row.
type CashFlow struct {
	ID                   CashFlowID
	Key                  string
	PositionID           PositionID
	AccountID            accounts.AccountID
	InstrumentID         instruments.InstrumentID
	SettlementAssetID    assets.AssetID
	Kind                 CashFlowKind
	Amount               money.Quantity
	Shortfall            money.Quantity
	JournalTransactionID *ledger.TransactionID
	// ReservationID is the capital reservation the flow consumed to take
	// value from the customer's WALLET: what it took from the wallet was
	// available -- not reserved or held for anything else -- when it was
	// taken (the database checks it, DV011). Nil when the flow took nothing
	// from the wallet.
	ReservationID *capital.ReservationID
	ReferenceType string
	ReferenceID   string
	OccurredAt    time.Time
}

// SettlementPriceRule says what decided a final settlement price.
type SettlementPriceRule string

// Settlement price rules.
const (
	// RuleExpiryMark is the settlement rule (SettlementRuleStatement): the
	// latest mark observed and recorded at or before expiry, fresh at
	// expiry.
	RuleExpiryMark SettlementPriceRule = "EXPIRY_MARK"
	// RuleOperatorOverride is a price one operator proposed, within the
	// policy's bound of the rule's reference price, and a second operator
	// approved.
	RuleOperatorOverride SettlementPriceRule = "OPERATOR_OVERRIDE"
)

// AllSettlementPriceRules returns exactly the list of
// derivative_settlements_price_rule_check.
func AllSettlementPriceRules() []SettlementPriceRule {
	return []SettlementPriceRule{RuleExpiryMark, RuleOperatorOverride}
}

// SettlementRuleStatement is the settlement rule, word for word as it is
// recorded in every settlement's evidence.
const SettlementRuleStatement = "EXPIRY_MARK v1: of the instrument's marks observed at or before expiry and recorded " +
	"at or before expiry (an option's: those carrying an index), take the one observed last (ties: recorded last, then " +
	"the greatest id). It must be fresh at expiry: observed no more than the instrument's oracle max age before it. " +
	"A future settles at that mark; an option at that mark's index expressed at the option's price scale and aligned " +
	"to its tick grid, half-even. Without such a mark, or with a stale one, nothing settles by the rule: the contract " +
	"waits for an operator override proposed by one operator, within the policy's bound of the price the rule derives " +
	"from that mark whether fresh or not, and approved by a second."

// OverrideStatus is the state of a settlement override proposal.
type OverrideStatus string

// Override statuses.
const (
	OverrideProposed OverrideStatus = "PROPOSED"
	OverrideApproved OverrideStatus = "APPROVED"
	OverrideRejected OverrideStatus = "REJECTED"
)

// OverrideDecision is a second operator's decision on a proposal.
type OverrideDecision string

// Override decisions.
const (
	DecisionApproved OverrideDecision = "APPROVED"
	DecisionRejected OverrideDecision = "REJECTED"
)

// AllOverrideDecisions returns exactly the list of
// derivative_settlement_override_decisions_decision_check.
func AllOverrideDecisions() []OverrideDecision {
	return []OverrideDecision{DecisionApproved, DecisionRejected}
}
