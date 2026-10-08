package bridge

// BreakerActorID is the actor of a route's circuit breaker. When a route's
// reconciliation finds its books disagreeing with the chains
// (ReconcileRoute), the breaker pauses the route (ACTIVE -> PAUSED) as this
// SYSTEM actor, and the route's transition row (bridge_route_transitions)
// carries it: that is how a breaker trip is told apart from an operator's
// pause, which carries the operator, and how the advanced metrics count it
// (advanced_circuit_breaker_events). Only an operator resumes the route.
const BreakerActorID = "bridge-circuit-breaker"
