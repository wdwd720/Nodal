package errs

import (
	"net/http"
	"strings"
)

// Code is a stable, machine-readable rejection code. Codes are part of the
// public API contract: they never change meaning and are never removed.
// Clients switch on Code, never on Detail.
type Code string

// Request, authentication and resource codes.
const (
	// CodeValidationFailed: the request is syntactically or semantically
	// invalid. Fields carries per-field messages. HTTP 400.
	CodeValidationFailed Code = "VALIDATION_FAILED"
	// CodeBodyTooLarge: the request body is larger than the route accepts.
	// HTTP 413.
	//
	// Distinct from CodeValidationFailed because it is answered BEFORE the
	// body is read -- often before a single byte of it is -- so there is
	// nothing to validate and nothing to say about a field. A client that
	// receives it must send less, not send different (F-85).
	CodeBodyTooLarge Code = "BODY_TOO_LARGE"
	// CodeUnauthenticated: no or invalid credentials. HTTP 401.
	CodeUnauthenticated Code = "UNAUTHENTICATED"
	// CodeForbidden: the principal lacks a permission or does not own the
	// resource (tenant scoping). HTTP 403.
	CodeForbidden Code = "FORBIDDEN"
	// CodeStepUpRequired: the action needs a recent strong authentication
	// (MFA/passkey). HTTP 403.
	CodeStepUpRequired Code = "STEP_UP_REQUIRED"
	// CodeNotFound: the resource does not exist or is not visible to the
	// principal (deliberately indistinguishable). HTTP 404.
	CodeNotFound Code = "NOT_FOUND"
	// CodeConflict: a generic optimistic-concurrency or uniqueness conflict.
	// HTTP 409.
	CodeConflict Code = "CONFLICT"
	// CodeRateLimited: the caller exceeded a rate limit; RetryAfter is set.
	// HTTP 429.
	CodeRateLimited Code = "RATE_LIMITED"
	// CodeInternal: an unexpected failure. The detail is always redacted.
	// HTTP 500.
	CodeInternal Code = "INTERNAL"
	// CodeUnsupported: the operation, asset class or venue is not supported
	// by this deployment. HTTP 422.
	CodeUnsupported Code = "UNSUPPORTED"
)

// Idempotency and state-machine codes.
const (
	// CodeInvalidIdempotencyReuse: an Idempotency-Key was reused with a
	// different request body. HTTP 409.
	CodeInvalidIdempotencyReuse Code = "INVALID_IDEMPOTENCY_REUSE"
	// CodeIdempotencyInProgress: a request with the same Idempotency-Key is
	// still being processed; retry after RetryAfter. HTTP 409.
	CodeIdempotencyInProgress Code = "IDEMPOTENCY_IN_PROGRESS"
	// CodeInvalidStateTransition: the aggregate is not in a state that
	// allows the requested transition. HTTP 409.
	CodeInvalidStateTransition Code = "INVALID_STATE_TRANSITION"
	// CodeSubmissionStateUnknown: an order or transfer was submitted to a
	// venue but its acceptance is unknown; the client must observe state
	// before acting again. Mapped to 409 rather than 422 because the
	// request itself was valid and the blocker is the resource's state,
	// which a blind retry could turn into a duplicate submission.
	CodeSubmissionStateUnknown Code = "SUBMISSION_STATE_UNKNOWN"
	// CodeReconciliationRequired: the account, position or ledger segment
	// is held until a reconciliation mismatch is resolved. Mapped to 409
	// for the same reason as SUBMISSION_STATE_UNKNOWN: a resource-state
	// conflict that no change to the request can fix.
	CodeReconciliationRequired Code = "RECONCILIATION_REQUIRED"
)

// Business rejections: the request was well-formed and authorized but a
// financial, risk, eligibility or safety rule rejects it. HTTP 422.
const (
	CodeInsufficientBuyingPower Code = "INSUFFICIENT_BUYING_POWER"
	CodeAccountFrozen           Code = "ACCOUNT_FROZEN"
	CodeAssetRestricted         Code = "ASSET_RESTRICTED"
	CodeQuoteExpired            Code = "QUOTE_EXPIRED"
	CodeRiskMaxPosition         Code = "RISK_MAX_POSITION"
	CodeRiskDailyLoss           Code = "RISK_DAILY_LOSS"
	// CodeRiskConcentration: the position the request would leave behind is
	// too large a share of something -- a market's circulating supply, or one
	// creator's assets within the account's own spending. Separate from
	// RISK_MAX_POSITION because the limit is a RATIO rather than a size, and
	// the remedy is different: a smaller order does not always fix it, and
	// waiting for the denominator to grow sometimes does.
	CodeRiskConcentration       Code = "RISK_CONCENTRATION"
	CodeEligibilityJurisdiction Code = "ELIGIBILITY_JURISDICTION"
	CodeCapabilityNotApproved   Code = "CAPABILITY_NOT_APPROVED"
	// CodeVerificationRequired: the action would be permitted at a higher
	// level of FINANCIAL identity verification, which is a different thing
	// from a recent strong authentication (goal PART XLVII) -- hence a
	// separate code from STEP_UP_REQUIRED. It is deliberately distinct from
	// FORBIDDEN because it has a next step the user can actually take.
	CodeVerificationRequired Code = "VERIFICATION_REQUIRED"
	CodeStaleMarketData      Code = "STALE_MARKET_DATA"
	CodeKillSwitchActive     Code = "KILL_SWITCH_ACTIVE"
	// CodeNoValidPlan: the settlement compiler found no execution plan that
	// satisfies every constraint. A legitimate outcome (goal PART 40).
	CodeNoValidPlan Code = "NO_VALID_PLAN"
	// CodeVenueLiquidityInsufficient: the venue reported that no route or
	// not enough liquidity exists for the requested size. Distinct from
	// VENUE_UNAVAILABLE (the venue is up and answered) and from
	// VALIDATION_FAILED (the request was well-formed). HTTP 422.
	CodeVenueLiquidityInsufficient Code = "VENUE_LIQUIDITY_INSUFFICIENT"
)

// Exact-arithmetic codes (internal/money). HTTP 422.
const (
	CodeOverflow      Code = "OVERFLOW"
	CodePrecisionLoss Code = "PRECISION_LOSS"
)

// Ledger integrity codes (internal/ledger). Raised by the posting service
// and by the database triggers of migration 00101 (SQLSTATE LG001-LG005).
// All are 422 except LEDGER_IMMUTABLE, which is a 409: the request addressed
// a posted row that can never change, so no change to the request can make
// it succeed.
const (
	CodeLedgerNegativeBalance Code = "LEDGER_NEGATIVE_BALANCE"
	CodeLedgerUnbalanced      Code = "LEDGER_UNBALANCED"
	CodeLedgerImmutable       Code = "LEDGER_IMMUTABLE"
	CodeLedgerAssetMismatch   Code = "LEDGER_ASSET_MISMATCH"
	CodeLedgerAccountClosed   Code = "LEDGER_ACCOUNT_CLOSED"
)

// Dependency availability codes. HTTP 503.
const (
	CodeProviderUnavailable Code = "PROVIDER_UNAVAILABLE"
	CodeVenueUnavailable    Code = "VENUE_UNAVAILABLE"
)

// Inbound provider boundary codes (internal/webhook). HTTP 400: the request
// is rejected before any persistence beyond a security event.
const (
	// CodeWebhookSignatureInvalid: the provider signature did not verify or
	// its timestamp is outside the replay tolerance. Fields carries reason.
	CodeWebhookSignatureInvalid Code = "WEBHOOK_SIGNATURE_INVALID"
)

// Withdrawal boundary codes (internal/withdrawal). HTTP 422.
const (
	// CodeWithdrawalVelocityLimit: the request exceeds the per-request or
	// rolling-window withdrawal limits of the account's policy.
	CodeWithdrawalVelocityLimit Code = "WITHDRAWAL_VELOCITY_LIMIT"
	// CodeTermsAcceptanceRequired: a legal document this action requires has
	// not been accepted at the version and bytes now served. Fields names the
	// documents in `documents`, so a client can present exactly those and
	// retry; nothing about the request itself is wrong. HTTP 422.
	//
	// Deliberately not VERIFICATION_REQUIRED, which would send a person into
	// an identity flow they may have already completed, and deliberately not
	// FORBIDDEN, which says the account may not do this at all. What is
	// missing is a signature on a document, and the difference is the whole
	// point of having a code (goal SS48).
	CodeTermsAcceptanceRequired Code = "TERMS_ACCEPTANCE_REQUIRED"
)

// Signing boundary and wallet codes (internal/signing, internal/wallet,
// internal/provider/privy). HTTP 422.
const (
	// CodeSigningRejected: the transaction inspector (or the wallet
	// provider's own policy) refused to sign. Fields carries reason codes.
	CodeSigningRejected Code = "SIGNING_REJECTED"
	// CodeDelegationNotVerified: the wallet's delegated-signing capability
	// has not been verified (wallets.delegation_verified_at is NULL or the
	// provider capability probe is not VERIFIED). Production fails closed.
	CodeDelegationNotVerified Code = "DELEGATION_NOT_VERIFIED"
	// CodeWalletInactive: the wallet is SUSPENDED or REVOKED.
	CodeWalletInactive Code = "WALLET_INACTIVE"
)

// Execution record codes (internal/execution, internal/settlement). HTTP 409:
// the request addressed a frozen row that can never change (migrations 00202
// and 00300 raise SQLSTATE LG003 with a PLAN_IMMUTABLE / FILL_IMMUTABLE
// message), so no change to the request can make it succeed.
const (
	// CodePlanImmutable: an approved execution plan's frozen fields were
	// addressed; replanning creates a new plan version instead.
	CodePlanImmutable Code = "PLAN_IMMUTABLE"
	// CodeFillImmutable: a fill's economic fields, or a set-once marker
	// (journal_transaction_id, position_applied_at) already set, were
	// addressed.
	CodeFillImmutable Code = "FILL_IMMUTABLE"
)

// Strategy compiler and model boundary codes (internal/strategy,
// internal/model). Stage 9.
const (
	// CodeEffectForbidden: a strategy IR declares (or a runtime re-derivation
	// finds) an effect outside the allowed table, such as TRANSFER_VALUE or
	// EXPORT_SECRET. Never retried. HTTP 422.
	CodeEffectForbidden Code = "EFFECT_FORBIDDEN"
	// CodeStrategyRejected: a candidate IR failed a compiler validation
	// stage; Fields carries the stage and the sorted failure codes. HTTP 422.
	CodeStrategyRejected Code = "STRATEGY_REJECTED"
	// CodeNeedsClarification: the natural-language input is ambiguous; the
	// model returned clarification questions and no version was created.
	// HTTP 422.
	CodeNeedsClarification Code = "NEEDS_CLARIFICATION"
	// CodeStrategyVersionImmutable: the compiled fields of a strategy
	// version were addressed (migration 00500 raises SQLSTATE ST001); compile
	// a new version instead. HTTP 409.
	CodeStrategyVersionImmutable Code = "STRATEGY_VERSION_IMMUTABLE"
	// CodeModelUnavailable: the model provider failed, timed out or
	// returned an unusable stop reason; no output was invented (PART 177).
	// HTTP 503.
	CodeModelUnavailable Code = "MODEL_UNAVAILABLE"
	// CodeBudgetExhausted: a model or data budget (calls or spend) is used
	// up; the call was refused before dialing. Fields names the budget.
	// HTTP 422.
	CodeBudgetExhausted Code = "BUDGET_EXHAUSTED"
	// CodeAtCapacity: a deployment-tier ceiling is reached, so the action was
	// refused before anything was taken -- the launch cohort is full, the
	// daily transaction ceiling is spent, the money-at-risk ceiling is
	// reached, or the free-tier database is near its quota. Fields names the
	// ceiling.
	//
	// Deliberately not RATE_LIMITED: nothing about the caller is wrong and
	// waiting a second will not help. Deliberately not BUDGET_EXHAUSTED,
	// which is a model or data budget and is the caller's own. Deliberately
	// not FORBIDDEN or CAPABILITY_NOT_APPROVED: the deployment IS approved to
	// do this and has simply run out of room, and confusing the two would let
	// a capacity problem look like a revoked approval. HTTP 503.
	CodeAtCapacity Code = "AT_CAPACITY"
	// CodeSecretInModelContext: a model request contained content matching
	// the secret denylist; the request was refused before leaving the
	// process (PART 67). HTTP 400.
	CodeSecretInModelContext Code = "SECRET_IN_MODEL_CONTEXT"
)

// Point-in-time reality engine codes (internal/archive, internal/reality).
// Stage 11.
const (
	// CodeArchiveIntegrityViolation: an archived raw object no longer hashes
	// to the value recorded when it was written (or is missing). Evidence
	// has been tampered with or lost; nothing derived from it may be
	// trusted until an operator investigates. HTTP 409: the stored state
	// conflicts with its own index, and no change to the request can fix it.
	CodeArchiveIntegrityViolation Code = "ARCHIVE_INTEGRITY_VIOLATION"
)

// codeInfo is the per-code projection to HTTP.
type codeInfo struct {
	status int
	title  string
}

// registry is the single source of truth for status and title. Every Code
// constant must appear here exactly once; codes_test.go enforces it.
var registry = map[Code]codeInfo{
	CodeValidationFailed: {http.StatusBadRequest, "Validation failed"},
	CodeBodyTooLarge:     {http.StatusRequestEntityTooLarge, "Request body too large"},
	CodeUnauthenticated:  {http.StatusUnauthorized, "Authentication required"},
	CodeForbidden:        {http.StatusForbidden, "Forbidden"},
	CodeStepUpRequired:   {http.StatusForbidden, "Step-up authentication required"},
	CodeNotFound:         {http.StatusNotFound, "Not found"},
	CodeConflict:         {http.StatusConflict, "Conflict"},
	CodeRateLimited:      {http.StatusTooManyRequests, "Rate limited"},
	CodeInternal:         {http.StatusInternalServerError, "Internal error"},
	CodeUnsupported:      {http.StatusUnprocessableEntity, "Unsupported"},

	CodeInvalidIdempotencyReuse: {http.StatusConflict, "Idempotency key reused with a different request"},
	CodeIdempotencyInProgress:   {http.StatusConflict, "Request with this idempotency key is in progress"},
	CodeInvalidStateTransition:  {http.StatusConflict, "Invalid state transition"},
	CodeSubmissionStateUnknown:  {http.StatusConflict, "Submission state unknown"},
	CodeReconciliationRequired:  {http.StatusConflict, "Reconciliation required"},

	CodeInsufficientBuyingPower:    {http.StatusUnprocessableEntity, "Insufficient buying power"},
	CodeAccountFrozen:              {http.StatusUnprocessableEntity, "Account frozen"},
	CodeAssetRestricted:            {http.StatusUnprocessableEntity, "Asset restricted"},
	CodeQuoteExpired:               {http.StatusUnprocessableEntity, "Quote expired"},
	CodeRiskMaxPosition:            {http.StatusUnprocessableEntity, "Maximum position limit exceeded"},
	CodeRiskDailyLoss:              {http.StatusUnprocessableEntity, "Daily loss limit reached"},
	CodeRiskConcentration:          {http.StatusUnprocessableEntity, "Concentration limit exceeded"},
	CodeEligibilityJurisdiction:    {http.StatusUnprocessableEntity, "Not eligible in this jurisdiction"},
	CodeCapabilityNotApproved:      {http.StatusUnprocessableEntity, "Capability not approved"},
	CodeVerificationRequired:       {http.StatusUnprocessableEntity, "Identity verification required"},
	CodeStaleMarketData:            {http.StatusUnprocessableEntity, "Stale market data"},
	CodeKillSwitchActive:           {http.StatusUnprocessableEntity, "Kill switch active"},
	CodeNoValidPlan:                {http.StatusUnprocessableEntity, "No valid execution plan"},
	CodeVenueLiquidityInsufficient: {http.StatusUnprocessableEntity, "Venue liquidity insufficient"},

	CodeOverflow:      {http.StatusUnprocessableEntity, "Numeric overflow"},
	CodePrecisionLoss: {http.StatusUnprocessableEntity, "Precision loss"},

	CodeLedgerNegativeBalance: {http.StatusUnprocessableEntity, "Ledger account would go negative"},
	CodeLedgerUnbalanced:      {http.StatusUnprocessableEntity, "Journal transaction does not balance per asset"},
	CodeLedgerImmutable:       {http.StatusConflict, "Posted ledger rows are immutable"},
	CodeLedgerAssetMismatch:   {http.StatusUnprocessableEntity, "Entry asset does not match ledger account asset"},
	CodeLedgerAccountClosed:   {http.StatusUnprocessableEntity, "Ledger account is closed"},

	CodeProviderUnavailable: {http.StatusServiceUnavailable, "Provider unavailable"},
	CodeVenueUnavailable:    {http.StatusServiceUnavailable, "Venue unavailable"},

	CodeWebhookSignatureInvalid: {http.StatusBadRequest, "Webhook signature invalid"},
	CodeWithdrawalVelocityLimit: {http.StatusUnprocessableEntity, "Withdrawal velocity limit exceeded"},
	CodeTermsAcceptanceRequired: {http.StatusUnprocessableEntity, "A required legal document has not been accepted"},

	CodeSigningRejected:       {http.StatusUnprocessableEntity, "Transaction signing rejected"},
	CodeDelegationNotVerified: {http.StatusUnprocessableEntity, "Wallet delegation not verified"},
	CodeWalletInactive:        {http.StatusUnprocessableEntity, "Wallet inactive"},

	CodePlanImmutable: {http.StatusConflict, "Approved execution plans are immutable"},
	CodeFillImmutable: {http.StatusConflict, "Recorded fills are immutable"},

	CodeEffectForbidden:          {http.StatusUnprocessableEntity, "Strategy effect forbidden"},
	CodeStrategyRejected:         {http.StatusUnprocessableEntity, "Strategy rejected by the compiler"},
	CodeNeedsClarification:       {http.StatusUnprocessableEntity, "Strategy text needs clarification"},
	CodeStrategyVersionImmutable: {http.StatusConflict, "Compiled strategy versions are immutable"},
	CodeModelUnavailable:         {http.StatusServiceUnavailable, "Model unavailable"},
	CodeBudgetExhausted:          {http.StatusUnprocessableEntity, "Budget exhausted"},
	CodeAtCapacity:               {http.StatusServiceUnavailable, "At capacity"},
	CodeSecretInModelContext:     {http.StatusBadRequest, "Secret material in model context"},

	CodeArchiveIntegrityViolation: {http.StatusConflict, "Archive integrity violation"},
}

// allCodes lists every code in declaration order, for AllCodes and for the
// exhaustiveness tests. Keep in sync with the constants above.
var allCodes = []Code{
	CodeValidationFailed,
	CodeBodyTooLarge,
	CodeUnauthenticated,
	CodeForbidden,
	CodeStepUpRequired,
	CodeNotFound,
	CodeConflict,
	CodeRateLimited,
	CodeInternal,
	CodeUnsupported,
	CodeInvalidIdempotencyReuse,
	CodeIdempotencyInProgress,
	CodeInvalidStateTransition,
	CodeSubmissionStateUnknown,
	CodeReconciliationRequired,
	CodeInsufficientBuyingPower,
	CodeAccountFrozen,
	CodeAssetRestricted,
	CodeQuoteExpired,
	CodeRiskMaxPosition,
	CodeRiskDailyLoss,
	CodeRiskConcentration,
	CodeEligibilityJurisdiction,
	CodeCapabilityNotApproved,
	CodeVerificationRequired,
	CodeStaleMarketData,
	CodeKillSwitchActive,
	CodeNoValidPlan,
	CodeVenueLiquidityInsufficient,
	CodeOverflow,
	CodePrecisionLoss,
	CodeLedgerNegativeBalance,
	CodeLedgerUnbalanced,
	CodeLedgerImmutable,
	CodeLedgerAssetMismatch,
	CodeLedgerAccountClosed,
	CodeProviderUnavailable,
	CodeVenueUnavailable,
	CodeWebhookSignatureInvalid,
	CodeWithdrawalVelocityLimit,
	CodeTermsAcceptanceRequired,
	CodeSigningRejected,
	CodeDelegationNotVerified,
	CodeWalletInactive,
	CodePlanImmutable,
	CodeFillImmutable,
	CodeEffectForbidden,
	CodeStrategyRejected,
	CodeNeedsClarification,
	CodeStrategyVersionImmutable,
	CodeModelUnavailable,
	CodeBudgetExhausted,
	CodeAtCapacity,
	CodeSecretInModelContext,
	CodeArchiveIntegrityViolation,
}

// AllCodes returns every registered code in declaration order. The slice is
// a copy; callers may modify it freely.
func AllCodes() []Code {
	return append([]Code(nil), allCodes...)
}

// String returns the code as its wire form.
func (c Code) String() string {
	return string(c)
}

// Known reports whether c is a registered code. Unknown codes are treated
// as INTERNAL by ToProblem so that a typo can never leak an unmapped error.
func (c Code) Known() bool {
	_, ok := registry[c]
	return ok
}

// HTTPStatus returns the HTTP status for code. The mapping is a fixed table
// covering every registered code; unknown codes map to 500.
func HTTPStatus(code Code) int {
	if info, ok := registry[code]; ok {
		return info.status
	}
	return http.StatusInternalServerError
}

// Title returns the short human-readable summary used as the Problem title.
// Unknown codes return the INTERNAL title.
func Title(code Code) string {
	if info, ok := registry[code]; ok {
		return info.title
	}
	return registry[CodeInternal].title
}

// ProblemType returns the brand-neutral RFC 9457 type URI for code, of the
// form urn:problem:<lowercase-code>. Unknown codes map to INTERNAL.
func ProblemType(code Code) string {
	if !code.Known() {
		code = CodeInternal
	}
	return "urn:problem:" + strings.ToLower(string(code))
}
