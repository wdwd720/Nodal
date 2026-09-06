// Package httpapi is the HTTP boundary of the control plane: the handlers that
// implement the generated chi strict server (internal/gen/api) for the v1
// contract in openapi/openapi.yaml, the middleware chain around them, and the
// wiring helpers that adapt the domain packages to the narrow ports the
// handlers depend on.
//
// # Responsibilities
//
//   - Server implements api.StrictServerInterface. Every method translates an
//     HTTP request into one domain call and the domain's answer back into the
//     generated response type. It decodes, it authorizes, it maps errors; it
//     never decides anything financial.
//   - Router assembles the middleware chain (request/correlation ids, secure
//     headers, structured logging, metrics, panic recovery, body capture, CORS
//     preflight refusal, rate limiting, session loading, CSRF) and mounts the
//     generated routes under /v1.
//   - Authorization is deny-by-default and per operation: authorize consults
//     the operationPolicies table keyed by the generated operation id, and an
//     operation with no entry is refused with FORBIDDEN before the handler
//     runs. TestEveryOperationHasAPolicy fails if a new operation is generated
//     without one.
//   - Every failure leaves as application/problem+json through errs.ToProblem,
//     including panics and request-binding errors, so no driver message, SQL
//     string, provider URL or secret can reach a client.
//   - Every mutating endpoint runs inside the idempotency contract of
//     internal/idempotency: a replay returns the original status and body, and
//     the same key with a different body is INVALID_IDEMPOTENCY_REUSE.
//
// # What this package must never do
//
//   - Edit a balance, post a ledger row, reserve capital, sign, or move money.
//     There is no endpoint for any of those; money moves only through the
//     domain packages' own posting paths, reached through the ports below.
//   - Decide a financial rule: sizing, eligibility, risk, buying power,
//     valuation, finality and gate activation all live in domain packages.
//     Handlers may only assemble numbers those packages already computed, and
//     only with exact money arithmetic.
//   - Turn a transport timeout into a definitive failure. A cancelled or timed
//     out request answers PROVIDER_UNAVAILABLE / SUBMISSION_STATE_UNKNOWN, never
//     "rejected".
//   - Trust anything a client supplies for authority: the principal comes from
//     the session cookie through internal/auth and internal/security only.
//   - Use floating point for money. USD and quantities cross the wire as
//     decimal strings produced by internal/money.
package httpapi
