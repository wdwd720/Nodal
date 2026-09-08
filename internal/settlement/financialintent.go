package settlement

import (
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// FinancialIntent is the shape gola.md PART XXV requires every manual and
// agent action to take before anything happens: a typed request, never an
// arbitrary transaction.
//
// # Why this type exists next to IntentSnapshot rather than replacing it
//
// IntentSnapshot is the V1 external-trade projection: an instrument, a USD
// notional, a venue. It is exactly right for Domain C and cannot describe
// "buy 500 Credits of this creator's token" or "pay out 250 Credits of
// DATA_SALE_EARNING", because those have no instrument, no venue and no USD
// notional. Widening it would have meant a struct in which most fields are
// meaningless for most actions, which is how a type stops carrying
// information.
//
// So FinancialIntent is the union that the COMPILER consumes, and the compiler
// projects it onto whatever the chosen rail's planner needs. A Domain C intent
// still becomes an IntentSnapshot and still goes to the V1 planner unchanged.
//
// # What the caller must state and may not infer
//
// CapitalDomain is DECLARED, never derived from the subject. A client that
// says "buy this asset" and lets the server decide which pot of money it comes
// out of is exactly the ambiguity PART IX exists to remove: the compiler
// checks that the declared domain matches the subject's, and a mismatch is a
// refusal rather than a correction.
type FinancialIntent struct {
	// Identity of the request.
	IntentID       string
	AccountID      string
	ActorType      security.ActorType
	ActorID        string
	IdempotencyKey string
	CorrelationID  string
	RequestedAt    time.Time

	// Provenance of the decision (PART XXV: agent_id, strategy_version,
	// source_prediction). Empty means a human acted directly.
	AgentID            string
	AgentAuthority     agentauthority.Level
	StrategyVersionID  string
	SourcePredictionID string

	// What is being asked for.
	CapitalDomain valuedomain.Domain
	ActionType    ActionType
	Subject       Subject
	Side          Side

	// Amounts. Which of these is required is an ActionType question and is
	// enforced by Validate; the compiler never guesses one from another.
	Quantity       *money.Quantity
	NotionalUSD    *money.USD
	MaximumDebit   *money.Quantity
	MinimumReceive *money.Quantity

	// Constraints.
	Deadline    time.Time
	Constraints IntentConstraints

	// Context the legal router keys on. These are deployment and account
	// facts resolved by the caller, never client input.
	Jurisdiction string
	Verification valuedomain.VerificationLevel
	ValueOrigin  valuedomain.CreditOrigin
	PayoutMode   string
	Provider     string
	Compensation string
}

// ActionType is PART XXV's action vocabulary. It is deliberately more
// specific than intent.Action: BUY_NATIVE_ASSET and BUY_ONCHAIN_ASSET are the
// same verb on different rails, and collapsing them would put the rail
// decision back into inference.
type ActionType string

// Action types.
const (
	// ActionBuyNativeAsset buys a Nodal-native asset with Credits.
	ActionBuyNativeAsset ActionType = "BUY_NATIVE_ASSET"
	// ActionSellNativeAsset sells a Nodal-native asset for Credits.
	ActionSellNativeAsset ActionType = "SELL_NATIVE_ASSET"
	// ActionBuyHostedAsset buys inside a hosted partner account.
	ActionBuyHostedAsset ActionType = "BUY_HOSTED_ASSET"
	// ActionSellHostedAsset sells inside a hosted partner account.
	ActionSellHostedAsset ActionType = "SELL_HOSTED_ASSET"
	// ActionBuyOnchainAsset buys on a public chain from a customer wallet.
	ActionBuyOnchainAsset ActionType = "BUY_ONCHAIN_ASSET"
	// ActionSellOnchainAsset sells on a public chain from a customer wallet.
	ActionSellOnchainAsset ActionType = "SELL_ONCHAIN_ASSET"
	// ActionPurchaseInternalService buys another user's product for Credits.
	ActionPurchaseInternalService ActionType = "PURCHASE_INTERNAL_SERVICE"
	// ActionRequestPayout asks for eligible value to leave the system.
	ActionRequestPayout ActionType = "REQUEST_PAYOUT"
	// ActionSimulatedTrade executes against simulated markets. It moves
	// nothing and is here so that Domain B goes through the same compiler as
	// everything else rather than around it.
	ActionSimulatedTrade ActionType = "SIMULATED_TRADE"
	// ActionCreateNativeAsset publishes a Nodal-native asset.
	ActionCreateNativeAsset ActionType = "CREATE_NATIVE_ASSET"
)

var allActionTypes = []ActionType{
	ActionBuyNativeAsset, ActionSellNativeAsset,
	ActionBuyHostedAsset, ActionSellHostedAsset,
	ActionBuyOnchainAsset, ActionSellOnchainAsset,
	ActionPurchaseInternalService, ActionRequestPayout,
	ActionSimulatedTrade, ActionCreateNativeAsset,
}

// AllActionTypes returns every declared action type in declaration order.
func AllActionTypes() []ActionType { return append([]ActionType(nil), allActionTypes...) }

// Valid reports whether a is declared.
func (a ActionType) Valid() bool {
	for _, x := range allActionTypes {
		if x == a {
			return true
		}
	}
	return false
}

func (a ActionType) String() string { return string(a) }

// Side is the direction of a trade. Actions that are not trades carry
// SideNone, which is a value rather than an empty string so that "nobody set
// it" and "this action has no side" are different states.
type Side string

// Sides.
const (
	SideNone Side = "NONE"
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// Valid reports whether s is declared.
func (s Side) Valid() bool { return s == SideNone || s == SideBuy || s == SideSell }

// SubjectType names what kind of thing an intent is about. The compiler needs
// it because the same identifier shape (a uuid) means entirely different
// things on different rails, and guessing from context is how a market id ends
// up interpreted as an instrument id.
type SubjectType string

// Subject types.
const (
	SubjectInstrument      SubjectType = "INSTRUMENT"
	SubjectNativeMarket    SubjectType = "NATIVE_MARKET"
	SubjectNativeAsset     SubjectType = "NATIVE_ASSET"
	SubjectInternalProduct SubjectType = "INTERNAL_PRODUCT"
	SubjectPayoutRequest   SubjectType = "PAYOUT_DESTINATION"
)

var allSubjectTypes = []SubjectType{
	SubjectInstrument, SubjectNativeMarket, SubjectNativeAsset,
	SubjectInternalProduct, SubjectPayoutRequest,
}

// AllSubjectTypes returns every declared subject type (a copy).
func AllSubjectTypes() []SubjectType { return append([]SubjectType(nil), allSubjectTypes...) }

// Valid reports whether s is declared.
func (s SubjectType) Valid() bool {
	for _, x := range allSubjectTypes {
		if x == s {
			return true
		}
	}
	return false
}

// Subject is what the intent is about.
type Subject struct {
	Type SubjectType
	ID   string
	// AssetID is the asset the subject trades or holds, when there is one. It
	// is carried separately because the legal router keys on the asset and the
	// subject id is not one.
	AssetID string
}

// requiredSubject maps each action type to the subject type it must carry.
// An action absent from this map would be undecided, which validateActionShape
// refuses.
var requiredSubject = map[ActionType]SubjectType{
	ActionBuyNativeAsset:          SubjectNativeMarket,
	ActionSellNativeAsset:         SubjectNativeMarket,
	ActionBuyHostedAsset:          SubjectInstrument,
	ActionSellHostedAsset:         SubjectInstrument,
	ActionBuyOnchainAsset:         SubjectInstrument,
	ActionSellOnchainAsset:        SubjectInstrument,
	ActionPurchaseInternalService: SubjectInternalProduct,
	ActionRequestPayout:           SubjectPayoutRequest,
	ActionSimulatedTrade:          SubjectInstrument,
	ActionCreateNativeAsset:       SubjectNativeAsset,
}

// RequiredSubject returns the subject type an action must carry.
func RequiredSubject(a ActionType) (SubjectType, bool) {
	s, ok := requiredSubject[a]
	return s, ok
}

// sideOf maps each action type to the side it must declare.
var sideOf = map[ActionType]Side{
	ActionBuyNativeAsset:          SideBuy,
	ActionSellNativeAsset:         SideSell,
	ActionBuyHostedAsset:          SideBuy,
	ActionSellHostedAsset:         SideSell,
	ActionBuyOnchainAsset:         SideBuy,
	ActionSellOnchainAsset:        SideSell,
	ActionPurchaseInternalService: SideBuy,
	ActionRequestPayout:           SideNone,
	ActionSimulatedTrade:          SideBuy,
	ActionCreateNativeAsset:       SideNone,
}

// SideOf returns the side an action implies.
func SideOf(a ActionType) (Side, bool) {
	s, ok := sideOf[a]
	return s, ok
}

// Validate checks the intent's own shape. It performs no I/O, consults no
// clock and makes no legal judgement -- that is Compile's work. What it
// guarantees is that an intent reaching the compiler is internally coherent,
// so a refusal from the compiler is about policy rather than about a
// half-filled request.
func (fi FinancialIntent) Validate() error {
	fields := map[string]any{}

	if strings.TrimSpace(fi.IntentID) == "" {
		fields["intent_id"] = "required"
	}
	if strings.TrimSpace(fi.AccountID) == "" {
		fields["account_id"] = "required"
	}
	if strings.TrimSpace(fi.IdempotencyKey) == "" {
		fields["idempotency_key"] = "required"
	}
	if fi.RequestedAt.IsZero() {
		fields["requested_at"] = "required"
	}
	if !fi.ActionType.Valid() {
		fields["action_type"] = "unknown action type"
	}
	if !fi.CapitalDomain.Valid() {
		fields["capital_domain"] = "unknown value domain"
	}
	if !fi.ActorType.Valid() {
		fields["actor_type"] = "unknown actor type"
	}
	if fi.ActorType == security.ActorAgent {
		if strings.TrimSpace(fi.AgentID) == "" {
			fields["agent_id"] = "an agent intent must name the agent"
		}
		if !fi.AgentAuthority.Valid() {
			fields["agent_authority"] = "an agent intent must state the authority level it acted under"
		}
	}
	if !fi.Side.Valid() {
		fields["side"] = "unknown side"
	}
	if !fi.Subject.Type.Valid() {
		fields["subject.type"] = "unknown subject type"
	}
	if strings.TrimSpace(fi.Subject.ID) == "" {
		fields["subject.id"] = "required"
	}

	if fi.ActionType.Valid() {
		validateActionShape(fi, fields)
	}

	if len(fields) > 0 {
		return errs.New(errs.CodeValidationFailed, "settlement: invalid financial intent").
			WithField("problems", fields)
	}
	return nil
}

// validateActionShape checks the per-action rules: the right subject, the
// right side, and exactly the amounts the action needs.
func validateActionShape(fi FinancialIntent, fields map[string]any) {
	wantSubject, ok := RequiredSubject(fi.ActionType)
	if !ok {
		fields["action_type"] = "declared but has no required subject; this is a programming error"
		return
	}
	if fi.Subject.Type != wantSubject {
		fields["subject.type"] = string(fi.ActionType) + " must carry a " + string(wantSubject) + " subject"
	}
	if want, ok := SideOf(fi.ActionType); ok && fi.Side != want {
		fields["side"] = string(fi.ActionType) + " is always " + string(want)
	}

	positive := func(q *money.Quantity) bool { return q != nil && q.Sign() > 0 }

	switch fi.ActionType {
	case ActionBuyNativeAsset, ActionSellNativeAsset, ActionPurchaseInternalService, ActionRequestPayout:
		// Internal actions are denominated in exact base units. A USD notional
		// here would mean somebody converted a price into money outside the
		// ledger, which is the one thing PART VIII forbids.
		if !positive(fi.Quantity) {
			fields["quantity"] = "an internal action states an exact quantity in base units"
		}
		if fi.NotionalUSD != nil {
			fields["notional_usd"] = "an internal action is never denominated in USD"
		}
	case ActionBuyHostedAsset, ActionSellHostedAsset, ActionBuyOnchainAsset, ActionSellOnchainAsset, ActionSimulatedTrade:
		// External and simulated trades keep the V1 shape: a USD notional or
		// an exact quantity, never both and never neither.
		hasUSD := fi.NotionalUSD != nil && fi.NotionalUSD.Sign() > 0
		if hasUSD == positive(fi.Quantity) {
			fields["amount"] = "state exactly one of notional_usd or quantity"
		}
	case ActionCreateNativeAsset:
		if fi.Quantity != nil || fi.NotionalUSD != nil {
			fields["amount"] = "creating an asset moves no value and takes no amount"
		}
	}

	if fi.ActionType == ActionRequestPayout && fi.ValueOrigin != "" && !fi.ValueOrigin.Valid() {
		fields["value_origin"] = "unknown credit origin"
	}
}
