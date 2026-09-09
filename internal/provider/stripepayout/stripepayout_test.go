package stripepayout

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
)

var testNow = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func client(t *testing.T, base string, av payout.Availability) *Client {
	t.Helper()
	c, err := NewClient(Options{
		Mode: config.ProviderModeSandbox, Env: config.EnvTest, BaseURL: base,
		APIKey: "sk_test_x", Clock: clock.NewFake(testNow),
		Availability: av, ContractReference: "acct_TEST/nodal-payout",
	})
	require.NoError(t, err)
	return c
}

func request() payout.SubmitRequest {
	return payout.SubmitRequest{
		IdempotencyKey:       "po-idem-1",
		DestinationReference: "acct_connected_1",
		DestinationKind:      payout.DestinationCryptoWallet,
		Amount:               money.USDFromMinor(1000),
		Currency:             "USD",
		Reference:            "payout-req-1",
	}
}

// ---------------------------------------------------------------------------
// what the capability set is actually allowed to claim
// ---------------------------------------------------------------------------

func TestCapabilities_ReportOnlyWhatStripeDocuments(t *testing.T) {
	t.Parallel()
	caps := client(t, "https://example.invalid", payout.AvailabilityRequiresApplication).Capabilities()

	require.Equal(t, []string{"USDC"}, caps.SupportedAssets)
	require.Equal(t, []string{"base", "polygon"}, caps.SupportedNetworks)

	// The assertion this whole workstream turns on. The rest of the system is
	// built around Solana wallets and Stripe does not send USDC there.
	require.False(t, caps.SupportsNetwork("solana"),
		"Stripe Express processes USDC over Base and Polygon; claiming Solana would promise a payout that cannot be made")
	require.False(t, caps.SupportsNetwork("ethereum"))
	require.True(t, caps.SupportsNetwork("base"))
	require.True(t, caps.SupportsNetwork("polygon"))

	require.True(t, caps.SupportsAsset("usdc"), "asset matching is case-insensitive")
	require.False(t, caps.SupportsAsset("USDT"))
	require.False(t, caps.SupportsAsset("SOL"))
	require.False(t, caps.SupportsAsset(""))

	require.True(t, caps.RequiresConnect)
	require.True(t, caps.RequiresRecipientAccount)
	require.True(t, caps.RequiresKYC)
	require.True(t, caps.KYCPerformedByProvider,
		"with dashboard=express the requirements collector is Stripe, which is what keeps identity documents out of Nodal")
	require.True(t, caps.SupportsExternalWallet)
	require.True(t, caps.DestinationHeldByProvider,
		"the recipient links their wallet in the Express Dashboard; a Nodal-side address would be a second source of truth")

	require.True(t, caps.SupportsRecipientKind("individual"))
	require.True(t, caps.SupportsRecipientKind("sole_proprietor"))
	require.False(t, caps.SupportsRecipientKind("company"),
		"payouts to companies and non-profits are not supported yet")

	// Stripe publishes no bounds. Zero must read as unknown, never as
	// unlimited.
	require.True(t, caps.MinimumAmount.IsZero())
	require.True(t, caps.MaximumAmount.IsZero())

	// Only the crypto destination kind.
	require.True(t, caps.Supports(payout.DestinationCryptoWallet))
	require.False(t, caps.Supports(payout.DestinationBank))
	require.False(t, caps.Supports(payout.DestinationCardPush))
	require.False(t, caps.Supports(payout.DestinationFiatWallet))
}

func TestSupportedCountries_MatchThePublishedList(t *testing.T) {
	t.Parallel()
	// The count is asserted so that editing the list is a deliberate act with
	// a source, rather than something that drifts.
	require.Len(t, SupportedCountries(), 67)
	require.True(t, SupportsCountry("US"))
	require.True(t, SupportsCountry("de") == false, "Germany is not on the published list")
	require.True(t, SupportsCountry("GB") == false, "the United Kingdom is not on the published list")
	require.False(t, SupportsCountry("RU"))
	require.False(t, SupportsCountry(""))
}

func TestExcludedUSStates(t *testing.T) {
	t.Parallel()
	require.False(t, SupportsUSState("NY"))
	require.False(t, SupportsUSState("HI"))
	require.True(t, SupportsUSState("CA"))
	require.False(t, SupportsUSState(""),
		"not knowing which state is not the same as any state")
}

// ---------------------------------------------------------------------------
// availability is a refusal, not a comment
// ---------------------------------------------------------------------------

func TestSubmit_RefusedUntilTheAccountIsGrantedTheProduct(t *testing.T) {
	t.Parallel()
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	for _, av := range []payout.Availability{
		payout.AvailabilityUnknown,
		payout.AvailabilityNotOffered,
		payout.AvailabilityRequiresApplication,
		payout.AvailabilityApplicationPending,
		payout.AvailabilityApplicationDenied,
	} {
		c := client(t, srv.URL, av)
		_, err := c.Submit(context.Background(), request())
		require.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err), "availability %q", av)
	}
	require.False(t, called, "not one request may reach Stripe while the product is ungranted")
}

func TestAvailability_ZeroValueIsNotUsable(t *testing.T) {
	t.Parallel()
	// The default has to be the refusing one. An operator who forgets to set
	// this must get a refusal, never a payout.
	require.False(t, payout.Availability("").Usable())
	require.False(t, payout.AvailabilityRequiresApplication.Usable())
	require.False(t, payout.AvailabilityApplicationDenied.Usable())
	require.True(t, payout.AvailabilityLive.Usable())
	require.True(t, payout.AvailabilitySandbox.Usable())
}

func TestNewClient_LiveAvailabilityRequiresLiveMode(t *testing.T) {
	t.Parallel()
	_, err := NewClient(Options{
		Mode: config.ProviderModeSandbox, Env: config.EnvTest,
		APIKey: "sk_test_x", Clock: clock.NewFake(testNow),
		Availability: payout.AvailabilityLive,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "a test key cannot move real value")
}

// ---------------------------------------------------------------------------
// submit
// ---------------------------------------------------------------------------

func TestSubmit_TransferCarriesTheIdempotencyKeyInMetadata(t *testing.T) {
	t.Parallel()
	// The bug this test exists for: Lookup searches transfers by
	// metadata[nodal_idempotency_key]. Stripe's Idempotency-Key header is not
	// a queryable property of the object it created, so a key that lives only
	// in the header cannot be found afterwards -- and Lookup is the only thing
	// that resolves PAYOUT_STATUS_UNKNOWN. Without this metadata every
	// timed-out submission stays ambiguous forever.
	var form, header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		form = r.Form.Encode()
		header = r.Header.Get("Idempotency-Key")
		_, _ = w.Write([]byte(`{"id":"tr_1","object":"transfer","amount":1000,"currency":"usd","livemode":false,"created":1757332800}`))
	}))
	defer srv.Close()

	c := client(t, srv.URL, payout.AvailabilitySandbox)
	res, err := c.Submit(context.Background(), request())
	require.NoError(t, err)

	require.Equal(t, "po-idem-1", header)
	require.Contains(t, form, "metadata%5B"+MetaIdempotencyKey+"%5D=po-idem-1",
		"the key must be on the object, not only in the header, or Lookup can never find it")
	require.Contains(t, form, "metadata%5B"+MetaWorkstream+"%5D=NODAL")
	require.Contains(t, form, "destination=acct_connected_1")
	require.NotContains(t, form, "0x", "this adapter never submits a wallet address")

	require.Equal(t, payout.ProviderAccepted, res.Status,
		"a transfer reaches the connected account's balance; the wallet payout is a later event")
	require.Equal(t, "tr_1", res.ProviderReference)
}

func TestSubmit_RefusesAWalletAddressAsTheDestination(t *testing.T) {
	t.Parallel()
	c := client(t, "https://example.invalid", payout.AvailabilitySandbox)
	req := request()
	req.DestinationReference = "0x1234567890abcdef1234567890abcdef12345678"
	_, err := c.Submit(context.Background(), req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "connected account, not a wallet address")
}

func TestSubmit_RefusesNonCryptoDestinationKinds(t *testing.T) {
	t.Parallel()
	c := client(t, "https://example.invalid", payout.AvailabilitySandbox)
	for _, k := range []payout.DestinationKind{
		payout.DestinationBank, payout.DestinationCardPush, payout.DestinationFiatWallet,
	} {
		req := request()
		req.DestinationKind = k
		_, err := c.Submit(context.Background(), req)
		require.Equal(t, errs.CodeUnsupported, errs.CodeOf(err), "%s", k)
	}
}

func TestSubmit_TimeoutIsUnknownAndNeverFailure(t *testing.T) {
	t.Parallel()
	// The single most important error mapping in this package. If a timeout
	// were reported as a failure, the caller would release the reservation and
	// be free to submit again -- which is how a payout gets sent twice.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, err := NewClient(Options{
		Mode: config.ProviderModeSandbox, Env: config.EnvTest, BaseURL: srv.URL,
		APIKey: "sk_test_x", Clock: clock.NewFake(testNow), Timeout: time.Millisecond,
		Availability: payout.AvailabilitySandbox, ContractReference: "acct_TEST",
	})
	require.NoError(t, err)

	_, err = c.Submit(context.Background(), request())
	require.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))
	require.NotEqual(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
}

func TestSubmit_ServerErrorOnAWriteIsUnknown(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := client(t, srv.URL, payout.AvailabilitySandbox)
	_, err := c.Submit(context.Background(), request())
	require.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err),
		"a 5xx on a submission leaves the outcome genuinely unknown")
}

// ---------------------------------------------------------------------------
// lookup
// ---------------------------------------------------------------------------

func TestLookup_FindsTheTransferItSubmitted(t *testing.T) {
	t.Parallel()
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		_, _ = w.Write([]byte(`{"object":"search_result","data":[{"id":"tr_9","object":"transfer","amount":1000,"currency":"usd"}]}`))
	}))
	defer srv.Close()

	c := client(t, srv.URL, payout.AvailabilitySandbox)
	res, err := c.Lookup(context.Background(), "po-idem-1")
	require.NoError(t, err)
	require.Equal(t, payout.ProviderAccepted, res.Status)
	require.Equal(t, "tr_9", res.ProviderReference)
	require.Contains(t, gotQuery, MetaIdempotencyKey)
	require.Contains(t, gotQuery, "po-idem-1")
}

func TestLookup_NothingFoundIsTheOnlyAnswerThatLicensesResubmission(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"search_result","data":[]}`))
	}))
	defer srv.Close()
	c := client(t, srv.URL, payout.AvailabilitySandbox)
	res, err := c.Lookup(context.Background(), "po-idem-1")
	require.NoError(t, err)
	require.Equal(t, payout.ProviderFailed, res.Status,
		"the provider never took the request, so the caller may safely submit")
}

func TestLookup_TwoTransfersUnderOneKeyIsUnknownNotResolved(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"search_result","data":[{"id":"tr_1","object":"transfer"},{"id":"tr_2","object":"transfer"}]}`))
	}))
	defer srv.Close()
	c := client(t, srv.URL, payout.AvailabilitySandbox)
	res, err := c.Lookup(context.Background(), "po-idem-1")
	require.NoError(t, err)
	require.Equal(t, payout.ProviderUnknown, res.Status,
		"two objects under one key is a broken idempotency guarantee and the worst thing to resolve automatically")
}

func TestLookup_UsesAReadNotAReplayedWrite(t *testing.T) {
	t.Parallel()
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_, _ = w.Write([]byte(`{"object":"search_result","data":[]}`))
	}))
	defer srv.Close()
	c := client(t, srv.URL, payout.AvailabilitySandbox)
	_, err := c.Lookup(context.Background(), "po-idem-1")
	require.NoError(t, err)
	require.Equal(t, http.MethodGet, method,
		"discovering whether a write happened must not be done by sending a write")
	require.True(t, strings.HasSuffix(path, "/v1/transfers/search"), path)
}

func TestLookup_EscapesTheSearchLiteral(t *testing.T) {
	t.Parallel()
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		_, _ = w.Write([]byte(`{"object":"search_result","data":[]}`))
	}))
	defer srv.Close()
	c := client(t, srv.URL, payout.AvailabilitySandbox)
	_, err := c.Lookup(context.Background(), `a'b`)
	require.NoError(t, err)
	require.Contains(t, gotQuery, `a\'b`, "a quote in a key must not terminate the literal")
}

// ---------------------------------------------------------------------------
// registry
// ---------------------------------------------------------------------------

func TestRegistry_AcceptsTheAdapter(t *testing.T) {
	t.Parallel()
	c := client(t, "https://example.invalid", payout.AvailabilityRequiresApplication)
	require.NoError(t, payout.NewRegistry(false).Register(c))
}

func TestRegistry_RefusesACryptoAdapterThatNamesNoNetwork(t *testing.T) {
	t.Parallel()
	// An adapter that has not been read against a real product reports
	// nothing, and registering it would let a payout be attempted against a
	// destination nobody has confirmed the provider can reach.
	err := payout.NewRegistry(false).Register(unverified{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "names no supported network")
}

type unverified struct{}

func (unverified) Name() string { return "unverified_crypto" }
func (unverified) Capabilities() payout.Capabilities {
	return payout.Capabilities{
		SupportsCryptoPayout: true, SupportsLookup: true,
		SupportedAssets:   []string{"USDC"},
		ContractReference: "somewhere",
	}
}
func (unverified) Submit(context.Context, payout.SubmitRequest) (payout.SubmitResult, error) {
	return payout.SubmitResult{}, nil
}
func (unverified) Lookup(context.Context, string) (payout.SubmitResult, error) {
	return payout.SubmitResult{}, nil
}
