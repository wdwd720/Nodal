package errs_test

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// expectedStatus is the documented mapping, spelled out independently of the
// registry so a change to either side is a visible diff here.
var expectedStatus = map[errs.Code]int{
	errs.CodeValidationFailed: http.StatusBadRequest,
	errs.CodeUnauthenticated:  http.StatusUnauthorized,
	errs.CodeForbidden:        http.StatusForbidden,
	errs.CodeStepUpRequired:   http.StatusForbidden,
	errs.CodeNotFound:         http.StatusNotFound,
	errs.CodeConflict:         http.StatusConflict,
	errs.CodeRateLimited:      http.StatusTooManyRequests,
	errs.CodeInternal:         http.StatusInternalServerError,
	errs.CodeUnsupported:      http.StatusUnprocessableEntity,

	errs.CodeInvalidIdempotencyReuse: http.StatusConflict,
	errs.CodeIdempotencyInProgress:   http.StatusConflict,
	errs.CodeInvalidStateTransition:  http.StatusConflict,
	errs.CodeSubmissionStateUnknown:  http.StatusConflict,
	errs.CodeReconciliationRequired:  http.StatusConflict,

	errs.CodeInsufficientBuyingPower:    http.StatusUnprocessableEntity,
	errs.CodeAccountFrozen:              http.StatusUnprocessableEntity,
	errs.CodeAssetRestricted:            http.StatusUnprocessableEntity,
	errs.CodeQuoteExpired:               http.StatusUnprocessableEntity,
	errs.CodeRiskMaxPosition:            http.StatusUnprocessableEntity,
	errs.CodeRiskDailyLoss:              http.StatusUnprocessableEntity,
	errs.CodeRiskConcentration:          http.StatusUnprocessableEntity,
	errs.CodeEligibilityJurisdiction:    http.StatusUnprocessableEntity,
	errs.CodeCapabilityNotApproved:      http.StatusUnprocessableEntity,
	errs.CodeVerificationRequired:       http.StatusUnprocessableEntity,
	errs.CodeStaleMarketData:            http.StatusUnprocessableEntity,
	errs.CodeKillSwitchActive:           http.StatusUnprocessableEntity,
	errs.CodeNoValidPlan:                http.StatusUnprocessableEntity,
	errs.CodeVenueLiquidityInsufficient: http.StatusUnprocessableEntity,

	errs.CodeOverflow:      http.StatusUnprocessableEntity,
	errs.CodePrecisionLoss: http.StatusUnprocessableEntity,

	errs.CodeLedgerNegativeBalance: http.StatusUnprocessableEntity,
	errs.CodeLedgerUnbalanced:      http.StatusUnprocessableEntity,
	errs.CodeLedgerImmutable:       http.StatusConflict,
	errs.CodeLedgerAssetMismatch:   http.StatusUnprocessableEntity,
	errs.CodeLedgerAccountClosed:   http.StatusUnprocessableEntity,

	errs.CodeProviderUnavailable: http.StatusServiceUnavailable,
	errs.CodeVenueUnavailable:    http.StatusServiceUnavailable,

	errs.CodeWebhookSignatureInvalid: http.StatusBadRequest,
	errs.CodeWithdrawalVelocityLimit: http.StatusUnprocessableEntity,

	errs.CodeSigningRejected:       http.StatusUnprocessableEntity,
	errs.CodeDelegationNotVerified: http.StatusUnprocessableEntity,
	errs.CodeWalletInactive:        http.StatusUnprocessableEntity,

	errs.CodePlanImmutable: http.StatusConflict,
	errs.CodeFillImmutable: http.StatusConflict,

	errs.CodeEffectForbidden:          http.StatusUnprocessableEntity,
	errs.CodeStrategyRejected:         http.StatusUnprocessableEntity,
	errs.CodeNeedsClarification:       http.StatusUnprocessableEntity,
	errs.CodeStrategyVersionImmutable: http.StatusConflict,
	errs.CodeModelUnavailable:         http.StatusServiceUnavailable,
	errs.CodeBudgetExhausted:          http.StatusUnprocessableEntity,
	errs.CodeSecretInModelContext:     http.StatusBadRequest,

	errs.CodeArchiveIntegrityViolation: http.StatusConflict,
}

// conventionsCodes is the minimum set required by CONVENTIONS.md.
var conventionsCodes = []string{
	"INSUFFICIENT_BUYING_POWER", "ACCOUNT_FROZEN", "ASSET_RESTRICTED", "VENUE_UNAVAILABLE", "QUOTE_EXPIRED",
	"RISK_MAX_POSITION", "RISK_DAILY_LOSS", "ELIGIBILITY_JURISDICTION", "CAPABILITY_NOT_APPROVED",
	"SUBMISSION_STATE_UNKNOWN", "RECONCILIATION_REQUIRED", "PROVIDER_UNAVAILABLE", "STALE_MARKET_DATA",
	"INVALID_IDEMPOTENCY_REUSE", "IDEMPOTENCY_IN_PROGRESS", "VALIDATION_FAILED", "NOT_FOUND", "UNAUTHENTICATED",
	"FORBIDDEN", "STEP_UP_REQUIRED", "CONFLICT", "RATE_LIMITED", "INTERNAL", "INVALID_STATE_TRANSITION",
	"OVERFLOW", "PRECISION_LOSS", "UNSUPPORTED", "KILL_SWITCH_ACTIVE", "NO_VALID_PLAN",
}

func TestAllCodes_UniqueAndWellFormed(t *testing.T) {
	t.Parallel()
	codes := errs.AllCodes()
	require.NotEmpty(t, codes)
	require.GreaterOrEqual(t, len(codes), len(conventionsCodes))

	pattern := regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$`)
	seen := make(map[errs.Code]struct{}, len(codes))
	for _, c := range codes {
		_, dup := seen[c]
		require.False(t, dup, "duplicate code %s", c)
		seen[c] = struct{}{}
		require.Regexp(t, pattern, string(c), "code %q is not UPPER_SNAKE", c)
		require.True(t, c.Known(), "code %s is not registered", c)
		require.Equal(t, string(c), c.String())
	}
}

func TestAllCodes_CoversConventions(t *testing.T) {
	t.Parallel()
	have := make(map[string]bool)
	for _, c := range errs.AllCodes() {
		have[string(c)] = true
	}
	for _, want := range conventionsCodes {
		require.True(t, have[want], "CONVENTIONS code %s missing", want)
	}
}

func TestAllCodes_ReturnsCopy(t *testing.T) {
	t.Parallel()
	a := errs.AllCodes()
	a[0] = "MUTATED"
	require.NotEqual(t, errs.Code("MUTATED"), errs.AllCodes()[0])
}

func TestHTTPStatus_EveryCodeExplicitlyMapped(t *testing.T) {
	t.Parallel()
	codes := errs.AllCodes()
	require.Len(t, expectedStatus, len(codes), "expectedStatus table and AllCodes disagree on the code set")
	for _, c := range codes {
		want, ok := expectedStatus[c]
		require.True(t, ok, "no expected status for %s", c)
		require.Equal(t, want, errs.HTTPStatus(c), "status for %s", c)
		if c != errs.CodeInternal {
			require.NotEqual(t, http.StatusInternalServerError, errs.HTTPStatus(c), "%s must not fall through to 500", c)
		}
		require.GreaterOrEqual(t, errs.HTTPStatus(c), 400)
		require.Less(t, errs.HTTPStatus(c), 600)
	}
}

func TestHTTPStatus_Unknown(t *testing.T) {
	t.Parallel()
	require.Equal(t, http.StatusInternalServerError, errs.HTTPStatus("NOT_A_CODE"))
	require.Equal(t, http.StatusInternalServerError, errs.HTTPStatus(""))
	require.False(t, errs.Code("NOT_A_CODE").Known())
}

func TestTitle_EveryCode(t *testing.T) {
	t.Parallel()
	seen := map[string]errs.Code{}
	for _, c := range errs.AllCodes() {
		title := errs.Title(c)
		require.NotEmpty(t, title, "title for %s", c)
		require.NotEqual(t, string(c), title, "title for %s should be prose, not the code", c)
		prev, dup := seen[title]
		require.False(t, dup, "title %q shared by %s and %s", title, prev, c)
		seen[title] = c
	}
	require.Equal(t, "Internal error", errs.Title("NOT_A_CODE"))
}

func TestProblemType(t *testing.T) {
	t.Parallel()
	require.Equal(t, "urn:problem:insufficient_buying_power", errs.ProblemType(errs.CodeInsufficientBuyingPower))
	require.Equal(t, "urn:problem:internal", errs.ProblemType(errs.CodeInternal))
	require.Equal(t, "urn:problem:internal", errs.ProblemType("NOT_A_CODE"))
	for _, c := range errs.AllCodes() {
		require.Regexp(t, `^urn:problem:[a-z0-9_]+$`, errs.ProblemType(c))
		require.NotContains(t, errs.ProblemType(c), "http", "type must be brand-neutral, no domain")
	}
}
