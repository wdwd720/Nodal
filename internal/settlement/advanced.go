package settlement

import (
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The advanced financial capabilities (ADR-0020) route through this compiler
// like everything else. Each
// economic intent kind of internal/econ is one action type here, prefixed
// SIMULATED_ because every one of them runs on the simulated rail in this
// build: domain SIMULATED, executor SIMULATED, product SIMULATION.
//
// What the compiler contributes is the part that must not be re-decided by
// every subsystem: the agent-authority ladder (TRANSFER_VALUE and WITHDRAW map
// to the two actions an agent may never take, at any level), the legal router,
// and the refusal of a declared domain other than SIMULATED. Whether the
// capability may run in this deployment is gates.DecideMode's question, asked
// by econ.Authorizer -- a capability gate is not listed in these profiles for
// the same reason SIMULATED_TRADE lists none: in LOCAL a fresh deployment must
// be able to demonstrate itself.

// Additional subject types for the advanced actions.
const (
	// SubjectAccount is another customer account (the recipient of a
	// transfer).
	SubjectAccount SubjectType = "ACCOUNT"
	// SubjectFacility is a platform-operated facility: a lending pool, a
	// stable issuer, a bridge.
	SubjectFacility SubjectType = "FACILITY"
	// SubjectDestination is an external (simulated) withdrawal destination.
	SubjectDestination SubjectType = "DESTINATION"
)

// amountRule says which amounts an advanced action takes.
type amountRule int

const (
	amountQuantity           amountRule = iota // an exact quantity, never USD
	amountNotionalOrQuantity                   // exactly one of notional or quantity
	amountOptionalQuantity                     // a quantity or nothing, never USD
)

type advancedAction struct {
	action      ActionType
	subject     SubjectType
	side        Side
	agentAction agentauthority.Action
	amount      amountRule
}

var advancedActions = []advancedAction{
	{"SIMULATED_ACQUIRE_EXPOSURE", SubjectInstrument, SideBuy, agentauthority.ActionExecuteApprovedRule, amountNotionalOrQuantity},
	{"SIMULATED_REDUCE_EXPOSURE", SubjectInstrument, SideSell, agentauthority.ActionExecuteApprovedRule, amountNotionalOrQuantity},
	// Value moving between customers is an action no agent may take at any
	// authority level (agentauthority.ActionTransferValue is always
	// forbidden); a person sends a transfer.
	{"SIMULATED_TRANSFER_VALUE", SubjectAccount, SideNone, agentauthority.ActionTransferValue, amountQuantity},
	{"SIMULATED_BORROW", SubjectFacility, SideNone, agentauthority.ActionExecuteApprovedRule, amountQuantity},
	{"SIMULATED_REPAY", SubjectFacility, SideNone, agentauthority.ActionExecuteApprovedRule, amountQuantity},
	{"SIMULATED_OPEN_POSITION", SubjectInstrument, SideNone, agentauthority.ActionExecuteApprovedRule, amountQuantity},
	{"SIMULATED_CLOSE_POSITION", SubjectInstrument, SideNone, agentauthority.ActionExecuteApprovedRule, amountOptionalQuantity},
	{"SIMULATED_EXERCISE_OPTION", SubjectInstrument, SideNone, agentauthority.ActionExecuteApprovedRule, amountOptionalQuantity},
	{"SIMULATED_MINT_STABLE_ASSET", SubjectFacility, SideBuy, agentauthority.ActionExecuteApprovedRule, amountQuantity},
	{"SIMULATED_BURN_STABLE_ASSET", SubjectFacility, SideSell, agentauthority.ActionExecuteApprovedRule, amountQuantity},
	{"SIMULATED_PROVIDE_LIQUIDITY", SubjectInstrument, SideNone, agentauthority.ActionExecuteApprovedRule, amountQuantity},
	{"SIMULATED_TAKE_LIQUIDITY", SubjectInstrument, SideNone, agentauthority.ActionExecuteApprovedRule, amountQuantity},
	// An agent never withdraws. A delegated withdrawal is an agent PROPOSAL
	// that a policy or a person approves; the WITHDRAW intent itself is then
	// carried out by a service, never by the agent (internal/delegation).
	{"SIMULATED_WITHDRAW", SubjectDestination, SideNone, agentauthority.ActionWithdraw, amountQuantity},
	{"SIMULATED_BRIDGE_VALUE", SubjectFacility, SideNone, agentauthority.ActionExecuteApprovedRule, amountQuantity},
	{"SIMULATED_CREATE_PREDICTION_POSITION", SubjectInstrument, SideBuy, agentauthority.ActionExecuteApprovedRule, amountQuantity},
	{"SIMULATED_RESOLVE_PREDICTION_POSITION", SubjectInstrument, SideNone, agentauthority.ActionExecuteApprovedRule, amountOptionalQuantity},
}

// AdvancedActionFor returns the action type for an econ intent kind name.
func AdvancedActionFor(kind string) (ActionType, bool) {
	a := ActionType("SIMULATED_" + kind)
	for _, x := range advancedActions {
		if x.action == a {
			return a, true
		}
	}
	return "", false
}

// AdvancedActionTypes returns every advanced action type in declaration order.
func AdvancedActionTypes() []ActionType {
	out := make([]ActionType, 0, len(advancedActions))
	for _, a := range advancedActions {
		out = append(out, a.action)
	}
	return out
}

func lookupAdvanced(a ActionType) (advancedAction, bool) {
	for _, x := range advancedActions {
		if x.action == a {
			return x, true
		}
	}
	return advancedAction{}, false
}

func withAdvancedActionTypes(in []ActionType) []ActionType {
	return append(in, AdvancedActionTypes()...)
}

func withAdvancedSubjects(m map[ActionType]SubjectType) map[ActionType]SubjectType {
	for _, a := range advancedActions {
		m[a.action] = a.subject
	}
	return m
}

func withAdvancedSides(m map[ActionType]Side) map[ActionType]Side {
	for _, a := range advancedActions {
		m[a.action] = a.side
	}
	return m
}

func withAdvancedProfiles(m map[ActionType]actionProfile) map[ActionType]actionProfile {
	for _, a := range advancedActions {
		if _, dup := m[a.action]; dup {
			panic("settlement: advanced action " + string(a.action) + " already has a profile")
		}
		m[a.action] = actionProfile{
			domain: valuedomain.Simulated, rail: valuedomain.RailSimulated,
			product:                legalrouter.ProductSimulation,
			capabilities:           nil,
			executor:               ExecutorSimulated,
			agentAction:            a.agentAction,
			minVerification:        valuedomain.VerificationNone,
			requiresQuote:          false,
			requiresReservation:    a.amount != amountOptionalQuantity,
			requiresRiskEvaluation: true,
			payoutMode:             legalrouter.PayoutModeNone,
		}
	}
	return m
}

// validateAdvancedAmounts applies the amount rule of an advanced action.
func validateAdvancedAmounts(fi FinancialIntent, fields map[string]any) {
	a, ok := lookupAdvanced(fi.ActionType)
	if !ok {
		return
	}
	hasQty := fi.Quantity != nil && fi.Quantity.Sign() > 0
	hasUSD := fi.NotionalUSD != nil && fi.NotionalUSD.Sign() > 0
	switch a.amount {
	case amountQuantity:
		if !hasQty {
			fields["quantity"] = string(fi.ActionType) + " states an exact quantity in base units"
		}
		if fi.NotionalUSD != nil {
			fields["notional_usd"] = string(fi.ActionType) + " is never denominated in USD"
		}
	case amountNotionalOrQuantity:
		if hasUSD == hasQty {
			fields["amount"] = "state exactly one of notional_usd or quantity"
		}
	case amountOptionalQuantity:
		if fi.NotionalUSD != nil {
			fields["notional_usd"] = string(fi.ActionType) + " is never denominated in USD"
		}
		if fi.Quantity != nil && fi.Quantity.Sign() <= 0 {
			fields["quantity"] = "a stated quantity must be positive"
		}
	}
}
