package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/eligibility"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/verification"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// --- fakes ------------------------------------------------------------------

type fakeVerification struct {
	snapshot    verification.Snapshot
	started     verification.Started
	session     verification.Session
	sandboxTier bool
	outcome     verification.SandboxOutcome
	err         error
}

func newFakeVerification() *fakeVerification {
	session := verification.Session{
		ID: verification.NewSessionID(), Purpose: verification.PurposePayoutKYC,
		Provider: "sandbox_verification", ProviderRef: "sandbox-verif-1",
		Status: verification.SessionPendingUserAction, JurisdictionCountry: "US", JurisdictionRegion: "CA",
		RulesVersion: rules.Version, Environment: "TEST", Sandbox: true, CreatedAt: testNow,
	}
	return &fakeVerification{
		sandboxTier: true,
		session:     session,
		started: verification.Started{
			Session: session, HostedURL: "sandbox:verification/sandbox-verif-1",
			Sandbox: true, SandboxControlPath: verification.SandboxControlPath,
		},
		snapshot: verification.Snapshot{
			AccountID: testAccountID, State: verification.StateStarted,
			Level:                 valuedomain.VerificationNodalIdentity,
			Jurisdiction:          rules.Jurisdiction{Country: "US", Region: "CA"},
			JurisdictionSupported: true, MinimumAge: 18,
			SanctionsState: compliance.SanctionsUnknown, Sandbox: true, RulesVersion: rules.Version,
			Provider: "sandbox_verification", ProviderAvailability: verification.AvailabilitySandbox,
			Session: &session,
			Checks: []verification.Check{{
				ID: verification.NewCheckID(), Kind: verification.CheckAge,
				Outcome: verification.OutcomeUnknown, Provider: "sandbox_verification",
				RulesVersion: rules.Version, Environment: "TEST", Sandbox: true, RecordedAt: testNow,
			}},
			Missing: []verification.Requirement{{
				Code: verification.RequirementSession, Detail: "your verification is open and has not been completed",
				Action: verification.ActionContinueVerification,
			}},
		},
	}
}

func (f *fakeVerification) SandboxTier() bool { return f.sandboxTier }

func (f *fakeVerification) Profile(context.Context, accounts.AccountID) (verification.Snapshot, error) {
	return f.snapshot, f.err
}

func (f *fakeVerification) Start(context.Context, StartVerification) (verification.Started, error) {
	return f.started, f.err
}

func (f *fakeVerification) Poll(context.Context, accounts.AccountID, verification.SessionID) (verification.Session, error) {
	return f.session, f.err
}

func (f *fakeVerification) SandboxOutcome(_ context.Context, _ accounts.AccountID, o verification.SandboxOutcome) (verification.Session, error) {
	f.outcome = o
	return f.session, f.err
}

type fakeEligibility struct {
	explanation eligibility.WithdrawalExplanation
	err         error
}

func newFakeEligibility() *fakeEligibility {
	in := eligibility.WithdrawalInput{
		Policy:     valuedomain.SandboxPolicy(),
		Verified:   valuedomain.VerificationNodalIdentity,
		ActiveCaps: map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		Holdings: []eligibility.OriginHolding{
			{
				Origin: valuedomain.OriginPurchased, OriginFloor: valuedomain.OriginPurchased,
				Quantity: money.QuantityFromInt64(5000),
				Finality: valuedomain.FinalitySettled, HeldDays: 30,
			},
			{
				Origin: valuedomain.OriginPromotional, OriginFloor: valuedomain.OriginPromotional,
				Quantity: money.QuantityFromInt64(9000),
				Finality: valuedomain.FinalityUnfunded, HeldDays: 30,
			},
			// The bucket D-131 is about: proceeds of the grant above. Its own
			// origin is one the sandbox policy releases and its floor is not,
			// so the route has to render the floor or the refusal reads as a
			// statement about the trade.
			{
				Origin: valuedomain.OriginMarketTradingProceeds, OriginFloor: valuedomain.OriginPromotional,
				Quantity: money.QuantityFromInt64(2000),
				Finality: valuedomain.FinalityUnfunded, HeldDays: 30,
			},
		},
		PolicyValid: true,
		Gross:       money.QuantityFromInt64(16000),
		Spendable:   money.QuantityFromInt64(16000),

		JurisdictionSupported: true,
		ProviderAvailable:     true,
		ProviderName:          "sandbox_payout",
		DestinationConfigured: true,
		Sandbox:               true,
	}
	return &fakeEligibility{explanation: eligibility.ExplainWithdrawal(in)}
}

func (f *fakeEligibility) Withdrawal(context.Context, accounts.AccountID) (eligibility.WithdrawalExplanation, error) {
	return f.explanation, f.err
}

type fakeConversion struct {
	destinations []payout.Destination
	added        AddPayoutDestination
	quote        payout.Quote
	provenance   []payout.ProvenanceSlice
	// openAfterDisable is what DisableDestination reports as still pointing at
	// the destination. The real adapter reads it inside the disabling
	// transaction (F-263).
	openAfterDisable []payout.RequestID
	err              error
}

func newFakeConversion() *fakeConversion {
	dest := payout.Destination{
		ID: payout.NewDestinationID(), AccountID: testAccountID, Kind: payout.DestinationBank,
		Provider: "sandbox_payout", ProviderReference: "sandbox-handle-checking-001",
		DisplayLabel: "Checking", MaskedDisplay: "****4242", Currency: "USD", Country: "US",
		Status: payout.DestinationVerified, Sandbox: true, CreatedAt: testNow,
	}
	return &fakeConversion{
		destinations: []payout.Destination{dest},
		quote: payout.Quote{
			ID: payout.NewQuoteID(), AccountID: testAccountID, DestinationID: dest.ID,
			Provider:      "sandbox_payout",
			GrossQuantity: money.QuantityFromInt64(500000000),
			FeeQuantity:   money.QuantityFromInt64(5000000),
			NetQuantity:   money.QuantityFromInt64(495000000),
			Currency:      "USD", GrossAmountMinor: 500, FeeAmountMinor: 5, NetAmountMinor: 495,
			MinimumOK: true, MinimumAmountMinor: 100,
			FeeModelVersion: "SANDBOX-PLACEHOLDER-NOT-A-PRICE",
			Sandbox:         true, ExpiresAt: testNow.Add(5 * time.Minute), CreatedAt: testNow,
		},
		provenance: []payout.ProvenanceSlice{{
			Origin: valuedomain.OriginPurchased, Quantity: money.QuantityFromInt64(500000000),
			ConsumptionRank: 4,
		}},
	}
}

func (f *fakeConversion) Destinations(context.Context, accounts.AccountID, int) ([]payout.Destination, error) {
	return f.destinations, f.err
}

func (f *fakeConversion) AddDestination(_ context.Context, r AddPayoutDestination) (payout.Destination, error) {
	f.added = r
	if f.err != nil {
		return payout.Destination{}, f.err
	}
	return f.destinations[0], nil
}

func (f *fakeConversion) DisableDestination(context.Context, accounts.AccountID, payout.DestinationID) (payout.Destination, []payout.RequestID, error) {
	d := f.destinations[0]
	d.Status = payout.DestinationDisabled
	return d, f.openAfterDisable, f.err
}

func (f *fakeConversion) Quote(context.Context, CreatePayoutQuote) (payout.Quote, []payout.ProvenanceSlice, error) {
	return f.quote, f.provenance, f.err
}

// --- tests ------------------------------------------------------------------

func accountQuery() string { return "?account_id=" + testAccountID.String() }

// The §24 profile area carries a state, a level, the evidence and what to do
// next — and no personal data at all.
func TestGetMeVerification_RendersTheProfileWithoutPII(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res := h.do(http.MethodGet, "/v1/me/verification"+accountQuery(), nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	body := res.raw()
	assert.Equal(t, "STARTED", body["state"])
	assert.Equal(t, "NODAL_IDENTITY", body["level"])
	assert.Equal(t, false, body["payout_ready"])
	assert.Equal(t, true, body["sandbox"], "a rehearsal-derived profile says so")
	assert.Equal(t, float64(18), body["minimum_age"])
	assert.NotEmpty(t, body["missing"], "a profile that is not ready says what is missing")
	assert.NotEmpty(t, body["checks"])

	// The list of things that must never appear. Each is a field some other
	// system would have and this one refuses to have.
	for _, forbidden := range []string{
		"document_id", "document_number", "ssn", "date_of_birth", "dob",
		"full_name", "hosted_url", "provider_token",
	} {
		assert.NotContains(t, res.Body.String(), forbidden,
			"the verification profile must never carry %q", forbidden)
	}
}

// The sandbox control is a sandbox-tier affordance and the handler refuses it
// before the service is asked. That is the first of three refusals; the service
// and a database CHECK are the other two.
func TestPostMeVerificationSandboxOutcome_RefusedOutsideASandboxTier(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body := map[string]any{"account_id": testAccountID.String(), "outcome": "VERIFIED"}

	h.ports.verification.sandboxTier = false
	res := h.do(http.MethodPost, "/v1/me/verification/sandbox-outcome", body,
		"Idempotency-Key", "sandbox-outcome-1")
	require.Equal(t, http.StatusForbidden, res.Code, res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	assert.Empty(t, h.ports.verification.outcome, "the service was never asked")

	h.ports.verification.sandboxTier = true
	res = h.do(http.MethodPost, "/v1/me/verification/sandbox-outcome", body,
		"Idempotency-Key", "sandbox-outcome-2")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.Equal(t, verification.SandboxVerified, h.ports.verification.outcome)
	assert.Equal(t, true, res.raw()["sandbox"])
}

// There is no default outcome, and the API cannot be talked into one: an
// outcome it does not declare is a validation failure, not an approval.
func TestPostMeVerificationSandboxOutcome_RefusesAnUndeclaredOutcome(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// "verified" is absent from this list on purpose: ParseSandboxOutcome
	// upper-cases and trims, so the canonical form of a declared outcome is
	// accepted however it was typed. What must never be accepted is a value
	// nobody declared.
	for _, outcome := range []string{"", "APPROVED", "YES", "VERIFIED_ISH", "PASS"} {
		res := h.do(http.MethodPost, "/v1/me/verification/sandbox-outcome",
			map[string]any{"account_id": testAccountID.String(), "outcome": outcome},
			"Idempotency-Key", "bad-outcome-"+outcome+"-0000")
		assert.NotEqualf(t, http.StatusOK, res.Code, "outcome %q must not be accepted", outcome)
	}
}

// A session start returns a hosted link and, for a rehearsal, the control that
// decides it — which is what makes a sandbox session visibly a rehearsal.
func TestPostMeVerificationSessions_ReturnsAHostedLinkAndTheSandboxLabel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res := h.do(http.MethodPost, "/v1/me/verification/sessions",
		map[string]any{
			"account_id": testAccountID.String(), "purpose": "PAYOUT_KYC",
			"jurisdiction_country": "US", "jurisdiction_region": "CA",
		}, "Idempotency-Key", "start-verification-1")
	require.Equal(t, http.StatusCreated, res.Code, res.Body.String())

	body := res.raw()
	assert.Equal(t, true, body["sandbox"])
	assert.Equal(t, verification.SandboxControlPath, body["sandbox_control_path"])
	assert.Equal(t, "sandbox:verification/sandbox-verif-1", body["hosted_url"],
		"the sandbox link is deliberately not navigable")

	// The jurisdiction is required, and the refusal says where the answer has
	// to come from rather than guessing.
	res = h.do(http.MethodPost, "/v1/me/verification/sessions",
		map[string]any{"account_id": testAccountID.String(), "jurisdiction_country": "usa"},
		"Idempotency-Key", "start-verification-2")
	require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
	assert.Contains(t, res.Body.String(), "network address")
}

// Eligibility is per origin, and REQUIRES_VERIFICATION is a next step rather
// than a denial.
func TestGetMeEligibility_ExplainsPerOrigin(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res := h.do(http.MethodGet, "/v1/me/eligibility"+accountQuery(), nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	body := res.raw()
	assert.Equal(t, false, body["eligible"])
	assert.Equal(t, true, body["verification_would_suffice"],
		"the value is there and its provenance is approved; the only obstacle is identity")
	assert.Equal(t, "NODAL_IDENTITY", body["current_verification"])
	assert.Equal(t, "PAYOUT_KYC", body["required_verification"])
	assert.Equal(t, true, body["sandbox"])

	buckets, ok := body["buckets"].([]any)
	require.True(t, ok)
	assert.Len(t, buckets, len(valuedomain.AllOrigins()),
		"every declared origin gets a bucket, including the empty ones")

	byOrigin := map[string]map[string]any{}
	for _, b := range buckets {
		m, ok := b.(map[string]any)
		require.True(t, ok)
		origin, ok := m["origin"].(string)
		require.True(t, ok)
		byOrigin[origin] = m
	}
	purchased := byOrigin["PURCHASED"]
	require.NotNil(t, purchased)
	assert.Equal(t, []any{"REQUIRES_VERIFICATION"}, purchased["reasons"])
	assert.Equal(t, true, purchased["verification_would_suffice"])

	promotional := byOrigin["PROMOTIONAL"]
	require.NotNil(t, promotional)
	assert.Contains(t, promotional["reasons"], "ORIGIN_NOT_WITHDRAWABLE")
	assert.NotContains(t, promotional, "verification_would_suffice",
		"verifying does not make a promotional grant withdrawable, and the product must not imply it does")
	assert.NotContains(t, promotional, "origin_floor",
		"a bucket whose floor is its own origin does not repeat itself")

	// The bucket D-131 is about. Its own origin is one the sandbox policy
	// releases; its floor is the grant that funded it, and the answer has to
	// name the grant or the refusal reads as a statement about the trade.
	proceeds := byOrigin["MARKET_TRADING_PROCEEDS"]
	require.NotNil(t, proceeds)
	assert.Equal(t, "PROMOTIONAL", proceeds["origin_floor"],
		"the person has to be able to read where the value came from, not only that it cannot leave")
	assert.Contains(t, proceeds["reasons"], "ORIGIN_NOT_WITHDRAWABLE")
	assert.Equal(t, "0", proceeds["withdrawable"])
	assert.NotContains(t, proceeds, "verification_would_suffice",
		"verifying does not release a grant that has been traded, and the product must not imply it does")
}

// The destination surface takes a provider token and refuses the thing the
// token replaces. It also never echoes the token back.
func TestPostMePayoutDestinations_RefusesRawAccountNumbers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, raw := range []string{"4242424242424242", "021000021", "GB82 WEST 1234 5698 7654 32"} {
		res := h.do(http.MethodPost, "/v1/me/payout-destinations",
			map[string]any{"account_id": testAccountID.String(), "kind": "BANK", "provider_token": raw},
			"Idempotency-Key", "dest-refuse-0001")
		require.Equalf(t, http.StatusBadRequest, res.Code, "%q: %s", raw, res.Body.String())
		assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
		assert.Emptyf(t, h.ports.conversion.added.ProviderToken, "%q reached the domain", raw)
		assert.NotContainsf(t, res.Body.String(), raw,
			"the refusal must not echo %q back into a log or a cache", raw)
	}

	res := h.do(http.MethodPost, "/v1/me/payout-destinations",
		map[string]any{
			"account_id": testAccountID.String(), "kind": "BANK",
			"provider_token": "sandbox-handle-checking-001", "masked_display": "****4242",
			"currency": "usd", "country": "US",
		}, "Idempotency-Key", "dest-accept-0001")
	require.Equal(t, http.StatusCreated, res.Code, res.Body.String())
	assert.Equal(t, "sandbox-handle-checking-001", h.ports.conversion.added.ProviderToken)
	assert.Equal(t, "USD", h.ports.conversion.added.Currency, "a currency is normalised before the domain sees it")

	body := res.raw()
	assert.Equal(t, "****4242", body["masked_display"])
	assert.Equal(t, true, body["usable"])
	assert.Equal(t, true, body["sandbox"])
	assert.NotContains(t, res.Body.String(), "sandbox-handle-checking-001",
		"the provider token is a credential for moving money and is never echoed")
}

// Disabling a destination names the payouts it has just stranded (F-263,
// D-132's sibling decision).
//
// payout.Service.Submit now refuses a destination that is not Usable(), which
// is what stops the sweep from handing a provider the token a person removed
// because it was compromised. The cost of that refusal is that the request
// stays in VERIFIED with its value held out of the balance until somebody
// cancels it -- so the response to the removal says which requests those are.
// A person who is not told has money that has simply gone quiet.
//
// The disable itself is never refused for having open requests: §25 makes
// removing a destination the act of somebody whose destination is compromised,
// and a removal an attacker can block by starting a payout is not a control.
func TestDeleteMePayoutDestinations_NamesThePayoutsItStranded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	stranded := []payout.RequestID{payout.NewRequestID(), payout.NewRequestID()}
	h.ports.conversion.openAfterDisable = stranded

	dest := h.ports.conversion.destinations[0].ID.String()
	res := h.do(http.MethodDelete, "/v1/me/payout-destinations/"+dest+accountQuery(), nil,
		"Idempotency-Key", "dest-disable-0001")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	body := res.raw()
	assert.Equal(t, "DISABLED", body["status"])
	assert.Equal(t, false, body["usable"])
	ids, ok := body["open_payout_ids"].([]any)
	require.Truef(t, ok, "the response does not say what the removal stranded: %s", res.Body.String())
	require.Len(t, ids, 2)
	for _, want := range stranded {
		assert.Contains(t, ids, want.String())
	}
}

// And it says nothing when there is nothing to say, rather than an empty list
// a client has to interpret.
func TestDeleteMePayoutDestinations_SaysNothingWhenNothingIsStranded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ports.conversion.openAfterDisable = nil

	dest := h.ports.conversion.destinations[0].ID.String()
	res := h.do(http.MethodDelete, "/v1/me/payout-destinations/"+dest+accountQuery(), nil,
		"Idempotency-Key", "dest-disable-0002")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.NotContains(t, res.Body.String(), "open_payout_ids")
}

// A destination says where it pays into, and the route refuses one that does
// not (F-228, D-122).
//
// The provider is asked `CanPayRecipient` before a destination is registered,
// and it cannot be asked about a recipient whose country nobody stated. The
// version of this route that let the field be omitted skipped the question
// entirely: the same body with "country":"FR" was refused 503
// RECIPIENT_COUNTRY_UNSUPPORTED, and without it the destination was accepted
// AND marked VERIFIED.
func TestPostMePayoutDestinations_RequiresTheCountryItPaysInto(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	before := h.ports.conversion.added.ProviderToken

	for _, country := range []any{nil, "", "usa", "U1"} {
		payload := map[string]any{
			"account_id": testAccountID.String(), "kind": "BANK",
			"provider_token": "sandbox-handle-nocountry",
		}
		if country != nil {
			payload["country"] = country
		}
		res := h.do(http.MethodPost, "/v1/me/payout-destinations", payload,
			"Idempotency-Key", "dest-country-0001")
		require.Equalf(t, http.StatusBadRequest, res.Code, "%v: %s", country, res.Body.String())
		assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
		assert.Contains(t, res.Body.String(), "country")
		assert.Equal(t, before, h.ports.conversion.added.ProviderToken,
			"a destination with no answerable country reached the domain")
	}

	// The region rides along when the caller gives one, so a provider that
	// excludes subdivisions has something to refuse on.
	res := h.do(http.MethodPost, "/v1/me/payout-destinations",
		map[string]any{
			"account_id": testAccountID.String(), "kind": "BANK",
			"provider_token": "sandbox-handle-with-region", "country": "us", "region": "ca",
		}, "Idempotency-Key", "dest-country-0002")
	require.Equal(t, http.StatusCreated, res.Code, res.Body.String())
	assert.Equal(t, "US", h.ports.conversion.added.Country, "a country is normalised before the domain sees it")
	assert.Equal(t, "CA", h.ports.conversion.added.Region)
}

// A quote carries both sides, the minimum judged net of fees, an expiry and the
// provenance that would leave — before anybody commits.
func TestPostPayoutsQuote_ShowsTheFeeTheNetAndWhatWouldLeave(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res := h.do(http.MethodPost, "/v1/payouts/quote",
		map[string]any{
			"account_id":     testAccountID.String(),
			"destination_id": h.ports.conversion.destinations[0].ID.String(),
			"amount":         "500000000",
		}, "Idempotency-Key", "quote-0000001")
	require.Equal(t, http.StatusCreated, res.Code, res.Body.String())

	body := res.raw()
	assert.Equal(t, "500000000", body["gross_quantity"])
	assert.Equal(t, "495000000", body["net_quantity"])
	assert.Equal(t, float64(5), body["fee_amount_minor"])
	assert.Equal(t, true, body["minimum_ok"])
	assert.Equal(t, true, body["sandbox"])
	assert.Equal(t, "SANDBOX-PLACEHOLDER-NOT-A-PRICE", body["fee_model_version"],
		"a placeholder fee says in words that it is not a price anybody agreed")
	assert.NotEmpty(t, body["expires_at"])
	assert.NotEmpty(t, body["provenance"], "a person sees what value would leave before committing")
}

// A deployment with none of this wired answers UNSUPPORTED rather than
// pretending: that is the honest state of a system with no identity vendor and
// no conversion contract.
func TestTheWithdrawalRoutesAnswerUnsupportedWhenNothingIsWired(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.opts.Ports.Verification = nil
	h.server.opts.Ports.Eligibility = nil
	h.server.opts.Ports.Conversion = nil

	for _, probe := range []struct {
		method, path string
		body         any
		key          string
	}{
		{http.MethodGet, "/v1/me/verification" + accountQuery(), nil, ""},
		{http.MethodGet, "/v1/me/eligibility" + accountQuery(), nil, ""},
		{http.MethodGet, "/v1/me/payout-destinations" + accountQuery(), nil, ""},
		{
			http.MethodPost, "/v1/payouts/quote",
			map[string]any{
				"account_id": testAccountID.String(), "destination_id": testAccountID.String(), "amount": "1",
			},
			"unsupported-quote-1",
		},
	} {
		var res *response
		if probe.key == "" {
			res = h.do(probe.method, probe.path, probe.body)
		} else {
			res = h.do(probe.method, probe.path, probe.body, "Idempotency-Key", probe.key)
		}
		require.Equalf(t, http.StatusUnprocessableEntity, res.Code, "%s %s: %s", probe.method, probe.path, res.Body.String())
		assert.Equalf(t, errs.CodeUnsupported, res.problem().Code, "%s %s", probe.method, probe.path)
	}
}
