package jupiter

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
)

// ---- config ---------------------------------------------------------------

func TestConfig_Validate(t *testing.T) {
	t.Parallel()
	ok := DefaultConfig(config.EnvTest)
	require.NoError(t, ok.Validate())

	prod := DefaultConfig(config.EnvProd)
	require.Error(t, prod.Validate(), "prod requires an api key")
	prod.APIKeyRef = "aws-sm://jupiter/key"
	require.NoError(t, prod.Validate())
	prod.BaseURL = "http://api.jup.ag/swap/v2"
	require.Error(t, prod.Validate(), "https required in prod")
	prod.BaseURL = DefaultBaseURL
	prod.APIKeyRef = "plain-value"
	require.Error(t, prod.Validate(), "plain secrets rejected in prod")

	sandbox := DefaultConfig(config.EnvTest)
	sandbox.Mode = config.ProviderModeSandbox
	require.Error(t, sandbox.Validate())

	bad := DefaultConfig(config.EnvTest)
	bad.ProgramID = "not-a-key"
	require.Error(t, bad.Validate())
	bad = DefaultConfig(config.EnvTest)
	bad.OrderTimeout = 0
	require.Error(t, bad.Validate())
	bad = DefaultConfig(config.EnvTest)
	bad.MaxRetries = 11
	require.Error(t, bad.Validate())
	bad = DefaultConfig(config.EnvTest)
	bad.BaseURL = "https://api.jup.ag/swap/v2?x=1"
	require.Error(t, bad.Validate())
	bad = DefaultConfig("NOPE")
	require.Error(t, bad.Validate())

	fake := DefaultConfig(config.EnvDev)
	fake.Mode = config.ProviderModeFake
	fake.BaseURL = ""
	require.NoError(t, fake.Validate(), "fake ignores transport settings")
	fake.Env = config.EnvStaging
	require.Error(t, fake.Validate())

	fp := FromProviderConfig(config.EnvTest, config.ProviderConfig{Mode: config.ProviderModeLive, BaseURL: " https://x.example/swap/v2 ", APIKeyRef: "env://K", Timeout: 3 * time.Second})
	require.Equal(t, "https://x.example/swap/v2", fp.BaseURL)
	require.Equal(t, 3*time.Second, fp.ExecuteTimeout)
	require.Equal(t, config.SecretRef("env://K"), fp.APIKeyRef)
	fp = FromProviderConfig(config.EnvTest, config.ProviderConfig{Mode: config.ProviderModeLive})
	require.Equal(t, DefaultBaseURL, fp.BaseURL)
	require.Equal(t, DefaultOrderTimeout, fp.OrderTimeout)
}

// ---- canonical JSON --------------------------------------------------------

func TestCanonicalJSON(t *testing.T) {
	t.Parallel()
	out, err := canonicalJSON([]byte(` {"b": [1.50, 2e3, {"z":null,"a":"x<y"}], "a": true} `))
	require.NoError(t, err)
	require.Equal(t, `{"a":true,"b":[1.50,2e3,{"a":"x<y","z":null}]}`, string(out), "numbers verbatim, keys sorted, no HTML escaping")
	_, err = canonicalJSON([]byte(`{"a":1} trailing`))
	require.Error(t, err)
	_, err = canonicalJSON([]byte(`{`))
	require.Error(t, err)
	summary, hash, err := routeSummary(nil)
	require.NoError(t, err)
	require.Equal(t, "[]", string(summary))
	require.Len(t, hash, 32)
}

// ---- retry ----------------------------------------------------------------

func TestRetryAfterFrom(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	h := http.Header{}
	require.Equal(t, time.Duration(0), retryAfterFrom(h, now))
	h.Set(HeaderRetryAfter, "3")
	require.Equal(t, 3*time.Second, retryAfterFrom(h, now))
	h.Set(HeaderRetryAfter, now.Add(90*time.Second).Format(http.TimeFormat))
	require.Equal(t, 90*time.Second, retryAfterFrom(h, now))
	h.Set(HeaderRetryAfter, now.Add(-time.Second).Format(http.TimeFormat))
	require.Equal(t, time.Duration(0), retryAfterFrom(h, now))
	h = http.Header{}
	h.Set(HeaderRateLimitReset, strconv.FormatInt(now.Unix()+5, 10))
	require.Equal(t, 5*time.Second, retryAfterFrom(h, now))
	h.Set(HeaderRateLimitReset, "1")
	require.Equal(t, time.Duration(0), retryAfterFrom(h, now), "reset in the past")
	h = http.Header{}
	h.Set(HeaderRateLimitRemaining, "-2")
	h.Set(HeaderRateLimitCurrent, "12")
	rl := parseRateLimit(h)
	require.True(t, rl.Present)
	require.Equal(t, int64(-2), rl.Remaining)
	require.Equal(t, int64(12), rl.Current)
}

func TestBackoffDelay(t *testing.T) {
	t.Parallel()
	zero := func(int64) int64 { return 0 }
	maxJ := func(n int64) int64 { return n - 1 }
	require.Equal(t, 100*time.Millisecond, backoffDelay(1, 200*time.Millisecond, 2*time.Second, zero))
	require.Equal(t, 200*time.Millisecond, backoffDelay(2, 200*time.Millisecond, 2*time.Second, zero))
	require.Equal(t, time.Second, backoffDelay(5, 200*time.Millisecond, 2*time.Second, zero), "capped at max then halved")
	require.Less(t, backoffDelay(5, 200*time.Millisecond, 2*time.Second, maxJ), 2*time.Second)
	require.Equal(t, time.Duration(0), backoffDelay(1, 0, time.Second, zero))
	for i := 0; i < 100; i++ {
		j := cryptoJitter(10)
		require.GreaterOrEqual(t, j, int64(0))
		require.Less(t, j, int64(10))
	}
	require.Equal(t, int64(0), cryptoJitter(0))
	require.True(t, retryableStatus(429))
	require.True(t, retryableStatus(503))
	require.False(t, retryableStatus(400))
}

// ---- errors ----------------------------------------------------------------

func TestErrorMapping(t *testing.T) {
	t.Parallel()
	require.Equal(t, errs.CodeVenueLiquidityInsufficient, mapRead400(OpOrder, errorBody{message: "No route found"}, "").Code)
	require.Equal(t, errs.CodeQuoteExpired, mapRead400(OpOrder, errorBody{message: "order expired"}, "").Code)
	require.Equal(t, errs.CodeValidationFailed, mapRead400(OpOrder, errorBody{message: "bad mint"}, "").Code)
	require.Equal(t, errs.CodeProviderUnavailable, mapReadStatus(OpOrder, 404, nil, "", 0, 1).Code)
	require.Equal(t, errs.CodeProviderUnavailable, mapReadStatus(OpOrder, 418, nil, "", 0, 1).Code)
	rl := mapReadStatus(OpOrder, 429, []byte("x"), "gw", 2*time.Second, 3)
	require.Equal(t, errs.CodeRateLimited, rl.Code)
	require.Equal(t, 2*time.Second, *rl.RetryAfter)
	require.Equal(t, "gw", rl.Fields[fieldGatewayRequestID])

	require.Equal(t, "INSUFFICIENT_FUNDS", mapOrderErrorCode("metis", 1, "", "r", "").Fields[fieldReason])
	require.Equal(t, "INSUFFICIENT_SOL_FOR_GAS", mapOrderErrorCode("metis", 2, "", "r", "").Fields[fieldReason])
	require.Equal(t, "MISSING_ATA", mapOrderErrorCode("jupiterz", 2, "", "r", "").Fields[fieldReason])
	require.Equal(t, "BELOW_GASLESS_MINIMUM", mapOrderErrorCode("metis", 3, "", "r", "").Fields[fieldReason])
	rfq := mapOrderErrorCode("JupiterZ", 3, "", "r", "")
	require.Equal(t, errs.CodeVenueUnavailable, rfq.Code)
	require.Equal(t, "UNDOCUMENTED_ERROR_CODE", mapOrderErrorCode("metis", 9, "", "r", "").Fields[fieldReason])

	c := int64(-2003)
	require.Equal(t, errs.CodeSubmissionStateUnknown, mapExecuteStatus(400, []byte(`{"error":"x","code":-2003}`), "", "sig", 0).Code)
	_ = c
	require.Equal(t, errs.CodeProviderUnavailable, mapExecuteStatus(404, nil, "", "sig", 0).Code)
	require.Equal(t, errs.CodeSubmissionStateUnknown, mapExecuteStatus(503, nil, "", "sig", 0).Code)
	u := mapExecuteStatus(500, []byte(`{"signature":"other","error":"boom"}`), "gw", "sig", 0)
	require.Equal(t, "other", u.Fields["provider_signature"])
	require.Equal(t, true, u.Fields[fieldSubmitted])
	require.True(t, isTimeout(timeoutErr{}))
	require.False(t, isTimeout(nil))
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// ---- evidence ---------------------------------------------------------------

func TestRedactHeaders(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set(HeaderAPIKey, "secret-value")
	h.Set("Authorization", "Bearer abcdefghijklmnopqrstuvwxyz0123456789")
	h.Set("Accept", "application/json")
	r := redactHeaders(h)
	require.Equal(t, observability.RedactedMarker, r.Get(HeaderAPIKey))
	require.Equal(t, observability.RedactedMarker, r.Get("Authorization"))
	require.Equal(t, "application/json", r.Get("Accept"))
	require.Equal(t, "secret-value", h.Get(HeaderAPIKey), "original untouched")
}

// ---- transaction -------------------------------------------------------------

func TestSummarizeTransaction(t *testing.T) {
	t.Parallel()
	_, err := summarizeTransaction(nil)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = summarizeTransaction(bytes.Repeat([]byte{1}, MaxTransactionBytes+1))
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = summarizeTransaction([]byte{0x80, 1, 2})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = signatureOfSigned([]byte{1, 2, 3})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	taker := solana.MustPublicKeyFromBase58("11111111111111111111111111111112")
	raw, err := BuildFakeRouteTransaction(FakeRouteParams{
		ProgramID: solana.MustPublicKeyFromBase58(DefaultProgramID), Taker: taker,
		InputMint: solana.PublicKeyFromBytes(sha256Of([]byte("mint-in"))), OutputMint: solana.MustPublicKeyFromBase58("So11111111111111111111111111111111111111112"),
		InAmount: 5, QuotedOutAmount: 7, SlippageBPS: 50, RecentBlockhash: solana.HashFromBytes(sha256Of([]byte("h"))),
		ComputeUnitLimit: 123_456, ComputeUnitPriceMicroLamports: 789,
	})
	require.NoError(t, err)
	s, err := summarizeTransaction(raw)
	require.NoError(t, err)
	require.True(t, s.versioned)
	require.Equal(t, taker.String(), s.feePayer)
	require.Equal(t, []string{taker.String()}, s.requiredSigners)
	require.True(t, s.hasUnitLimit)
	require.Equal(t, uint32(123_456), s.computeUnitLimit)
	require.True(t, s.hasUnitPrice)
	require.Equal(t, uint64(789), s.computeUnitPrice)
	require.Equal(t, 0, s.numSigned)
	require.Equal(t, 1, s.numSignatureSlots)
	_, err = signatureOfSigned(raw)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "unsigned")

	_, err = BuildFakeRouteTransaction(FakeRouteParams{})
	require.Error(t, err)
	sum := sha256.Sum256([]byte("global:route"))
	require.Equal(t, sum[:8], fakeRouteDiscriminator())
}

func FuzzSummarizeTransaction(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add([]byte{0x80, 0x00})
	f.Add(bytes.Repeat([]byte{0xff}, 100))
	f.Add([]byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if _, err := summarizeTransaction(data); err != nil && errs.CodeOf(err) != errs.CodeValidationFailed {
			t.Fatalf("unexpected code: %v", err)
		}
	})
}
