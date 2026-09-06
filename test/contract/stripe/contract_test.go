// Package stripecontract holds the contract suite of the Stripe fiat-to-crypto
// onramp adapter (goal PART 148): every case runs the real net/http adapter
// against httptest fixtures built from the official object reference, and
// asserts the adapter fails safely. See README.md for which fixture fields
// are documented and which are assumed.
package stripecontract

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/stripe"
	"github.com/nodal/controlplane/internal/webhook"
)

const (
	apiKey        = "sk_test_contract"
	webhookSecret = "whsec_contract"
)

var testNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// The adapter satisfies the funding contract at compile time.
var _ funding.FundingProvider = (*stripe.Client)(nil)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

type env struct {
	t      *testing.T
	client *stripe.Client
	health *provider.Tracker
	clk    *clock.Fake
	server *httptest.Server
}

// defaultTimeout is deliberately generous. Every test here talks to an
// httptest server over the loopback interface, and under a contended `-race`
// run that round trip can take far longer than the few milliseconds it costs on
// an idle machine. A tight deadline here would not test anything — no assertion
// in this file depends on how long a *successful* call takes — it would only
// convert CPU contention into a spurious PROVIDER_UNAVAILABLE. The one test
// that needs a short deadline is TestContract_Timeout, which sets its own
// against a handler that never answers.
const (
	defaultTimeout = 10 * time.Second
	hangTimeout    = 2 * time.Second
)

func newEnv(t *testing.T, h http.HandlerFunc, opts ...func(*stripe.Options)) *env {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	clk := clock.NewFake(testNow)
	health, err := provider.NewTracker("stripe", provider.DefaultThresholds(), testNow)
	require.NoError(t, err)
	o := stripe.Options{
		Mode: config.ProviderModeSandbox, Env: config.EnvTest, BaseURL: srv.URL, APIKey: apiKey, WebhookSecret: webhookSecret,
		Timeout: defaultTimeout, Clock: clk, Health: health,
	}
	for _, fn := range opts {
		fn(&o)
	}
	c, err := stripe.NewClient(o)
	require.NoError(t, err)
	return &env{t: t, client: c, health: health, clk: clk, server: srv}
}

func createRequest() funding.CreateSessionRequest {
	return funding.CreateSessionRequest{
		IdempotencyKey: "dep-1", DestinationNetwork: "solana", DestinationCurrency: "usdc",
		WalletAddress: "So11111111111111111111111111111111111111112", LockWalletAddress: true, SourceCurrency: "usd", SourceAmount: "100.00",
		Metadata: map[string]string{"deposit_id": "dep-1"},
	}
}

func respond(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Case 1: valid response — the documented create call and object.
func TestContract_CreateSession_Valid(t *testing.T) {
	var seenKeys atomic.Int32
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/crypto/onramp_sessions", r.URL.Path)
		require.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte(apiKey+":")), r.Header.Get("Authorization"))
		require.Equal(t, "dep-1", r.Header.Get("Idempotency-Key"))
		require.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		require.NoError(t, r.ParseForm())
		require.Equal(t, "So11111111111111111111111111111111111111112", r.PostForm.Get("wallet_addresses[solana]"))
		require.Equal(t, "true", r.PostForm.Get("lock_wallet_address"))
		require.Equal(t, "solana", r.PostForm.Get("destination_network"))
		require.Equal(t, "usdc", r.PostForm.Get("destination_currency"))
		require.Equal(t, "solana", r.PostForm.Get("destination_networks[]"))
		require.Equal(t, "usdc", r.PostForm.Get("destination_currencies[]"))
		require.Equal(t, "usd", r.PostForm.Get("source_currency"))
		require.Equal(t, "100.00", r.PostForm.Get("source_amount"))
		require.Equal(t, "dep-1", r.PostForm.Get("metadata[deposit_id]"))
		seenKeys.Add(1)
		respond(w, http.StatusOK, fixture(t, "session_create.json"))
	})
	s, err := e.client.CreateSession(context.Background(), createRequest())
	require.NoError(t, err)
	require.Equal(t, "cos_1ABC2DEF3ghi4jkl5", s.ID)
	require.Equal(t, funding.ProviderStatusInitialized, s.Status)
	require.Equal(t, "initialized", s.RawStatus)
	require.Equal(t, "cos_1ABC2DEF3ghi4jkl5_secret_contracttest", s.ClientSecret)
	require.Equal(t, "https://crypto.link.com/?session_id=cos_1ABC2DEF3ghi4jkl5", s.RedirectURL)
	require.Equal(t, "", s.DestinationAmount, "null amount stays empty, never zero")
	require.Equal(t, "100.00", s.SourceAmount)
	require.Equal(t, "usdc", s.DestinationCurrency)
	require.Equal(t, "solana", s.DestinationNetwork)
	require.Equal(t, "So11111111111111111111111111111111111111112", s.WalletAddress)
	require.False(t, s.Livemode)
	require.Equal(t, "dep-1", s.Metadata["deposit_id"])
	// A retry with the same key sends the same Idempotency-Key.
	_, err = e.client.CreateSession(context.Background(), createRequest())
	require.NoError(t, err)
	require.Equal(t, int32(2), seenKeys.Load())
	snap := e.health.Snapshot(e.clk.Now())
	require.Equal(t, 2, snap.Samples)
	require.Zero(t, snap.ErrorRateBPS)

	// GET retrieve.
	e2 := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/crypto/onramp_sessions/cos_1ABC2DEF3ghi4jkl5", r.URL.Path)
		require.Empty(t, r.Header.Get("Idempotency-Key"))
		respond(w, http.StatusOK, fixture(t, "session_fulfilled.json"))
	})
	s, err = e2.client.GetSession(context.Background(), "cos_1ABC2DEF3ghi4jkl5")
	require.NoError(t, err)
	require.Equal(t, funding.ProviderStatusConfirmed, s.Status)
	require.Equal(t, "95.890000", s.DestinationAmount)
	require.Equal(t, "cxt_1ABC2DEF3ghi4jkl5", s.TransactionID)
	require.Equal(t, "0.07", s.NetworkFee)
	require.Equal(t, "4.04", s.TransactionFee)
	q, err := stripe.ParseAmount(s.DestinationAmount, 6)
	require.NoError(t, err)
	require.Equal(t, "95890000", q.String())
}

// Case 2: invalid response — not JSON, wrong shape, or a float where a
// decimal string is documented.
func TestContract_InvalidResponse(t *testing.T) {
	for name, body := range map[string][]byte{
		"html":          []byte("<html><body>maintenance</body></html>"),
		"wrong object":  []byte(`{"id":"ch_1","object":"charge","status":"succeeded"}`),
		"number amount": fixture(t, "session_number_amount.json"),
		"empty":         nil,
		"truncated":     fixture(t, "session_create.json")[:40],
	} {
		e := newEnv(t, func(w http.ResponseWriter, _ *http.Request) { respond(w, http.StatusOK, body) })
		_, err := e.client.CreateSession(context.Background(), createRequest())
		require.Error(t, err, name)
		require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err), name)
		ee, ok := errs.As(err)
		require.True(t, ok)
		require.Equal(t, "malformed_response", ee.Fields["provider_error"], name)
	}
}

// Case 3: timeout — the adapter bounds every call and reports it as
// PROVIDER_UNAVAILABLE; the health tracker sees the failure.
func TestContract_Timeout(t *testing.T) {
	release := make(chan struct{})
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		respond(w, http.StatusOK, fixture(t, "session_create.json"))
	}, func(o *stripe.Options) { o.Timeout = hangTimeout })
	t.Cleanup(func() { close(release) })
	started := time.Now()
	_, err := e.client.CreateSession(context.Background(), createRequest())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	require.Less(t, time.Since(started), 5*time.Second)
	ee, _ := errs.As(err)
	require.Equal(t, "timeout", ee.Fields["provider_error"])
	snap := e.health.Snapshot(e.clk.Now())
	require.Equal(t, 1, snap.Samples)
	require.Equal(t, int64(10_000), snap.ErrorRateBPS)
}

// Case 4: rate limit — 429 with Retry-After becomes RATE_LIMITED with the
// hint, so callers back off instead of hammering.
func TestContract_RateLimited(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		respond(w, http.StatusTooManyRequests, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	})
	_, err := e.client.CreateSession(context.Background(), createRequest())
	require.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	ee, _ := errs.As(err)
	require.NotNil(t, ee.RetryAfter)
	require.Equal(t, 7*time.Second, *ee.RetryAfter)
	require.Equal(t, 429, ee.Fields["http_status"])
	require.NotContains(t, err.Error(), "slow down", "provider messages are never surfaced")
}

// Case 5: 5xx — provider failure, never a validation error.
func TestContract_ServerError(t *testing.T) {
	for _, status := range []int{500, 502, 503} {
		e := newEnv(t, func(w http.ResponseWriter, _ *http.Request) {
			respond(w, status, []byte(`{"error":{"type":"api_error","message":"boom"}}`))
		})
		_, err := e.client.GetSession(context.Background(), "cos_1")
		require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err), status)
		require.Equal(t, int64(10_000), e.health.Snapshot(e.clk.Now()).ErrorRateBPS)
	}
}

// Documented 4xx codes map to stable codes; credentials problems are
// internal, never customer-facing validation.
func TestContract_ClientErrors(t *testing.T) {
	cases := []struct {
		status int
		body   []byte
		want   errs.Code
		code   string
	}{
		{400, fixture(t, "error_unsupported_country.json"), errs.CodeEligibilityJurisdiction, "crypto_onramp_unsupported_country"},
		{400, fixture(t, "error_invalid_source_amount.json"), errs.CodeValidationFailed, "crypto_onramp_invalid_source_amount"},
		{400, []byte(`{"error":{"type":"invalid_request_error","code":"crypto_onramp_disabled","message":"x"}}`), errs.CodeProviderUnavailable, "crypto_onramp_disabled"},
		{401, []byte(`{"error":{"type":"invalid_request_error","message":"Invalid API Key"}}`), errs.CodeInternal, ""},
		{404, []byte(`{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such session"}}`), errs.CodeNotFound, "resource_missing"},
	}
	for _, c := range cases {
		e := newEnv(t, func(w http.ResponseWriter, _ *http.Request) { respond(w, c.status, c.body) })
		_, err := e.client.CreateSession(context.Background(), createRequest())
		require.Equal(t, c.want, errs.CodeOf(err), "status %d", c.status)
		ee, _ := errs.As(err)
		require.Equal(t, c.status, ee.Fields["http_status"])
		require.Equal(t, c.code, ee.Fields["provider_error_code"])
		require.Zero(t, e.health.Snapshot(e.clk.Now()).ErrorRateBPS, "4xx is a functioning provider")
	}
}

func webhookBody(t *testing.T, eventType string, session []byte, livemode bool) []byte {
	t.Helper()
	var obj json.RawMessage = session
	raw, err := json.Marshal(map[string]any{
		"id": "evt_1PQRStuv", "object": "event", "type": eventType, "created": testNow.Unix(), "livemode": livemode,
		"data": map[string]any{"object": obj},
	})
	require.NoError(t, err)
	return raw
}

func signed(raw []byte, at time.Time) http.Header {
	h := http.Header{}
	h.Set("Stripe-Signature", stripe.SignatureHeaderValue(webhookSecret, raw, at))
	return h
}

// Case 6: duplicate webhook — the adapter is deterministic, so the two
// deliveries carry the same identity and the inbox deduplicates on it
// (the pipeline property is proven in internal/webhook).
func TestContract_DuplicateWebhook(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, _ *http.Request) { respond(w, http.StatusOK, nil) })
	raw := webhookBody(t, "crypto.onramp_session.updated", fixture(t, "session_fulfilled.json"), false)
	first, err := e.client.ParseWebhook(context.Background(), raw, signed(raw, testNow))
	require.NoError(t, err)
	second, err := e.client.ParseWebhook(context.Background(), raw, signed(raw, testNow.Add(30*time.Second)))
	require.NoError(t, err, "a redelivery is signed afresh")
	require.Equal(t, first.Identity.EventID, second.Identity.EventID)
	require.Equal(t, first.Identity.EventType, second.Identity.EventType)
	require.Equal(t, first.Session, second.Session)
	require.Equal(t, "evt_1PQRStuv", first.Identity.EventID)
	require.True(t, first.SessionKnown)
	require.Equal(t, funding.ProviderStatusConfirmed, first.Session.Status)
	require.Empty(t, first.Session.ClientSecret)
}

// Case 7: unknown webhook event — signed, acknowledged, not modeled.
func TestContract_UnknownWebhookEvent(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, _ *http.Request) { respond(w, http.StatusOK, nil) })
	raw := webhookBody(t, "customer.updated", []byte(`{"id":"cus_1","object":"customer"}`), false)
	ev, err := e.client.ParseWebhook(context.Background(), raw, signed(raw, testNow))
	require.NoError(t, err)
	require.False(t, ev.SessionKnown)
	require.Equal(t, "customer.updated", ev.Identity.EventType)
}

// Case 8: schema missing a required field — refused as malformed, never
// guessed.
func TestContract_MissingRequiredField(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(w, http.StatusOK, fixture(t, "session_missing_status.json"))
	})
	_, err := e.client.GetSession(context.Background(), "cos_1ABC2DEF3ghi4jkl5")
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))

	raw := webhookBody(t, "crypto.onramp_session.updated", fixture(t, "session_missing_status.json"), false)
	_, err = e.client.ParseWebhook(context.Background(), raw, signed(raw, testNow))
	require.True(t, errors.Is(err, webhook.ErrMalformed))
	noID, err := json.Marshal(map[string]any{"object": "event", "type": "crypto.onramp_session.updated", "data": map[string]any{"object": json.RawMessage(fixture(t, "session_fulfilled.json"))}})
	require.NoError(t, err)
	_, err = e.client.ParseWebhook(context.Background(), noID, signed(noID, testNow))
	require.True(t, errors.Is(err, webhook.ErrMalformed))
}

// Case 9: provider unexpected state — the status enum is open; the
// undocumented quote_ready is customer action, anything unknown is UNKNOWN
// (the funding service escalates), and a livemode flip is refused.
func TestContract_UnexpectedStatus(t *testing.T) {
	for raw, want := range map[string]funding.ProviderStatus{
		"quote_ready": funding.ProviderStatusCustomerActionRequired, "fulfillment_paused": funding.ProviderStatusUnknown, "REJECTED": funding.ProviderStatusRejected,
	} {
		var obj map[string]any
		require.NoError(t, json.Unmarshal(fixture(t, "session_create.json"), &obj))
		obj["status"] = raw
		body, err := json.Marshal(obj)
		require.NoError(t, err)
		e := newEnv(t, func(w http.ResponseWriter, _ *http.Request) { respond(w, http.StatusOK, body) })
		s, err := e.client.GetSession(context.Background(), "cos_1ABC2DEF3ghi4jkl5")
		require.NoError(t, err, raw)
		require.Equal(t, want, s.Status, raw)
		require.Equal(t, raw, s.RawStatus)
	}
	var obj map[string]any
	require.NoError(t, json.Unmarshal(fixture(t, "session_create.json"), &obj))
	obj["livemode"] = true
	body, err := json.Marshal(obj)
	require.NoError(t, err)
	e := newEnv(t, func(w http.ResponseWriter, _ *http.Request) { respond(w, http.StatusOK, body) })
	_, err = e.client.GetSession(context.Background(), "cos_1ABC2DEF3ghi4jkl5")
	require.Equal(t, errs.CodeInternal, errs.CodeOf(err), "a live object in sandbox mode is a wiring fault")
}

// Case 10: forged or replayed webhook — no adapter path accepts it.
func TestContract_WebhookForgeryAndReplay(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, _ *http.Request) { respond(w, http.StatusOK, nil) })
	raw := webhookBody(t, "crypto.onramp_session.updated", fixture(t, "session_fulfilled.json"), false)
	h := http.Header{}
	h.Set("Stripe-Signature", stripe.SignatureHeaderValue("whsec_attacker", raw, testNow))
	_, err := e.client.ParseWebhook(context.Background(), raw, h)
	require.True(t, errors.Is(err, webhook.ErrSignatureInvalid))
	require.Equal(t, errs.CodeWebhookSignatureInvalid, errs.CodeOf(err))
	_, err = e.client.ParseWebhook(context.Background(), raw, signed(raw, testNow.Add(-10*time.Minute)))
	require.True(t, errors.Is(err, webhook.ErrTimestampOutOfTolerance))
	_, err = e.client.ParseWebhook(context.Background(), raw, http.Header{})
	require.True(t, errors.Is(err, webhook.ErrSignatureInvalid))
	live := webhookBody(t, "crypto.onramp_session.updated", fixture(t, "session_fulfilled.json"), true)
	_, err = e.client.ParseWebhook(context.Background(), live, signed(live, testNow))
	require.True(t, errors.Is(err, webhook.ErrMalformed), "live event against a sandbox adapter")
}

// Stale quote (PART 148) is not applicable: the adapter implements only the
// hosted/embedded flow, so no quote is ever held or executed by this
// platform. This test pins that boundary: the adapter exposes no quote or
// checkout operation.
func TestContract_NoQuoteOrCheckoutSurface(t *testing.T) {
	typ := reflect.TypeOf(&stripe.Client{})
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		require.NotContains(t, []string{"Quote", "RefreshQuote", "Checkout"}, name)
	}
}
