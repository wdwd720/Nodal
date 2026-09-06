package jupiter_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/provider/jupiter"
	"github.com/nodal/controlplane/internal/provider/jupiter/jupitertest"
)

var testNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

type sleepRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	return ctx.Err()
}

type harness struct {
	srv      *jupitertest.Server
	archive  *jupitertest.MemoryArchive
	security *jupitertest.RecordingSecuritySink
	tracker  *provider.Tracker
	sleeps   *sleepRecorder
	clk      *clock.Fake
	cfg      jupiter.Config
}

func newHarness(t *testing.T, edit func(*jupiter.Config)) (*jupiter.Client, *harness) {
	t.Helper()
	h := &harness{
		srv: jupitertest.NewServer(t), archive: &jupitertest.MemoryArchive{}, security: &jupitertest.RecordingSecuritySink{},
		sleeps: &sleepRecorder{}, clk: clock.NewFake(testNow),
	}
	tr, err := provider.NewTracker(jupiter.ProviderName, provider.DefaultThresholds(), testNow)
	require.NoError(t, err)
	h.tracker = tr
	cfg := jupiter.DefaultConfig(config.EnvTest)
	cfg.BaseURL = h.srv.URL + "/swap/v2"
	cfg.APIKeyRef = "test-api-key-value"
	// Per-call timeouts are generous so that a normal round trip can never
	// spuriously time out under CPU contention; tests that need a timeout use
	// a hanging fixture and override these explicitly (see hangTimeout).
	cfg.OrderTimeout, cfg.BuildTimeout, cfg.ExecuteTimeout = 10*time.Second, 10*time.Second, 10*time.Second
	cfg.RetryBaseDelay, cfg.RetryMaxDelay = time.Millisecond, 4*time.Millisecond
	if edit != nil {
		edit(&cfg)
	}
	h.cfg = cfg
	c, err := jupiter.NewClient(context.Background(), cfg, jupiter.Dependencies{
		Secrets: config.NewResolver(config.EnvTest, nil), Clock: h.clk, Archive: h.archive,
		Security: h.security, Tracker: h.tracker, Sleep: h.sleeps.sleep, Jitter: func(int64) int64 { return 0 },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	return c, h
}

var (
	taker      = jupitertest.NewWallet("unit-taker")
	fixtureKey = "fixture-blockhash-unit"
)

// hangTimeout is the client timeout used against a hanging fixture. The
// handler blocks until the request context is done (never a fixed sleep),
// so the only timing requirement is that the request reaches the server
// before the deadline; seconds of margin keep that true under heavy load,
// and every assertion is on error classification and call count.
const hangTimeout = 2 * time.Second

func fixtureTx(t *testing.T) []byte {
	t.Helper()
	raw, err := jupiter.BuildFakeRouteTransaction(jupiter.FakeRouteParams{
		ProgramID: mustPK(jupiter.DefaultProgramID), Taker: taker.PublicKey,
		InputMint: mustPK(jupitertest.MintUSDC), OutputMint: mustPK(jupitertest.MintSOL),
		InAmount: 1_000_000, QuotedOutAmount: 6_500_000, SlippageBPS: 50,
		RecentBlockhash: hashOf(fixtureKey), ComputeUnitLimit: 200_000, ComputeUnitPriceMicroLamports: 1_000,
	})
	require.NoError(t, err)
	return raw
}

func orderBody(t *testing.T, tx []byte, edit func(m map[string]any)) string {
	t.Helper()
	m := map[string]any{
		"mode": "manual", "router": "metis", "requestId": "req-unit-1",
		"inAmount": "1000000", "outAmount": "6500000", "otherAmountThreshold": "6467500",
		"priceImpact": json.Number("0.05"), "slippageBps": 50, "feeBps": 10,
		"routePlan": []any{map[string]any{"swapInfo": map[string]any{
			"ammKey": "amm", "label": "Orca", "inputMint": jupitertest.MintUSDC, "outputMint": jupitertest.MintSOL,
			"inAmount": "1000000", "outAmount": "6500000",
		}, "percent": 100, "bps": 10000}},
		"transaction": nil, "lastValidBlockHeight": "250000150",
		"signatureFeeLamports": 5000, "prioritizationFeeLamports": 1000, "gasless": false,
	}
	if tx != nil {
		m["transaction"] = base64.StdEncoding.EncodeToString(tx)
	}
	if edit != nil {
		edit(m)
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return string(b)
}

func orderReq() jupiter.OrderRequest {
	s := money.BPS(50)
	return jupiter.OrderRequest{
		InputMint: jupitertest.MintUSDC, OutputMint: jupitertest.MintSOL,
		Amount: money.QuantityFromInt64(1_000_000), SlippageBPS: &s, TakerPubkey: taker.PublicKey.String(),
	}
}

func TestClient_Order_Valid(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, nil)
	tx := fixtureTx(t)
	h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.JSON(orderBody(t, tx, nil)))

	o, err := c.Order(context.Background(), orderReq())
	require.NoError(t, err)
	require.Equal(t, "req-unit-1", o.QuoteID)
	require.Equal(t, "1000000", o.InAmount.String())
	require.Equal(t, "6467500", o.OtherAmountThreshold.String())
	require.Equal(t, money.BPS(5), o.PriceImpactBPS)
	require.False(t, o.PriceImpactUnavailable)
	require.True(t, o.HasTransaction)
	require.Equal(t, tx, o.UnsignedTransaction)
	require.Equal(t, taker.PublicKey.String(), o.FeePayer)
	require.Equal(t, hashOf(fixtureKey).String(), o.RecentBlockhash)
	require.Equal(t, uint32(200_000), o.ComputeUnitLimit)
	require.Equal(t, "transaction", o.ComputeUnitLimitSource)
	require.Equal(t, uint64(1_000), o.ComputeUnitPriceMicroLamports)
	require.Contains(t, o.ProgramIDs, jupiter.DefaultProgramID)
	require.Equal(t, uint64(250000150), o.LastValidBlockHeight)
	require.Equal(t, "250000150", o.LastValidBlockHeightRaw)
	require.Equal(t, testNow, o.ReceivedAt)
	require.True(t, o.ExpiresAtAssumed)
	require.Equal(t, testNow.Add(jupiter.DefaultAssumedOrderTTL), o.ExpiresAt)
	require.NotEmpty(t, o.RawRef)
	require.Len(t, o.RawHash, 32)
	require.Len(t, o.RouteHash, 32)
	require.NotEmpty(t, o.GatewayRequestID)

	calls := h.srv.Calls()
	require.Len(t, calls, 1)
	require.True(t, calls[0].HasHeader(jupiter.HeaderAPIKey, "test-api-key-value"))
	require.Equal(t, "1000000", calls[0].Query.Get("amount"))
	require.Equal(t, "50", calls[0].Query.Get("slippageBps"))
	require.Equal(t, taker.PublicKey.String(), calls[0].Query.Get("taker"))

	// Evidence: redacted request, full response, hash matches.
	items := h.archive.Items()
	require.Len(t, items, 1)
	require.Equal(t, observability.RedactedMarker, items[0].RequestHeaders.Get(jupiter.HeaderAPIKey))
	require.NotContains(t, items[0].RequestURL, "test-api-key-value")
	require.JSONEq(t, orderBody(t, tx, nil), string(items[0].ResponseBody))
	require.Equal(t, o.RawHash, items[0].ResponseHash)
	require.Equal(t, 1, h.tracker.Snapshot(testNow).Samples)
	require.Equal(t, provider.Healthy, h.tracker.Health())
}

func TestClient_Order_RetriesOn429HonoringRetryAfterThen5xx(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/order",
		jupitertest.Response{Status: 429, Headers: map[string]string{"Retry-After": "2"}, Body: []byte("[API Gateway] Too many requests")},
		jupitertest.Response{Status: 503, Body: []byte(`{"error":"upstream"}`)},
		jupitertest.JSON(orderBody(t, nil, func(m map[string]any) { m["transaction"] = nil })),
	)
	req := orderReq()
	req.TakerPubkey = ""
	o, err := c.Order(context.Background(), req)
	require.NoError(t, err)
	require.False(t, o.HasTransaction)
	require.Equal(t, 3, h.srv.CallCount("GET", "/swap/v2/order"))
	require.Equal(t, []time.Duration{2 * time.Second, time.Millisecond}, h.sleeps.waits, "Retry-After honored, then exponential backoff (base 1ms doubled to 2ms, jitter 0: half)")
	require.Equal(t, 3, h.archive.Len(), "every attempt is archived")
}

func TestClient_Order_RateLimitExhausted(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, func(cfg *jupiter.Config) { cfg.MaxRetries = 1 })
	h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.Response{Status: 429, Headers: map[string]string{"x-ratelimit-reset": strconv.FormatInt(testNow.Unix()+1, 10), "x-ratelimit-remaining": "-1"}, Body: []byte("Too many requests")})
	req := orderReq()
	req.TakerPubkey = ""
	_, err := c.Order(context.Background(), req)
	require.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.NotNil(t, e.RetryAfter)
	require.Equal(t, time.Second, *e.RetryAfter, "x-ratelimit-reset is one second after the fake clock")
	require.Equal(t, 2, h.srv.CallCount("GET", "/swap/v2/order"))
	require.Equal(t, 2, e.Fields["attempts"])
}

func TestClient_Order_RetryAfterTooLongStopsImmediately(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, func(cfg *jupiter.Config) { cfg.MaxRetryAfterWait = time.Second })
	h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.Response{Status: 429, Headers: map[string]string{"Retry-After": "30"}, Body: []byte("x")})
	req := orderReq()
	req.TakerPubkey = ""
	_, err := c.Order(context.Background(), req)
	require.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	require.Equal(t, 1, h.srv.CallCount("GET", "/swap/v2/order"))
	require.Empty(t, h.sleeps.waits)
}

func TestClient_Order_ServerErrorsExhaustBudget(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, func(cfg *jupiter.Config) { cfg.MaxRetries = 5 })
	h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.Response{Status: 502, Body: []byte("bad gateway")})
	req := orderReq()
	req.TakerPubkey = ""
	_, err := c.Order(context.Background(), req)
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	require.Equal(t, 6, h.srv.CallCount("GET", "/swap/v2/order"))
	require.Equal(t, provider.Unhealthy, h.tracker.Health(), "six failed samples: 100% error rate above the unhealthy threshold")
}

func TestClient_Order_Timeout(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, func(cfg *jupiter.Config) { cfg.MaxRetries = 1; cfg.OrderTimeout = hangTimeout })
	h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.Response{Hang: true})
	req := orderReq()
	req.TakerPubkey = ""
	_, err := c.Order(context.Background(), req)
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, true, e.Fields["timed_out"])
	require.Equal(t, 2, h.srv.CallCount("GET", "/swap/v2/order"))
	items := h.archive.Items()
	require.Len(t, items, 2)
	require.NotEmpty(t, items[0].TransportError)
	require.Equal(t, 0, items[0].ResponseStatus)
}

func TestClient_Order_AuthRejectedEmitsSecurityEvent(t *testing.T) {
	t.Parallel()
	for _, status := range []int{401, 403} {
		c, h := newHarness(t, nil)
		h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.Response{Status: status, Headers: map[string]string{jupiter.HeaderGatewayRequestID: "gw-auth"}, Body: []byte(`{"error":"invalid key"}`)})
		req := orderReq()
		req.TakerPubkey = ""
		_, err := c.Order(context.Background(), req)
		require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
		require.Equal(t, 1, h.srv.CallCount("GET", "/swap/v2/order"), "auth failures are not retried")
		evs := h.security.Events()
		require.Len(t, evs, 1)
		require.Equal(t, jupiter.SecurityEventProviderAuthRejected, evs[0].Kind)
		require.Equal(t, status, evs[0].HTTPStatus)
		require.Equal(t, "gw-auth", evs[0].GatewayRequestID)
		require.Equal(t, jupiter.OpOrder, evs[0].Operation)
	}
}

func TestClient_Order_400Mapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		body string
		code errs.Code
	}{
		{`{"requestId":"r","error":"Could not find any route"}`, errs.CodeVenueLiquidityInsufficient},
		{`{"requestId":"r","error":"insufficient liquidity for amount"}`, errs.CodeVenueLiquidityInsufficient},
		{`{"requestId":"r","error":"invalid inputMint"}`, errs.CodeValidationFailed},
		{`{"requestId":"r","error":"blockhash not found"}`, errs.CodeQuoteExpired},
		{`garbage`, errs.CodeValidationFailed},
	}
	for _, tc := range cases {
		c, h := newHarness(t, nil)
		h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.Response{Status: 400, Body: []byte(tc.body)})
		req := orderReq()
		req.TakerPubkey = ""
		_, err := c.Order(context.Background(), req)
		require.Equal(t, tc.code, errs.CodeOf(err), tc.body)
		require.Equal(t, 1, h.srv.CallCount("GET", "/swap/v2/order"), "400 is never retried")
	}
}

func TestClient_Order_ErrorCodeInBody(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.JSON(`{"requestId":"r","router":"metis","errorCode":1,"errorMessage":"insufficient funds","transaction":""}`))
	_, err := c.Order(context.Background(), orderReq())
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, "INSUFFICIENT_FUNDS", e.Fields["reason"])
	require.Equal(t, int64(1), e.Fields["provider_code"])
}

func TestClient_Order_InvalidResponses(t *testing.T) {
	t.Parallel()
	tx := fixtureTx(t)
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"float amount", orderBody(t, tx, func(m map[string]any) { m["inAmount"] = json.Number("1000000.0") }), "inAmount"},
		{"missing threshold", orderBody(t, tx, func(m map[string]any) { delete(m, "otherAmountThreshold") }), "otherAmountThreshold"},
		{"in amount mismatch", orderBody(t, tx, func(m map[string]any) { m["inAmount"] = "999" }), "inAmount"},
		{"threshold above out", orderBody(t, tx, func(m map[string]any) { m["otherAmountThreshold"] = "7000000" }), "otherAmountThreshold"},
		{"taker but no tx", orderBody(t, nil, nil), "transaction"},
		{"garbage tx bytes", orderBody(t, []byte{1, 2, 3}, nil), "transaction"},
		{"route mint mismatch", orderBody(t, tx, func(m map[string]any) {
			objAt(m, "routePlan", 0, "swapInfo")["outputMint"] = jupitertest.MintUSDC
		}), "routePlan"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, h := newHarness(t, nil)
			h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.JSON(tc.body))
			_, err := c.Order(context.Background(), orderReq())
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			e, _ := errs.As(err)
			require.Equal(t, tc.field, e.Fields["field"])
			require.NotEmpty(t, e.Fields["raw_ref"], "the offending payload is archived")
		})
	}
}

func TestClient_Order_UnexpectedStatusAndArchiveFailure(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.Response{Status: 302, Headers: map[string]string{"Location": "https://elsewhere.example"}, Body: []byte("moved")})
	req := orderReq()
	req.TakerPubkey = ""
	_, err := c.Order(context.Background(), req)
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	require.Equal(t, 1, h.srv.CallCount("GET", "/swap/v2/order"), "redirects are not followed")

	c2, h2 := newHarness(t, nil)
	h2.archive.FailWith = errors.New("s3 down")
	h2.srv.Enqueue("GET", "/swap/v2/order", jupitertest.JSON(orderBody(t, nil, nil)))
	_, err = c2.Order(context.Background(), req)
	require.Equal(t, errs.CodeInternal, errs.CodeOf(err), "evidence is mandatory")
}

func TestClient_Order_RequestValidation(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, nil)
	bad := orderReq()
	bad.Receiver = bad.TakerPubkey
	_, err := c.Order(context.Background(), bad)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	bad = orderReq()
	bad.Amount = money.QuantityFromInt64(0)
	_, err = c.Order(context.Background(), bad)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	bad = orderReq()
	bad.SwapMode = "ExactOut"
	_, err = c.Order(context.Background(), bad)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	bad = orderReq()
	fee := money.BPS(10)
	bad.ReferralAccount, bad.ReferralFeeBPS = taker.PublicKey.String(), &fee
	_, err = c.Order(context.Background(), bad)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	require.Equal(t, 0, len(h.srv.Calls()), "nothing is sent for an invalid request")
}

func signedFixture(t *testing.T) ([]byte, string) {
	t.Helper()
	signed := jupitertest.Sign(t, fixtureTx(t), taker)
	return signed, jupitertest.SignatureOf(t, signed)
}

func execBody(sig, status string, extra map[string]any) string {
	m := map[string]any{
		"status": status, "signature": sig, "slot": "250000100", "code": 0, "error": nil,
		"totalInputAmount": "1000000", "totalOutputAmount": "6500000", "inputAmountResult": "1000000", "outputAmountResult": "6510000",
		"swapEvents": []any{map[string]any{"inputMint": jupitertest.MintUSDC, "inputAmount": "1000000", "outputMint": jupitertest.MintSOL, "outputAmount": "6510000"}},
	}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestClient_Execute_Success(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, nil)
	signed, sig := signedFixture(t)
	h.srv.Enqueue("POST", "/swap/v2/execute", jupitertest.JSON(execBody(sig, "Success", nil)))
	res, err := c.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: signed, RequestID: "req-unit-1", LastValidBlockHeight: "250000150"})
	require.NoError(t, err)
	require.Equal(t, jupiter.ExecuteSuccess, res.Status)
	require.Equal(t, sig, res.Signature)
	require.Equal(t, uint64(250000100), res.Slot)
	require.Equal(t, "6510000", res.OutputAmountResult.String())
	require.Len(t, res.SwapEvents, 1)
	require.NotEmpty(t, res.RawRef)

	calls := h.srv.Calls()
	require.Len(t, calls, 1)
	var body map[string]any
	require.NoError(t, json.Unmarshal(calls[0].Body, &body))
	require.Equal(t, "req-unit-1", body["requestId"])
	require.Equal(t, "250000150", body["lastValidBlockHeight"], "passed through as the /order string")
	require.Equal(t, base64.StdEncoding.EncodeToString(signed), body["signedTransaction"])
	require.True(t, calls[0].HasHeader("Content-Type", "application/json"))
}

func TestClient_Execute_TimeoutIsUnknownAndNeverRetried(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, func(cfg *jupiter.Config) { cfg.ExecuteTimeout = hangTimeout; cfg.MaxRetries = 5 })
	signed, sig := signedFixture(t)
	h.srv.Enqueue("POST", "/swap/v2/execute", jupitertest.Response{Hang: true})
	res, err := c.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: signed, RequestID: "req-unit-1"})
	require.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, true, e.Fields["timed_out"])
	require.Equal(t, true, e.Fields["submitted"])
	require.Equal(t, sig, e.Fields["signature"])
	require.Equal(t, sig, res.Signature, "the local signature is always known")
	require.Equal(t, 1, h.srv.CallCount("POST", "/swap/v2/execute"), "exactly one attempt, ever")
	require.Empty(t, h.sleeps.waits)
	require.Equal(t, 1, h.archive.Len())
}

func TestClient_Execute_StatusMapping(t *testing.T) {
	t.Parallel()
	signed, sig := signedFixture(t)
	cases := []struct {
		name      string
		resp      jupitertest.Response
		code      errs.Code
		submitted any
	}{
		{"400 missing cached order", jupitertest.Response{Status: 400, Body: []byte(`{"error":"missing cached order","code":-1}`)}, errs.CodeQuoteExpired, false},
		{"400 invalid signed tx", jupitertest.Response{Status: 400, Body: []byte(`{"error":"bad tx","code":-2}`)}, errs.CodeValidationFailed, false},
		{"400 aggregator failure", jupitertest.Response{Status: 400, Body: []byte(`{"error":"failed","code":-1001}`)}, errs.CodeSubmissionStateUnknown, true},
		{"400 undocumented", jupitertest.Response{Status: 400, Body: []byte(`{"error":"?"}`)}, errs.CodeSubmissionStateUnknown, true},
		{"401", jupitertest.Response{Status: 401, Body: []byte(`{"error":"unauthorized"}`)}, errs.CodeProviderUnavailable, false},
		{"429", jupitertest.Response{Status: 429, Headers: map[string]string{"Retry-After": "1"}, Body: []byte("Too many requests")}, errs.CodeRateLimited, false},
		{"500 with signature", jupitertest.Response{Status: 500, Body: []byte(`{"signature":"` + sig + `","error":"boom"}`)}, errs.CodeSubmissionStateUnknown, true},
		{"unexpected 302", jupitertest.Response{Status: 302, Body: []byte("moved")}, errs.CodeSubmissionStateUnknown, true},
		{"200 unexpected status", jupitertest.JSON(execBody(sig, "Pending", nil)), errs.CodeSubmissionStateUnknown, true},
		{"200 signature mismatch", jupitertest.JSON(execBody("5"+strings.Repeat("1", 86), "Success", nil)), errs.CodeSubmissionStateUnknown, true},
		{"200 undecodable", jupitertest.JSON(`{"status":"Success","slot":100}`), errs.CodeSubmissionStateUnknown, true},
		{"200 success without signature", jupitertest.JSON(`{"status":"Success"}`), errs.CodeSubmissionStateUnknown, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, h := newHarness(t, nil)
			h.srv.Enqueue("POST", "/swap/v2/execute", tc.resp)
			_, err := c.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: signed, RequestID: "req-unit-1"})
			require.Equal(t, tc.code, errs.CodeOf(err), "%v", err)
			e, _ := errs.As(err)
			require.Equal(t, tc.submitted, e.Fields["submitted"])
			require.Equal(t, 1, h.srv.CallCount("POST", "/swap/v2/execute"))
			if tc.resp.Status == 401 {
				require.Len(t, h.security.Events(), 1)
			}
		})
	}
}

func TestClient_Execute_FailedStatusIsReturnedNotErrored(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, nil)
	signed, sig := signedFixture(t)
	h.srv.Enqueue("POST", "/swap/v2/execute", jupitertest.JSON(execBody(sig, "Failed", map[string]any{"code": -1002, "error": "slippage"})))
	res, err := c.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: signed, RequestID: "req-unit-1"})
	require.NoError(t, err)
	require.Equal(t, jupiter.ExecuteFailed, res.Status)
	require.Equal(t, int64(-1002), res.ProviderCode)
	require.Equal(t, "slippage", res.ProviderError)
	require.Equal(t, sig, res.Signature)
}

func TestClient_Execute_LocalRejectionsNeverSend(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, nil)
	_, err := c.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: fixtureTx(t), RequestID: "r"})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "unsigned transaction")
	signed, _ := signedFixture(t)
	_, err = c.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: signed})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "missing request id")
	_, err = c.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: signed, RequestID: "r", LastValidBlockHeight: "-1"})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Execute(ctx, jupiter.ExecuteRequest{SignedTransaction: signed, RequestID: "r"})
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, false, e.Fields["submitted"])
	require.Equal(t, 0, len(h.srv.Calls()))
}

func TestClient_Execute_ArchiveFailureIsUnknown(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, nil)
	h.archive.FailWith = errors.New("s3 down")
	signed, sig := signedFixture(t)
	h.srv.Enqueue("POST", "/swap/v2/execute", jupitertest.JSON(execBody(sig, "Success", nil)))
	_, err := c.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: signed, RequestID: "r"})
	require.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))
}

func TestClient_Status_Unsupported(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, nil)
	_, err := c.Status(context.Background(), "sig")
	require.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	require.Equal(t, 0, len(h.srv.Calls()))
	require.Equal(t, provider.SafeRetry, jupiter.RetryClassOf(jupiter.OpOrder))
	require.Equal(t, provider.SafeRetry, jupiter.RetryClassOf(jupiter.OpBuild))
	require.Equal(t, provider.UnknownEffectWrite, jupiter.RetryClassOf(jupiter.OpExecute))
	require.Equal(t, provider.UnknownEffectWrite, jupiter.RetryClassOf("cancel"))
	require.Equal(t, provider.CodeComplete, c.VerificationLabel())
	require.Equal(t, jupiter.ProviderName, c.Name())
}

func TestClient_Build_Valid(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/build", jupitertest.JSON(strings.ReplaceAll(buildJSON, "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU", jupitertest.MintUSDC)))
	s := money.BPS(50)
	r, err := c.Build(context.Background(), jupiter.BuildRequest{
		InputMint: jupitertest.MintUSDC, OutputMint: jupitertest.MintSOL, Amount: money.QuantityFromInt64(1_000_000),
		Taker: taker.PublicKey.String(), SlippageBPS: &s, MaxAccounts: 40, BlockhashSlotsToExpiry: 100,
	})
	require.NoError(t, err)
	require.True(t, r.ComputeUnitLimitUnavailable)
	require.Equal(t, money.BPS(1), r.PriceImpactBPS)
	require.Equal(t, uint64(250000150), r.LastValidBlockHeight)
	require.Equal(t, "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4", r.SwapInstruction.ProgramID)
	require.True(t, r.ExpiresAtAssumed)
	q := h.srv.Calls()[0].Query
	require.Equal(t, "40", q.Get("maxAccounts"))
	require.Equal(t, "100", q.Get("blockhashSlotsToExpiry"))
	require.Equal(t, "50", q.Get("slippageBps"))

	_, err = c.Build(context.Background(), jupiter.BuildRequest{InputMint: jupitertest.MintUSDC, OutputMint: jupitertest.MintSOL, Amount: money.QuantityFromInt64(1)})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "taker required")
	_, err = c.Build(context.Background(), jupiter.BuildRequest{InputMint: jupitertest.MintUSDC, OutputMint: jupitertest.MintSOL, Amount: money.QuantityFromInt64(1), Taker: taker.PublicKey.String(), SlippageBPS: &s, SlippageRTSE: true})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "rtse and bps exclusive")
}

const buildJSON = `{
  "inAmount":"1000000","outAmount":"6500000","otherAmountThreshold":"6467500","slippageBps":50,"priceImpactPct":"0.0001",
  "routePlan":[{"swapInfo":{"ammKey":"amm1","label":"Orca","inputMint":"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU","outputMint":"So11111111111111111111111111111111111111112","inAmount":"1000000","outAmount":"6500000"},"percent":100,"bps":10000}],
  "computeBudgetInstructions":[{"programId":"ComputeBudget111111111111111111111111111111","accounts":[],"data":"AsBcFQA="}],
  "setupInstructions":[],
  "swapInstruction":{"programId":"JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4","accounts":[{"pubkey":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA","isSigner":false,"isWritable":false}],"data":"5RfLl3rjrSo="},
  "cleanupInstruction":null,"otherInstructions":[],"tipInstruction":null,"addressesByLookupTableAddress":null,
  "blockhashWithMetadata":{"blockhash":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32],"lastValidBlockHeight":250000150,"fetchedAt":{"secs_since_epoch":1760000000,"nanos_since_epoch":5}}
}`

func TestNewClient_Guards(t *testing.T) {
	t.Parallel()
	cfg := jupiter.DefaultConfig(config.EnvTest)
	cfg.Mode = config.ProviderModeFake
	_, err := jupiter.NewClient(context.Background(), cfg, jupiter.Dependencies{Clock: clock.NewFake(testNow), Archive: &jupitertest.MemoryArchive{}})
	require.Error(t, err, "fake mode must use NewFake")

	cfg = jupiter.DefaultConfig(config.EnvTest)
	_, err = jupiter.NewClient(context.Background(), cfg, jupiter.Dependencies{Clock: clock.NewFake(testNow)})
	require.Error(t, err, "archive required")

	cfg.APIKeyRef = "env://JUPITER_KEY_UNSET_FOR_TEST"
	_, err = jupiter.NewClient(context.Background(), cfg, jupiter.Dependencies{Clock: clock.NewFake(testNow), Archive: &jupitertest.MemoryArchive{}, Secrets: config.EnvResolver{Lookup: func(string) (string, bool) { return "", false }}})
	require.Error(t, err, "unresolvable key")

	cfg.APIKeyRef = "env://JUPITER_KEY"
	c, err := jupiter.NewClient(context.Background(), cfg, jupiter.Dependencies{Clock: clock.NewFake(testNow), Archive: &jupitertest.MemoryArchive{}, Secrets: config.EnvResolver{Lookup: func(string) (string, bool) { return "k", true }}})
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestClient_KeylessSendsNoHeader(t *testing.T) {
	t.Parallel()
	c, h := newHarness(t, func(cfg *jupiter.Config) { cfg.APIKeyRef = "" })
	h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.JSON(orderBody(t, nil, nil)))
	req := orderReq()
	req.TakerPubkey = ""
	_, err := c.Order(context.Background(), req)
	require.NoError(t, err)
	require.Empty(t, h.srv.Calls()[0].Headers.Get(jupiter.HeaderAPIKey))
	require.Equal(t, http.MethodGet, h.srv.Calls()[0].Method)
}
