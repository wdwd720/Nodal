package httpapi

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

var (
	testEconIntentID = econ.NewIntentID()
	testEconEffectID = econ.NewEffectID()
	testAdvInstr     = instruments.NewInstrumentID()
)

// fakeAdvanced is the advanced core port. It records the account and limit
// each call was scoped to, so a test can prove the handler passed the scoped
// account rather than the raw query value.
type fakeAdvanced struct {
	stubErr
	mu        sync.Mutex
	env       string
	states    []econ.CapabilityState
	instr     AdvancedInstrumentView
	intent    econ.Intent
	effect    econ.Effect
	decision  risk.AdvancedDecisionRecord
	bp        buyingpower.V2
	next      string
	lastAcct  accounts.AccountID
	lastLimit int
	lastKind  instruments.InstrumentType
}

func (f *fakeAdvanced) record(acct accounts.AccountID, limit int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAcct, f.lastLimit = acct, limit
}

func (f *fakeAdvanced) Capabilities(context.Context) (string, []econ.CapabilityState, error) {
	return f.env, f.states, f.fail()
}

func (f *fakeAdvanced) Instruments(_ context.Context, kind instruments.InstrumentType, limit int) ([]AdvancedInstrumentView, error) {
	f.mu.Lock()
	f.lastKind, f.lastLimit = kind, limit
	f.mu.Unlock()
	return []AdvancedInstrumentView{{Instrument: f.instr.Instrument, Current: f.instr.Current}}, f.fail()
}

func (f *fakeAdvanced) Instrument(context.Context, instruments.InstrumentID) (AdvancedInstrumentView, error) {
	return f.instr, f.fail()
}

func (f *fakeAdvanced) Intents(_ context.Context, acct accounts.AccountID, _ string, limit int) ([]econ.Intent, string, error) {
	f.record(acct, limit)
	return []econ.Intent{f.intent}, f.next, f.fail()
}

func (f *fakeAdvanced) Intent(context.Context, econ.IntentID) (econ.Intent, error) {
	return f.intent, f.fail()
}

func (f *fakeAdvanced) Effects(_ context.Context, acct accounts.AccountID, _ string, limit int) ([]econ.Effect, string, error) {
	f.record(acct, limit)
	return []econ.Effect{f.effect}, f.next, f.fail()
}

func (f *fakeAdvanced) RiskDecisions(_ context.Context, acct accounts.AccountID, limit int) ([]risk.AdvancedDecisionRecord, error) {
	f.record(acct, limit)
	return []risk.AdvancedDecisionRecord{f.decision}, f.fail()
}

func (f *fakeAdvanced) BuyingPower(_ context.Context, acct accounts.AccountID) (buyingpower.V2, error) {
	f.record(acct, 0)
	return f.bp, f.fail()
}

// newFakeAdvanced holds realistic values: real capability states computed by
// the same DecideMode the authorizer uses, an option with two terms versions,
// an intent and an applied effect of testAccountID.
func newFakeAdvanced() *fakeAdvanced {
	states := make([]econ.CapabilityState, 0, len(gates.AdvancedSpecs()))
	for _, sp := range gates.AdvancedSpecs() {
		st := econ.CapabilityState{Spec: sp, Decisions: map[gates.Mode]gates.ModeDecision{}}
		for _, m := range gates.AllModes() {
			st.Decisions[m] = gates.DecideMode(gates.ModeRequest{Capability: sp.Capability, Environment: "TEST", Mode: m})
		}
		states = append(states, st)
	}
	strike := money.QuantityFromInt64(250_000_000)
	expiry := testNow.Add(30 * 24 * time.Hour)
	under := instruments.NewInstrumentID()
	terms := instruments.Terms{
		Kind: instruments.TypeOption, Symbol: "SBXETH-C-2500", VenueCode: "NODAL-SANDBOX",
		SettlementAssetID: testAssetID, UnderlyingInstrumentID: &under,
		ContractMultiplier: money.QuantityFromInt64(1), PriceScale: 6, TickSize: money.QuantityFromInt64(10_000),
		LotSize: money.QuantityFromInt64(1), MinQuantity: money.QuantityFromInt64(1), QuantityDecimals: 0,
		Expiry: &expiry, StrikeMantissa: &strike, OptionRight: instruments.Call, ExerciseStyle: instruments.European,
		SettlementStyle: instruments.SettleCash, TradingCalendar: "24x7", JurisdictionTags: []string{"SANDBOX"},
		OracleMaxAgeMS: 5000, OracleSources: []string{"devnet-oracle"}, MarginModel: instruments.AllMarginModels()[0],
		CorporateActionBehavior: instruments.AllCorporateActionBehaviors()[0], Transferability: instruments.AllTransferabilities()[0],
	}
	v1 := instruments.Version{InstrumentID: testAdvInstr, Version: 1, Terms: terms, ContentHash: []byte{0xaa, 0x01}, Reason: "listed", ActorType: "SYSTEM", CreatedAt: testNow}
	v2 := v1
	v2.Version, v2.ContentHash, v2.Reason = 2, []byte{0xbb, 0x02}, "tick size change"
	inst := instruments.Instrument{
		ID: testAdvInstr, Type: instruments.TypeOption, CanonicalName: "SBXETH-C-2500", SettlementAssetID: testAssetID,
		RiskClass: assets.RiskSpeculative, Status: assets.StatusActive, ActiveFrom: testNow,
	}
	notional := usd("1250.00")
	qty := money.QuantityFromInt64(5)
	jid := ledger.NewTransactionID()
	return &fakeAdvanced{
		env: "TEST", states: states,
		instr: AdvancedInstrumentView{Instrument: inst, Current: v2, History: []instruments.Version{v1, v2}},
		intent: econ.Intent{
			ID: testEconIntentID, AccountID: testAccountID, Kind: econ.AllIntentKinds()[0], Mode: gates.ModePaper,
			Capability: gates.Derivatives, ActorType: security.ActorUser, ActorID: testUserID.String(),
			Quantity: &qty, NotionalUSD: &notional, Terms: []byte(`{"side":"BUY"}`), ContentHash: []byte{0x01, 0x02},
			Status: econ.AllIntentStatuses()[0], CreatedAt: testNow, UpdatedAt: testNow,
		},
		effect: econ.Effect{
			ID: testEconEffectID, Key: "derivatives:fill:1", Subsystem: econ.AllSubsystems()[0], Kind: "SIM_TRADE",
			IntentID: &testEconIntentID, AccountID: testAccountID.String(), PostsLedger: true, Status: econ.EffectApplied,
			JournalTransactionID: &jid, EvidenceRef: "proof://advanced/x.json", EvidenceSHA256: []byte{0xde, 0xad},
			CreatedAt: testNow, UpdatedAt: testNow,
		},
		decision: risk.AdvancedDecisionRecord{
			ID: testDecisionID(), IntentID: testEconIntentID.String(), AccountID: testAccountID.String(),
			IntentKind: "OPEN_DERIVATIVE", Stage: risk.AllStages()[0], Verdict: risk.AllVerdicts()[0],
			ReasonCodes: []string{}, InputHash: "ih", PolicyVersion: "advanced-risk/1", PolicyHash: "ph",
			EvaluatorVersion: risk.AdvancedEvaluatorVersion, DecisionHash: "dh", EvaluatedAt: testNow,
		},
		bp: buyingpower.V2{
			GrossEquity: usd("1234.56"), EligibleCollateral: usd("1200.00"), Haircuts: usd("34.56"),
			EligibleEquity: usd("1200.00"), Withdrawable: usd("1000.01"), TradeBuyingPower: usd("1100.00"),
			RiskIncreasingBuyingPower: usd("1100.00"), PolicyVersion: "buyingpower-v2/1", AsOf: testNow,
			Reasons: []buyingpower.V2Reason{{Code: "STALE_MARK", Detail: "a mark is stale", Amount: usd("10.00")}},
			Lines:   []buyingpower.V2Line{{Field: "gross_equity", Component: "WALLET", AssetID: testAssetID.String(), AmountUSD: usd("1234.56"), Detail: "held"}},
		},
	}
}

// testDecisionID is a risk decision id; the risk package mints them only when
// it persists a decision, so the test decodes a fixed one.
func testDecisionID() risk.DecisionID {
	var d risk.DecisionID
	if err := d.UnmarshalText([]byte("0193b2e0-0000-7000-8000-0000000000d1")); err != nil {
		panic(err)
	}
	return d
}

func newAdvancedHarness(t *testing.T) (*harness, *fakeAdvanced) {
	t.Helper()
	h := newHarness(t)
	fa := newFakeAdvanced()
	ports := h.ports.ports()
	ports.Advanced = fa
	h.server.opts.Ports = ports
	return h, fa
}

func TestAdvancedCapabilities_LiveIsRefusedForEveryCapability(t *testing.T) {
	t.Parallel()
	h, _ := newAdvancedHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/capabilities", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.AdvancedCapabilityList
	res.json(&out)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)
	assert.Equal(t, "TEST", out.Environment)
	require.Len(t, out.Items, len(gates.AdvancedSpecs()))
	for _, c := range out.Items {
		assert.False(t, c.LiveImplemented, c.Capability)
		assert.Equal(t, "0.00", c.MaxLiveCapital, c.Capability)
		require.Len(t, c.Decisions, len(gates.AllModes()), "every mode is reported, LIVE included: %s", c.Capability)
		for _, d := range c.Decisions {
			if d.Mode == api.AdvancedMode(gates.ModeLive) {
				assert.False(t, d.Allowed, "%s LIVE must be refused", c.Capability)
				assert.Contains(t, d.Reasons, gates.ReasonLiveNotImplemented, c.Capability)
			}
		}
		assert.NotContains(t, c.SimulatedModes, api.AdvancedMode(gates.ModeLive), c.Capability)
	}
}

// A capability state missing a mode is an internal fault, not a mode silently
// left out of the table.
func TestAdvancedCapabilities_MissingModeDecisionIsInternal(t *testing.T) {
	t.Parallel()
	h, fa := newAdvancedHarness(t)
	delete(fa.states[0].Decisions, gates.ModeLive)
	res := h.do(http.MethodGet, "/v1/advanced/capabilities", nil)
	require.Equal(t, http.StatusInternalServerError, res.Code, "body=%s", res.Body.String())
}

func TestAdvancedInstruments_ListAndDetailRenderExactTerms(t *testing.T) {
	t.Parallel()
	h, fa := newAdvancedHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/instruments?kind=OPTION&limit=5000", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var list api.AdvancedInstrumentList
	res.json(&list)
	require.Len(t, list.Items, 1)
	assert.Equal(t, instruments.TypeOption, fa.lastKind)
	assert.Equal(t, maxPageLimit, fa.lastLimit, "the limit is clamped")
	it := list.Items[0]
	assert.Equal(t, "bb02", it.TermsHash)
	assert.Equal(t, 2, it.TermsVersion)
	require.NotNil(t, it.Terms.StrikeMantissa)
	assert.Equal(t, "250000000", *it.Terms.StrikeMantissa)
	require.NotNil(t, it.Terms.OptionRight)
	assert.Equal(t, api.InstrumentTermsOptionRight("CALL"), *it.Terms.OptionRight)

	res = h.do(http.MethodGet, "/v1/advanced/instruments/"+testAdvInstr.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var d api.AdvancedInstrumentDetail
	res.json(&d)
	require.Len(t, d.Versions, 2)
	assert.Equal(t, "aa01", d.Versions[0].TermsHash)
	assert.Equal(t, "tick size change", d.Versions[1].Reason)
}

func TestAdvancedInstruments_UnknownKindIsRefused(t *testing.T) {
	t.Parallel()
	h, _ := newAdvancedHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/instruments?kind=CREDIT_DEFAULT_SWAP", nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
}

func TestAdvancedAccountReads_AreScopedToTheCallersAccount(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/v1/advanced/intents", "/v1/advanced/effects", "/v1/advanced/risk/decisions", "/v1/advanced/buying-power"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			h, fa := newAdvancedHarness(t)
			res := h.do(http.MethodGet, path+"?account_id="+testAccountID.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, testAccountID, fa.lastAcct)

			res = h.do(http.MethodGet, path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)

			op := operatorPrincipal()
			h.as(&op)
			res = h.do(http.MethodGet, path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "an operator with account:read_any reads any account; body=%s", res.Body.String())
			assert.Equal(t, testOtherAcct, fa.lastAcct)
		})
	}
}

// An intent fetched by its own id is scoped by the account stored on it, and a
// stranger's intent answers NOT_FOUND so the refusal does not confirm the id.
func TestAdvancedIntentByID_IsScopedByTheIntentsOwnAccount(t *testing.T) {
	t.Parallel()
	h, _ := newAdvancedHarness(t)
	path := "/v1/advanced/intents/" + testEconIntentID.String()
	res := h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var in api.EconomicIntent
	res.json(&in)
	assert.Equal(t, "1250.00", *in.NotionalUsd)
	assert.Equal(t, "5", *in.Quantity)
	assert.Equal(t, "BUY", in.Terms["side"])
	assert.Equal(t, "0102", in.ContentHash)

	stranger := customerPrincipal()
	stranger.AccountIDs = []string{testOtherAcct.String()}
	h.as(&stranger)
	res = h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)
}

func TestAdvancedEffects_RenderJournalAndEvidence(t *testing.T) {
	t.Parallel()
	h, fa := newAdvancedHarness(t)
	fa.next = "cursor-2"
	res := h.do(http.MethodGet, "/v1/advanced/effects?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var page api.EconomicEffectPage
	res.json(&page)
	require.Len(t, page.Items, 1)
	e := page.Items[0]
	assert.Equal(t, api.EconomicEffectStatus("APPLIED"), e.Status)
	require.NotNil(t, e.JournalTransactionId)
	require.NotNil(t, e.EvidenceSha256)
	assert.Equal(t, "dead", *e.EvidenceSha256)
	v, err := page.NextCursor.Get()
	require.NoError(t, err)
	assert.Equal(t, "cursor-2", v)
}

func TestAdvancedBuyingPower_IsExactDecimalStringsInTheSimulatedDomain(t *testing.T) {
	t.Parallel()
	h, _ := newAdvancedHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/buying-power?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	assert.Equal(t, "1234.56", raw["gross_equity"])
	assert.Equal(t, "1000.01", raw["withdrawable"])
	assert.Equal(t, "0.00", raw["maintenance_margin"])
	lines := raw["lines"].([]any)
	require.Len(t, lines, 1)
	assert.Equal(t, "1234.56", lines[0].(map[string]any)["amount_usd"])
}

// A deployment without the advanced services says so on every advanced route.
func TestAdvancedRoutes_UnwiredPortIsUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, path := range []string{
		"/v1/advanced/capabilities",
		"/v1/advanced/instruments",
		"/v1/advanced/instruments/" + testAdvInstr.String(),
		"/v1/advanced/intents?account_id=" + testAccountID.String(),
		"/v1/advanced/intents/" + testEconIntentID.String(),
		"/v1/advanced/effects?account_id=" + testAccountID.String(),
		"/v1/advanced/risk/decisions?account_id=" + testAccountID.String(),
		"/v1/advanced/buying-power?account_id=" + testAccountID.String(),
	} {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", path, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, path)
	}
}

// Port failures carry their code to the wire unchanged.
func TestAdvancedRoutes_PortErrorsKeepTheirCode(t *testing.T) {
	t.Parallel()
	h, fa := newAdvancedHarness(t)
	fa.err = errs.New(errs.CodeNotFound, "the sandbox dollar is not provisioned in this deployment")
	res := h.do(http.MethodGet, "/v1/advanced/buying-power?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)
}
