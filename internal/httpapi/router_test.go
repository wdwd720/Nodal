package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/auth/httpmw"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stream"
)

// --- happy paths -------------------------------------------------------------

func TestReadEndpoints_HappyPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		path      string
		principal func() security.Principal
		check     func(t *testing.T, res *response)
	}{
		{
			name: "accounts", path: "/v1/accounts", principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out []api.Account
				res.json(&out)
				require.Len(t, out, 1)
				assert.Equal(t, testAccountID.String(), out[0].Id.String())
			},
		},
		{
			name: "account", path: "/v1/accounts/" + testAccountID.String(), principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out api.Account
				res.json(&out)
				assert.Equal(t, api.AccountStatusACTIVE, out.Status)
			},
		},
		{
			name: "buying power", path: "/v1/accounts/" + testAccountID.String() + "/buying-power",
			principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				raw := res.raw()
				// Every money field is a decimal string, never a JSON number.
				for _, field := range []string{
					"portfolio_value", "buying_power", "available_now",
					"reserved", "pending", "withdrawable",
				} {
					v, ok := raw[field].(string)
					require.True(t, ok, "%s must be a string, got %T", field, raw[field])
					assert.Regexp(t, `^-?[0-9]+\.[0-9]{2}$`, v, field)
				}
				balances, ok := raw["underlying_balances"].([]any)
				require.True(t, ok)
				first, ok := balances[0].(map[string]any)
				require.True(t, ok)
				assert.IsType(t, "", first["quantity"])
				assert.IsType(t, "", first["usd_value"])
			},
		},
		{
			name: "holdings", path: "/v1/accounts/" + testAccountID.String() + "/holdings",
			principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				raw := res.raw()
				list, ok := raw["holdings"].([]any)
				require.True(t, ok)
				h, ok := list[0].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, "1000.00", h["usd_mark"])
				assert.Equal(t, "950.00", h["cost_basis_usd"])
				assert.Equal(t, "50.00", h["unrealized_pnl_usd"])
				assert.Equal(t, "1000000000", h["quantity"])
			},
		},
		{
			name: "ledger", path: "/v1/accounts/" + testAccountID.String() + "/ledger/transactions",
			principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				raw := res.raw()
				assert.Contains(t, raw, "next_cursor")
				assert.Nil(t, raw["next_cursor"], "an exhausted page reports a null cursor")
			},
		},
		{
			name: "activity", path: "/v1/accounts/" + testAccountID.String() + "/activity",
			principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out api.ActivityPage
				res.json(&out)
				require.Len(t, out.Items, 1)
			},
		},
		{
			name: "assets", path: "/v1/assets", principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out []api.Asset
				res.json(&out)
				require.Len(t, out, 1)
				assert.Equal(t, "USDC", out[0].Symbol)
			},
		},
		{
			name: "instruments", path: "/v1/instruments", principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out []api.Instrument
				res.json(&out)
				require.Len(t, out, 1)
			},
		},
		{
			name: "instrument detail", path: "/v1/instruments/" + testInstrument.String(),
			principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out api.InstrumentDetail
				res.json(&out)
				assert.Equal(t, "SOL/USDC", out.CanonicalName)
			},
		},
		{
			name: "intents", path: "/v1/intents?account_id=" + testAccountID.String(),
			principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out api.TradeIntentPage
				res.json(&out)
				require.Len(t, out.Items, 1)
				require.NotNil(t, out.Items[0].NotionalUsd)
				assert.Equal(t, "100.00", *out.Items[0].NotionalUsd)
			},
		},
		{
			name: "intent detail", path: "/v1/intents/" + testIntentID.String(), principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out api.TradeIntentDetail
				res.json(&out)
				assert.Equal(t, testIntentID.String(), out.Id.String())
			},
		},
		{
			name: "orders", path: "/v1/orders?account_id=" + testAccountID.String(),
			principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out api.OrderPage
				res.json(&out)
				require.Len(t, out.Items, 1)
				assert.Equal(t, "1000000", out.Items[0].InputQuantity)
			},
		},
		{
			name: "order detail", path: "/v1/orders/" + testOrderID.String(), principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out api.OrderDetail
				res.json(&out)
				assert.Equal(t, testOrderID.String(), out.Id.String())
			},
		},
		{
			name: "deposits", path: "/v1/funding/deposits?account_id=" + testAccountID.String(),
			principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out api.DepositPage
				res.json(&out)
				require.Len(t, out.Items, 1)
			},
		},
		{
			name: "deposit detail", path: "/v1/funding/deposits/" + testDepositID.String(),
			principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out api.DepositDetail
				res.json(&out)
				assert.Equal(t, testDepositID.String(), out.Id.String())
				assert.Nil(t, out.ClientSecretRef, "a client secret is returned once, at creation, never on a read")
			},
		},
		{
			name: "me", path: "/v1/me", principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out api.Principal
				res.json(&out)
				assert.Equal(t, api.PrincipalActorType("USER"), out.ActorType)
				require.NotNil(t, out.StepUpValidUntil)
			},
		},
		{
			name: "sessions", path: "/v1/sessions", principal: customerPrincipal,
			check: func(t *testing.T, res *response) {
				var out []api.SessionSummary
				res.json(&out)
				require.Len(t, out, 1)
				require.NotNil(t, out[0].Current)
				assert.True(t, *out[0].Current)
			},
		},
		{
			name: "admin accounts", path: "/v1/admin/accounts", principal: operatorPrincipal,
			check: func(t *testing.T, res *response) {
				var out api.AccountPage
				res.json(&out)
				require.Len(t, out.Items, 1)
			},
		},
		{
			name: "admin gates", path: "/v1/admin/gates", principal: operatorPrincipal,
			check: func(t *testing.T, res *response) {
				var out []api.CapabilityGate
				res.json(&out)
				require.Len(t, out, 1)
				assert.False(t, out[0].Active, "every live capability gate defaults DISABLED")
				assert.Equal(t, api.CapabilityGateState("DISABLED"), out[0].State)
			},
		},
		{
			name: "admin kill switches", path: "/v1/admin/kill-switches", principal: operatorPrincipal,
			check: func(t *testing.T, res *response) {
				var out []api.KillSwitch
				res.json(&out)
				require.Len(t, out, 1)
			},
		},
		{
			name: "admin providers", path: "/v1/admin/providers", principal: operatorPrincipal,
			check: func(t *testing.T, res *response) {
				var out []api.ProviderStatus
				res.json(&out)
				require.Len(t, out, 1)
			},
		},
		{
			name: "admin actions", path: "/v1/admin/actions", principal: operatorPrincipal,
			check: func(t *testing.T, res *response) {
				var out api.AdminActionPage
				res.json(&out)
				assert.NotNil(t, out.Items)
			},
		},
		{
			name: "admin reconciliation", path: "/v1/admin/reconciliation/records", principal: operatorPrincipal,
			check: func(t *testing.T, res *response) {
				var out api.ReconciliationRecordPage
				res.json(&out)
				assert.NotNil(t, out.Items)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			p := tc.principal()
			h.as(&p)
			res := h.do(http.MethodGet, tc.path, nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, "application/json", res.Header().Get("Content-Type"))
			tc.check(t, res)
		})
	}
}

func TestSystemEndpoints(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.as(nil)

	assert.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/healthz", nil).Code)
	assert.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/readyz", nil).Code)

	res := h.do(http.MethodGet, "/v1/version", nil)
	require.Equal(t, http.StatusOK, res.Code)
	raw := res.raw()
	assert.Equal(t, "test-build", raw["build_version"])
	assert.Equal(t, "hash-1", raw["config_hash"])
	assert.Equal(t, "TEST", raw["environment"])
}

func TestReadyzReportsNotReadyWhenTheDatabaseIsDown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.as(nil)
	h.ports.health.err = errors.New("dial tcp 127.0.0.1:5432: connection refused")
	res := h.do(http.MethodGet, "/v1/readyz", nil)
	assert.Equal(t, http.StatusServiceUnavailable, res.Code)
	assert.NotContains(t, res.Body.String(), "127.0.0.1", "a readiness probe never echoes a driver message")
}

func TestReadyzReportsNotReadyWhileDraining(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.as(nil)
	require.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/readyz", nil).Code)
	h.server.StopStreams()
	assert.Equal(t, http.StatusServiceUnavailable, h.do(http.MethodGet, "/v1/readyz", nil).Code)
}

// --- the error contract (PART 108 / PART 37) ---------------------------------

// TestBusinessRejectionsReachTheWireWithTheirCode drives every rejection a
// domain package can raise through the boundary and asserts the HTTP status and
// the machine-readable code the TypeScript client switches on. A business
// rejection is always a 4xx, never a 500.
func TestBusinessRejectionsReachTheWireWithTheirCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code   errs.Code
		status int
		fields map[string]any
		// dependency marks the two availability codes, which are 503 by the
		// errs contract because the request was fine and a dependency was not.
		dependency bool
	}{
		{
			errs.CodeInsufficientBuyingPower, http.StatusUnprocessableEntity,
			map[string]any{"available": "10.00", "requested": "500.00"},
			false,
		},
		{errs.CodeAccountFrozen, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeAssetRestricted, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeQuoteExpired, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeRiskMaxPosition, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeRiskDailyLoss, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeEligibilityJurisdiction, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeCapabilityNotApproved, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeStaleMarketData, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeKillSwitchActive, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeNoValidPlan, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeVenueLiquidityInsufficient, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeUnsupported, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeOverflow, http.StatusUnprocessableEntity, nil, false},
		{errs.CodePrecisionLoss, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeSigningRejected, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeDelegationNotVerified, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeWalletInactive, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeWithdrawalVelocityLimit, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeLedgerNegativeBalance, http.StatusUnprocessableEntity, nil, false},
		{errs.CodeSubmissionStateUnknown, http.StatusConflict, nil, false},
		{errs.CodeReconciliationRequired, http.StatusConflict, nil, false},
		{errs.CodeInvalidStateTransition, http.StatusConflict, nil, false},
		{errs.CodeConflict, http.StatusConflict, nil, false},
		{errs.CodeInvalidIdempotencyReuse, http.StatusConflict, nil, false},
		{errs.CodeValidationFailed, http.StatusBadRequest, nil, false},
		{errs.CodeNotFound, http.StatusNotFound, nil, false},
		{errs.CodeForbidden, http.StatusForbidden, nil, false},
		{errs.CodeStepUpRequired, http.StatusForbidden, nil, false},
		{errs.CodeUnauthenticated, http.StatusUnauthorized, nil, false},
		{errs.CodeRateLimited, http.StatusTooManyRequests, nil, false},
		{errs.CodeProviderUnavailable, http.StatusServiceUnavailable, nil, true},
		{errs.CodeVenueUnavailable, http.StatusServiceUnavailable, nil, true},
	}

	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			p := customerPrincipal()
			h.as(&p)

			e := errs.New(tc.code, "domain said no")
			if tc.fields != nil {
				e = e.WithFields(tc.fields)
			}
			h.ports.intents.err = e

			res := h.do(http.MethodPost, "/v1/intents", map[string]any{
				"account_id":    testAccountID.String(),
				"instrument_id": testInstrument.String(),
				"action":        "ACQUIRE_NOTIONAL",
				"notional_usd":  "100.00",
				"mode":          "PAPER",
			}, "Idempotency-Key", "rejection-key-01")

			require.Equal(t, tc.status, res.Code, "body=%s", res.Body.String())
			if !tc.dependency {
				require.Less(t, res.Code, 500, "a business rejection is never a 5xx")
			}
			problem := res.problem()
			assert.Equal(t, tc.code, problem.Code)
			assert.Equal(t, "urn:problem:"+strings.ToLower(string(tc.code)), problem.Type)
			assert.Equal(t, "/v1/intents", problem.Instance)
			assert.NotEmpty(t, problem.RequestID, "every problem carries the request id")
			if tc.fields != nil {
				assert.Equal(t, tc.fields, problem.Fields)
			}
			if tc.code == errs.CodeRateLimited {
				assert.NotEmpty(t, res.Header().Get("Retry-After"))
			}
		})
	}
}

// TestInternalErrorsNeverLeak: a raw driver message, a SQL string, a provider
// URL and a secret must not reach a client, and the answer must still be
// problem+json.
func TestInternalErrorsNeverLeak(t *testing.T) {
	t.Parallel()
	leaky := errors.New(
		`pq: relation "ledger_balances" does not exist; SELECT * FROM ledger_balances; ` +
			`dsn=postgres://cp_app:hunter2@db.internal:5432/controlplane ` +
			`provider=https://api.stripe.com/v1/crypto/onramp_sessions api_key=sk_live_abc123`,
	)

	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	h.ports.accounts.err = leaky

	res := h.do(http.MethodGet, "/v1/accounts", nil)
	require.Equal(t, http.StatusInternalServerError, res.Code)
	problem := res.problem()
	assert.Equal(t, errs.CodeInternal, problem.Code)
	assert.Equal(t, "internal error", problem.Detail)
	assert.Empty(t, problem.Fields)

	body := res.Body.String()
	for _, secret := range []string{
		"ledger_balances", "SELECT", "hunter2", "cp_app", "db.internal",
		"api.stripe.com", "sk_live_abc123", "postgres://",
	} {
		assert.NotContains(t, body, secret, "the response leaked %q", secret)
	}
}

// TestPanicsBecomeProblemJSON: a panic in a handler is recovered and rendered
// as an INTERNAL problem, and the panic text never reaches the client.
func TestPanicsBecomeProblemJSON(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	h.ports.assets.err = panicError{msg: "boom: secret-token-xyz"}

	res := h.do(http.MethodGet, "/v1/assets", nil)
	require.Equal(t, http.StatusInternalServerError, res.Code)
	assert.Equal(t, errs.CodeInternal, res.problem().Code)
	assert.NotContains(t, res.Body.String(), "secret-token-xyz")
}

// panicError panics when the fake asks it for its error, which puts the panic
// inside the handler, under the recovery middleware.
type panicError struct{ msg string }

func (p panicError) Error() string { panic(p.msg) }

// TestUnknownRouteAndMethodAreProblemJSON keeps the contract uniform even for
// requests that never reach a handler.
func TestUnknownRouteAndMethodAreProblemJSON(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)

	res := h.do(http.MethodGet, "/v1/nope", nil)
	require.Equal(t, http.StatusNotFound, res.Code)
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)

	res = h.do(http.MethodDelete, "/v1/accounts", nil)
	require.Equal(t, http.StatusMethodNotAllowed, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.ContentType, res.Header().Get("Content-Type"))
}

// TestMalformedBodyIsValidationFailed: the generated binder's failure is
// rendered through the same contract and never echoes the body.
func TestMalformedBodyIsValidationFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)

	res := h.do(http.MethodPost, "/v1/intents", `{"account_id": `, "Idempotency-Key", "malformed-key-1")
	require.Equal(t, http.StatusBadRequest, res.Code)
	problem := res.problem()
	assert.Equal(t, errs.CodeValidationFailed, problem.Code)
	assert.NotContains(t, res.Body.String(), `"account_id": `)
}

// TestTransportTimeoutIsNotAFailure: a cancelled request context must never
// become a definitive rejection. It answers PROVIDER_UNAVAILABLE, which tells
// the client to observe state rather than assume nothing happened.
func TestTransportTimeoutIsNotAFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"deadline exceeded", context.DeadlineExceeded},
		{"cancelled", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			p := customerPrincipal()
			h.as(&p)
			h.ports.intents.err = tc.err

			res := h.do(http.MethodPost, "/v1/intents", map[string]any{
				"account_id":    testAccountID.String(),
				"instrument_id": testInstrument.String(),
				"action":        "ACQUIRE_NOTIONAL",
				"notional_usd":  "100.00",
				"mode":          "PAPER",
			}, "Idempotency-Key", "timeout-key-0001")

			require.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
			p2 := res.problem()
			assert.Equal(t, errs.CodeProviderUnavailable, p2.Code)
			assert.NotContains(t, strings.ToLower(p2.Detail), "reject")
			assert.NotContains(t, strings.ToLower(p2.Detail), "fail")
		})
	}
}

// --- tenant scoping -----------------------------------------------------------

func TestCrossTenantAccessIsRefused(t *testing.T) {
	t.Parallel()
	paths := []string{
		"/v1/accounts/" + testOtherAcct.String(),
		"/v1/accounts/" + testOtherAcct.String() + "/buying-power",
		"/v1/accounts/" + testOtherAcct.String() + "/holdings",
		"/v1/accounts/" + testOtherAcct.String() + "/ledger/transactions",
		"/v1/accounts/" + testOtherAcct.String() + "/activity",
		"/v1/accounts/" + testOtherAcct.String() + "/export",
		"/v1/intents?account_id=" + testOtherAcct.String(),
		"/v1/orders?account_id=" + testOtherAcct.String(),
		"/v1/funding/deposits?account_id=" + testOtherAcct.String(),
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			p := customerPrincipal()
			h.as(&p)
			res := h.do(http.MethodGet, path, nil)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)
		})
	}
}

// TestRecordScopingUsesTheRecordsOwnAccount: fetching an intent, order or
// deposit by its own id is still tenant scoped, using the account stored on the
// record rather than anything the client supplied.
func TestRecordScopingUsesTheRecordsOwnAccount(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"/v1/intents/" + testIntentID.String(),
		"/v1/orders/" + testOrderID.String(),
		"/v1/funding/deposits/" + testDepositID.String(),
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			// The record belongs to testAccountID; the caller owns another.
			p := customerPrincipal()
			p.AccountIDs = []string{testOtherAcct.String()}
			h.as(&p)
			res := h.do(http.MethodGet, path, nil)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		})
	}
}

// --- commands and idempotency (PART 36) ---------------------------------------

func TestSubmitIntent_AcceptedThenReplayed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)

	body := map[string]any{
		"account_id":    testAccountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"notional_usd":  "100.00",
		"mode":          "PAPER",
	}
	const key = "user-action-key-0001"

	first := h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusAccepted, first.Code, "body=%s", first.Body.String())
	var accepted api.TradeIntent
	first.json(&accepted)
	assert.Equal(t, testIntentID.String(), accepted.Id.String())
	assert.Equal(t, 1, h.ports.intents.submitCount())

	// A replay returns the original result with the spec's replay status and
	// does not re-execute the command.
	second := h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusOK, second.Code, "body=%s", second.Body.String())
	var replayed api.TradeIntent
	second.json(&replayed)
	assert.Equal(t, accepted, replayed, "a replay reproduces the original response")
	assert.Equal(t, 1, h.ports.intents.submitCount(), "a replay must not re-execute the command")
}

func TestIdempotencyKeyReuseWithADifferentBodyIsAConflict(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	const key = "user-action-key-0002"

	body := map[string]any{
		"account_id":    testAccountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"notional_usd":  "100.00",
		"mode":          "PAPER",
	}
	require.Equal(t, http.StatusAccepted, h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key).Code)

	body["notional_usd"] = "500.00"
	res := h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, res.problem().Code)
	assert.Equal(t, 1, h.ports.intents.submitCount(), "a conflicting reuse never executes")
}

// TestRecordedRejectionIsReplayedNotReExecuted: a business rejection is a
// conclusion. Replaying its key reproduces the same problem instead of running
// the command a second time.
func TestRecordedRejectionIsReplayedNotReExecuted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	h.ports.intents.err = errs.New(errs.CodeInsufficientBuyingPower, "not enough").
		WithField("available", "10.00")

	body := map[string]any{
		"account_id":    testAccountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"notional_usd":  "100.00",
		"mode":          "PAPER",
	}
	const key = "rejected-key-000001"

	first := h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusUnprocessableEntity, first.Code)
	assert.Equal(t, errs.CodeInsufficientBuyingPower, first.problem().Code)

	// Even if the domain would now succeed, the recorded conclusion stands.
	h.ports.intents.err = nil
	second := h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusUnprocessableEntity, second.Code)
	p2 := second.problem()
	assert.Equal(t, errs.CodeInsufficientBuyingPower, p2.Code)
	assert.Equal(t, "10.00", p2.Fields["available"])
	assert.Equal(t, 0, h.ports.intents.submitCount())
}

// TestInternalFailureIsNotRecordedAsAConclusion: an internal error is not an
// outcome, so the same key may be retried and can still succeed.
func TestInternalFailureIsNotRecordedAsAConclusion(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	h.ports.intents.err = errors.New("transient database blip")

	body := map[string]any{
		"account_id":    testAccountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"notional_usd":  "100.00",
		"mode":          "PAPER",
	}
	const key = "retryable-key-00001"

	require.Equal(t, http.StatusInternalServerError,
		h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key).Code)

	h.ports.intents.err = nil
	res := h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key)
	require.Equal(t, http.StatusAccepted, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 1, h.ports.intents.submitCount())
}

func TestIdempotencyKeyIsValidated(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"account_id":    testAccountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"notional_usd":  "100.00",
		"mode":          "PAPER",
	}
	for _, key := range []string{"short", strings.Repeat("k", 129)} {
		t.Run(key[:5], func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			p := customerPrincipal()
			h.as(&p)
			res := h.do(http.MethodPost, "/v1/intents", body, "Idempotency-Key", key)
			require.Equal(t, http.StatusBadRequest, res.Code)
			assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
		})
	}
}

func TestMissingIdempotencyKeyIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	res := h.do(http.MethodPost, "/v1/intents", map[string]any{
		"account_id":    testAccountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"mode":          "PAPER",
	})
	require.Equal(t, http.StatusBadRequest, res.Code)
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
	assert.Equal(t, 0, h.ports.intents.submitCount())
}

// --- withdrawals, gates and kill switches ------------------------------------

// TestWithdrawalRefusedByTheCapabilityGate: the gate is DISABLED in every
// environment, so the only honest answer is a 422 with the machine-readable
// code the client can explain (PART 94).
func TestWithdrawalRefusedByTheCapabilityGate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	h.ports.withdrawals.err = errs.New(errs.CodeCapabilityNotApproved,
		"the WITHDRAWALS capability is not active in this environment")

	res := h.do(http.MethodPost, "/v1/withdrawals", map[string]any{
		"account_id":          testAccountID.String(),
		"asset_id":            testAssetID.String(),
		"quantity":            "1000",
		"destination_address": "SoL1111111111111111111111111111111111111111",
	}, "Idempotency-Key", "withdrawal-key-01")

	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeCapabilityNotApproved, res.problem().Code)
}

func TestWithdrawalRequiresStepUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	p.AMR = []string{"pwd"} // no strong factor
	h.as(&p)

	res := h.do(http.MethodPost, "/v1/withdrawals", map[string]any{
		"account_id":          testAccountID.String(),
		"asset_id":            testAssetID.String(),
		"quantity":            "1000",
		"destination_address": "SoL1111111111111111111111111111111111111111",
	}, "Idempotency-Key", "withdrawal-key-02")

	require.Equal(t, http.StatusForbidden, res.Code)
	assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
}

func TestKillSwitchActivationDoesNotRequireStepUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := operatorPrincipal()
	p.AuthTime = testNow.Add(-2 * stepUpMaxAge) // stale strong authentication
	h.as(&p)

	res := h.do(http.MethodPost, "/v1/admin/kill-switches", map[string]any{
		"kind":   "GLOBAL_NEW_RISK_KILL",
		"action": "activate",
		"reason": "incident 42: stop new risk",
	}, "Idempotency-Key", "kill-activate-001")

	require.Equal(t, http.StatusOK, res.Code, "activation must never wait for a step-up; body=%s", res.Body.String())
	assert.Equal(t, 1, h.ports.kill.activated)
	var out api.KillSwitch
	res.json(&out)
	assert.True(t, out.Active)
}

func TestGateActionRequiresAReason(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := operatorPrincipal()
	h.as(&p)

	res := h.do(http.MethodPost, "/v1/admin/gates/LIVE_FUNDING/propose",
		map[string]any{"reason": "short"}, "Idempotency-Key", "gate-key-00000001")
	require.Equal(t, http.StatusBadRequest, res.Code)
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
}

func TestAccountStatusChangeRequiresAReason(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := operatorPrincipal()
	h.as(&p)

	res := h.do(http.MethodPost, "/v1/admin/accounts/"+testAccountID.String()+"/status",
		map[string]any{"to": "FROZEN", "reason": "x"}, "Idempotency-Key", "acct-status-00001")
	require.Equal(t, http.StatusBadRequest, res.Code)
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
	assert.Empty(t, h.ports.accounts.transitions)
}

func TestAccountStatusChangeRecordsActorAndReason(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := operatorPrincipal()
	h.as(&p)

	res := h.do(http.MethodPost, "/v1/admin/accounts/"+testAccountID.String()+"/status",
		map[string]any{"to": "FROZEN", "reason": "compliance hold 12345"},
		"Idempotency-Key", "acct-status-00002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.Len(t, h.ports.accounts.transitions, 1)
	ch := h.ports.accounts.transitions[0]
	assert.Equal(t, accounts.StatusFrozen, ch.To)
	assert.Equal(t, "compliance hold 12345", ch.Reason)
	assert.Equal(t, string(security.ActorOperator), ch.ActorType)
	assert.Equal(t, p.SubjectID, ch.ActorID)
}

// --- quotes -------------------------------------------------------------------

func TestQuotePreviewWithoutAnAdapterIsProviderUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	// Remove the adapter the way an unconfigured deployment would.
	ports := h.ports.ports()
	ports.Quotes = nil
	srv, err := New(Options{
		Env: h.server.opts.Env, Clock: h.server.clk, Ports: ports,
		Authenticator: h.server.opts.Authenticator,
	})
	require.NoError(t, err)
	h.server = srv

	res := h.do(http.MethodPost, "/v1/quotes/preview", map[string]any{
		"account_id":    testAccountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"notional_usd":  "100.00",
	})
	require.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeProviderUnavailable, res.problem().Code)
}

func TestQuotePreviewDisclosesEveryFeeAsAString(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)

	res := h.do(http.MethodPost, "/v1/quotes/preview", map[string]any{
		"account_id":    testAccountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"notional_usd":  "100.00",
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	for _, field := range []string{
		"input_quantity", "expected_output", "minimum_output",
		"venue_fee", "network_fee_estimate", "platform_fee", "total_estimated_cost_usd",
		"effective_price",
	} {
		assert.IsType(t, "", raw[field], "%s must be an exact string", field)
	}
	assert.Equal(t, "100.51", raw["total_estimated_cost_usd"])
	assert.Equal(t, "JUPITER", raw["venue"])
	assert.Equal(t, "1.000000", raw["effective_price"], "effective_price is a bare decimal")
}

func TestBadMoneyStringsAreRejected(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, field, value string }{
		{"too many decimals", "notional_usd", "100.555"},
		{"exponent notional", "notional_usd", "1e2"},
		{"non numeric notional", "notional_usd", "abc"},
		{"empty notional", "notional_usd", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			p := customerPrincipal()
			h.as(&p)
			res := h.do(http.MethodPost, "/v1/quotes/preview", map[string]any{
				"account_id":    testAccountID.String(),
				"instrument_id": testInstrument.String(),
				"action":        "ACQUIRE_NOTIONAL",
				tc.field:        tc.value,
			})
			require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
		})
	}
}

// --- auth flows ---------------------------------------------------------------

func TestLoginRedirectsToTheIdentityProvider(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.as(nil)
	res := h.do(http.MethodGet, "/v1/auth/login?step_up=true", nil)
	require.Equal(t, http.StatusFound, res.Code)
	assert.Equal(t, "https://idp.test/authorize?state=abc", res.Header().Get("Location"))

	// No SESSION exists before the callback. The login-state cookie does, and
	// must: it is what binds the flow to this browser (F-87). Asserting
	// "no cookies at all" would now be asserting the absence of the control.
	for _, c := range res.Result().Cookies() {
		assert.NotEqual(t, "cp_session", c.Name, "no session exists before the callback")
	}
	state := loginStateCookie(t, res)
	require.NotNil(t, state, "the login redirect must bind the flow to this browser")
	assert.True(t, state.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, state.SameSite,
		"Strict would not be sent on the top-level GET the identity provider redirects to")
	assert.NotContains(t, state.Value, "abc", "the cookie carries a digest, not the state itself")
}

func TestCallbackSetsAnHttpOnlySessionCookie(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.as(nil)

	// The flow begins in this browser, which is what the callback requires.
	begin := h.do(http.MethodGet, "/v1/auth/login", nil)
	require.Equal(t, http.StatusFound, begin.Code)

	res := h.doWithCookies(http.MethodGet, "/v1/auth/callback?code=abc&state=abc", nil,
		begin.Result().Cookies())
	require.Equal(t, http.StatusFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "/portfolio", res.Header().Get("Location"))

	session := namedCookie(res, "cp_session")
	require.NotNil(t, session)
	assert.True(t, session.HttpOnly, "the session cookie must be HttpOnly")
	assert.Equal(t, http.SameSiteLaxMode, session.SameSite)
	assert.Equal(t, "raw-session-token-value", session.Value)
	assert.NotContains(t, res.Body.String(), "raw-session-token-value",
		"the raw token leaves only in the cookie")

	cleared := namedCookie(res, httpmw.LoginStateCookieName)
	require.NotNil(t, cleared, "a completed flow clears the state cookie")
	assert.Negative(t, cleared.MaxAge)
}

// TestAPlantedCallbackDoesNotSignAnybodyIn is the exploit (F-87).
//
// `state` was a server-side lookup key consumed once, which stops a callback
// being REPLAYED and does nothing about one being PLANTED. The attacker starts
// a flow in their own browser, authenticates as themselves, keeps
// code=C&state=S without following the redirect, and induces the victim's
// browser to navigate to the callback. The server used to find the attempt,
// exchange the code and set a session cookie -- in the victim's browser, for
// the attacker's subject. Everything the victim then did landed in the
// attacker's account, attributed to the attacker.
func TestAPlantedCallbackDoesNotSignAnybodyIn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.as(nil)

	// The attacker's browser begins a flow and keeps the callback parameters.
	attacker := h.do(http.MethodGet, "/v1/auth/login", nil)
	require.Equal(t, http.StatusFound, attacker.Code)
	require.NotNil(t, loginStateCookie(t, attacker))

	// The victim's browser follows the planted link. It has no login-state
	// cookie, because the flow did not begin here.
	victim := h.do(http.MethodGet, "/v1/auth/callback?code=abc&state=abc", nil)
	require.Equal(t, http.StatusUnauthorized, victim.Code,
		"a callback that did not begin in this browser signed somebody in; body=%s", victim.Body.String())
	assert.Nil(t, namedCookie(victim, "cp_session"), "no session may be established")

	// A cookie for a DIFFERENT state is no better than none.
	wrong := &http.Cookie{Name: httpmw.LoginStateCookieName, Value: httpmw.LoginStateDigest("some-other-state")}
	res := h.doWithCookies(http.MethodGet, "/v1/auth/callback?code=abc&state=abc", nil, []*http.Cookie{wrong})
	require.Equal(t, http.StatusUnauthorized, res.Code)
	assert.Nil(t, namedCookie(res, "cp_session"))

	// The control: the browser that began the flow still completes it. Without
	// this the three refusals above could be a login that no longer works.
	ok := h.doWithCookies(http.MethodGet, "/v1/auth/callback?code=abc&state=abc", nil, attacker.Result().Cookies())
	require.Equal(t, http.StatusFound, ok.Code, "body=%s", ok.Body.String())
	require.NotNil(t, namedCookie(ok, "cp_session"))
}

func namedCookie(res *response, name string) *http.Cookie {
	for _, c := range res.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func loginStateCookie(t *testing.T, res *response) *http.Cookie {
	t.Helper()
	return namedCookie(res, httpmw.LoginStateCookieName)
}

func TestLogoutRevokesAndClearsTheCookie(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	res := h.do(http.MethodPost, "/v1/auth/logout", nil)
	require.Equal(t, http.StatusNoContent, res.Code)
	cookies := res.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Empty(t, cookies[0].Value)
	assert.Less(t, cookies[0].MaxAge, 0)
}

func TestSessionRevocationIsScopedToTheCaller(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)

	res := h.do(http.MethodDelete, "/v1/sessions/"+testSessionID, nil)
	require.Equal(t, http.StatusNoContent, res.Code)
	assert.Equal(t, []string{testSessionID}, h.ports.sessions.revoked)

	other := "0193b2e0-0000-7000-8000-0000000000ff"
	res = h.do(http.MethodDelete, "/v1/sessions/"+other, nil)
	require.Equal(t, http.StatusNotFound, res.Code)
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)
	assert.Len(t, h.ports.sessions.revoked, 1, "another subject's session is never revoked")
}

// --- webhooks -----------------------------------------------------------------

// TestWebhookReceivesTheRawBody: a provider signature is verified over the
// exact bytes, so the ingestion pipeline must see them, not a re-encoding.
func TestWebhookReceivesTheRawBody(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.as(nil)
	const raw = `{"id":"evt_1","type":"crypto.onramp_session.updated","data":{"object":{"id":"cos_1"}}}`

	res := h.do(http.MethodPost, "/v1/webhooks/stripe", raw, "Stripe-Signature", "t=1,v1=deadbeef")
	require.Equal(t, http.StatusOK, res.Code)
	assert.Equal(t, 1, h.ports.webhook.seen)
	assert.Equal(t, raw, string(h.ports.webhook.raw), "the pipeline must see the exact bytes")
	assert.Empty(t, res.Body.String(), "an acknowledgement tells a provider nothing about internal state")
}

func TestWebhookForAnUnknownProviderIsNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.as(nil)
	res := h.do(http.MethodPost, "/v1/webhooks/stripe", `{"id":"evt"}`)
	require.Equal(t, http.StatusOK, res.Code)

	h.ports.webhook.status = http.StatusBadRequest
	res = h.do(http.MethodPost, "/v1/webhooks/stripe", `{"id":"evt"}`)
	assert.Equal(t, http.StatusBadRequest, res.Code)
}

// --- transport policy ----------------------------------------------------------

func TestSecureHeadersAndRequestIDArePresent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	res := h.do(http.MethodGet, "/v1/accounts", nil)
	assert.Equal(t, "nosniff", res.Header().Get("X-Content-Type-Options"))
	assert.NotEmpty(t, res.Header().Get("X-Request-Id"))
}

func TestClientSuppliedRequestIDIsEchoedAndUsedInProblems(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	h.ports.accounts.err = errs.New(errs.CodeNotFound, "gone")

	const rid = "req-0123456789abcdef"
	res := h.do(http.MethodGet, "/v1/accounts", nil, "X-Request-Id", rid)
	assert.Equal(t, rid, res.Header().Get("X-Request-Id"))
	assert.Equal(t, rid, res.problem().RequestID)
}

func TestCORSPreflightOnlyAnswersAllowedOrigins(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.as(nil)

	res := h.do(http.MethodOptions, "/v1/accounts", nil,
		"Origin", "https://app.test", "Access-Control-Request-Method", "GET")
	require.Equal(t, http.StatusNoContent, res.Code)
	assert.Equal(t, "https://app.test", res.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", res.Header().Get("Access-Control-Allow-Credentials"))

	res = h.do(http.MethodOptions, "/v1/accounts", nil,
		"Origin", "https://evil.test", "Access-Control-Request-Method", "GET")
	require.Equal(t, http.StatusForbidden, res.Code)
	assert.Empty(t, res.Header().Get("Access-Control-Allow-Origin"))
}

// TestCSRFAppliesOnlyToCookieAuthenticatedRequests: a browser request carrying
// the session cookie is checked; the signature-verified webhook, which carries
// no ambient authority, is not.
func TestCSRFAppliesOnlyToCookieAuthenticatedRequests(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)

	res := h.do(http.MethodPost, "/v1/intents", map[string]any{
		"account_id":    testAccountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"notional_usd":  "100.00",
		"mode":          "PAPER",
	}, "Idempotency-Key", "csrf-key-00000001",
		"Cookie", "cp_session=token", "Origin", "https://evil.test")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 0, h.ports.intents.submitCount())

	// The same request from the allowed origin passes.
	ok := h.do(http.MethodPost, "/v1/intents", map[string]any{
		"account_id":    testAccountID.String(),
		"instrument_id": testInstrument.String(),
		"action":        "ACQUIRE_NOTIONAL",
		"notional_usd":  "100.00",
		"mode":          "PAPER",
	}, "Idempotency-Key", "csrf-key-00000002",
		"Cookie", "cp_session=token", "Origin", "https://app.test")
	require.Equal(t, http.StatusAccepted, ok.Code, "body=%s", ok.Body.String())

	// A webhook with no cookie is not subject to the check.
	wh := h.do(http.MethodPost, "/v1/webhooks/stripe", `{"id":"evt"}`, "Origin", "https://evil.test")
	assert.Equal(t, http.StatusOK, wh.Code)
}

func TestOversizedBodyIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	srv, err := New(Options{
		Env: h.server.opts.Env, Clock: h.server.clk, MaxBodyBytes: 32,
		Authenticator: h.server.opts.Authenticator, Ports: h.ports.ports(),
	})
	require.NoError(t, err)
	h.server = srv

	res := h.do(http.MethodPost, "/v1/intents", strings.Repeat("x", 4096), "Idempotency-Key", "big-body-000001")
	// 413, not 400: the answer is given before the body is read, so there is
	// nothing to validate. A configured maximum below the route's own limit
	// lowers it -- the two are a minimum, not a choice (F-85).
	require.Equal(t, http.StatusRequestEntityTooLarge, res.Code)
	assert.Equal(t, errs.CodeBodyTooLarge, res.problem().Code)
}

// --- SSE ----------------------------------------------------------------------

// TestEventStreamIsHandedToTheStreamPackage proves the endpoint reaches
// internal/stream with the live request, so heartbeats, replay and per-client
// cleanup are that package's real behavior rather than a reimplementation.
func TestEventStreamIsHandedToTheStreamPackage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)

	seen := make(chan string, 1)
	h.ports.stream = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("id: 1\nevent: resync\ndata: {}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	srv, err := New(Options{
		Env: h.server.opts.Env, Clock: h.server.clk,
		Authenticator: h.server.opts.Authenticator, Ports: h.ports.ports(),
	})
	require.NoError(t, err)
	h.server = srv

	res := h.do(http.MethodGet, "/v1/events/stream", nil, "Last-Event-ID", "42")
	require.Equal(t, http.StatusOK, res.Code)
	assert.Equal(t, "text/event-stream", res.Header().Get("Content-Type"))
	assert.Contains(t, res.Body.String(), "event: resync")
	select {
	case got := <-seen:
		assert.Equal(t, "42", got, "Last-Event-ID must reach the stream package")
	case <-time.After(time.Second):
		t.Fatal("the stream handler was never invoked")
	}
}

// TestEventStreamEndsWhenTheProcessDrains: an SSE connection never completes on
// its own, so StopStreams must end it or a graceful shutdown would block.
func TestEventStreamEndsWhenTheProcessDrains(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)

	started := make(chan struct{})
	h.ports.stream = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		close(started)
		<-r.Context().Done() // the real stream.Handler does exactly this
	})
	srv, err := New(Options{
		Env: h.server.opts.Env, Clock: h.server.clk,
		Authenticator: h.server.opts.Authenticator, Ports: h.ports.ports(),
	})
	require.NoError(t, err)
	h.server = srv

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.do(http.MethodGet, "/v1/events/stream", nil)
	}()
	<-started
	h.server.StopStreams()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not end when the process started draining")
	}
}

// --- not-wired ports -----------------------------------------------------------

// TestUnwiredPortsAnswerUnsupported: a deployment without a service answers
// with a stable code, never with a fabricated result and never with a 500.
func TestUnwiredPortsAnswerUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := operatorPrincipal()
	h.as(&p)
	srv, err := New(Options{
		Env: h.server.opts.Env, Clock: h.server.clk,
		Authenticator: h.server.opts.Authenticator,
		Ports:         Ports{Idempotency: h.ports.idem},
	})
	require.NoError(t, err)
	h.server = srv

	for _, path := range []string{
		"/v1/accounts",
		"/v1/assets",
		"/v1/instruments",
		"/v1/admin/gates",
		"/v1/admin/kill-switches",
		"/v1/admin/reconciliation/records",
	} {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", path, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, path)
	}
}

// --- export -------------------------------------------------------------------

func TestExportRendersJSONAndCSV(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)

	res := h.do(http.MethodGet, "/v1/accounts/"+testAccountID.String()+"/export?format=json", nil)
	require.Equal(t, http.StatusOK, res.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, testAccountID.String(), out["account_id"])

	res = h.do(http.MethodGet, "/v1/accounts/"+testAccountID.String()+"/export?format=csv", nil)
	require.Equal(t, http.StatusOK, res.Code)
	assert.Equal(t, "text/csv", res.Header().Get("Content-Type"))
	assert.Equal(t, "\"a\"\r\n", res.Body.String())
}

func TestExportRejectsAnInvertedWindow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	res := h.do(http.MethodGet,
		"/v1/accounts/"+testAccountID.String()+"/export?from=2026-09-06T00:00:00Z&to=2026-09-05T00:00:00Z", nil)
	require.Equal(t, http.StatusBadRequest, res.Code)
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
}

// TestEventStream_RealStreamPackageOverARealConnection drives the SSE endpoint
// end to end over a real HTTP connection with the real internal/stream handler:
// the framing, the heartbeat, the per-write flush and the per-client cleanup on
// disconnect are that package's behavior, reached through the whole middleware
// chain and the generated strict server.
func TestEventStream_RealStreamPackageOverARealConnection(t *testing.T) {
	t.Parallel()
	hub := stream.NewHub(16, slog.New(slog.DiscardHandler))
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	h.ports.stream = stream.NewHandler(hub, 60*time.Millisecond, nil)
	srv, err := New(Options{
		Env: config.EnvTest, Clock: clock.NewFake(testNow),
		Authenticator: h.server.opts.Authenticator, Ports: h.ports.ports(),
	})
	require.NoError(t, err)
	h.server = srv

	ts := httptest.NewServer(srv.Router())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/events/stream", nil)
	require.NoError(t, err)
	res, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "text/event-stream", res.Header.Get("Content-Type"))
	require.Equal(t, "no-cache", res.Header.Get("Cache-Control"))
	require.Equal(t, "no", res.Header.Get("X-Accel-Buffering"))

	reader := bufio.NewReader(res.Body)
	lines := make(chan string, 64)
	readErr := make(chan error, 1)
	go func() {
		for {
			line, rerr := reader.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if rerr != nil {
				readErr <- rerr
				return
			}
		}
	}()

	// A published event reaches this client, flushed immediately.
	published := hub.Publish(stream.Event{
		Type:       stream.TypeOrderTransitioned,
		OccurredAt: testNow,
		ResourceID: testOrderID.String(),
		AccountID:  testAccountID.String(),
	})
	require.NotZero(t, published.ID)

	var sawEvent, sawHeartbeat bool
	deadline := time.After(5 * time.Second)
	for !sawEvent || !sawHeartbeat {
		select {
		case line := <-lines:
			switch {
			case strings.HasPrefix(line, "event: order.transitioned"):
				sawEvent = true
			case strings.HasPrefix(line, ": keepalive"):
				sawHeartbeat = true
			}
		case err := <-readErr:
			t.Fatalf("stream ended early: %v", err)
		case <-deadline:
			t.Fatalf("stream produced neither an event (%v) nor a heartbeat (%v) in time", sawEvent, sawHeartbeat)
		}
	}

	// Draining ends the connection and unsubscribes the client.
	srv.StopStreams()
	select {
	case <-readErr:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end when the process started draining")
	}
}

// TestEventStreamRefusesAnAgent: the realtime surface is for humans; an agent
// consumes the bus, not the customer stream.
func TestEventStreamRefusesAnAgent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	agent := agentPrincipal()
	h.as(&agent)
	res := h.do(http.MethodGet, "/v1/events/stream", nil)
	require.Equal(t, http.StatusForbidden, res.Code)
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)
}

// TestAccessLogCarriesIdentifiersAndNoSecrets: the access log must name the
// route, the outcome and who made the request, and must never contain a
// cookie, an authorization header, a query string or a request body.
func TestAccessLogCarriesIdentifiersAndNoSecrets(t *testing.T) {
	t.Parallel()
	var buf lockedBuffer
	fx := newFixtures()
	p := customerPrincipal()
	holder := &p
	srv, err := New(Options{
		Env: config.EnvTest, Clock: clock.NewFake(testNow), CookieName: "cp_session",
		Logger: observability.NewLogger(config.EnvTest, &buf),
		Authenticator: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(security.WithPrincipal(r.Context(), *holder)))
			})
		},
		Ports: fx.ports(),
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/accounts/"+testAccountID.String()+"?secret_query=hunter2", nil)
	req.Header.Set("Cookie", "cp_session=super-secret-token")
	req.Header.Set("Authorization", "Bearer sk_live_abc123")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	logged := buf.String()
	require.Contains(t, logged, `"msg":"http request"`)
	assert.Contains(t, logged, `"route":"/v1/accounts/{accountId}"`)
	assert.Contains(t, logged, `"status":200`)
	assert.Contains(t, logged, `"actor_type":"USER"`)
	assert.Contains(t, logged, `"subject_id":"`+testUserID.String()+`"`)
	assert.Contains(t, logged, `"request_id":"`)
	for _, secret := range []string{"super-secret-token", "sk_live_abc123", "hunter2", "Authorization", "Cookie"} {
		assert.NotContains(t, logged, secret, "the access log leaked %q", secret)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestClientIPIsAPlainAddress: internal/audit refuses anything but a bare IP,
// so an audited login, logout or provider record must never receive a port, a
// zone or a rate-limiter key prefix. This is the defect an end-to-end run of
// the real binary found: the login callback answered 400
// "audit: source_ip must be a plain IP address" because the value carried the
// limiter's "ip:" namespace.
func TestClientIPIsAPlainAddress(t *testing.T) {
	t.Parallel()
	_, loopback, err := net.ParseCIDR("127.0.0.0/8")
	require.NoError(t, err)

	cases := []struct {
		name       string
		remoteAddr string
		forwarded  string
		trusted    []*net.IPNet
		want       string
	}{
		{"ipv4 with port", "127.0.0.1:54321", "", nil, "127.0.0.1"},
		{"ipv4 bare", "10.1.2.3", "", nil, "10.1.2.3"},
		{"ipv6 with port", "[2001:db8::1]:443", "", nil, "2001:db8::1"},
		{"ipv6 bare", "2001:db8::1", "", nil, "2001:db8::1"},
		// A zone is link-local routing detail, not identity; it is
		// stripped so internal/audit accepts the address.
		{"ipv6 zone is stripped", "fe80::1%eth0", "", nil, "fe80::1"},
		{"garbage is refused", "not-an-address", "", nil, ""},
		{"empty is empty", "", "", nil, ""},
		{"forwarded ignored without trust", "203.0.113.9:80", "198.51.100.7", nil, "203.0.113.9"},
		{"forwarded ignored from an untrusted peer", "203.0.113.9:80", "198.51.100.7", []*net.IPNet{loopback}, "203.0.113.9"},
		{"forwarded honored from a trusted peer", "127.0.0.1:80", "198.51.100.7", []*net.IPNet{loopback}, "198.51.100.7"},
		{"leftmost forwarded entry wins", "127.0.0.1:80", "198.51.100.7, 10.0.0.1", []*net.IPNet{loopback}, "198.51.100.7"},
		{"malformed forwarded falls back", "127.0.0.1:80", "nonsense", []*net.IPNet{loopback}, "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			got := clientIP(r, tc.trusted)
			assert.Equal(t, tc.want, got)
			if got != "" {
				// The value must satisfy what internal/audit accepts.
				addr, perr := netip.ParseAddr(got)
				require.NoError(t, perr, "audit requires a parsable address")
				assert.Empty(t, addr.Zone(), "audit refuses a zone")
				assert.False(t, strings.HasPrefix(got, "ip:"), "no limiter namespace")
				if addr.Is4() {
					assert.NotContains(t, got, ":", "an audited IPv4 address carries no port")
				}
				assert.Equal(t, got, addr.String(), "the value is already canonical")
			}
		})
	}
}

// TestNonSpecRoutesAreRefusedOutsideDevelopmentEnvironments: a route mounted
// outside the /v1 contract bypasses the per-operation authorization table, so
// the server refuses to build one where real money is at stake.
func TestNonSpecRoutesAreRefusedOutsideDevelopmentEnvironments(t *testing.T) {
	t.Parallel()
	routes := map[string]http.Handler{
		"/auth/dev/login": http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	}
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		_, err := New(Options{Env: env, Ports: Ports{}, NonSpecRoutes: routes})
		require.Error(t, err, "%s", env)
		assert.Contains(t, err.Error(), "/auth/dev/login")
		assert.Contains(t, err.Error(), string(env))
	}
	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev} {
		srv, err := New(Options{Env: env, Ports: Ports{}, NonSpecRoutes: routes})
		require.NoError(t, err, "%s", env)
		assert.NotNil(t, srv)
	}
}

// TestNonSpecRouteIsServedThroughTheSameMiddlewareChain: a mounted dev route
// still gets the request id, the secure headers and panic recovery, and it
// cannot shadow a /v1 route.
func TestNonSpecRouteIsServedThroughTheSameMiddlewareChain(t *testing.T) {
	t.Parallel()
	fx := newFixtures()
	srv, err := New(Options{
		Env: config.EnvLocal, Clock: clock.NewFake(testNow), Ports: fx.ports(),
		NonSpecRoutes: map[string]http.Handler{
			"/auth/dev/login": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte("<html>picker</html>"))
			}),
			"/auth/dev/panic": http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				panic("dev handler exploded")
			}),
		},
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/dev/login", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "picker")
	assert.NotEmpty(t, rec.Header().Get("X-Request-Id"), "the dev route is inside the id middleware")
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))

	// Recovery still applies.
	rec = httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/dev/panic", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, errs.CodeInternal, (&response{ResponseRecorder: rec, t: t}).problem().Code)
	assert.NotContains(t, rec.Body.String(), "exploded")

	// The v1 surface is untouched and still fails closed.
	rec = httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/accounts", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
