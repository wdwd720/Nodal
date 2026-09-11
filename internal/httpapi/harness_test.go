package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/identity"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider/stripecredit"
	"github.com/nodal/controlplane/internal/quote"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/withdrawal"
)

// Fixed identifiers so tests read as data, not as ceremony.
var (
	testUserID     = accounts.NewUserID()
	testAccountID  = accounts.NewAccountID()
	testOtherAcct  = accounts.NewAccountID()
	testInstrument = instruments.NewInstrumentID()
	testIntentID   = intent.NewIntentID()
	testOrderID    = execution.NewOrderID()
	testDepositID  = funding.NewDepositID()
	testAssetID    = assets.NewAssetID()
	testSessionID  = "0193b2e0-0000-7000-8000-000000000001"
	testNow        = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
)

// customerPrincipal is an ordinary customer who owns testAccountID and has a
// recent strong authentication.
func customerPrincipal() security.Principal {
	return security.Principal{
		SubjectID:  testUserID.String(),
		ActorType:  security.ActorUser,
		Roles:      []security.Role{security.RoleCustomer},
		AccountIDs: []string{testAccountID.String()},
		SessionID:  testSessionID,
		AuthTime:   testNow.Add(-time.Minute),
		AMR:        []string{"pwd", "mfa"},
	}
}

// operatorPrincipal holds every non-dual-control permission (ADMIN).
func operatorPrincipal() security.Principal {
	return security.Principal{
		SubjectID: testUserID.String(),
		ActorType: security.ActorOperator,
		Roles:     []security.Role{security.RoleAdmin},
		SessionID: testSessionID,
		AuthTime:  testNow.Add(-time.Minute),
		AMR:       []string{"pwd", "mfa"},
	}
}

// agentPrincipal is an AGENT actor. It must never reach the HTTP surface.
func agentPrincipal() security.Principal {
	return security.AgentPrincipal(testUserID.String(), testAccountID.String())
}

type harness struct {
	t       *testing.T
	server  *Server
	ports   *fixtures
	princip *security.Principal
}

// fixtures holds every double so a test can reach in and set an error.
type fixtures struct {
	identity    *fakeIdentity
	sessions    *fakeSessions
	accounts    *fakeAccounts
	buyingPower *fakeBuyingPower
	holdings    *fakeHoldings
	ledger      *fakeLedger
	activity    *fakeActivity
	export      *fakeExport
	assets      *fakeAssets
	instruments *fakeInstruments
	quotes      *fakeQuotes
	intents     *fakeIntents
	orders      *fakeOrders
	funding     *fakeFunding
	withdrawals *fakeWithdrawals
	gates       *fakeGates
	kill        *fakeKillSwitches
	adminActs   *fakeAdminActions
	providers   *fakeProviders
	reconcile   *fakeReconciliation
	health      *fakeHealth
	webhook     *fakeWebhook
	idem        *fakeIdempotency
	stream      http.Handler

	// The withdrawal journey (goal PARTS 19-25). Defined in
	// handlers_verification_test.go, beside the tests that drive them.
	verification *fakeVerification
	eligibility  *fakeEligibility
	conversion   *fakeConversion
}

func newFixtures() *fixtures {
	acct := accounts.Account{
		ID: testAccountID, OwnerUserID: testUserID, Kind: accounts.KindCustomer,
		Status: accounts.StatusActive, CreatedAt: testNow,
	}
	ti := intent.TradeIntent{
		ID: testIntentID, AccountID: testAccountID.String(), ActorType: security.ActorUser,
		ActorID: testUserID.String(), Action: intent.ActionAcquireNotional, InstrumentID: testInstrument,
		Status: intent.StatusReceived, Mode: intent.ModePaper, RequestedAt: testNow, ReceivedAt: testNow,
		CorrelationID: "corr-1",
	}
	notional := usd("100.00")
	ti.NotionalUSD = &notional

	ord := execution.Order{
		ID: testOrderID, IntentID: testIntentID.String(), AccountID: testAccountID,
		InstrumentID: testInstrument.String(), Side: execution.SideBuy, Mode: execution.ModePaper,
		Status: execution.OrderCreated, InputAssetID: testAssetID, InputQuantity: qty("1000000"),
		OutputAssetID: testAssetID, MinOutputQuantity: qty("900000"),
		FilledInputQuantity: qty("0"), FilledOutputQuantity: qty("0"), CreatedAt: testNow,
	}
	dep := funding.Deposit{
		ID: testDepositID, AccountID: testAccountID, Provider: "stripe",
		Status: funding.StatusSessionCreated, ExpectedAssetID: testAssetID, CreatedAt: testNow,
	}
	inst := instruments.Instrument{
		ID: testInstrument, Type: instruments.TypeSpotPair, CanonicalName: "SOL/USDC",
		SettlementAssetID: testAssetID, RiskClass: assets.RiskMajor, Status: assets.StatusActive,
		ActiveFrom: testNow,
	}

	return &fixtures{
		identity: &fakeIdentity{
			begun: identity.BeginResult{
				RedirectURL: "https://idp.test/authorize?state=abc",
				State:       "abc", ExpiresAt: testNow.Add(10 * time.Minute),
			},
			complete: identity.Completed{
				Issued:   auth.Issued{Session: auth.Session{ID: testSessionID}, Token: "raw-session-token-value"},
				ReturnTo: "/portfolio",
			},
		},
		sessions: &fakeSessions{items: []auth.Summary{{
			ID: testSessionID, CreatedAt: testNow, LastSeenAt: testNow, ExpiresAt: testNow.Add(time.Hour),
		}}},
		accounts:    &fakeAccounts{account: acct, owned: []accounts.Account{acct}, page: AccountPage{Items: []accounts.Account{acct}}},
		buyingPower: &fakeBuyingPower{value: sampleBuyingPower()},
		holdings:    &fakeHoldings{value: sampleHoldings()},
		ledger:      &fakeLedger{},
		activity:    &fakeActivity{page: ActivityPage{Items: []ActivityItem{{ID: "1", Kind: "INTENT", OccurredAt: testNow, Summary: "intent RECEIVED"}}}},
		export:      &fakeExport{doc: ExportDocument{JSON: map[string]any{"account_id": testAccountID.String()}, CSV: []byte("\"a\"\r\n")}},
		assets:      &fakeAssets{items: []assets.Asset{sampleAsset()}},
		instruments: &fakeInstruments{items: []instruments.Instrument{inst}, detail: InstrumentDetail{Instrument: inst, Base: sampleAsset(), Quote: sampleAsset()}},
		quotes:      &fakeQuotes{view: QuoteView{Disclosure: sampleDisclosure(), Venue: "JUPITER", TotalEstimatedCostUSD: usd("100.51")}},
		intents:     &fakeIntents{value: ti, detail: IntentDetail{Intent: ti}, page: intent.Page{Items: []intent.TradeIntent{ti}}},
		orders:      &fakeOrders{page: OrderPage{Items: []execution.Order{ord}}, detail: OrderDetail{Order: ord}},
		funding: &fakeFunding{
			result: funding.StartResult{Deposit: dep},
			items:  []funding.Deposit{dep},
			detail: DepositDetail{Deposit: dep},
		},
		withdrawals: &fakeWithdrawals{value: withdrawal.Withdrawal{
			ID: withdrawal.NewWithdrawalID(), AccountID: testAccountID, AssetID: testAssetID,
			Quantity: qty("1000"), DestinationAddress: "SoL1111111111111111111111111111111111111111",
			Status: withdrawal.StatusRequested, CreatedAt: testNow,
		}},
		gates: &fakeGates{items: []GateView{{
			Gate:    gates.Gate{Capability: gates.LiveFunding, Environment: "TEST", State: gates.StateDisabled},
			Verdict: gates.Verdict{Active: false, Reason: gates.ReasonConfigDisabled, State: gates.StateDisabled},
		}}},
		kill:      &fakeKillSwitches{items: []killswitch.Switch{{Kind: killswitch.GlobalNewRiskKill, ScopeID: "*", Active: false, Severity: killswitch.SeveritySevere, Reason: "none"}}},
		adminActs: &fakeAdminActions{},
		providers: &fakeProviders{items: []ProviderView{{Name: "stripe", Role: "FundingProvider", Health: "HEALTHY", Verification: "CODE_COMPLETE", Mode: "fake"}}},
		reconcile: &fakeReconciliation{},
		health:    &fakeHealth{},
		webhook:   &fakeWebhook{status: http.StatusOK},
		idem:      newFakeIdempotency(),
		stream:    http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, ": keepalive\n\n") }),

		verification: newFakeVerification(),
		eligibility:  newFakeEligibility(),
		conversion:   newFakeConversion(),
	}
}

func (f *fixtures) ports() Ports {
	return Ports{
		Identity: f.identity, Sessions: f.sessions, Accounts: f.accounts,
		BuyingPower: f.buyingPower, Holdings: f.holdings, Ledger: f.ledger,
		Activity: f.activity, Export: f.export, Assets: f.assets, Instruments: f.instruments,
		Quotes: f.quotes, Intents: f.intents, Orders: f.orders, Funding: f.funding,
		Withdrawals: f.withdrawals, Gates: f.gates, KillSwitches: f.kill,
		AdminActions: f.adminActs, Providers: f.providers, Reconciliation: f.reconcile,
		Health: f.health, Idempotency: f.idem,
		// Keyed by the constant the service actually registers under, not by a
		// literal. F-124 changed that key from "stripe" to "stripe_credit" and
		// this harness kept the old one, so every webhook test in this package
		// exercised a provider key production does not have -- and the
		// public-route probe was answered 404 by the provider lookup, which
		// satisfied its "not 401" assertion while measuring nothing (F-132).
		Webhooks: map[string]WebhookPort{stripecredit.ProviderName: f.webhook},
		Stream:   f.stream,

		Verification: f.verification, Eligibility: f.eligibility, Conversion: f.conversion,
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fx := newFixtures()
	p := customerPrincipal()
	h := &harness{t: t, ports: fx, princip: &p}
	srv, err := New(Options{
		Env:           config.EnvTest,
		BuildVersion:  "test-build",
		ConfigHash:    "hash-1",
		PublicBaseURL: "https://app.test",
		CORSOrigins:   []string{"https://app.test"},
		CookieName:    "cp_session",
		SessionTTL:    time.Hour,
		Clock:         clock.NewFake(testNow),
		Authenticator: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if h.princip == nil {
					next.ServeHTTP(w, r)
					return
				}
				next.ServeHTTP(w, r.WithContext(security.WithPrincipal(r.Context(), *h.princip)))
			})
		},
		Ports: fx.ports(),
	})
	require.NoError(t, err)
	h.server = srv
	return h
}

// as runs the next request with the given principal (nil = anonymous).
func (h *harness) as(p *security.Principal) *harness {
	h.princip = p
	return h
}

type response struct {
	*httptest.ResponseRecorder
	t *testing.T
}

func (h *harness) do(method, path string, body any, headers ...string) *response {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			reader = strings.NewReader(v)
		default:
			reader = strings.NewReader(string(marshalJSON(v)))
		}
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.server.Router().ServeHTTP(rec, req)
	return &response{ResponseRecorder: rec, t: h.t}
}

// doWithCookies is do with cookies attached, for a flow whose second request
// has to prove it came from the browser that made the first.
func (h *harness) doWithCookies(method, path string, body any, cookies []*http.Cookie) *response {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(marshalJSON(body)))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.server.Router().ServeHTTP(rec, req)
	return &response{ResponseRecorder: rec, t: h.t}
}

// problem decodes the body as an RFC 9457 document and asserts the media type.
func (r *response) problem() errs.Problem {
	r.t.Helper()
	require.Equal(r.t, errs.ContentType, r.Header().Get("Content-Type"),
		"every failure must be application/problem+json; body=%s", r.Body.String())
	var p errs.Problem
	require.NoError(r.t, json.Unmarshal(r.Body.Bytes(), &p), "body=%s", r.Body.String())
	require.Equal(r.t, r.Code, p.Status, "problem.status must equal the HTTP status")
	require.NotEmpty(r.t, p.Type)
	require.NotEmpty(r.t, p.Title)
	require.NotEmpty(r.t, p.Code)
	return p
}

func (r *response) json(v any) {
	r.t.Helper()
	require.NoError(r.t, json.Unmarshal(r.Body.Bytes(), v), "body=%s", r.Body.String())
}

func (r *response) raw() map[string]any {
	r.t.Helper()
	var m map[string]any
	r.json(&m)
	return m
}

// --- sample domain values ---------------------------------------------------

func sampleAsset() assets.Asset {
	return assets.Asset{
		ID: testAssetID, Chain: "solana", MintAddress: "Es9vMFrzaCER", Kind: assets.KindSPLToken, ValueDomain: valuedomain.SelfCustodialCrypto,
		Symbol: "USDC", Name: "USD Coin", Decimals: 6, IsStablecoin: true, PegCurrency: "USD",
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive, CreatedAt: testNow,
	}
}

func sampleBuyingPower() buyingpower.BuyingPower {
	return buyingpower.BuyingPower{
		PortfolioValue: usd("1000.00"),
		BuyingPower:    usd("900.00"),
		AvailableNow:   usd("900.00"),
		Reserved:       usd("50.00"),
		Pending:        usd("0.00"),
		Withdrawable:   usd("0.00"),
		UnderlyingBalances: []buyingpower.UnderlyingBalance{{
			AssetID: testAssetID, Symbol: "USDC", Decimals: 6, Quantity: qty("1000000000"),
			USDValue: usd("1000.00"), PriceRef: "face:USD", Status: "NORMAL",
		}},
		Haircuts: []buyingpower.Haircut{{AssetID: testAssetID, FactorBPS: money.OneHundredPercent, Reason: ""}},
		Restrictions: []buyingpower.Restriction{{
			Code: buyingpower.RestrictionStalePrice, Detail: "price is stale",
			Scope: buyingpower.ScopeAsset, AssetID: testAssetID, Blocking: false,
		}},
		PolicyVersion: "buyingpower/1", AsOf: testNow, Purpose: buyingpower.PurposeDisplay,
	}
}

func sampleDisclosure() quote.Disclosure {
	price, err := money.NewPrice(qty("1000000"), 6, "USDC", "jupiter", testNow)
	if err != nil {
		panic(err)
	}
	return quote.Disclosure{
		QuoteID: quote.NewQuoteID(), Provider: "jupiter",
		PayAssetID: testAssetID, PayQuantity: qty("100000000"),
		ReceiveAssetID: testAssetID, ExpectedReceive: qty("990000"), MinimumReceive: qty("980000"),
		EffectivePrice: price, PriceImpactBPS: 12, SlippageBPS: 50,
		VenueFee:        quote.FeeLine{Amount: qty("100")},
		NetworkEstimate: quote.FeeLine{Amount: qty("5000")},
		PlatformFee:     quote.FeeLine{Amount: qty("0")},
		PlatformFeeBPS:  0, FeePolicyVersion: "fees/1",
		ReceivedAt: testNow, ExpiresAt: testNow.Add(30 * time.Second),
	}
}

func sampleHoldings() HoldingsView {
	return HoldingsView{AsOf: testNow, Holdings: []HoldingView{{
		AssetID: testAssetID, Symbol: "USDC", Chain: "solana", MintAddress: "Es9vMFrzaCER",
		Decimals: 6, Quantity: qty("1000000000"), USDMark: usd("1000.00"), PriceRef: "face:USD",
		CostBasisUSD: usd("950.00"), UnrealizedUSD: usd("50.00"),
		CustodyAddress: "SoL1111111111111111111111111111111111111111",
	}}}
}
