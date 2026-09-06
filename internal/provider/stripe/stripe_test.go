package stripe

import (
	"context"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/webhook"
)

var (
	testNow    = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	testSecret = "whsec_test_secret"
)

const sessionJSON = `{
  "id": "cos_1ABC2DEF3ghi4jkl5",
  "object": "crypto.onramp_session",
  "client_secret": "cos_1ABC2DEF3ghi4jkl5_secret_xyz",
  "created": 1757073600,
  "kyc_details_provided": false,
  "livemode": false,
  "metadata": {"deposit_id": "d1"},
  "redirect_url": "https://crypto.link.com/?session_id=cos_1ABC2DEF3ghi4jkl5",
  "status": "fulfillment_processing",
  "transaction_details": {
    "destination_amount": "0.029133919178255537",
    "destination_currency": "eth",
    "destination_network": "ethereum",
    "destination_currencies": ["eth"],
    "destination_networks": ["ethereum"],
    "fees": {"network_fee_monetary": "0.07", "transaction_fee_monetary": "4.04"},
    "lock_wallet_address": true,
    "source_amount": "100.00",
    "source_currency": "usd",
    "transaction_id": "cxt_1ABC2DEF3ghi4jkl5",
    "wallet_address": "0xabc",
    "wallet_addresses": {"ethereum": "0xabc"}
  }
}`

func TestMapStatus(t *testing.T) {
	t.Parallel()
	cases := map[string]funding.ProviderStatus{
		"initialized": funding.ProviderStatusInitialized, "requires_payment": funding.ProviderStatusCustomerActionRequired,
		"quote_ready": funding.ProviderStatusCustomerActionRequired, "fulfillment_processing": funding.ProviderStatusProcessing,
		"fulfillment_complete": funding.ProviderStatusConfirmed, "rejected": funding.ProviderStatusRejected,
		"FULFILLMENT_COMPLETE": funding.ProviderStatusConfirmed, "": funding.ProviderStatusUnknown,
		"settled_v2": funding.ProviderStatusUnknown, "quote_ready_v2": funding.ProviderStatusUnknown,
	}
	for in, want := range cases {
		require.Equal(t, want, MapStatus(in), in)
	}
}

func TestValidateAndParseAmount(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"100.00", "0.123400000000000000", "7", "0.000001", "0"} {
		require.NoError(t, ValidateAmount(ok), ok)
	}
	for _, bad := range []string{"", ".", "1.", ".5", "-1", "+1", "1e3", "1E-3", " 1", "1 ", "1,000", "0x1", "NaN", "Infinity", strings.Repeat("9", 41)} {
		require.Error(t, ValidateAmount(bad), bad)
	}
	q, err := ParseAmount("100.00", 6)
	require.NoError(t, err)
	require.Equal(t, "100000000", q.String())
	q, err = ParseAmount("0.029133919178255537", 18)
	require.NoError(t, err)
	require.Equal(t, "29133919178255537", q.String())
	_, err = ParseAmount("0.1234567", 6)
	require.Equal(t, errs.CodePrecisionLoss, errs.CodeOf(err))
	_, err = ParseAmount("1e3", 6)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	d, ok := CurrencyDecimals("solana", "usdc")
	require.True(t, ok)
	require.Equal(t, uint8(6), d)
	d, ok = CurrencyDecimals("Solana", "SOL")
	require.True(t, ok)
	require.Equal(t, uint8(9), d)
	_, ok = CurrencyDecimals("ethereum", "usdc")
	require.False(t, ok)
}

func TestDecodeSession(t *testing.T) {
	t.Parallel()
	s, err := decodeSession([]byte(sessionJSON))
	require.NoError(t, err)
	require.Equal(t, "cos_1ABC2DEF3ghi4jkl5", s.ID)
	require.Equal(t, funding.ProviderStatusProcessing, s.Status)
	require.Equal(t, "fulfillment_processing", s.RawStatus)
	require.Equal(t, "0.029133919178255537", s.DestinationAmount)
	require.Equal(t, "100.00", s.SourceAmount)
	require.Equal(t, "cxt_1ABC2DEF3ghi4jkl5", s.TransactionID)
	require.Equal(t, "0xabc", s.WalletAddress)
	require.Equal(t, "0.07", s.NetworkFee)
	require.Equal(t, "4.04", s.TransactionFee)
	require.Equal(t, time.Unix(1757073600, 0).UTC(), s.CreatedAt)
	require.Equal(t, "d1", s.Metadata["deposit_id"])
	require.False(t, s.Livemode)

	mutate := func(fn func(m map[string]any)) []byte {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(sessionJSON), &m))
		fn(m)
		b, err := json.Marshal(m)
		require.NoError(t, err)
		return b
	}
	_, err = decodeSession(mutate(func(m map[string]any) { delete(m, "id") }))
	require.Error(t, err, "id required")
	_, err = decodeSession(mutate(func(m map[string]any) { m["id"] = "sess_1" }))
	require.Error(t, err, "cos_ prefix required")
	_, err = decodeSession(mutate(func(m map[string]any) { delete(m, "status") }))
	require.Error(t, err, "status required")
	_, err = decodeSession(mutate(func(m map[string]any) { m["object"] = "checkout.session" }))
	require.Error(t, err, "wrong object")
	td := func(m map[string]any) map[string]any {
		t.Helper()
		d, ok := m["transaction_details"].(map[string]any)
		require.True(t, ok)
		return d
	}
	_, err = decodeSession(mutate(func(m map[string]any) { td(m)["destination_amount"] = 0.0291 }))
	require.Error(t, err, "a JSON number amount is never parsed as a float")
	_, err = decodeSession(mutate(func(m map[string]any) { td(m)["destination_amount"] = "2.9e-2" }))
	require.Error(t, err, "exponent notation rejected")
	_, err = decodeSession([]byte(`{"id":"cos_1","status":"initialized"} trailing`))
	require.Error(t, err)
	_, err = decodeSession([]byte(`<html>`))
	require.Error(t, err)
	// Unknown extra fields are tolerated (Stripe adds fields).
	_, err = decodeSession(mutate(func(m map[string]any) { m["brand_new_field"] = true }))
	require.NoError(t, err)
	// Wallet address falls back to the per-network map.
	s, err = decodeSession(mutate(func(m map[string]any) { delete(td(m), "wallet_address") }))
	require.NoError(t, err)
	require.Equal(t, "0xabc", s.WalletAddress)
}

func signedEvent(t *testing.T, secret string, body map[string]any, at time.Time) ([]byte, http.Header) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	h := http.Header{}
	h.Set(SignatureHeader, SignatureHeaderValue(secret, raw, at))
	return raw, h
}

func sessionEvent(t *testing.T, typ string) map[string]any {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(sessionJSON), &obj))
	return map[string]any{"id": "evt_1", "object": "event", "type": typ, "created": testNow.Unix(), "livemode": false, "data": map[string]any{"object": obj}}
}

func TestVerifySignature(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"id":"evt_1"}`)
	good := func(at time.Time) http.Header {
		h := http.Header{}
		h.Set(SignatureHeader, SignatureHeaderValue(testSecret, raw, at))
		return h
	}
	signedAt, err := verifySignature(good(testNow), raw, testSecret, testNow, SignatureTolerance)
	require.NoError(t, err)
	require.Equal(t, testNow.Truncate(time.Second), signedAt)

	// Boundary: exactly 300 s is accepted, 301 s is not.
	_, err = verifySignature(good(testNow.Add(-300*time.Second)), raw, testSecret, testNow, SignatureTolerance)
	require.NoError(t, err)
	_, err = verifySignature(good(testNow.Add(-301*time.Second)), raw, testSecret, testNow, SignatureTolerance)
	require.True(t, errors.Is(err, webhook.ErrTimestampOutOfTolerance))
	require.Equal(t, errs.CodeWebhookSignatureInvalid, errs.CodeOf(err))
	_, err = verifySignature(good(testNow.Add(301*time.Second)), raw, testSecret, testNow, SignatureTolerance)
	require.True(t, errors.Is(err, webhook.ErrTimestampOutOfTolerance), "future timestamps are replays too")

	bad := func(value string) error {
		h := http.Header{}
		if value != "" {
			h.Set(SignatureHeader, value)
		}
		_, err := verifySignature(h, raw, testSecret, testNow, SignatureTolerance)
		return err
	}
	ts := testNow.Unix()
	sig := Sign(testSecret, raw, testNow)
	require.True(t, errors.Is(bad(""), webhook.ErrSignatureInvalid), "missing header")
	require.True(t, errors.Is(bad("t="+itoa(ts)+",v1="+Sign("whsec_other", raw, testNow)), webhook.ErrSignatureInvalid), "wrong secret")
	require.True(t, errors.Is(bad("t="+itoa(ts)+",v0="+sig), webhook.ErrSignatureInvalid), "v0 is ignored")
	require.True(t, errors.Is(bad("v1="+sig), webhook.ErrSignatureInvalid), "timestamp missing")
	require.True(t, errors.Is(bad("t=abc,v1="+sig), webhook.ErrSignatureInvalid), "timestamp not integer")
	require.True(t, errors.Is(bad("t="+itoa(ts)+",v1=zz"), webhook.ErrSignatureInvalid), "non-hex v1")
	require.True(t, errors.Is(bad("t="+itoa(ts)+",v1="+sig[:10]), webhook.ErrSignatureInvalid), "short v1")
	require.True(t, errors.Is(bad("t="+itoa(ts+1)+",v1="+sig), webhook.ErrSignatureInvalid), "timestamp not the signed one")
	tampered := append([]byte(nil), raw...)
	tampered[2] = 'x'
	_, err = verifySignature(good(testNow), tampered, testSecret, testNow, SignatureTolerance)
	require.True(t, errors.Is(err, webhook.ErrSignatureInvalid), "body tampered")
	// Secret rolling: several v1 values, one of them valid.
	_, err = verifySignature(headerOf("t="+itoa(ts)+",v1="+Sign("old", raw, testNow)+",v1="+sig+",v0=ignored"), raw, testSecret, testNow, SignatureTolerance)
	require.NoError(t, err)
	_, err = verifySignature(good(testNow), raw, "", testNow, SignatureTolerance)
	require.True(t, errors.Is(err, webhook.ErrSignatureInvalid), "no secret configured fails closed")
}

func headerOf(v string) http.Header {
	h := http.Header{}
	h.Set(SignatureHeader, v)
	return h
}

func itoa(n int64) string { return time.Unix(n, 0).UTC().Format("") + formatInt(n) }

func formatInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestParseWebhook_Events(t *testing.T) {
	t.Parallel()
	raw, h := signedEvent(t, testSecret, sessionEvent(t, EventOnrampSessionUpdated), testNow)
	ev, err := parseWebhook(raw, h, testSecret, testNow, SignatureTolerance, false)
	require.NoError(t, err)
	require.True(t, ev.SessionKnown)
	require.Equal(t, ProviderName, ev.Identity.Provider)
	require.Equal(t, "evt_1", ev.Identity.EventID)
	require.Equal(t, EventOnrampSessionUpdated, ev.Identity.EventType)
	require.Equal(t, testNow.Truncate(time.Second), ev.Identity.SignedAt)
	require.Equal(t, testNow.Truncate(time.Second), ev.Identity.PublishedAt)
	require.Equal(t, funding.ProviderStatusProcessing, ev.Session.Status)
	require.Empty(t, ev.Session.ClientSecret, "client secret never leaves the API response path")
	require.Equal(t, raw, ev.Raw)

	raw, h = signedEvent(t, testSecret, sessionEvent(t, "customer.created"), testNow)
	ev, err = parseWebhook(raw, h, testSecret, testNow, SignatureTolerance, false)
	require.NoError(t, err)
	require.False(t, ev.SessionKnown, "unknown types are signed but unmodelled")

	body := sessionEvent(t, EventOnrampSessionUpdated)
	delete(body, "id")
	raw, h = signedEvent(t, testSecret, body, testNow)
	_, err = parseWebhook(raw, h, testSecret, testNow, SignatureTolerance, false)
	require.True(t, errors.Is(err, webhook.ErrMalformed))

	body = sessionEvent(t, EventOnrampSessionUpdated)
	data, ok := body["data"].(map[string]any)
	require.True(t, ok)
	obj, ok := data["object"].(map[string]any)
	require.True(t, ok)
	delete(obj, "status")
	raw, h = signedEvent(t, testSecret, body, testNow)
	_, err = parseWebhook(raw, h, testSecret, testNow, SignatureTolerance, false)
	require.True(t, errors.Is(err, webhook.ErrMalformed), "missing required session field")

	body = sessionEvent(t, EventOnrampSessionUpdated)
	body["livemode"] = true
	raw, h = signedEvent(t, testSecret, body, testNow)
	_, err = parseWebhook(raw, h, testSecret, testNow, SignatureTolerance, false)
	require.True(t, errors.Is(err, webhook.ErrMalformed), "livemode mismatch")

	raw, h = signedEvent(t, testSecret, sessionEvent(t, EventOnrampSessionUpdated), testNow)
	_, err = parseWebhook(raw, h, "whsec_other", testNow, SignatureTolerance, false)
	require.True(t, errors.Is(err, webhook.ErrSignatureInvalid))
	_, err = parseWebhook([]byte("not json"), headerOf(SignatureHeaderValue(testSecret, []byte("not json"), testNow)), testSecret, testNow, SignatureTolerance, false)
	require.True(t, errors.Is(err, webhook.ErrMalformed))
}

func TestSessionForm(t *testing.T) {
	t.Parallel()
	form, err := sessionForm(funding.CreateSessionRequest{
		IdempotencyKey: "dep-1", DestinationNetwork: "Solana", DestinationCurrency: "USDC", WalletAddress: "wallet", LockWalletAddress: true,
		SourceCurrency: "USD", SourceAmount: "100.00", CustomerIPAddress: "203.0.113.5",
		Customer: &funding.CustomerInformation{Email: "a@b.test", FirstName: "A", LastName: "B"}, Metadata: map[string]string{"deposit_id": "dep-1"},
	})
	require.NoError(t, err)
	require.Equal(t, "wallet", form.Get("wallet_addresses[solana]"))
	require.Equal(t, "true", form.Get("lock_wallet_address"))
	require.Equal(t, "solana", form.Get("destination_networks[]"))
	require.Equal(t, "usdc", form.Get("destination_currencies[]"))
	require.Equal(t, "solana", form.Get("destination_network"))
	require.Equal(t, "usdc", form.Get("destination_currency"))
	require.Equal(t, "usd", form.Get("source_currency"))
	require.Equal(t, "100.00", form.Get("source_amount"))
	require.Equal(t, "203.0.113.5", form.Get("customer_ip_address"))
	require.Equal(t, "a@b.test", form.Get("customer_information[email]"))
	require.Equal(t, "dep-1", form.Get("metadata[deposit_id]"))
	for _, k := range form {
		require.NotContains(t, k, "client_secret")
	}
	_, err = sessionForm(funding.CreateSessionRequest{IdempotencyKey: "k", DestinationNetwork: "ethereum", DestinationCurrency: "eth", WalletAddress: "w"})
	require.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	_, err = sessionForm(funding.CreateSessionRequest{DestinationNetwork: "solana", DestinationCurrency: "usdc", WalletAddress: "w"})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "idempotency key required")
	_, err = sessionForm(funding.CreateSessionRequest{IdempotencyKey: "k", DestinationNetwork: "solana", DestinationCurrency: "usdc", WalletAddress: "w", SourceAmount: "1e2"})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestNewClient_ModeAndKeys(t *testing.T) {
	t.Parallel()
	base := Options{Env: config.EnvTest, WebhookSecret: testSecret, Clock: clock.NewFake(testNow)}
	o := base
	o.Mode, o.APIKey = config.ProviderModeFake, "sk_test_x"
	_, err := NewClient(o)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "fake is not a client")
	o = base
	o.Mode, o.APIKey = config.ProviderModeLive, "sk_test_x"
	_, err = NewClient(o)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "live needs a live key")
	o = base
	o.Mode, o.APIKey = config.ProviderModeSandbox, "sk_live_x"
	_, err = NewClient(o)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "sandbox needs a test key")
	o = base
	o.Mode, o.APIKey, o.BaseURL = config.ProviderModeLive, "sk_live_x", "http://api.stripe.com"
	_, err = NewClient(o)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "live requires https")
	o = base
	o.Mode, o.APIKey = config.ProviderModeSandbox, "sk_test_x"
	o.WebhookSecret = ""
	_, err = NewClient(o)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "webhook secret required")
	o = base
	o.Mode, o.APIKey = config.ProviderModeSandbox, "sk_test_x"
	c, err := NewClient(o)
	require.NoError(t, err)
	require.Equal(t, ProviderName, c.Name())
	require.Equal(t, provider.CodeComplete, c.Verification())
	require.Equal(t, config.ProviderModeSandbox, c.Mode())
	_, err = c.GetSession(context.Background(), "../secrets")
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestNewFake_EnvironmentGate(t *testing.T) {
	t.Parallel()
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		_, err := NewFake(Options{Mode: config.ProviderModeFake, Env: env, Clock: clock.NewFake(testNow)})
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), env)
	}
	_, err := NewFake(Options{Mode: config.ProviderModeSandbox, Env: config.EnvTest})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev} {
		f, err := NewFake(Options{Mode: config.ProviderModeFake, Env: env, Clock: clock.NewFake(testNow)})
		require.NoError(t, err, env)
		require.Equal(t, FakeWebhookSecret, f.WebhookSecret())
	}
	// New() from config honors the same gate.
	_, err = New(context.Background(), config.ProviderConfig{Mode: config.ProviderModeFake, Name: ProviderName}, config.EnvProd, nil, clock.NewFake(testNow), nil, nil)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	p, err := New(context.Background(), config.ProviderConfig{Mode: config.ProviderModeFake}, config.EnvTest, nil, clock.NewFake(testNow), nil, nil)
	require.NoError(t, err)
	require.Equal(t, ProviderName, p.Name())
	_, err = New(context.Background(), config.ProviderConfig{Mode: config.ProviderModeSandbox, Name: "adyen"}, config.EnvTest, nil, clock.NewFake(testNow), nil, nil)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	res := config.EnvResolver{Lookup: func(k string) (string, bool) {
		return map[string]string{"K": "sk_test_1", "W": "whsec_1"}[k], true
	}}
	p, err = New(context.Background(), config.ProviderConfig{Mode: config.ProviderModeSandbox, APIKeyRef: "env://K", WebhookSecretRef: "env://W"}, config.EnvTest, res, clock.NewFake(testNow), nil, nil)
	require.NoError(t, err)
	require.Equal(t, provider.CodeComplete, p.Verification())
}

func TestFake_RoundTrip(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(testNow)
	f, err := NewFake(Options{Mode: config.ProviderModeFake, Env: config.EnvTest, Clock: clk})
	require.NoError(t, err)
	ctx := context.Background()
	req := funding.CreateSessionRequest{IdempotencyKey: "dep-1", DestinationNetwork: "solana", DestinationCurrency: "usdc", WalletAddress: "So11111111111111111111111111111111111111112", LockWalletAddress: true, SourceCurrency: "usd", SourceAmount: "100.00"}
	s, err := f.CreateSession(ctx, req)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(s.ID, SessionIDPrefix))
	require.Equal(t, funding.ProviderStatusInitialized, s.Status)
	require.NotEmpty(t, s.ClientSecret)
	require.False(t, s.Livemode)
	again, err := f.CreateSession(ctx, req)
	require.NoError(t, err)
	require.Equal(t, s.ID, again.ID, "idempotent on the key")
	_, err = f.CreateSession(ctx, funding.CreateSessionRequest{IdempotencyKey: "x", DestinationNetwork: "ethereum", DestinationCurrency: "eth", WalletAddress: "w"})
	require.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))

	require.NoError(t, f.SetStatus(s.ID, StatusFulfillmentComplete, &Fulfillment{DestinationAmount: "99.990000", TransactionID: "cxt_1", NetworkFee: "0.01", TransactionFee: "4.04"}))
	got, err := f.GetSession(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, funding.ProviderStatusConfirmed, got.Status)
	require.Equal(t, "99.990000", got.DestinationAmount)
	require.Equal(t, "cxt_1", got.TransactionID)
	_, err = f.GetSession(ctx, "cos_missing")
	require.Equal(t, errs.CodeNotFound, errs.CodeOf(err))

	raw, h, err := f.Webhook("evt_9", s.ID)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "secret_", "webhooks never carry the client secret")
	ev, err := f.ParseWebhook(ctx, raw, h)
	require.NoError(t, err)
	require.True(t, ev.SessionKnown)
	require.Equal(t, "evt_9", ev.Identity.EventID)
	require.Equal(t, s.ID, ev.Session.ID)
	require.Equal(t, funding.ProviderStatusConfirmed, ev.Session.Status)
	require.Equal(t, "99.990000", ev.Session.DestinationAmount)

	clk.Advance(10 * time.Minute)
	_, err = f.ParseWebhook(ctx, raw, h)
	require.True(t, errors.Is(err, webhook.ErrTimestampOutOfTolerance), "old deliveries are replays")
	raw, h, err = f.WebhookOfType("evt_10", "customer.created", s.ID, clk.Now())
	require.NoError(t, err)
	ev, err = f.ParseWebhook(ctx, raw, h)
	require.NoError(t, err)
	require.False(t, ev.SessionKnown)
	require.NoError(t, f.SetStatus(s.ID, "brand_new_status", nil))
	got, err = f.GetSession(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, funding.ProviderStatusUnknown, got.Status)
	require.Equal(t, "brand_new_status", got.RawStatus)
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	d, ok := retryAfter("7", testNow)
	require.True(t, ok)
	require.Equal(t, 7*time.Second, d)
	d, ok = retryAfter(testNow.Add(90*time.Second).UTC().Format(http.TimeFormat), testNow)
	require.True(t, ok)
	require.Equal(t, 90*time.Second, d)
	_, ok = retryAfter("", testNow)
	require.False(t, ok)
	_, ok = retryAfter("soon", testNow)
	require.False(t, ok)
	d, ok = retryAfter(testNow.Add(-time.Minute).UTC().Format(http.TimeFormat), testNow)
	require.True(t, ok)
	require.Zero(t, d)
}

func FuzzVerifySignatureHeader(f *testing.F) {
	raw := []byte(`{"id":"evt_1"}`)
	f.Add(SignatureHeaderValue(testSecret, raw, testNow))
	f.Add("t=,v1=")
	f.Add("t=1,v1=00,v1=zz,v0=x,,=,")
	f.Fuzz(func(t *testing.T, header string) {
		h := http.Header{}
		h.Set(SignatureHeader, header)
		_, err := verifySignature(h, raw, testSecret, testNow, SignatureTolerance)
		if err == nil {
			// The property is that only the genuine signature verifies, and
			// "genuine" is a property of the BYTES, not of their spelling.
			// verifySignature hex-decodes both sides and compares with
			// hmac.Equal, which is the correct thing to do: the comparison is
			// constant-time and independent of hex case.
			//
			// An earlier version asserted strings.Contains(header, Sign(...))
			// and the fuzzer duly found an uppercase rendering of the real
			// signature. That verified — correctly, because it IS the real
			// signature — while failing a string match against the lowercase
			// form. Tightening the verifier to demand lowercase would have been
			// the wrong fix: for a signature, byte equality is the property and
			// canonical-string equality is the fragile approximation of it.
			//
			// (Note this is the opposite call from identifiers. A UUID or an
			// idempotency key is canonicalised at the edge — D-038, D-039 —
			// because those feed controls that key off the raw string. Nothing
			// keys off the raw signature text.)
			expected, decErr := hex.DecodeString(Sign(testSecret, raw, testNow))
			require.NoError(t, decErr)
			var matched bool
			for _, part := range strings.Split(header, ",") {
				k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
				if !ok || k != "v1" {
					continue
				}
				if got, err := hex.DecodeString(v); err == nil && hmac.Equal(got, expected) {
					matched = true
					break
				}
			}
			require.True(t, matched,
				"verifySignature accepted a header carrying no v1 that decodes to the genuine signature: %q", header)
		}
	})
}

func FuzzDecodeEvent(f *testing.F) {
	f.Add([]byte(`{"id":"evt_1","object":"event","type":"crypto.onramp_session.updated","created":1,"data":{"object":{"id":"cos_1","status":"initialized"}}}`))
	f.Add([]byte(`{"id":"evt_1","type":"x"}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"id":"evt_1","type":"crypto.onramp_session.updated","data":{"object":{"id":"cos_1","status":"initialized","transaction_details":{"destination_amount":1.5}}}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		ev, err := decodeEvent(raw, testNow)
		if err != nil {
			require.True(t, errors.Is(err, webhook.ErrMalformed))
			return
		}
		require.NotEmpty(t, ev.Identity.EventID)
		require.NotEmpty(t, ev.Identity.EventType)
		if ev.SessionKnown {
			require.True(t, strings.HasPrefix(ev.Session.ID, SessionIDPrefix))
			if ev.Session.DestinationAmount != "" {
				require.NoError(t, ValidateAmount(ev.Session.DestinationAmount))
			}
		}
	})
}
