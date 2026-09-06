// Package jupitercontract holds the provider contract tests for the Jupiter
// Swap API V2 client (goal PART 148) against recorded fixtures under
// testdata/. See README.md for which fixture fields are documented and
// which are assumed.
package jupitercontract

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
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

// Recorded fixture identity: everything below is derived deterministically
// by the generator described in README.md and re-derived by
// TestContract_FixtureIsReproducible.
const (
	fixtureRequestID = "contract-order-0001"
	fixtureBlockhash = "EaK8kTUjTya66X7PC6MV2B1yb8UbGFX2vR6hHRebSvqR"
	fixtureSignature = "DK4E9eUKdCdQhq4G4uusBtHEVA9CmsBVXfEZYeH6VeaZtHawz64HpA63eEZkHjfmjiYYPb5AwMf3Nxd4VpKMLAm"
	fixtureLVBH      = "250000150"
)

var (
	now         = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) // unix 1788609600
	takerWallet = jupitertest.NewWallet("contract-taker")
)

// hangTimeout is the client timeout used against a hanging fixture, whose
// handler blocks until the request context is done (never a fixed sleep).
// The only timing requirement is that the request reaches the server before
// the deadline, so the bound is generous; assertions are on classification
// and call count, never on elapsed time.
const hangTimeout = 2 * time.Second

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err, name)
	return b
}

func fixtureJSON(t *testing.T, name string) jupitertest.Response {
	t.Helper()
	return jupitertest.JSON(string(fixture(t, name)))
}

type sleeps struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	return ctx.Err()
}

type harness struct {
	client   *jupiter.Client
	srv      *jupitertest.Server
	archive  *jupitertest.MemoryArchive
	security *jupitertest.RecordingSecuritySink
	tracker  *provider.Tracker
	sleeps   *sleeps
}

func newHarness(t *testing.T, edit func(*jupiter.Config)) *harness {
	t.Helper()
	h := &harness{srv: jupitertest.NewServer(t), archive: &jupitertest.MemoryArchive{}, security: &jupitertest.RecordingSecuritySink{}, sleeps: &sleeps{}}
	tr, err := provider.NewTracker(jupiter.ProviderName, provider.DefaultThresholds(), now)
	require.NoError(t, err)
	h.tracker = tr
	cfg := jupiter.DefaultConfig(config.EnvTest)
	cfg.BaseURL = h.srv.URL + "/swap/v2"
	cfg.APIKeyRef = "contract-test-api-key"
	// Generous per-call timeouts: a normal round trip must never time out
	// under CPU contention. Timeout cases use a hanging fixture and
	// hangTimeout instead.
	cfg.OrderTimeout, cfg.BuildTimeout, cfg.ExecuteTimeout = 10*time.Second, 10*time.Second, 10*time.Second
	cfg.RetryBaseDelay, cfg.RetryMaxDelay = time.Millisecond, 4*time.Millisecond
	if edit != nil {
		edit(&cfg)
	}
	h.client, err = jupiter.NewClient(context.Background(), cfg, jupiter.Dependencies{
		Secrets: config.NewResolver(config.EnvTest, nil), Clock: clock.NewFake(now), Archive: h.archive, Security: h.security,
		Tracker: h.tracker, Sleep: h.sleeps.sleep, Jitter: func(int64) int64 { return 0 },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	return h
}

func orderReq() jupiter.OrderRequest {
	s := money.BPS(50)
	return jupiter.OrderRequest{
		InputMint: jupitertest.MintUSDC, OutputMint: jupitertest.MintSOL, Amount: money.QuantityFromInt64(1_000_000),
		SlippageBPS: &s, TakerPubkey: takerWallet.PublicKey.String(),
	}
}

func fixtureTxBytes(t *testing.T) []byte {
	t.Helper()
	var w struct {
		Transaction string `json:"transaction"`
	}
	require.NoError(t, json.Unmarshal(fixture(t, "order_valid.json"), &w))
	raw, err := base64.StdEncoding.DecodeString(w.Transaction)
	require.NoError(t, err)
	return raw
}

func signedFixture(t *testing.T) []byte {
	t.Helper()
	return jupitertest.Sign(t, fixtureTxBytes(t), takerWallet)
}

// TestContract_FixtureIsReproducible proves the recorded transaction in
// order_valid.json is exactly what BuildFakeRouteTransaction produces for
// the documented parameters, and that signing it with the deterministic
// taker wallet yields the signature recorded in execute_success.json.
func TestContract_FixtureIsReproducible(t *testing.T) {
	t.Parallel()
	seed := sha256.Sum256([]byte("contract-fixture-blockhash"))
	raw, err := jupiter.BuildFakeRouteTransaction(jupiter.FakeRouteParams{
		ProgramID: solana.MustPublicKeyFromBase58(jupiter.DefaultProgramID), Taker: takerWallet.PublicKey,
		InputMint: solana.MustPublicKeyFromBase58(jupitertest.MintUSDC), OutputMint: solana.MustPublicKeyFromBase58(jupitertest.MintSOL),
		InAmount: 1_000_000, QuotedOutAmount: 6_500_000, SlippageBPS: 50,
		RecentBlockhash: solana.HashFromBytes(seed[:]), ComputeUnitLimit: 200_000, ComputeUnitPriceMicroLamports: 1_000,
	})
	require.NoError(t, err)
	require.Equal(t, raw, fixtureTxBytes(t), "order_valid.json transaction is the recorded generator output")
	require.Equal(t, fixtureBlockhash, solana.HashFromBytes(seed[:]).String())
	require.Equal(t, fixtureSignature, jupitertest.SignatureOf(t, signedFixture(t)))
	require.Equal(t, "8WrtGLJ9CqssHJzG7s9fbaFEa1A4xafiFhNTSKtXPLHb", takerWallet.PublicKey.String())
}

// ---- /order ----------------------------------------------------------------

func TestContract_Order_Valid(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/order", fixtureJSON(t, "order_valid.json"))
	o, err := h.client.Order(context.Background(), orderReq())
	require.NoError(t, err)

	require.Equal(t, fixtureRequestID, o.QuoteID)
	require.Equal(t, "manual", o.Mode)
	require.Equal(t, "metis", o.Router)
	require.Equal(t, "1000000", o.InAmount.String())
	require.Equal(t, "6500000", o.OutAmount.String())
	require.Equal(t, "6467500", o.OtherAmountThreshold.String())
	require.Equal(t, money.BPS(50), o.SlippageBPS)
	require.Equal(t, money.BPS(10), o.FeeBPS)
	require.Equal(t, money.BPS(2), o.PriceImpactBPS, "priceImpact 0.0123 percentage points -> 1.23 bps -> RoundCeil 2")
	require.Equal(t, "priceImpact", o.PriceImpactSource)
	require.NotNil(t, o.PlatformFee)
	require.Equal(t, "6500", o.PlatformFee.Amount.String())
	require.Len(t, o.Route, 1)
	require.Equal(t, "6500000", o.Route[0].OutAmount.String())
	require.Len(t, o.RouteHash, 32)
	require.True(t, o.HasTransaction)
	require.Equal(t, fixtureTxBytes(t), o.UnsignedTransaction)
	require.Equal(t, takerWallet.PublicKey.String(), o.FeePayer)
	require.Equal(t, fixtureBlockhash, o.RecentBlockhash)
	require.Equal(t, []string{takerWallet.PublicKey.String()}, o.RequiredSigners)
	require.Equal(t, uint32(200_000), o.ComputeUnitLimit)
	require.Equal(t, "transaction", o.ComputeUnitLimitSource)
	require.Equal(t, uint64(1_000), o.ComputeUnitPriceMicroLamports)
	require.Equal(t, uint64(250_000_150), o.LastValidBlockHeight)
	require.Equal(t, fixtureLVBH, o.LastValidBlockHeightRaw)
	require.Equal(t, "5000", o.SignatureFeeLamports.String())
	require.Equal(t, "12345", o.PrioritizationFeeLamports.String())
	require.Equal(t, "2039280", o.RentFeeLamports.String())
	require.False(t, o.Gasless)
	require.Nil(t, o.RFQ)
	require.Equal(t, now, o.ReceivedAt)
	require.True(t, o.ExpiresAtAssumed, "aggregator routes carry no expireAt; TTL is assumed")
	require.Equal(t, now.Add(jupiter.DefaultAssumedOrderTTL), o.ExpiresAt)
	require.NotEmpty(t, o.RawRef)
	require.Equal(t, sha256.Size, len(o.RawHash))
	require.NotEmpty(t, o.GatewayRequestID)

	require.NoError(t, jupiter.ValidateQuote(o, now.Add(time.Second), jupiter.ValidationPolicy{
		MaxAge: 10 * time.Second, ExpectedInputMint: jupitertest.MintUSDC, ExpectedOutputMint: jupitertest.MintSOL,
		ExpectedTaker: takerWallet.PublicKey.String(), ExpectedMinOut: qty(6_400_000), MaxSlippageBPS: bps(100), MaxPriceImpactBPS: bps(50),
		RequirePriceImpact: true, CurrentBlockHeight: 250_000_000, MinBlockHeightMargin: 20, RequireTransaction: true,
	}))

	call := h.srv.Calls()[0]
	require.Equal(t, "GET", call.Method)
	require.True(t, call.HasHeader(jupiter.HeaderAPIKey, "contract-test-api-key"))
	require.Equal(t, jupitertest.MintUSDC, call.Query.Get("inputMint"))
	require.Equal(t, "1000000", call.Query.Get("amount"), "amount is sent as a string")
	require.Equal(t, provider.Healthy, h.tracker.Health())
}

func TestContract_Order_QuoteOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/order", fixtureJSON(t, "order_quote_only.json"))
	req := orderReq()
	req.TakerPubkey = ""
	o, err := h.client.Order(context.Background(), req)
	require.NoError(t, err)
	require.False(t, o.HasTransaction)
	require.Empty(t, o.UnsignedTransaction)
	require.Nil(t, o.PlatformFee)
	require.Empty(t, h.srv.Calls()[0].Query.Get("taker"))
	err = jupiter.ValidateQuote(o, now, jupiter.ValidationPolicy{RequireTransaction: true})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestContract_Order_InvalidResponse_FloatAmount(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/order", fixtureJSON(t, "order_float_amount.json"))
	_, err := h.client.Order(context.Background(), orderReq())
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, "inAmount", e.Fields["field"])
	require.NotEmpty(t, e.Fields["raw_ref"], "the offending body is archived")
	require.Equal(t, 1, h.srv.CallCount("GET", "/swap/v2/order"), "a malformed body is not retried")
}

func TestContract_Order_MissingRequiredField(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/order", fixtureJSON(t, "order_missing_required.json"))
	_, err := h.client.Order(context.Background(), orderReq())
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, "otherAmountThreshold", e.Fields["field"])
}

func TestContract_Order_BuildErrorCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/order", fixtureJSON(t, "order_build_error.json"))
	_, err := h.client.Order(context.Background(), orderReq())
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, "INSUFFICIENT_SOL_FOR_GAS", e.Fields["reason"])
	require.Equal(t, int64(2), e.Fields["provider_code"])
	require.Equal(t, "contract-order-build-error", e.Fields["provider_request_id"])
}

func TestContract_Order_Timeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *jupiter.Config) { c.OrderTimeout = hangTimeout; c.MaxRetries = 1 })
	h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.Response{Hang: true})
	_, err := h.client.Order(context.Background(), orderReq())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, true, e.Fields["timed_out"])
	require.Equal(t, 2, h.srv.CallCount("GET", "/swap/v2/order"), "reads are SAFE_RETRY: retried once within budget")
	require.Equal(t, 2, h.archive.Len())
	require.NotEmpty(t, h.archive.Items()[0].TransportError)
}

func TestContract_Order_RateLimited_RetryAfter(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/order",
		jupitertest.Response{Status: 429, Headers: map[string]string{"Retry-After": "3", jupiter.HeaderRateLimitRemaining: "-1", jupiter.HeaderRateLimitReset: strconv.FormatInt(now.Unix()+3, 10)}, Body: fixture(t, "rate_limited.txt")},
		fixtureJSON(t, "order_valid.json"),
	)
	o, err := h.client.Order(context.Background(), orderReq())
	require.NoError(t, err)
	require.Equal(t, fixtureRequestID, o.QuoteID)
	require.Equal(t, 2, h.srv.CallCount("GET", "/swap/v2/order"))
	require.Equal(t, []time.Duration{3 * time.Second}, h.sleeps.waits, "Retry-After honored exactly")

	h2 := newHarness(t, func(c *jupiter.Config) { c.MaxRetries = 2 })
	h2.srv.Enqueue("GET", "/swap/v2/order", jupitertest.Response{Status: 429, Headers: map[string]string{jupiter.HeaderRateLimitReset: strconv.FormatInt(now.Unix()+2, 10)}, Body: fixture(t, "rate_limited.txt")})
	_, err = h2.client.Order(context.Background(), orderReq())
	require.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.NotNil(t, e.RetryAfter)
	require.Equal(t, 2*time.Second, *e.RetryAfter, "x-ratelimit-reset drives RetryAfter when Retry-After is absent")
	require.Equal(t, 3, h2.srv.CallCount("GET", "/swap/v2/order"))
	require.Equal(t, 3, e.Fields["attempts"])
}

func TestContract_Order_ServerError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *jupiter.Config) { c.MaxRetries = 5 })
	h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.Response{Status: 503, Body: []byte(`{"error":"upstream unavailable"}`)})
	_, err := h.client.Order(context.Background(), orderReq())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	require.Equal(t, 6, h.srv.CallCount("GET", "/swap/v2/order"), "bounded: 1 + MaxRetries")
	require.Len(t, h.sleeps.waits, 5)
	for _, w := range h.sleeps.waits {
		require.LessOrEqual(t, w, 4*time.Millisecond)
	}
	require.Equal(t, provider.Unhealthy, h.tracker.Health(), "health sampled per attempt")
}

func TestContract_Order_StaleQuote(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/order", fixtureJSON(t, "order_rfq_expired.json"))
	o, err := h.client.Order(context.Background(), orderReq())
	require.NoError(t, err, "the client returns the order; freshness is a validation decision")
	require.NotNil(t, o.RFQ)
	require.Equal(t, "rfq-quote-42", o.RFQ.QuoteID)
	require.False(t, o.ExpiresAtAssumed, "RFQ routes carry the documented expireAt")
	require.Equal(t, now.Add(-10*time.Second), o.ExpiresAt)

	err = jupiter.ValidateQuote(o, now, jupiter.ValidationPolicy{})
	require.Equal(t, errs.CodeQuoteExpired, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Contains(t, e.Fields["reasons"], jupiter.ReasonExpired)

	fresh := o
	fresh.ExpiresAt = now.Add(time.Minute)
	err = jupiter.ValidateQuote(fresh, now, jupiter.ValidationPolicy{CurrentBlockHeight: 250_000_000, MinBlockHeightMargin: 10})
	require.Equal(t, errs.CodeQuoteExpired, errs.CodeOf(err), "lastValidBlockHeight 249999990 is behind the chain")
	e, _ = errs.As(err)
	require.Contains(t, e.Fields["reasons"], jupiter.ReasonBlockHeight)
}

func TestContract_Order_UnexpectedStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []int{302, 418, 204} {
		h := newHarness(t, nil)
		h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.Response{Status: status, Body: []byte("surprise")})
		_, err := h.client.Order(context.Background(), orderReq())
		if status == 204 {
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "2xx without a body is a decode failure")
		} else {
			require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err), "status %d", status)
		}
		require.Equal(t, 1, h.srv.CallCount("GET", "/swap/v2/order"))
	}
}

func TestContract_Order_400NoRoute(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.Response{Status: 400, Body: fixture(t, "order_400_no_route.json")})
	_, err := h.client.Order(context.Background(), orderReq())
	require.Equal(t, errs.CodeVenueLiquidityInsufficient, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, "contract-order-400", e.Fields["provider_request_id"])
	require.Equal(t, 1, h.srv.CallCount("GET", "/swap/v2/order"))
}

func TestContract_Order_AuthRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/order", jupitertest.Response{Status: 403, Headers: map[string]string{jupiter.HeaderGatewayRequestID: "gw-403"}, Body: []byte(`{"error":"missing Swap permission"}`)})
	_, err := h.client.Order(context.Background(), orderReq())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	evs := h.security.Events()
	require.Len(t, evs, 1)
	require.Equal(t, jupiter.SecurityEventProviderAuthRejected, evs[0].Kind)
	require.Equal(t, 403, evs[0].HTTPStatus)
	require.Equal(t, "gw-403", evs[0].GatewayRequestID)
	require.Equal(t, int64(10_000), h.tracker.Snapshot(now).ErrorRateBPS, "auth failures count as failed health samples")
}

// ---- /execute ---------------------------------------------------------------

func TestContract_Execute_Success(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.srv.Enqueue("POST", "/swap/v2/execute", fixtureJSON(t, "execute_success.json"))
	signed := signedFixture(t)
	res, err := h.client.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: signed, RequestID: fixtureRequestID, LastValidBlockHeight: fixtureLVBH})
	require.NoError(t, err)
	require.Equal(t, jupiter.ExecuteSuccess, res.Status)
	require.Equal(t, fixtureSignature, res.Signature)
	require.Equal(t, uint64(250_000_101), res.Slot)
	require.Equal(t, int64(0), res.ProviderCode)
	require.Equal(t, "6512345", res.OutputAmountResult.String())
	require.Equal(t, "6500000", res.TotalOutputAmount.String())
	require.Len(t, res.SwapEvents, 1)
	require.Equal(t, "6512345", res.SwapEvents[0].OutputAmount.String())
	require.NotEmpty(t, res.RawRef)
	require.Len(t, res.RawHash, 32)

	call := h.srv.Calls()[0]
	require.Equal(t, "POST", call.Method)
	var body map[string]any
	require.NoError(t, json.Unmarshal(call.Body, &body))
	require.Equal(t, base64.StdEncoding.EncodeToString(signed), body["signedTransaction"])
	require.Equal(t, fixtureRequestID, body["requestId"])
	require.Equal(t, fixtureLVBH, body["lastValidBlockHeight"], "sent as the /order string, unchanged")
	require.Len(t, body, 3, "only the documented fields are sent")
}

func TestContract_Execute_Timeout_SubmissionUnknown_NoSecondCall(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *jupiter.Config) { c.ExecuteTimeout = hangTimeout; c.MaxRetries = 5 })
	h.srv.Enqueue("POST", "/swap/v2/execute", jupitertest.Response{Hang: true})
	signed := signedFixture(t)
	res, err := h.client.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: signed, RequestID: fixtureRequestID})
	require.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))
	e, _ := errs.As(err)
	require.Equal(t, true, e.Fields["timed_out"])
	require.Equal(t, true, e.Fields["submitted"])
	require.Equal(t, fixtureSignature, e.Fields["signature"], "the signature to reconcile is known even without a response")
	require.Equal(t, fixtureSignature, res.Signature)
	require.Equal(t, 1, h.srv.CallCount("POST", "/swap/v2/execute"), "UNKNOWN_EFFECT_WRITE: exactly one HTTP call, ever")
	require.Empty(t, h.sleeps.waits)
	require.Equal(t, 1, h.archive.Len())
	require.Equal(t, 0, h.archive.Items()[0].ResponseStatus)
}

func TestContract_Execute_Failed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.srv.Enqueue("POST", "/swap/v2/execute", fixtureJSON(t, "execute_failed.json"))
	res, err := h.client.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: signedFixture(t), RequestID: fixtureRequestID})
	require.NoError(t, err, "Failed is a result, not an error: the caller reconciles on chain before treating it as final")
	require.Equal(t, jupiter.ExecuteFailed, res.Status)
	require.Equal(t, int64(-1001), res.ProviderCode)
	require.Equal(t, fixtureSignature, res.Signature)
}

func TestContract_Execute_ErrorStatuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		resp      jupitertest.Response
		code      errs.Code
		submitted bool
	}{
		{"400 missing cached order", jupitertest.Response{Status: 400, Body: fixture(t, "execute_400_missing_order.json")}, errs.CodeQuoteExpired, false},
		{"500 with signature", jupitertest.Response{Status: 500, Body: fixture(t, "execute_500.json")}, errs.CodeSubmissionStateUnknown, true},
		{"429", jupitertest.Response{Status: 429, Headers: map[string]string{"Retry-After": "1"}, Body: fixture(t, "rate_limited.txt")}, errs.CodeRateLimited, false},
		{"401", jupitertest.Response{Status: 401, Body: []byte(`{"error":"unauthorized"}`)}, errs.CodeProviderUnavailable, false},
		{"unexpected 302", jupitertest.Response{Status: 302, Body: []byte("moved")}, errs.CodeSubmissionStateUnknown, true},
		{"200 undecodable", jupitertest.JSON(`{"status":"Success","slot":1}`), errs.CodeSubmissionStateUnknown, true},
		{"200 foreign signature", jupitertest.JSON(`{"status":"Success","signature":"1111111111111111111111111111111111111111111111111111111111111111111111111111111111111111","slot":"1"}`), errs.CodeSubmissionStateUnknown, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, func(c *jupiter.Config) { c.MaxRetries = 5 })
			h.srv.Enqueue("POST", "/swap/v2/execute", tc.resp)
			_, err := h.client.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: signedFixture(t), RequestID: fixtureRequestID})
			require.Equal(t, tc.code, errs.CodeOf(err), "%v", err)
			e, _ := errs.As(err)
			require.Equal(t, tc.submitted, e.Fields["submitted"])
			require.Equal(t, 1, h.srv.CallCount("POST", "/swap/v2/execute"), "never a second call")
		})
	}
}

// ---- /build, status, evidence ----------------------------------------------------

func TestContract_Build_Valid(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/build", fixtureJSON(t, "build_valid.json"))
	s := money.BPS(50)
	r, err := h.client.Build(context.Background(), jupiter.BuildRequest{
		InputMint: jupitertest.MintUSDC, OutputMint: jupitertest.MintSOL, Amount: money.QuantityFromInt64(1_000_000),
		Taker: takerWallet.PublicKey.String(), SlippageBPS: &s,
	})
	require.NoError(t, err)
	require.Equal(t, "6467500", r.OtherAmountThreshold.String())
	require.Equal(t, money.BPS(2), r.PriceImpactBPS, "priceImpactPct 0.000123 (fraction) -> 1.23 bps -> RoundCeil 2")
	require.Len(t, r.ComputeBudgetInstructions, 2)
	require.Equal(t, jupiter.DefaultProgramID, r.SwapInstruction.ProgramID)
	require.True(t, r.SwapInstruction.Accounts[1].IsSigner)
	require.Nil(t, r.CleanupInstruction)
	require.Nil(t, r.TipInstruction)
	require.Equal(t, []string{"11111111111111111111111111111111"}, r.AddressLookupTables["AddressLookupTab1e1111111111111111111111111"])
	require.Equal(t, uint64(250_000_150), r.LastValidBlockHeight)
	require.Equal(t, now, r.BlockhashFetchedAt)
	require.True(t, r.ComputeUnitLimitUnavailable, "/build never returns a CU limit; simulate to size it")
	require.True(t, r.ExpiresAtAssumed)
	require.NotEmpty(t, r.Blockhash)
	require.Equal(t, "50", h.srv.Calls()[0].Query.Get("slippageBps"))
}

func TestContract_Status_Unsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	_, err := h.client.Status(context.Background(), fixtureSignature)
	require.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	require.Empty(t, h.srv.Calls())
	require.Equal(t, provider.CodeComplete, h.client.VerificationLabel())
	require.Equal(t, "EB-011", jupiter.LiveVerificationBlocker)
}

func TestContract_Evidence_RedactedRequestFullResponse(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.srv.Enqueue("GET", "/swap/v2/order", fixtureJSON(t, "order_valid.json"))
	o, err := h.client.Order(context.Background(), orderReq())
	require.NoError(t, err)
	items := h.archive.Items()
	require.Len(t, items, 1)
	ev := items[0]
	require.Equal(t, jupiter.ProviderName, ev.Provider)
	require.Equal(t, jupiter.OpOrder, ev.Operation)
	require.Equal(t, observability.RedactedMarker, ev.RequestHeaders.Get(jupiter.HeaderAPIKey))
	require.NotContains(t, string(ev.RequestBody)+ev.RequestURL, "contract-test-api-key")
	require.JSONEq(t, string(fixture(t, "order_valid.json")), string(ev.ResponseBody), "the full raw response is retained")
	sum := sha256.Sum256(ev.ResponseBody)
	require.Equal(t, sum[:], ev.ResponseHash)
	require.Equal(t, ev.ResponseHash, o.RawHash)
	require.Equal(t, o.RawRef, "mem://jupiter/order/1/"+hexPrefix(ev.ResponseHash))
	require.Equal(t, 200, ev.ResponseStatus)
	require.NotEmpty(t, ev.GatewayRequestID)
}

// TestContract_FakeMatchesLiveShape runs the same validation policy against
// the Fake so that adapters developed against it cannot diverge from what
// the live client returns.
func TestContract_FakeMatchesLiveShape(t *testing.T) {
	t.Parallel()
	f, err := jupiter.NewFake(jupiter.FakeConfig{Env: config.EnvTest, Clock: clock.NewFake(now), Rates: map[string]jupiter.FakeRate{jupitertest.MintUSDC + ">" + jupitertest.MintSOL: {Num: 65, Den: 10}}})
	require.NoError(t, err)
	o, err := f.Order(context.Background(), orderReq())
	require.NoError(t, err)
	require.Equal(t, "6500000", o.OutAmount.String())
	require.NoError(t, jupiter.ValidateQuote(o, now.Add(time.Second), jupiter.ValidationPolicy{
		MaxAge: 10 * time.Second, ExpectedInputMint: jupitertest.MintUSDC, ExpectedOutputMint: jupitertest.MintSOL,
		ExpectedTaker: takerWallet.PublicKey.String(), ExpectedMinOut: qty(6_400_000), MaxSlippageBPS: bps(100), MaxPriceImpactBPS: bps(50),
		RequirePriceImpact: true, CurrentBlockHeight: 250_000_000, MinBlockHeightMargin: 20, RequireTransaction: true,
	}))
	signed := jupitertest.Sign(t, o.UnsignedTransaction, takerWallet)
	res, err := f.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: signed, RequestID: o.QuoteID, LastValidBlockHeight: o.LastValidBlockHeightRaw})
	require.NoError(t, err)
	require.Equal(t, jupiter.ExecuteSuccess, res.Status)
	require.Equal(t, jupitertest.SignatureOf(t, signed), res.Signature)

	f.InjectFault(jupiter.OpExecute, jupiter.FaultTimeout)
	o2, err := f.Order(context.Background(), orderReq())
	require.NoError(t, err)
	_, err = f.Execute(context.Background(), jupiter.ExecuteRequest{SignedTransaction: jupitertest.Sign(t, o2.UnsignedTransaction, takerWallet), RequestID: o2.QuoteID})
	require.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))
	require.Len(t, f.Submissions(), 2, "the second landed even though the response was lost")
	require.True(t, f.Submissions()[1].ResponseLost)
}

func qty(v int64) *money.Quantity {
	q := money.QuantityFromInt64(v)
	return &q
}

func bps(v money.BPS) *money.BPS { return &v }

func hexPrefix(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 16)
	for _, c := range b[:8] {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}
