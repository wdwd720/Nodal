package errs_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

const secretCause = "pq: password authentication failed for user cp_app at 10.0.0.7"

func TestToProblem_BusinessError(t *testing.T) {
	t.Parallel()
	err := errs.New(errs.CodeInsufficientBuyingPower, "need 100.00 USD, have 42.00 USD").
		WithField("required", "100.00").
		WithField("available", "42.00")
	p := errs.ToProblem(err, "/v1/intents", "req-1")

	require.Equal(t, errs.Problem{
		Type:      "urn:problem:insufficient_buying_power",
		Title:     "Insufficient buying power",
		Status:    http.StatusUnprocessableEntity,
		Detail:    "need 100.00 USD, have 42.00 USD",
		Instance:  "/v1/intents",
		Code:      errs.CodeInsufficientBuyingPower,
		Fields:    map[string]any{"required": "100.00", "available": "42.00"},
		RequestID: "req-1",
	}, p)

	// Fields are copied, not aliased.
	p.Fields["required"] = "changed"
	require.Equal(t, "100.00", err.Fields["required"])
}

func TestToProblem_EmptyDetailFallsBackToTitle(t *testing.T) {
	t.Parallel()
	p := errs.ToProblem(errs.New(errs.CodeKillSwitchActive, ""), "", "")
	require.Equal(t, "Kill switch active", p.Detail)
	require.Empty(t, p.Instance)
	require.Empty(t, p.RequestID)
}

func TestToProblem_RedactsInternal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain error", errors.New(secretCause)},
		{"fmt wrapped plain", fmt.Errorf("query: %w", errors.New(secretCause))},
		{"errs INTERNAL with cause", errs.Wrap(errors.New(secretCause), errs.CodeInternal, "db down: "+secretCause)},
		{"errs INTERNAL with fields", errs.New(errs.CodeInternal, secretCause).WithField("dsn", secretCause)},
		{"errs INTERNAL with retry", errs.New(errs.CodeInternal, secretCause).WithRetryAfter(time.Second)},
		{"unregistered code", errs.New("TYPO_CODE", secretCause).WithField("x", secretCause)},
		{"empty code", &errs.Error{Detail: secretCause}},
		{"typed nil *Error", (*errs.Error)(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := errs.ToProblem(tc.err, "/v1/x", "req-9")
			require.Equal(t, errs.Problem{
				Type:      "urn:problem:internal",
				Title:     "Internal error",
				Status:    http.StatusInternalServerError,
				Detail:    "internal error",
				Instance:  "/v1/x",
				Code:      errs.CodeInternal,
				RequestID: "req-9",
			}, p)
			raw, err := json.Marshal(p)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "cp_app")
			require.NotContains(t, string(raw), "10.0.0.7")
			require.NotContains(t, string(raw), "TYPO")
		})
	}
}

func TestToProblem_NeverExposesCauseForAnyCode(t *testing.T) {
	t.Parallel()
	for _, c := range errs.AllCodes() {
		err := errs.Wrap(errors.New(secretCause), c, "client-safe detail")
		p := errs.ToProblem(err, "/v1/x", "req")
		raw, mErr := json.Marshal(p)
		require.NoError(t, mErr)
		require.NotContains(t, string(raw), "cp_app", "code %s leaked its cause", c)
		require.Equal(t, errs.HTTPStatus(c), p.Status)
		require.Equal(t, c, p.Code)
		if c != errs.CodeInternal {
			require.Equal(t, "client-safe detail", p.Detail)
		}
	}
}

func TestToProblem_RetryAfter(t *testing.T) {
	t.Parallel()
	p := errs.ToProblem(errs.New(errs.CodeIdempotencyInProgress, ""), "", "")
	require.Equal(t, http.StatusConflict, p.Status)
	require.NotNil(t, p.RetryAfter)
	require.Equal(t, time.Second, *p.RetryAfter, "defaults to 1s")

	p = errs.ToProblem(errs.New(errs.CodeRateLimited, ""), "", "")
	require.NotNil(t, p.RetryAfter)
	require.Equal(t, time.Second, *p.RetryAfter)

	p = errs.ToProblem(errs.New(errs.CodeRateLimited, "").WithRetryAfter(30*time.Second), "", "")
	require.Equal(t, 30*time.Second, *p.RetryAfter, "explicit value wins")

	p = errs.ToProblem(errs.New(errs.CodeProviderUnavailable, "").WithRetryAfter(5*time.Second), "", "")
	require.Equal(t, 5*time.Second, *p.RetryAfter)

	p = errs.ToProblem(errs.New(errs.CodeNotFound, ""), "", "")
	require.Nil(t, p.RetryAfter)
}

func TestProblem_JSONGolden(t *testing.T) {
	t.Parallel()
	err := errs.New(errs.CodeValidationFailed, "amount must be positive").
		WithField("amount", "must be positive")
	p := errs.ToProblem(err, "/v1/orders", "req-123")
	raw, mErr := json.Marshal(p)
	require.NoError(t, mErr)
	require.Equal(t,
		`{"type":"urn:problem:validation_failed","title":"Validation failed","status":400,"detail":"amount must be positive","instance":"/v1/orders","code":"VALIDATION_FAILED","fields":{"amount":"must be positive"},"request_id":"req-123"}`,
		string(raw))

	p = errs.ToProblem(errors.New(secretCause), "/v1/orders", "req-123")
	raw, mErr = json.Marshal(p)
	require.NoError(t, mErr)
	require.Equal(t,
		`{"type":"urn:problem:internal","title":"Internal error","status":500,"detail":"internal error","instance":"/v1/orders","code":"INTERNAL","request_id":"req-123"}`,
		string(raw))

	// Minimal: no instance, request id or fields.
	p = errs.ToProblem(errs.New(errs.CodeNotFound, "no such order"), "", "")
	raw, mErr = json.Marshal(p)
	require.NoError(t, mErr)
	require.Equal(t,
		`{"type":"urn:problem:not_found","title":"Not found","status":404,"detail":"no such order","code":"NOT_FOUND"}`,
		string(raw))

	// RetryAfter never appears in the body.
	p = errs.ToProblem(errs.New(errs.CodeRateLimited, "slow down"), "", "")
	raw, mErr = json.Marshal(p)
	require.NoError(t, mErr)
	require.NotContains(t, string(raw), "retry")
}

func TestProblem_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	in := errs.ToProblem(errs.New(errs.CodeConflict, "version 3 expected").WithField("expected", 3.0), "/v1/x", "r")
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	var out errs.Problem
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, in, out)
}

func TestWriteProblem(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	p := errs.ToProblem(errs.New(errs.CodeForbidden, "no trade:create"), "/v1/orders", "req-7")
	errs.WriteProblem(rec, p)

	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	require.Equal(t, http.StatusForbidden, res.StatusCode)
	require.Equal(t, "application/problem+json", res.Header.Get("Content-Type"))
	require.Equal(t, "nosniff", res.Header.Get("X-Content-Type-Options"))
	require.Equal(t, "no-store", res.Header.Get("Cache-Control"))
	require.Empty(t, res.Header.Get("Retry-After"))

	var got errs.Problem
	require.NoError(t, json.NewDecoder(res.Body).Decode(&got))
	require.Equal(t, p, got)
}

func TestWriteProblem_RetryAfterHeader(t *testing.T) {
	t.Parallel()
	cases := []struct {
		d    time.Duration
		want string
	}{
		{time.Second, "1"},
		{2500 * time.Millisecond, "3"},
		{10 * time.Millisecond, "1"},
		{time.Minute, "60"},
	}
	for _, tc := range cases {
		t.Run(tc.d.String(), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			errs.WriteProblem(rec, errs.ToProblem(errs.New(errs.CodeIdempotencyInProgress, "").WithRetryAfter(tc.d), "", ""))
			require.Equal(t, http.StatusConflict, rec.Code)
			require.Equal(t, tc.want, rec.Header().Get("Retry-After"))
		})
	}

	rec := httptest.NewRecorder()
	errs.WriteProblem(rec, errs.ToProblem(errs.New(errs.CodeIdempotencyInProgress, ""), "", ""))
	require.Equal(t, "1", rec.Header().Get("Retry-After"), "default for IDEMPOTENCY_IN_PROGRESS")

	rec = httptest.NewRecorder()
	errs.WriteProblem(rec, errs.ToProblem(errs.New(errs.CodeRateLimited, ""), "", ""))
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "1", rec.Header().Get("Retry-After"))
}

func TestWriteProblem_ZeroStatusUsesCode(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	errs.WriteProblem(rec, errs.Problem{Code: errs.CodeNotFound, Detail: "x"})
	require.Equal(t, http.StatusNotFound, rec.Code)

	rec = httptest.NewRecorder()
	errs.WriteProblem(rec, errs.Problem{})
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestWriteProblem_UnmarshalableFieldFallsBackToInternal(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	p := errs.ToProblem(errs.New(errs.CodeValidationFailed, "bad").WithField("ch", make(chan int)), "/v1/x", "req-5")
	errs.WriteProblem(rec, p)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	body := rec.Body.String()
	require.NotContains(t, body, "chan")
	require.NotContains(t, body, "bad")
	var got errs.Problem
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Equal(t, errs.CodeInternal, got.Code)
	require.Equal(t, "internal error", got.Detail)
	require.Equal(t, "/v1/x", got.Instance)
	require.Equal(t, "req-5", got.RequestID)
}

func TestWriteError(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	errs.WriteError(rec, errors.New(secretCause), "/v1/x", "req-2")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	require.False(t, strings.Contains(rec.Body.String(), "cp_app"))
	require.Contains(t, rec.Body.String(), `"request_id":"req-2"`)
}
