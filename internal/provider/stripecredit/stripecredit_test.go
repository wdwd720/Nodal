package stripecredit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider/stripesig"
	"github.com/nodal/controlplane/internal/webhook"
)

var (
	testNow    = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	testSecret = "whsec_credit_test_secret"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func testClient(t *testing.T, base string) *Client {
	t.Helper()
	c, err := NewClient(Options{
		Mode:              config.ProviderModeSandbox,
		Env:               config.EnvTest,
		BaseURL:           base,
		APIKey:            "sk_test_abc",
		WebhookSecret:     testSecret,
		Clock:             clock.NewFake(testNow),
		ContractReference: "acct_TEST/nodal-credit",
		SharedAccount:     true,
		// Required on a shared account. "ACTR" is the live prefix this
		// deployment's Stripe account actually carries, so the budget the
		// suffix is checked against is the real one.
		StatementDescriptorPrefix: "ACTR",
		StatementDescriptorSuffix: "NODAL CREDITS",
	})
	require.NoError(t, err)
	return c
}

// signed renders a delivery the way Stripe would.
func signed(t *testing.T, body string, at time.Time) (http.Header, []byte) {
	t.Helper()
	raw := []byte(body)
	h := http.Header{}
	h.Set(stripesig.Header, stripesig.HeaderValue(testSecret, raw, at))
	return h, raw
}

// nodalMeta renders the metadata a Nodal PaymentIntent carries.
func nodalMeta(fundingID, env string) string {
	return fmt.Sprintf(`{
	  %q: %q, %q: %q, %q: "acct-1", %q: "credit-pricing-v1",
	  %q: "deadbeef", %q: "10000", %q: %q
	}`,
		MetaWorkstream, MetaWorkstreamValue,
		MetaFundingID, fundingID,
		MetaUserID,
		MetaPricingVersion,
		MetaPricingHash,
		MetaCreditQuantity,
		MetaEnvironment, env)
}

func piEvent(eventType, piStatus, meta string) string {
	return fmt.Sprintf(`{
	  "id": "evt_test_1", "object": "event", "type": %q, "created": %d, "livemode": false,
	  "data": {"object": {
	    "id": "pi_test_1", "object": "payment_intent", "status": %q,
	    "amount": 10000, "currency": "usd", "livemode": false, "created": %d,
	    "client_secret": "pi_test_1_secret_x",
	    "metadata": %s
	  }}
	}`, eventType, testNow.Unix(), piStatus, testNow.Unix(), meta)
}

// ---------------------------------------------------------------------------
// signature and replay
// ---------------------------------------------------------------------------

func TestParseWebhook_ForgedSignatureIsRejected(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	body := piEvent(EventPaymentIntentSucceeded, "succeeded", nodalMeta(credit.NewFundingID().String(), "TEST"))

	// A signature computed with a secret we do not hold.
	raw := []byte(body)
	h := http.Header{}
	h.Set(stripesig.Header, stripesig.HeaderValue("whsec_attacker", raw, testNow))

	_, err := c.ParseWebhook(context.Background(), raw, h)
	require.Error(t, err)
	require.ErrorIs(t, err, webhook.ErrSignatureInvalid)
	require.Equal(t, errs.CodeWebhookSignatureInvalid, errs.CodeOf(err))
}

func TestParseWebhook_UnsignedIsRejected(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	body := piEvent(EventPaymentIntentSucceeded, "succeeded", nodalMeta(credit.NewFundingID().String(), "TEST"))
	_, err := c.ParseWebhook(context.Background(), []byte(body), http.Header{})
	require.ErrorIs(t, err, webhook.ErrSignatureInvalid)
}

func TestParseWebhook_ReplayOutsideToleranceIsRejected(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	body := piEvent(EventPaymentIntentSucceeded, "succeeded", nodalMeta(credit.NewFundingID().String(), "TEST"))

	// Correctly signed, but signed an hour ago. The signature verifies and the
	// delivery is still refused: that is the whole point of the tolerance.
	h, raw := signed(t, body, testNow.Add(-time.Hour))
	_, err := c.ParseWebhook(context.Background(), raw, h)
	require.ErrorIs(t, err, webhook.ErrTimestampOutOfTolerance)
}

func TestParseWebhook_FutureSignatureIsRejected(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	body := piEvent(EventPaymentIntentSucceeded, "succeeded", nodalMeta(credit.NewFundingID().String(), "TEST"))
	h, raw := signed(t, body, testNow.Add(time.Hour))
	_, err := c.ParseWebhook(context.Background(), raw, h)
	require.ErrorIs(t, err, webhook.ErrTimestampOutOfTolerance,
		"a signature from the future is as suspicious as a stale one")
}

func TestParseWebhook_ModifiedBodyIsRejected(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	body := piEvent(EventPaymentIntentSucceeded, "succeeded", nodalMeta(credit.NewFundingID().String(), "TEST"))
	h, raw := signed(t, body, testNow)

	// Sign 10000, deliver 999999. This is the single most valuable forgery
	// available to an attacker who can see a real delivery.
	tampered := []byte(strings.Replace(string(raw), `"amount": 10000`, `"amount": 999999`, 1))
	require.NotEqual(t, string(raw), string(tampered))
	_, err := c.ParseWebhook(context.Background(), tampered, h)
	require.ErrorIs(t, err, webhook.ErrSignatureInvalid)
}

// ---------------------------------------------------------------------------
// the shared-account defence
// ---------------------------------------------------------------------------

func TestParseWebhook_ForeignEventIsIgnored(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")

	// A genuine, correctly signed payment_intent.succeeded belonging to the
	// OTHER product on the same Stripe account. Nodal must not act on it.
	body := piEvent(EventPaymentIntentSucceeded, "succeeded", `{"order_id": "actorvia-1234"}`)
	h, raw := signed(t, body, testNow)

	ev, err := c.ParseWebhook(context.Background(), raw, h)
	require.NoError(t, err, "a foreign event is not an error; it is somebody else's business")
	require.True(t, ev.Foreign)
	require.Contains(t, ev.ForeignReason, MetaWorkstream)
	require.True(t, ev.FundingID.IsZero())
}

func TestParseWebhook_EventWithNoMetadataIsForeign(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	body := piEvent(EventPaymentIntentSucceeded, "succeeded", `{}`)
	h, raw := signed(t, body, testNow)
	ev, err := c.ParseWebhook(context.Background(), raw, h)
	require.NoError(t, err)
	require.True(t, ev.Foreign)
}

func TestParseWebhook_OtherEnvironmentIsForeign(t *testing.T) {
	t.Parallel()
	// The dangerous one. A staging deployment sharing a live Stripe account
	// would otherwise act on production purchases, and every symptom of that
	// bug looks like the system working correctly.
	c := testClient(t, "https://example.invalid") // Env is TEST
	body := piEvent(EventPaymentIntentSucceeded, "succeeded",
		nodalMeta(credit.NewFundingID().String(), "PROD"))
	h, raw := signed(t, body, testNow)

	ev, err := c.ParseWebhook(context.Background(), raw, h)
	require.NoError(t, err)
	require.True(t, ev.Foreign)
	require.Contains(t, ev.ForeignReason, "PROD")
	require.True(t, ev.FundingID.IsZero(),
		"a foreign event must never carry a funding id into the service")
}

func TestParseWebhook_NodalEventIsRecognized(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	id := credit.NewFundingID()
	body := piEvent(EventPaymentIntentSucceeded, "succeeded", nodalMeta(id.String(), "TEST"))
	h, raw := signed(t, body, testNow)

	ev, err := c.ParseWebhook(context.Background(), raw, h)
	require.NoError(t, err)
	require.False(t, ev.Foreign, ev.ForeignReason)
	require.True(t, ev.Recognized)
	require.Equal(t, id, ev.FundingID)
	require.Equal(t, credit.PurchaseSucceeded, ev.Snapshot.Status)
	require.Equal(t, "pi_test_1", ev.Snapshot.ProviderReference)
	require.Equal(t, int64(10000), ev.Snapshot.Amount.Minor())
	require.Equal(t, "stripe_credit", ev.Identity.Provider)
	require.Equal(t, "evt_test_1", ev.Identity.EventID)
}

func TestParseWebhook_MarkerWithoutAValidFundingIDIsForeign(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	body := piEvent(EventPaymentIntentSucceeded, "succeeded", nodalMeta("not-a-uuid", "TEST"))
	h, raw := signed(t, body, testNow)
	ev, err := c.ParseWebhook(context.Background(), raw, h)
	require.NoError(t, err)
	require.True(t, ev.Foreign)
	require.True(t, ev.FundingID.IsZero())
}

func TestParseWebhook_LivemodeMismatchIsRefused(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid") // sandbox
	body := strings.Replace(
		piEvent(EventPaymentIntentSucceeded, "succeeded", nodalMeta(credit.NewFundingID().String(), "TEST")),
		`"livemode": false,
	  "data"`, `"livemode": true,
	  "data"`, 1)
	h, raw := signed(t, body, testNow)
	_, err := c.ParseWebhook(context.Background(), raw, h)
	require.ErrorIs(t, err, webhook.ErrMalformed,
		"a live event reaching a sandbox adapter is an environment-isolation failure, not a routine ignore")
}

// ---------------------------------------------------------------------------
// status mapping
// ---------------------------------------------------------------------------

func TestStatusFor_EveryDocumentedStripeStatus(t *testing.T) {
	t.Parallel()
	// Exactly the statuses the PaymentIntent object reference declares.
	cases := map[string]credit.PurchaseStatus{
		"requires_payment_method": credit.PurchasePaymentMethodRequired,
		"requires_confirmation":   credit.PurchasePaymentMethodRequired,
		"requires_action":         credit.PurchaseAuthenticationRequired,
		"processing":              credit.PurchaseProcessing,
		"requires_capture":        credit.PurchaseAuthorized,
		"succeeded":               credit.PurchaseSucceeded,
		"canceled":                credit.PurchaseCanceled,
	}
	for in, want := range cases {
		got, ok := statusFor(in)
		require.True(t, ok, in)
		require.Equal(t, want, got, in)
	}
	_, ok := statusFor("a_status_stripe_has_not_invented_yet")
	require.False(t, ok)
}

func TestSnapshotFrom_UnknownStatusStopsRatherThanGuesses(t *testing.T) {
	t.Parallel()
	snap, err := snapshotFrom(paymentIntent{
		ID: "pi_x", Object: "payment_intent", Status: "invented_tomorrow", Amount: 500, Currency: "usd",
	})
	require.NoError(t, err)
	require.Equal(t, credit.PurchaseManualReview, snap.Status)
	require.Equal(t, "invented_tomorrow", snap.RawStatus, "the provider's own word is kept verbatim")
}

func TestFundingStateFor_TotalOverDeclaredStatuses(t *testing.T) {
	t.Parallel()
	for _, s := range credit.AllPurchaseStatuses() {
		st, ok := credit.FundingStateFor(s)
		require.True(t, ok, "no funding state declared for %s", s)
		require.True(t, st.Valid(), "%s maps to undeclared funding state %q", s, st)
	}
	_, ok := credit.FundingStateFor(credit.PurchaseStatus("NOPE"))
	require.False(t, ok, "an undeclared status must not map to anything")
}

// ---------------------------------------------------------------------------
// refunds and disputes
// ---------------------------------------------------------------------------

func chargeEvent(eventType string, amount, refunded int64) string {
	return fmt.Sprintf(`{
	  "id": "evt_c1", "object": "event", "type": %q, "created": %d, "livemode": false,
	  "data": {"object": {
	    "id": "ch_1", "object": "charge", "payment_intent": "pi_test_1",
	    "amount": %d, "amount_refunded": %d, "currency": "usd", "livemode": false
	  }}
	}`, eventType, testNow.Unix(), amount, refunded)
}

func TestParseWebhook_FullRefund(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	h, raw := signed(t, chargeEvent(EventChargeRefunded, 10000, 10000), testNow)
	ev, err := c.ParseWebhook(context.Background(), raw, h)
	require.NoError(t, err)
	require.True(t, ev.Recognized)
	require.Equal(t, credit.PurchaseRefunded, ev.Snapshot.Status)
	require.Equal(t, "pi_test_1", ev.Snapshot.ProviderReference,
		"a charge carries no Nodal metadata, so the payment intent it names is the link")
}

func TestParseWebhook_PartialRefundStopsForAPerson(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	h, raw := signed(t, chargeEvent(EventChargeRefunded, 10000, 2500), testNow)
	ev, err := c.ParseWebhook(context.Background(), raw, h)
	require.NoError(t, err)
	require.Equal(t, credit.PurchaseManualReview, ev.Snapshot.Status,
		"REFUNDED is terminal and would destroy every Credit the purchase issued; ignoring it leaves the platform out of pocket. Neither is defensible for a quarter refund")
	require.Equal(t, int64(2500), ev.Snapshot.AmountRefundedMinor)
}

func disputeEventJSON(eventType, status string) string {
	return fmt.Sprintf(`{
	  "id": "evt_d1", "object": "event", "type": %q, "created": %d, "livemode": false,
	  "data": {"object": {
	    "id": "dp_1", "object": "dispute", "charge": "ch_1", "payment_intent": "pi_test_1",
	    "status": %q, "amount": 10000, "currency": "usd", "livemode": false
	  }}
	}`, eventType, testNow.Unix(), status)
}

func TestParseWebhook_DisputeOutcomes(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	cases := []struct {
		eventType, status string
		want              credit.PurchaseStatus
	}{
		{EventDisputeCreated, "needs_response", credit.PurchaseDisputed},
		{EventDisputeClosed, "lost", credit.PurchaseChargeback},
		{EventDisputeClosed, "won", credit.PurchaseDisputeWon},
		{EventDisputeClosed, "warning_closed", credit.PurchaseDisputeWon},
		{EventDisputeFundsWithdrawn, "under_review", credit.PurchaseChargeback},
		{EventDisputeFundsReinstated, "won", credit.PurchaseDisputeWon},
		// An outcome this binary has never seen must stop, because guessing
		// decides whether a user keeps Credits they may not have paid for.
		{EventDisputeClosed, "some_new_outcome", credit.PurchaseManualReview},
	}
	for _, tc := range cases {
		h, raw := signed(t, disputeEventJSON(tc.eventType, tc.status), testNow)
		ev, err := c.ParseWebhook(context.Background(), raw, h)
		require.NoError(t, err)
		require.True(t, ev.Recognized)
		require.Equal(t, tc.want, ev.Snapshot.Status, "%s/%s", tc.eventType, tc.status)
		require.Equal(t, "pi_test_1", ev.Snapshot.ProviderReference)
	}
}

func TestParseWebhook_FundsWithdrawnBeatsThePaperwork(t *testing.T) {
	t.Parallel()
	// Stripe can withdraw funds while a dispute is still under review. The
	// funding must follow the money, not the status string.
	c := testClient(t, "https://example.invalid")
	h, raw := signed(t, disputeEventJSON(EventDisputeFundsWithdrawn, "needs_response"), testNow)
	ev, err := c.ParseWebhook(context.Background(), raw, h)
	require.NoError(t, err)
	require.Equal(t, credit.PurchaseChargeback, ev.Snapshot.Status)
}

func TestParseWebhook_UnmodelledEventTypeIsIgnoredNotRejected(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	body := fmt.Sprintf(`{"id":"evt_z","object":"event","type":"customer.created","created":%d,"livemode":false,"data":{"object":{}}}`, testNow.Unix())
	h, raw := signed(t, body, testNow)
	ev, err := c.ParseWebhook(context.Background(), raw, h)
	require.NoError(t, err)
	require.False(t, ev.Recognized)
	require.False(t, ev.Foreign, "not modelled is not the same as not ours")
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

func newCreateRequest() credit.CreatePurchaseRequest {
	return credit.CreatePurchaseRequest{
		IdempotencyKey: "idem-1",
		FundingID:      credit.NewFundingID(),
		AccountID:      accounts.NewAccountID(),
		Amount:         money.USDFromMinor(10000),
		Currency:       "USD",
		CreditQuantity: money.QuantityFromInt64(10000),
		PricingVersion: "credit-pricing-v1",
		PricingHash:    "deadbeef",
		Environment:    "TEST",
	}
}

func TestCreatePurchase_SendsIdempotencyKeyAndNamespacedMetadata(t *testing.T) {
	t.Parallel()
	var gotKey string
	var gotForm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Idempotency-Key")
		require.NoError(t, r.ParseForm())
		gotForm = r.Form.Encode()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"pi_created_1","object":"payment_intent","status":"requires_payment_method","amount":10000,"currency":"usd","livemode":false,"created":1757332800,"client_secret":"pi_created_1_secret_y","metadata":{}}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	req := newCreateRequest()
	sess, err := c.CreatePurchase(context.Background(), req)
	require.NoError(t, err)

	require.Equal(t, "idem-1", gotKey, "a retried create must be the same payment, not a second charge")
	require.Equal(t, "pi_created_1", sess.ProviderReference)
	require.Equal(t, credit.PurchasePaymentMethodRequired, sess.Status)
	require.NotEmpty(t, sess.ClientSecret)

	for _, want := range []string{
		"metadata%5B" + MetaWorkstream + "%5D=NODAL",
		"metadata%5B" + MetaFundingID + "%5D=" + req.FundingID.String(),
		"metadata%5B" + MetaEnvironment + "%5D=TEST",
		"metadata%5B" + MetaPricingVersion + "%5D=credit-pricing-v1",
		"amount=10000",
	} {
		require.Contains(t, gotForm, want)
	}
	require.NotContains(t, gotForm, "confirm=", "this adapter never confirms server-side; the Element does")
	require.NotContains(t, gotForm, "payment_method=", "no payment method ever reaches Nodal")
}

func TestCreatePurchase_ProviderChargingADifferentAmountIsRefused(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Asked for 10000, provider says 500.
		_, _ = w.Write([]byte(`{"id":"pi_x","object":"payment_intent","status":"requires_payment_method","amount":500,"currency":"usd","livemode":false,"created":1757332800,"metadata":{}}`))
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	_, err := c.CreatePurchase(context.Background(), newCreateRequest())
	require.Error(t, err)
	require.Contains(t, err.Error(), "the provider created")
}

func TestCreatePurchase_LivemodeMismatchIsRefused(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"pi_x","object":"payment_intent","status":"requires_payment_method","amount":10000,"currency":"usd","livemode":true,"created":1757332800,"metadata":{}}`))
	}))
	defer srv.Close()
	c := testClient(t, srv.URL) // sandbox
	_, err := c.CreatePurchase(context.Background(), newCreateRequest())
	require.Error(t, err)
	require.Contains(t, err.Error(), "livemode")
}

func TestCreatePurchase_RefusesACurrencyItDoesNotSellIn(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	req := newCreateRequest()
	req.Currency = "EUR"
	_, err := c.CreatePurchase(context.Background(), req)
	require.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
}

func TestCreatePurchase_RateLimitCarriesRetryAfter(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","code":"rate_limit"}}`))
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	_, err := c.CreatePurchase(context.Background(), newCreateRequest())
	require.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
}

func TestCreatePurchase_ServerErrorIsUnavailableNotFailure(t *testing.T) {
	t.Parallel()
	// A 5xx on a create is the dangerous one: the payment intent may exist.
	// It must never be reported as "the payment failed", because the caller
	// would then be free to start a second one.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	_, err := c.CreatePurchase(context.Background(), newCreateRequest())
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
}

// ---------------------------------------------------------------------------
// construction rules
// ---------------------------------------------------------------------------

func TestNewClient_KeyPrefixMustMatchMode(t *testing.T) {
	t.Parallel()
	_, err := NewClient(Options{
		Mode: config.ProviderModeLive, Env: config.EnvProd,
		APIKey: "sk_test_abc", WebhookSecret: "whsec_x", Clock: clock.NewFake(testNow),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "live secret key")

	_, err = NewClient(Options{
		Mode: config.ProviderModeSandbox, Env: config.EnvTest,
		APIKey: "sk_live_abc", WebhookSecret: "whsec_x", Clock: clock.NewFake(testNow),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "test secret key")
}

func TestNewClient_ProductionRefusesSandboxMode(t *testing.T) {
	t.Parallel()
	// Section 32: test Stripe object ids must never reach production. The
	// cheapest place to enforce that is refusing to build the adapter.
	_, err := NewClient(Options{
		Mode: config.ProviderModeSandbox, Env: config.EnvProd,
		APIKey: "sk_test_abc", WebhookSecret: "whsec_x", Clock: clock.NewFake(testNow),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "test Stripe objects must never reach production")
}

func TestNewClient_LiveRequiresHTTPS(t *testing.T) {
	t.Parallel()
	_, err := NewClient(Options{
		Mode: config.ProviderModeLive, Env: config.EnvProd, BaseURL: "http://api.stripe.com",
		APIKey: "sk_live_abc", WebhookSecret: "whsec_x", Clock: clock.NewFake(testNow),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "https")
}

// ---------------------------------------------------------------------------
// registry
// ---------------------------------------------------------------------------

func TestRegistry_RefusesAnAdapterWithNoContractOutsideSandbox(t *testing.T) {
	t.Parallel()
	c, err := NewClient(Options{
		Mode: config.ProviderModeLive, Env: config.EnvProd,
		APIKey: "sk_live_abc", WebhookSecret: "whsec_x", Clock: clock.NewFake(testNow),
	})
	require.NoError(t, err)
	require.Empty(t, c.Capabilities().ContractReference)

	err = credit.NewPurchaseRegistry(false).Register(c)
	require.Error(t, err)
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	require.NoError(t, credit.NewPurchaseRegistry(true).Register(c))
}

func TestRegistry_AcceptsTheConfiguredAdapter(t *testing.T) {
	t.Parallel()
	c := testClient(t, "https://example.invalid")
	require.NoError(t, credit.NewPurchaseRegistry(false).Register(c))
	caps := c.Capabilities()
	require.True(t, caps.SupportsHostedPaymentUI)
	require.True(t, caps.SupportsIdempotentCreate)
	require.True(t, caps.SupportsLookup)
	require.True(t, caps.SupportsDisputeEvents)
	require.True(t, caps.SharedProviderAccount,
		"this deployment shares its Stripe account, and the code must be able to see that")
}

// ---------------------------------------------------------------------------
// secrets
// ---------------------------------------------------------------------------

func TestClientSecretIsNotInTheSnapshot(t *testing.T) {
	t.Parallel()
	// The client secret authorises confirming the payment. It goes to the
	// browser once and must never appear in anything that gets persisted.
	c := testClient(t, "https://example.invalid")
	id := credit.NewFundingID()
	body := piEvent(EventPaymentIntentSucceeded, "succeeded", nodalMeta(id.String(), "TEST"))
	h, raw := signed(t, body, testNow)
	ev, err := c.ParseWebhook(context.Background(), raw, h)
	require.NoError(t, err)

	blob, err := json.Marshal(ev.Snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(blob), "secret_x")
}

func TestModelledEventTypes_AreAllHandled(t *testing.T) {
	t.Parallel()
	// Every type this adapter says it listens for must actually produce a
	// recognized event. Subscribing to something nothing handles is how a
	// dispute silently does nothing.
	c := testClient(t, "https://example.invalid")
	id := credit.NewFundingID().String()
	for _, typ := range ModelledEventTypes() {
		var body string
		switch {
		case strings.HasPrefix(typ, "payment_intent."):
			body = piEvent(typ, "succeeded", nodalMeta(id, "TEST"))
		case typ == EventChargeRefunded:
			body = chargeEvent(typ, 10000, 10000)
		default:
			body = disputeEventJSON(typ, "lost")
		}
		h, raw := signed(t, body, testNow)
		ev, err := c.ParseWebhook(context.Background(), raw, h)
		require.NoError(t, err, typ)
		require.True(t, ev.Recognized, "%s is subscribed to and not handled", typ)
		require.False(t, ev.Foreign, typ)
	}
}

// ---------------------------------------------------------------------------
// what the cardholder sees
// ---------------------------------------------------------------------------

func TestNewClient_SharedAccountRequiresAStatementDescriptorSuffix(t *testing.T) {
	t.Parallel()
	// The live Actorvia account's static descriptor is "ACTORVIA". Without a
	// suffix, a Nodal Credit purchase appears on the cardholder's statement
	// under that name. Stripe's own guidance on running multiple businesses
	// from separate accounts names exactly this as a cause of disputes -- and
	// on this integration a dispute also destroys the Credits it bought.
	_, err := NewClient(Options{
		Mode: config.ProviderModeSandbox, Env: config.EnvTest,
		APIKey: "sk_test_x", WebhookSecret: "whsec_x", Clock: clock.NewFake(testNow),
		SharedAccount: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "under that product's name")

	// An account of Nodal's own needs no suffix, because there is no other
	// product to be confused with.
	_, err = NewClient(Options{
		Mode: config.ProviderModeSandbox, Env: config.EnvTest,
		APIKey: "sk_test_x", WebhookSecret: "whsec_x", Clock: clock.NewFake(testNow),
		SharedAccount: false,
	})
	require.NoError(t, err)
}

func TestSuffixBudget_MatchesStripesArithmetic(t *testing.T) {
	t.Parallel()
	// 22 characters total, including the "* " separator.
	require.Equal(t, 22, MaxStatementDescriptor)
	require.Equal(t, 16, SuffixBudget("ACTR"), `"ACTR* " leaves 16`)
	require.Equal(t, 13, SuffixBudget("RUNCLUB"), "the worked example in Stripe's own documentation")
	require.Equal(t, 20, SuffixBudget(""))
	require.Equal(t, 0, SuffixBudget(strings.Repeat("X", 40)), "a nonsense prefix leaves nothing, never a negative")
}

func TestNewClient_RefusesASuffixStripeWouldTruncate(t *testing.T) {
	t.Parallel()
	// Stripe truncates an over-long descriptor rather than refusing it, so
	// shipping one means shipping a descriptor nobody chose and discovering it
	// on a customer's statement.
	_, err := NewClient(Options{
		Mode: config.ProviderModeSandbox, Env: config.EnvTest,
		APIKey: "sk_test_x", WebhookSecret: "whsec_x", Clock: clock.NewFake(testNow),
		SharedAccount:             true,
		StatementDescriptorPrefix: "ACTR",
		StatementDescriptorSuffix: "NODAL CREDITS PURCHASE",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "would ship a descriptor nobody chose")

	// Exactly the budget is fine.
	_, err = NewClient(Options{
		Mode: config.ProviderModeSandbox, Env: config.EnvTest,
		APIKey: "sk_test_x", WebhookSecret: "whsec_x", Clock: clock.NewFake(testNow),
		SharedAccount:             true,
		StatementDescriptorPrefix: "ACTR",
		StatementDescriptorSuffix: strings.Repeat("N", 16),
	})
	require.NoError(t, err)
}

func TestNewClient_RefusesForbiddenDescriptorCharacters(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"NODAL<", "NODAL>", `NODAL\`, "NODAL'", `NODAL"`, "NODAL*"} {
		_, err := NewClient(Options{
			Mode: config.ProviderModeSandbox, Env: config.EnvTest,
			APIKey: "sk_test_x", WebhookSecret: "whsec_x", Clock: clock.NewFake(testNow),
			SharedAccount:             true,
			StatementDescriptorPrefix: "ACTR",
			StatementDescriptorSuffix: bad,
		})
		require.Error(t, err, "%q", bad)
		require.Contains(t, err.Error(), "character Stripe forbids", "%q", bad)
	}
}

func TestCreatePurchase_SendsTheStatementDescriptorSuffix(t *testing.T) {
	t.Parallel()
	var form string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		form = r.Form.Encode()
		_, _ = w.Write([]byte(`{"id":"pi_d1","object":"payment_intent","status":"requires_payment_method","amount":10000,"currency":"usd","livemode":false,"created":1757332800,"metadata":{}}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	_, err := c.CreatePurchase(context.Background(), newCreateRequest())
	require.NoError(t, err)
	require.Contains(t, form, "statement_descriptor_suffix=NODAL+CREDITS",
		"the cardholder must see ACTR* NODAL CREDITS, not ACTORVIA")
	require.NotContains(t, form, "statement_descriptor=",
		"a card payment may only set the suffix; the static descriptor is the account's")
}
