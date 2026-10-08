package bridge

import (
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
)

type (
	routeKind struct{}
	legKind   struct{}
)

// RouteID identifies a bridge route.
type RouteID = id.ID[routeKind]

// LegID identifies a bridge leg.
type LegID = id.ID[legKind]

// CrossChainCapitalState is where one leg of cross-chain capital stands: the
// status of a bridge_legs row.
type CrossChainCapitalState string

// The states, in the order a leg moves through them.
const (
	// StateAvailable: the request exists; nothing is reserved yet (the birth
	// of a leg).
	StateAvailable CrossChainCapitalState = "AVAILABLE"
	// StateReserved: the source units are reserved in WALLET(source).
	StateReserved CrossChainCapitalState = "RESERVED"
	// StateBridgeRequested: the BRIDGE_LOCK is signed and submitted (or about
	// to be); the units are still in WALLET(source) under the reservation.
	// A lock that is confirmed and then reorged away before finality stays
	// here (or fails once it can never land).
	StateBridgeRequested CrossChainCapitalState = "BRIDGE_REQUESTED"
	// StateSourceFinalized: the lock is FINALIZED; BRIDGE_SOURCE_FINALIZED
	// moved the units to BRIDGE_IN_FLIGHT(source). Finality never reverts.
	StateSourceFinalized CrossChainCapitalState = "SOURCE_FINALIZED"
	// StateMessageInFlight: a release is signed and relayed to the
	// destination.
	StateMessageInFlight CrossChainCapitalState = "MESSAGE_IN_FLIGHT"
	// StateDestinationObserved: the release is CONFIRMED on the destination
	// but not final; a reorg returns the leg to MESSAGE_IN_FLIGHT.
	StateDestinationObserved CrossChainCapitalState = "DESTINATION_OBSERVED"
	// StateDestinationFinalized: the release is FINALIZED;
	// BRIDGE_DESTINATION_FINALIZED moved the value to WALLET(destination).
	StateDestinationFinalized CrossChainCapitalState = "DESTINATION_FINALIZED"
	// StateReconciled: the leg's postings were checked against both chains.
	StateReconciled CrossChainCapitalState = "RECONCILED"
	// StateFailed: the leg ended without moving value across (its source
	// reservation released, or -- after manual review -- refunded).
	StateFailed CrossChainCapitalState = "FAILED"
	// StateManualReview: the destination could not complete (or reconcile)
	// on its own; an operator resumes the relay or refunds.
	StateManualReview CrossChainCapitalState = "MANUAL_REVIEW"
)

var allStates = []CrossChainCapitalState{
	StateAvailable, StateReserved, StateBridgeRequested, StateSourceFinalized, StateMessageInFlight,
	StateDestinationObserved, StateDestinationFinalized, StateReconciled, StateFailed, StateManualReview,
}

// AllStates returns every leg state (the Go twin of bridge_legs_status_check).
func AllStates() []CrossChainCapitalState { return append([]CrossChainCapitalState(nil), allStates...) }

var stateTransitions = map[CrossChainCapitalState][]CrossChainCapitalState{
	StateAvailable:            {StateReserved, StateFailed},
	StateReserved:             {StateBridgeRequested, StateFailed},
	StateBridgeRequested:      {StateSourceFinalized, StateFailed},
	StateSourceFinalized:      {StateMessageInFlight, StateManualReview},
	StateMessageInFlight:      {StateDestinationObserved, StateDestinationFinalized, StateManualReview},
	StateDestinationObserved:  {StateDestinationFinalized, StateMessageInFlight, StateManualReview},
	StateDestinationFinalized: {StateReconciled, StateManualReview},
	StateManualReview:         {StateMessageInFlight, StateReconciled, StateFailed},
}

// CanTransition reports whether from -> to is legal. It mirrors
// cp_bridge_leg_transition_legal (migration 00748). No edge leaves a
// finalized state for an earlier one: finality never reverts.
func CanTransition(from, to CrossChainCapitalState) bool {
	for _, t := range stateTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Terminal reports whether the leg can no longer change.
func (s CrossChainCapitalState) Terminal() bool { return s == StateReconciled || s == StateFailed }

// LedgerHolding says which ledger account holds a leg's value.
type LedgerHolding string

// Holdings.
const (
	// HoldingSourceReserved: still in WALLET(source), reserved.
	HoldingSourceReserved LedgerHolding = "WALLET_SOURCE_RESERVED"
	// HoldingInFlight: in BRIDGE_IN_FLIGHT(source).
	HoldingInFlight LedgerHolding = "BRIDGE_IN_FLIGHT"
	// HoldingDestination: in WALLET(destination).
	HoldingDestination LedgerHolding = "WALLET_DESTINATION"
	// HoldingNone: the leg moved nothing (failed before its source was final,
	// or refunded back to WALLET(source) unreserved).
	HoldingNone LedgerHolding = "NONE"
)

// RouteStatus is a route's status.
type RouteStatus string

// Route statuses.
const (
	RouteActive RouteStatus = "ACTIVE"
	// RoutePaused: no new legs (the reconciliation breaker or an operator);
	// legs already in flight still complete.
	RoutePaused RouteStatus = "PAUSED"
	RouteClosed RouteStatus = "CLOSED"
)

// AllRouteStatuses returns every route status (the Go twin of
// bridge_routes_status_check).
func AllRouteStatuses() []RouteStatus { return []RouteStatus{RouteActive, RoutePaused, RouteClosed} }

var routeTransitions = map[RouteStatus][]RouteStatus{
	RouteActive: {RoutePaused, RouteClosed},
	RoutePaused: {RouteActive, RouteClosed},
}

// CanTransitionRoute mirrors cp_bridge_route_transition_legal.
func CanTransitionRoute(from, to RouteStatus) bool {
	for _, t := range routeTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// AttemptKind is what a release attempt does.
type AttemptKind string

// Attempt kinds.
const (
	// AttemptRelease relays the lock to the destination.
	AttemptRelease AttemptKind = "RELEASE"
	// AttemptRefund returns the units on the source after a destination that
	// never released.
	AttemptRefund AttemptKind = "REFUND"
)

// AllAttemptKinds returns every attempt kind (the Go twin of
// bridge_release_attempts_kind_check).
func AllAttemptKinds() []AttemptKind { return []AttemptKind{AttemptRelease, AttemptRefund} }

// LegModes are the modes a leg runs in (the Go twin of
// bridge_legs_mode_check).
func LegModes() []gates.Mode { return []gates.Mode{gates.ModeSandbox, gates.ModeDevnet} }

// Effect kinds of the BRIDGE subsystem.
const (
	effectKindSource      = "BRIDGE_SOURCE"
	effectKindDestination = "BRIDGE_DESTINATION"
	effectKindRefund      = "BRIDGE_REFUND"
)
