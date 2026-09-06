// Package intent is the typed universal financial intent (goal PARTS 35, 36,
// 159, 173; SETTLEMENT_COMPILER.md §2). A TradeIntent is a request: a user,
// operator or agent asks the platform to acquire, reduce, close or target an
// economic exposure under explicit constraints. Everything that decides
// whether and how that request is honored (eligibility, risk, reservation,
// planning, quoting, signing, submission) lives in other packages and treats
// an agent intent exactly like a manual one.
//
// # Responsibilities
//
//   - Action, Mode, Status, Constraints and TradeIntent mirror the
//     trade_intents table exactly. Validate checks structure, action/field
//     consistency (at least as strictly as the table CHECK), constraint
//     sanity, and the agent linkage rule: AGENT intents carry AgentID,
//     StrategyVersionID and PredictionID; USER and OPERATOR intents carry
//     none of them. BUY_EVENT_OUTCOME is declared and rejected as UNSUPPORTED.
//   - Transitions is the explicit state machine; CanTransition consults it
//     and nothing else. Terminal statuses have no outgoing edges.
//   - Canonical is the deterministic encoding of an intent's semantic content
//     (sorted keys, money as strings, no server-assigned fields);
//     ContentHash is its sha256 and is persisted in trade_intents.content_hash.
//   - Repository persists intents and their transitions in the caller's
//     transaction. Create is idempotent on (account_id, idempotency_key):
//     the same key with the same content returns the original intent with
//     Existing = true; the same key with different content fails with
//     INVALID_IDEMPOTENCY_REUSE. Transition writes the intent_transitions
//     row that migration 00603 demands, updates the status, enqueues an
//     intent.transitioned outbox event and appends an audit event on the
//     account stream — all in the same transaction, so none of them can
//     exist without the others.
//   - Service.Submit is the command entry point for manual and agent
//     intents: it derives the actor from the principal, enforces tenant
//     scoping and permissions, claims the idempotency key through
//     idempotency.Store under endpoint "intent.submit", persists the RECEIVED
//     intent and records the response so a duplicate HTTP command replays
//     the original result (PART 173).
//
// # What this package must never do
//
//   - Authorize anything. An intent is a request; RECEIVED means "recorded",
//     not "allowed". No code here consults eligibility, risk, buying power,
//     kill switches or capability gates, and no code here reserves, plans,
//     signs or submits.
//   - Let an agent act outside its bound account or as any other actor type:
//     the actor is derived from the principal, RequireAccount is enforced on
//     every submission, and an agent principal that names another actor type
//     is refused with FORBIDDEN.
//   - Change the identity, economics or mode of a persisted intent (PART 159).
//     Only status, rejection code, terminal time and the decision, reservation,
//     plan and order links are writable; the trade_intents_immutable_identity
//     trigger of migration 00605 refuses everything else.
//   - Change a status without a transition row, an outbox event and an audit
//     event in the same transaction; a bare UPDATE is refused at COMMIT with
//     SQLSTATE AU001 (migration 00603).
//   - Reuse an idempotency key for a different financial intent, or infer
//     mode from anything but the explicit Mode field.
//   - Use floating point for any amount: notionals are money.USD, quantities
//     money.Quantity, limits money.BPS and prices money.Price.
package intent
