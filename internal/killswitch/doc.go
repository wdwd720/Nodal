// Package killswitch implements the emergency controls of goal PARTS 52, 53
// and 165 (POLICY_AUTHORITY §2): persisted switches that stop NEW RISK, and
// nothing else.
//
// # Responsibilities
//
//   - Kind is the closed set of twelve switches (GLOBAL_NEW_RISK_KILL,
//     ACCOUNT_FREEZE, AGENT_PAUSE, ...). Each has a scope discipline
//     (global "*", a specific id, or either for MODEL_DISABLE) and a
//     Severity: SEVERE switches (global, chain, provider, funding,
//     withdrawals) need a dual-controlled admin approval to release.
//   - Every guarded operation declares an ActionClass. Blocking is the
//     matrix of POLICY_AUTHORITY §2, implemented as the pure function
//     Blocking over the sorted list of active switches: NEW_RISK is blocked
//     by any matching switch; REDUCE_RISK only by INSTRUMENT_HALT,
//     CHAIN_DISABLE_NEW_ACTIONS, PROVIDER_DISABLE_NEW_ACTIONS and, when the
//     Policy says so, ACCOUNT_FREEZE; WITHDRAW by WITHDRAWALS_DISABLE,
//     ACCOUNT_FREEZE and GLOBAL_NEW_RISK_KILL; OBSERVE, SETTLE, RECONCILE,
//     LEDGER_POST and CANCEL are never blocked, and Check returns for them
//     without touching the database.
//   - Checker.Check is the authoritative check: callers pass the Querier of
//     the transaction that authorizes the action, so a switch activated
//     before that transaction is always seen. CachedChecker serves
//     pre-checks only (TTL at most one second) and must never be the last
//     word.
//   - Controller.Activate is the fast path: one operator with kill:activate
//     and a reason, no step-up, no approval, effective on the next check.
//     Controller.Release needs kill:release plus step-up, and for SEVERE
//     switches a verified KILL_SWITCH_RELEASE approval. Both record an
//     immutable kill_switch_transitions row and an audit event in the same
//     transaction.
//
// # What this package must never do
//
//   - Stop reconciliation, settlement, ledger posting, cancellation, or the
//     reading of external state: those action classes are never blocked,
//     whatever switches are active (proven by a property test).
//   - Let an AGENT (or SERVICE) principal activate or release a switch: they
//     are rejected with FORBIDDEN before any query.
//   - Slow activation down with approvals or step-up, or let a release
//     bypass the approval a SEVERE switch requires.
//   - Treat an in-process cache as authoritative, or cache for longer than
//     one second.
//   - Depend on a model, randomness, an implicit clock, or map iteration
//     order for a blocking decision.
package killswitch
