package treasury

import (
	"errors"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

// Every enumeration below has a SQL twin in migration 00745 (a CHECK (col IN
// (...)) constraint), and each AllXxx function returns exactly that list.

// Class is what kind of house book a treasury book is.
type Class string

// Classes.
const (
	// ClassPrincipal trades as principal: liquidity provision, market making,
	// hedging.
	ClassPrincipal Class = "PRINCIPAL"
	// ClassTreasury manages the platform's own sandbox inventory: inventory
	// rebalancing and treasury conversion.
	ClassTreasury Class = "TREASURY"
)

// AllClasses returns every class (treasury_books.class).
func AllClasses() []Class { return []Class{ClassPrincipal, ClassTreasury} }

// ClassOf is the class a house strategy belongs to; the database refuses a
// book whose class and strategy disagree (TY001).
func ClassOf(s econ.HouseStrategy) (Class, bool) {
	switch s {
	case econ.HouseLiquidityProvision, econ.HouseMarketMaking, econ.HouseHedging:
		return ClassPrincipal, true
	case econ.HouseInventoryRebalancing, econ.HouseTreasuryConversion:
		return ClassTreasury, true
	}
	return "", false
}

// AllActivities returns every activity a house action can be recorded under
// (treasury_actions.activity, treasury_paper_fills.activity): the house
// strategies themselves.
func AllActivities() []econ.HouseStrategy { return econ.AllHouseStrategies() }

// PermittedActivities is what a book of strategy s may do. A market maker or
// liquidity provider quotes and hedges; a hedging book hedges; a rebalancing
// book rebalances; a conversion book converts.
func PermittedActivities(s econ.HouseStrategy) []econ.HouseStrategy {
	switch s {
	case econ.HouseLiquidityProvision:
		return []econ.HouseStrategy{econ.HouseLiquidityProvision, econ.HouseHedging}
	case econ.HouseMarketMaking:
		return []econ.HouseStrategy{econ.HouseMarketMaking, econ.HouseHedging}
	case econ.HouseHedging:
		return []econ.HouseStrategy{econ.HouseHedging}
	case econ.HouseInventoryRebalancing:
		return []econ.HouseStrategy{econ.HouseInventoryRebalancing}
	case econ.HouseTreasuryConversion:
		return []econ.HouseStrategy{econ.HouseTreasuryConversion}
	}
	return nil
}

func permitted(s, activity econ.HouseStrategy) bool {
	for _, a := range PermittedActivities(s) {
		if a == activity {
			return true
		}
	}
	return false
}

// ActionKind names a house action in the register.
type ActionKind string

// Action kinds.
const (
	ActionRegister         ActionKind = "REGISTER"
	ActionSetPolicy        ActionKind = "SET_POLICY"
	ActionSetSigningPolicy ActionKind = "SET_SIGNING_POLICY"
	ActionFund             ActionKind = "FUND"
	ActionPlaceOrder       ActionKind = "PLACE_ORDER"
	ActionCancelOrder      ActionKind = "CANCEL_ORDER"
	ActionPaperFill        ActionKind = "PAPER_FILL"
	ActionMarkToMarket     ActionKind = "MARK_TO_MARKET"
)

// AllActionKinds returns every kind (treasury_actions.kind).
func AllActionKinds() []ActionKind {
	return []ActionKind{ActionRegister, ActionSetPolicy, ActionSetSigningPolicy, ActionFund, ActionPlaceOrder,
		ActionCancelOrder, ActionPaperFill, ActionMarkToMarket}
}

// administrative reports whether only an operator may take an action of kind k.
func (k ActionKind) administrative() bool {
	switch k {
	case ActionRegister, ActionSetPolicy, ActionSetSigningPolicy, ActionFund:
		return true
	}
	return false
}

// Outcome is what became of a house action.
type Outcome string

// Outcomes.
const (
	OutcomeApplied  Outcome = "APPLIED"
	OutcomeRejected Outcome = "REJECTED"
)

// AllOutcomes returns every outcome (treasury_actions.outcome).
func AllOutcomes() []Outcome { return []Outcome{OutcomeApplied, OutcomeRejected} }

// AuthorizedActorTypes are the only actor types that may ever act for a house
// book (treasury_actions.actor_type): the house service and an operator. A
// customer, an agent and an anonymous system actor never do.
func AuthorizedActorTypes() []security.ActorType {
	return []security.ActorType{security.ActorService, security.ActorOperator}
}

// PositionEventKind is the source of a position event.
type PositionEventKind string

// Position event kinds.
const (
	EventFunding   PositionEventKind = "FUNDING"
	EventBookFill  PositionEventKind = "BOOK_FILL"
	EventPaperFill PositionEventKind = "PAPER_FILL"
)

// AllPositionEventKinds returns every kind (treasury_position_events.kind).
func AllPositionEventKinds() []PositionEventKind {
	return []PositionEventKind{EventFunding, EventBookFill, EventPaperFill}
}

// Direction is which way a position event moves a position.
type Direction string

// Directions.
const (
	Increase Direction = "INCREASE"
	Decrease Direction = "DECREASE"
)

// AllDirections returns both (treasury_position_events.direction).
func AllDirections() []Direction { return []Direction{Increase, Decrease} }

// AllEvaluatedIntentKinds are the Risk Kernel V2 intent kinds a house order or
// fill is evaluated as (treasury_risk_decisions.intent_kind): a resting quote
// provides liquidity, anything that executes at once takes it.
func AllEvaluatedIntentKinds() []econ.IntentKind {
	return []econ.IntentKind{econ.KindProvideLiquidity, econ.KindTakeLiquidity}
}

// Fixed labels the database admits exactly.
const (
	// VenueLabelPaper is treasury_paper_fills.venue_label: the house paper
	// venue is simulated, and the database admits no other value.
	VenueLabelPaper = "SIMULATED_PAPER_VENUE"
	// KeyCustodyNone is the only key custody a sandbox signing policy has.
	KeyCustodyNone = "NONE_SANDBOX"
	// LedgerSegment is the audit tag of every house action and posting.
	LedgerSegment = "HOUSE"
)

// Stable reason codes. They are stored on a rejected action
// (treasury_actions.rejection_code / reason_codes), carried in the "reason"
// field of an error, and never change meaning.
const (
	ReasonNotAuthorized          = "TREASURY_NOT_AUTHORIZED"
	ReasonActivityNotPermitted   = "TREASURY_ACTIVITY_NOT_PERMITTED"
	ReasonPolicyMissing          = "TREASURY_POLICY_MISSING"
	ReasonQuoteNotValuation      = "TREASURY_QUOTE_ASSET_NOT_VALUATION"
	ReasonDailyLossLimit         = "TREASURY_DAILY_LOSS_LIMIT"
	ReasonInventoryCap           = "TREASURY_INVENTORY_CAP"
	ReasonAssetNotPermitted      = "TREASURY_ASSET_NOT_PERMITTED"
	ReasonInstrumentExposure     = "TREASURY_INSTRUMENT_EXPOSURE"
	ReasonInstrumentNotPermitted = "TREASURY_INSTRUMENT_NOT_PERMITTED"
	ReasonMarkUnavailable        = "TREASURY_MARK_UNAVAILABLE"
	ReasonInsufficientInventory  = "TREASURY_INSUFFICIENT_INVENTORY"
	ReasonRiskRejected           = "TREASURY_RISK_REJECTED"
	ReasonCapability             = "TREASURY_CAPABILITY_NOT_APPROVED"
	ReasonKillSwitch             = "TREASURY_KILL_SWITCH_ACTIVE"
	ReasonOrderRefused           = "TREASURY_ORDER_REFUSED"
	ReasonForeignTrade           = "TREASURY_FOREIGN_QUOTE_TRADE"
	ReasonValuationAsset         = "TREASURY_VALUATION_ASSET"
)

// AllReasonCodes returns every reason code this package emits.
func AllReasonCodes() []string {
	return []string{ReasonNotAuthorized, ReasonActivityNotPermitted, ReasonPolicyMissing, ReasonQuoteNotValuation,
		ReasonDailyLossLimit, ReasonInventoryCap, ReasonAssetNotPermitted, ReasonInstrumentExposure,
		ReasonInstrumentNotPermitted, ReasonMarkUnavailable, ReasonInsufficientInventory, ReasonRiskRejected,
		ReasonCapability, ReasonKillSwitch, ReasonOrderRefused, ReasonForeignTrade, ReasonValuationAsset}
}

// rejection is the error of a refused house action with its stable reason.
func rejection(code errs.Code, reason, detail string) *errs.Error {
	return errs.New(code, "treasury: "+detail).WithField("reason", reason)
}

// ReasonOf returns the stable reason an error from this package carries, or
// "" when it carries none.
func ReasonOf(err error) string {
	var e *errs.Error
	if !errors.As(err, &e) || e.Fields == nil {
		return ""
	}
	r, _ := e.Fields["reason"].(string)
	return r
}

// Identifier kinds.
type (
	actionKind     struct{}
	decisionKind   struct{}
	paperFillKind  struct{}
	fundingKind    struct{}
	eventKind      struct{}
	snapshotKind   struct{}
	disclosureKind struct{}
	policyKind     struct{}
)

// ActionID identifies a house action.
type ActionID = id.ID[actionKind]

// DecisionID identifies a treasury risk decision.
type DecisionID = id.ID[decisionKind]

// PaperFillID identifies a house paper-venue fill.
type PaperFillID = id.ID[paperFillKind]

// FundingID identifies a house funding.
type FundingID = id.ID[fundingKind]

// PositionEventID identifies a position event.
type PositionEventID = id.ID[eventKind]

// SnapshotID identifies a mark-to-market snapshot.
type SnapshotID = id.ID[snapshotKind]

// DisclosureID identifies a crossing disclosure.
type DisclosureID = id.ID[disclosureKind]

// PolicyID identifies a risk or signing policy version.
type PolicyID = id.ID[policyKind]
