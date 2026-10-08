package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// pmID makes an id of a type whose constructor the predmarket package keeps
// to itself, by decoding a fresh UUIDv7 into it.
func pmID[P interface {
	*T
	UnmarshalText([]byte) error
}, T any]() T {
	var v T
	if err := P(&v).UnmarshalText([]byte(id.New[id.Any]().String())); err != nil {
		panic(err)
	}
	return v
}

var (
	pmMarketID   = predmarket.NewMarketID()
	pmOperatorID = accounts.NewUserID()
	pmBase       = "/v1/advanced/prediction-markets"
	pmPath       = pmBase + "/" + pmMarketID.String()
)

// pmOperator is an operator who owns no account and authenticated strongly a
// minute ago.
func pmOperator() security.Principal {
	p := operatorPrincipal()
	p.SubjectID = pmOperatorID.String()
	return p
}

// fakePredmarket is the prediction market port. It records what each call was
// asked, so a test can prove the handler passed the scoped account, the
// principal and the namespaced key rather than raw client input.
type fakePredmarket struct {
	stubErr
	mu    sync.Mutex
	calls map[string]int

	oracle      *predmarket.SandboxOracle
	market      predmarket.Market
	collateral  assets.Asset
	resolutions []predmarket.Resolution
	disputes    []predmarket.Dispute
	settlement  *predmarket.Settlement
	books       predmarket.EscrowBooks
	residual    money.Quantity
	violations  []string
	positions   []predmarket.Position
	action      predmarket.ActionResult
	dispute     predmarket.Dispute
	plan        predmarket.SettlementPlan
	lines       []predmarket.ActionResult
	record      reconciliation.Record
	existing    bool
	replay      bool

	lastStatus   predmarket.MarketStatus
	lastLimit    int
	lastAccount  accounts.AccountID
	lastPosition predmarket.PositionRequest
	lastSpec     predmarket.MarketSpec
	lastActor    econ.Actor
	lastResolve  PredictionResolveCommand
	lastDispute  predmarket.DisputeRequest
	lastRuling   predmarket.RulingRequest
	lastVoid     predmarket.VoidRequest
	lastRecActor reconciliation.Actor
}

func (f *fakePredmarket) called(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
}

func (f *fakePredmarket) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakePredmarket) rec() PredictionMarketRecord {
	return PredictionMarketRecord{Market: f.market, Collateral: f.collateral}
}

func (f *fakePredmarket) Markets(_ context.Context, status predmarket.MarketStatus, limit int) ([]PredictionMarketRecord, error) {
	f.called("Markets")
	f.mu.Lock()
	f.lastStatus, f.lastLimit = status, limit
	f.mu.Unlock()
	return []PredictionMarketRecord{f.rec()}, f.fail()
}

func (f *fakePredmarket) Market(context.Context, predmarket.MarketID) (PredictionMarketDetailView, error) {
	f.called("Market")
	return PredictionMarketDetailView{Record: f.rec(), Resolutions: f.resolutions, Disputes: f.disputes, Settlement: f.settlement}, f.fail()
}

func (f *fakePredmarket) Books(context.Context, predmarket.MarketID) (PredictionBooksView, error) {
	f.called("Books")
	return PredictionBooksView{Record: f.rec(), Books: f.books, Residual: f.residual, Violations: f.violations}, f.fail()
}

func (f *fakePredmarket) Positions(_ context.Context, _ predmarket.MarketID, account accounts.AccountID) (PredictionPositionsView, error) {
	f.called("Positions")
	f.mu.Lock()
	f.lastAccount = account
	f.mu.Unlock()
	var mine []predmarket.Position
	for _, p := range f.positions {
		if p.AccountID == account {
			mine = append(mine, p)
		}
	}
	return PredictionPositionsView{Record: f.rec(), Positions: mine}, f.fail()
}

func (f *fakePredmarket) Oracle(context.Context) (PredictionOracleView, error) {
	f.called("Oracle")
	return PredictionOracleView{Source: f.oracle.Source(), PublicKey: f.oracle.PublicKey()}, f.fail()
}

func (f *fakePredmarket) Issue(_ context.Context, req predmarket.PositionRequest) (predmarket.ActionResult, error) {
	f.called("Issue")
	f.mu.Lock()
	f.lastPosition = req
	f.mu.Unlock()
	return f.action, f.fail()
}

func (f *fakePredmarket) Redeem(_ context.Context, req predmarket.PositionRequest) (predmarket.ActionResult, error) {
	f.called("Redeem")
	f.mu.Lock()
	f.lastPosition = req
	f.mu.Unlock()
	return f.action, f.fail()
}

func (f *fakePredmarket) Create(_ context.Context, spec predmarket.MarketSpec, by econ.Actor) (PredictionMarketRecord, bool, error) {
	f.called("Create")
	f.mu.Lock()
	f.lastSpec, f.lastActor = spec, by
	f.mu.Unlock()
	return f.rec(), f.existing, f.fail()
}

func (f *fakePredmarket) step(name string, by econ.Actor) (PredictionMarketRecord, error) {
	f.called(name)
	f.mu.Lock()
	f.lastActor = by
	f.mu.Unlock()
	return f.rec(), f.fail()
}

func (f *fakePredmarket) Open(_ context.Context, _ predmarket.MarketID, by econ.Actor) (PredictionMarketRecord, error) {
	return f.step("Open", by)
}

func (f *fakePredmarket) Close(_ context.Context, _ predmarket.MarketID, by econ.Actor) (PredictionMarketRecord, error) {
	return f.step("Close", by)
}

func (f *fakePredmarket) RequestResolution(_ context.Context, _ predmarket.MarketID, by econ.Actor) (PredictionMarketRecord, error) {
	return f.step("RequestResolution", by)
}

// Observe uses a real sandbox oracle, so the report a test receives is one
// the package itself would verify.
func (f *fakePredmarket) Observe(_ context.Context, mid predmarket.MarketID, outcome string, at time.Time, evidence []byte) (predmarket.OracleReport, error) {
	f.called("Observe")
	if err := f.fail(); err != nil {
		return predmarket.OracleReport{}, err
	}
	if err := f.oracle.Observe(mid, outcome, at, evidence); err != nil {
		return predmarket.OracleReport{}, err
	}
	return f.oracle.Report(context.Background(), mid)
}

func (f *fakePredmarket) Resolve(_ context.Context, cmd PredictionResolveCommand) (PredictionResolveView, error) {
	f.called("Resolve")
	f.mu.Lock()
	f.lastResolve = cmd
	f.mu.Unlock()
	return PredictionResolveView{Record: f.rec(), Resolution: f.resolutions[0], Replay: f.replay}, f.fail()
}

func (f *fakePredmarket) Dispute(_ context.Context, req predmarket.DisputeRequest) (PredictionDisputeView, error) {
	f.called("Dispute")
	f.mu.Lock()
	f.lastDispute = req
	f.mu.Unlock()
	return PredictionDisputeView{Record: f.rec(), Dispute: f.dispute}, f.fail()
}

func (f *fakePredmarket) Uphold(_ context.Context, req predmarket.RulingRequest) (PredictionResolveView, error) {
	f.called("Uphold")
	f.mu.Lock()
	f.lastRuling = req
	f.mu.Unlock()
	return PredictionResolveView{Record: f.rec(), Resolution: f.resolutions[len(f.resolutions)-1]}, f.fail()
}

func (f *fakePredmarket) settled() PredictionSettleView {
	v := PredictionSettleView{Record: f.rec(), Settlement: *f.settlement, Replay: f.replay}
	if !f.replay {
		v.Plan, v.Lines = f.plan, f.lines
	}
	return v
}

func (f *fakePredmarket) Void(_ context.Context, req predmarket.VoidRequest) (PredictionSettleView, error) {
	f.called("Void")
	f.mu.Lock()
	f.lastVoid = req
	f.mu.Unlock()
	return f.settled(), f.fail()
}

func (f *fakePredmarket) Settle(_ context.Context, req predmarket.SettleRequest) (PredictionSettleView, error) {
	f.called("Settle")
	f.mu.Lock()
	f.lastActor = req.Actor
	f.mu.Unlock()
	return f.settled(), f.fail()
}

func (f *fakePredmarket) Reconcile(_ context.Context, _ predmarket.MarketID, by reconciliation.Actor) (reconciliation.Record, error) {
	f.called("Reconcile")
	f.mu.Lock()
	f.lastRecActor = by
	f.mu.Unlock()
	return f.record, f.fail()
}

func pmSeed(label string) []byte {
	sum := sha256.Sum256([]byte(label))
	return sum[:]
}

// newFakePredmarket holds a realistic SANDBOX market with two outcomes that
// has been resolved, disputed, ruled on and settled: 5 complete sets were
// issued, YES won, and the escrow paid exactly what it held.
func newFakePredmarket(t *testing.T) *fakePredmarket {
	t.Helper()
	oracle, err := predmarket.NewSandboxOracle("pm-http-oracle", pmSeed("pm-http-oracle"))
	require.NoError(t, err)
	yes, no := assets.NewAssetID(), assets.NewAssetID()
	market := predmarket.Market{
		ID: pmMarketID, Symbol: "PMHTTP", Question: "Which outcome will the sandbox election board certify?",
		Mode: gates.ModeSandbox, FacilityID: econ.NewFacilityID(), InstrumentID: instruments.NewInstrumentID(),
		CollateralAssetID: econ.SandboxDollarID(),
		Outcomes: []predmarket.Outcome{
			{Index: 0, Symbol: "YES", ShareAssetID: yes, InstrumentID: instruments.NewInstrumentID()},
			{Index: 1, Symbol: "NO", ShareAssetID: no, InstrumentID: instruments.NewInstrumentID()},
		},
		UnitQuantity: predmarket.ShareUnit(), OpenAt: testNow, CloseAt: testNow.Add(time.Hour),
		DisputeWindow: 10 * time.Minute, OracleSource: oracle.Source(), OraclePublicKey: oracle.PublicKey(),
		DefinitionHash: []byte{0xab, 0xcd}, Status: predmarket.StatusSettled,
		CreatedBy: econ.Actor{Type: security.ActorOperator, ID: "0193b2e0-0000-7000-8000-0000000000c1"},
		CreatedAt: testNow, UpdatedAt: testNow,
	}
	first, ruling := pmID[*predmarket.ResolutionID](), pmID[*predmarket.ResolutionID]()
	disputeID, settlementID := pmID[*predmarket.DisputeID](), pmID[*predmarket.SettlementID]()
	observed := testNow.Add(time.Hour + 30*time.Second)
	resolver := econ.Actor{Type: security.ActorOperator, ID: "0193b2e0-0000-7000-8000-0000000000c2"}
	disputer := econ.Actor{Type: security.ActorOperator, ID: "0193b2e0-0000-7000-8000-0000000000c3"}
	winning := 0
	settlement := predmarket.Settlement{
		ID: settlementID, MarketID: pmMarketID, Kind: predmarket.SettlementPayout, ResolutionID: &ruling,
		WinningOutcomeIndex: &winning, OutcomeCount: 2, UnitQuantity: predmarket.ShareUnit(),
		RoundingPolicy: predmarket.RoundingExact, EscrowBefore: qty("5000000"), TotalPaid: qty("5000000"),
		Residual: qty("0"), HolderCount: 2, EvidenceRef: "proof://predmarket/settlement.json",
		EvidenceSHA256: []byte{0x5e, 0x77}, SettledBy: resolver, SettledAt: testNow.Add(2 * time.Hour),
	}
	q := qty("3000000")
	jid := ledger.NewTransactionID()
	intentID := econ.NewIntentID()
	action := predmarket.ActionResult{
		Intent: econ.Intent{
			ID: intentID, AccountID: testAccountID, Kind: econ.KindCreatePredictionPosition, Mode: gates.ModeSandbox,
			Capability: gates.PredictionMarkets, ActorType: security.ActorUser, ActorID: testUserID.String(),
			Quantity: &q, Terms: []byte(`{"action":"ISSUE_COMPLETE_SETS","complete_sets":"3"}`), ContentHash: []byte{0x01},
			Status: econ.StatusCompleted, CreatedAt: testNow, UpdatedAt: testNow,
		},
		Effect: econ.Effect{
			ID: econ.NewEffectID(), Key: "predmarket:issue:" + intentID.String(), Subsystem: econ.SubsystemPredictionMarkets,
			Kind: "PREDICTION_SET_ISSUANCE", IntentID: &intentID, AccountID: testAccountID.String(), PostsLedger: true,
			Status: econ.EffectApplied, JournalTransactionID: &jid, CreatedAt: testNow, UpdatedAt: testNow,
		},
		DecisionHash: "decision-hash-1", JournalTransactionID: jid,
	}
	payA, payB := econ.NewIntentID(), econ.NewIntentID()
	jidA, jidB := ledger.NewTransactionID(), ledger.NewTransactionID()
	return &fakePredmarket{
		calls: map[string]int{}, oracle: oracle, market: market,
		collateral: assets.Asset{
			ID: econ.SandboxDollarID(), Symbol: econ.SandboxDollarSymbol, Decimals: 6, Kind: assets.KindSimulated,
			ValueDomain: valuedomain.Simulated, IsStablecoin: true, PegCurrency: "USD", Status: assets.StatusActive,
		},
		resolutions: []predmarket.Resolution{
			{
				ID: first, MarketID: pmMarketID, Kind: predmarket.ResolutionOracle, OutcomeIndex: 0,
				ReportHash: []byte{0xfe, 0xed}, ReportSource: oracle.Source(), ReportObservedAt: &observed,
				EvidenceRef: "proof://predmarket/oracle.json", EvidenceSHA256: []byte{0x0a}, AcceptedBy: resolver,
				Reason: "oracle report accepted", AcceptedAt: testNow.Add(time.Hour + time.Minute),
			},
			{
				ID: ruling, MarketID: pmMarketID, Kind: predmarket.ResolutionDisputeUpheld, OutcomeIndex: 0,
				SupersedesID: &first, DisputeID: &disputeID, EvidenceRef: "proof://predmarket/ruling.json",
				EvidenceSHA256: []byte{0x0b}, AcceptedBy: resolver, Reason: "the board's certificate stands",
				AcceptedAt: testNow.Add(time.Hour + 5*time.Minute),
			},
		},
		disputes: []predmarket.Dispute{{
			ID: disputeID, MarketID: pmMarketID, ResolutionID: first, RaisedBy: disputer,
			Reason: "the certificate was published before the recount", EvidenceRef: "proof://predmarket/dispute.json",
			EvidenceSHA256: []byte{0x0c}, RaisedAt: testNow.Add(time.Hour + 2*time.Minute),
		}},
		settlement: &settlement,
		books: predmarket.EscrowBooks{
			Escrow: qty("5000000"), Collateral: qty("5000000"),
			SharesIssued:      []money.Quantity{qty("5000000"), qty("5000000")},
			SharesOutstanding: []money.Quantity{qty("5000000"), qty("5000000")},
			HeldByCustomers:   []money.Quantity{qty("5000000"), qty("5000000")},
		},
		residual: qty("0"),
		positions: []predmarket.Position{
			{MarketID: pmMarketID, AccountID: testAccountID, OutcomeIndex: 0, OutcomeSymbol: "YES", ShareAssetID: yes, Quantity: qty("3000000"), AsOf: testNow},
			{MarketID: pmMarketID, AccountID: testAccountID, OutcomeIndex: 1, OutcomeSymbol: "NO", ShareAssetID: no, Quantity: qty("3000000"), AsOf: testNow},
			{MarketID: pmMarketID, AccountID: testOtherAcct, OutcomeIndex: 0, OutcomeSymbol: "YES", ShareAssetID: yes, Quantity: qty("2000000"), AsOf: testNow},
		},
		action:  action,
		dispute: predmarket.Dispute{ID: disputeID, MarketID: pmMarketID, ResolutionID: first, RaisedBy: disputer, Reason: "the certificate was published before the recount", EvidenceRef: "proof://predmarket/dispute.json", EvidenceSHA256: []byte{0x0c}, RaisedAt: testNow},
		plan: predmarket.SettlementPlan{
			Kind: predmarket.SettlementPayout, OutcomeCount: 2, Unit: predmarket.ShareUnit(), WinningOutcome: 0,
			Policy: predmarket.RoundingExact, EscrowBefore: qty("5000000"), TotalPaid: qty("5000000"), Residual: qty("0"),
			Lines: []predmarket.PlanLine{
				{AccountID: testAccountID, Shares: []money.Quantity{qty("3000000"), qty("3000000")}, Amount: qty("3000000")},
				{AccountID: testOtherAcct, Shares: []money.Quantity{qty("2000000"), qty("2000000")}, Amount: qty("2000000")},
			},
		},
		lines: []predmarket.ActionResult{
			{Intent: econ.Intent{ID: payA}, JournalTransactionID: jidA},
			{Intent: econ.Intent{ID: payB}, JournalTransactionID: jidB},
		},
		record: reconciliation.Record{
			ID: pmID[*reconciliation.RecordID](), Kind: reconciliation.KindPredictionEscrow, Mode: reconciliation.ModeFull,
			ScopeType: predmarket.ScopePredictionMarket, ScopeID: pmMarketID.String(),
			Expected:   json.RawMessage(`{"residual":"0"}`),
			Observed:   json.RawMessage(`{"escrow":"5000000"}`),
			Difference: json.RawMessage(`{"violations":[]}`),
			Status:     reconciliation.StatusMatched, OpenedAt: testNow, MatchedAt: &observed,
		},
	}
}

func newPredmarketHarness(t *testing.T) (*harness, *fakePredmarket) {
	t.Helper()
	h := newHarness(t)
	fp := newFakePredmarket(t)
	ports := h.ports.ports()
	ports.PredictionMarkets = fp
	h.server.opts.Ports = ports
	return h, fp
}

// pmMutation is one POST route of the subsystem with a valid body, and a
// different valid body for the same route (a body the handler would refuse
// never reaches the idempotency check).
type pmMutation struct {
	name     string
	path     string
	body     any
	other    any
	call     string
	customer bool
}

func pmCreateBody(question string) map[string]any {
	return map[string]any{
		"symbol": "PMHTTP", "question": question,
		"outcomes": []string{"YES", "NO"}, "mode": "SANDBOX", "open_at": testNow, "close_at": testNow.Add(time.Hour),
		"dispute_window_ms": 600000, "oracle_source": "pm-http-oracle", "oracle_public_key": hex.EncodeToString(make([]byte, 32)),
	}
}

func pmMutations() []pmMutation {
	noBody := map[string]any{"different": "body"}
	set := func(q string) map[string]any {
		return map[string]any{"account_id": testAccountID.String(), "quantity": q}
	}
	reason := func(r string) map[string]any { return map[string]any{"reason": r} }
	return []pmMutation{
		{"create", pmBase, pmCreateBody("Which outcome will the sandbox election board certify?"),
			pmCreateBody("Which outcome will the sandbox board certify after the recount?"), "Create", false},
		{"issue", pmPath + "/issuances", set("3000000"), set("2000000"), "Issue", true},
		{"redeem", pmPath + "/redemptions", set("1000000"), set("2000000"), "Redeem", true},
		{"open", pmPath + "/open", nil, noBody, "Open", false},
		{"close", pmPath + "/close", nil, noBody, "Close", false},
		{"request-resolution", pmPath + "/request-resolution", nil, noBody, "RequestResolution", false},
		{"observe", pmPath + "/oracle-observations",
			map[string]any{"outcome": "YES", "observed_at": testNow.Add(time.Hour), "evidence": `{"certified":"YES"}`},
			map[string]any{"outcome": "NO", "observed_at": testNow.Add(time.Hour), "evidence": `{"certified":"NO"}`},
			"Observe", false},
		{"resolve", pmPath + "/resolve", reason("the board certified YES"), reason("the board certified it again"), "Resolve", false},
		{"dispute", pmPath + "/dispute",
			map[string]any{"reason": "the certificate was published before the recount", "evidence": map[string]any{"notice": "recount"}},
			map[string]any{"reason": "the certificate names the wrong district", "evidence": map[string]any{"notice": "district"}},
			"Dispute", false},
		{"uphold", pmPath + "/uphold", reason("the board's certificate stands"), reason("the recount confirmed it"), "Uphold", false},
		{"void", pmPath + "/void", reason("the event was cancelled by its organiser"), reason("the event never took place"), "Void", false},
		{"settle", pmPath + "/settle", nil, noBody, "Settle", false},
		{"reconcile", pmPath + "/reconcile", nil, noBody, "Reconcile", false},
	}
}

// pmReads are the GET routes, each with valid parameters.
func pmReads() []string {
	return []string{
		pmBase + "?status=OPEN",
		pmBase + "/oracle",
		pmPath,
		pmPath + "/books",
		pmPath + "/positions?account_id=" + testAccountID.String(),
	}
}

func TestPredmarket_UnwiredPortIsUnsupportedOnEveryRoute(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	op := pmOperator()
	h.as(&op)
	for _, path := range pmReads() {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", path, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, path)
	}
	for i, m := range pmMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "pm-unwired-"+itoa(i)+"-key")
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", m.name, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, m.name)
	}
}

func TestPredmarket_ListMarkets_ClampsTheLimitAndRendersExactFigures(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	res := h.do(http.MethodGet, pmBase+"?status=OPEN&limit=5000", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, predmarket.StatusOpen, fp.lastStatus)
	assert.Equal(t, maxPageLimit, fp.lastLimit, "the limit is clamped")

	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	assert.Equal(t, "OPEN", raw["status"])
	items := raw["items"].([]any)
	require.Len(t, items, 1)
	m := items[0].(map[string]any)
	assert.Equal(t, "SIMULATED", m["value_domain"])
	assert.Equal(t, pmMarketID.String(), m["id"])
	assert.Equal(t, "1000000", m["unit_quantity"], "base units as an exact integer string")
	assert.Equal(t, float64(6), m["share_decimals"])
	assert.Equal(t, econ.SandboxDollarSymbol, m["collateral_symbol"])
	assert.Equal(t, econ.SandboxDollarID().String(), m["collateral_asset_id"])
	assert.Equal(t, float64(600000), m["dispute_window_ms"])
	assert.Equal(t, hex.EncodeToString(fp.oracle.PublicKey()), m["oracle_public_key"])
	assert.Equal(t, "abcd", m["definition_hash"])
	assert.Equal(t, "SANDBOX", m["mode"])
	outcomes := m["outcomes"].([]any)
	require.Len(t, outcomes, 2)
	assert.Equal(t, "NO", outcomes[1].(map[string]any)["symbol"])
}

func TestPredmarket_ListMarkets_RefusesAStatusTheServiceDoesNotHave(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	res := h.do(http.MethodGet, pmBase+"?status=LIVE", nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
	assert.Zero(t, fp.count("Markets"), "a refused filter never reaches the service")

	res = h.do(http.MethodGet, pmBase, nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "status is required; body=%s", res.Body.String())
}

func TestPredmarket_MarketDetail_RendersItsResolutionsDisputesAndSettlement(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	res := h.do(http.MethodGet, pmPath, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var d api.PredictionMarketDetail
	res.json(&d)
	assert.Equal(t, api.ValueDomainSimulated("SIMULATED"), d.ValueDomain)
	assert.Equal(t, "SETTLED", d.Market.Status)
	require.Len(t, d.Resolutions, 2)
	assert.Equal(t, api.PredictionResolutionKind("ORACLE"), d.Resolutions[0].Kind)
	assert.Equal(t, "YES", d.Resolutions[0].OutcomeSymbol, "the index is named by the market's own outcome")
	require.NotNil(t, d.Resolutions[0].ReportHash)
	assert.Equal(t, "feed", *d.Resolutions[0].ReportHash)
	require.NotNil(t, d.Resolutions[1].SupersedesId)
	assert.Equal(t, fp.resolutions[0].ID.String(), d.Resolutions[1].SupersedesId.String())
	assert.Equal(t, "0b", d.Resolutions[1].EvidenceSha256)
	require.Len(t, d.Disputes, 1)
	assert.Equal(t, "0193b2e0-0000-7000-8000-0000000000c3", d.Disputes[0].RaisedBy.ActorId)
	require.NotNil(t, d.Settlement)
	assert.Equal(t, "PAYOUT", d.Settlement.Kind)
	assert.Equal(t, "5000000", d.Settlement.EscrowBefore)
	assert.Equal(t, "5000000", d.Settlement.TotalPaid)
	assert.Equal(t, "0", d.Settlement.Residual)
	require.NotNil(t, d.Settlement.WinningOutcomeSymbol)
	assert.Equal(t, "YES", *d.Settlement.WinningOutcomeSymbol)

	// A market that has not settled has no settlement key at all.
	fp.settlement = nil
	res = h.do(http.MethodGet, pmPath, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	_, present := res.raw()["settlement"]
	assert.False(t, present)
}

func TestPredmarket_MarketDetail_UnknownAndMalformedIds(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	fp.err = errs.New(errs.CodeNotFound, "predmarket: market not found")
	res := h.do(http.MethodGet, pmPath, nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)

	res = h.do(http.MethodGet, pmBase+"/not-a-uuid", nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
}

func TestPredmarket_Books_StateTheFiguresExactlyAndTheIdentityVerdict(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	res := h.do(http.MethodGet, pmPath+"/books", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	assert.Equal(t, "5000000", raw["escrow"])
	assert.Equal(t, "5000000", raw["collateral"])
	assert.Equal(t, "0", raw["residual"])
	assert.Equal(t, true, raw["reconciles"])
	assert.Equal(t, []any{}, raw["violations"], "an empty list, never null")
	outcomes := raw["outcomes"].([]any)
	require.Len(t, outcomes, 2)
	yes := outcomes[0].(map[string]any)
	assert.Equal(t, "YES", yes["symbol"])
	assert.Equal(t, "5000000", yes["shares_issued"])
	assert.Equal(t, "5000000", yes["held_by_customers"])

	fp.violations = []string{"outcome 0: customers hold 4000000 shares and 5000000 are outstanding"}
	res = h.do(http.MethodGet, pmPath+"/books", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.Equal(t, false, raw["reconciles"])
	assert.Equal(t, []any{fp.violations[0]}, raw["violations"])

	// Books that do not name every outcome are a fault, not a figure.
	fp.books.HeldByCustomers = fp.books.HeldByCustomers[:1]
	res = h.do(http.MethodGet, pmPath+"/books", nil)
	require.Equal(t, http.StatusInternalServerError, res.Code, "body=%s", res.Body.String())
}

func TestPredmarket_Positions_AreScopedToTheCallersAccount(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	res := h.do(http.MethodGet, pmPath+"/positions?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testAccountID, fp.lastAccount)
	var list api.PredictionPositionList
	res.json(&list)
	assert.Equal(t, api.ValueDomainSimulated("SIMULATED"), list.ValueDomain)
	assert.Equal(t, testAccountID.String(), list.AccountId.String())
	assert.Equal(t, 6, list.ShareDecimals)
	require.Len(t, list.Items, 2, "only the caller's own shares")
	assert.Equal(t, "3000000", list.Items[0].Quantity)

	// Another customer's account is refused before any market is looked up,
	// so the refusal is the same whether or not the market exists.
	calls := fp.count("Positions")
	res = h.do(http.MethodGet, pmPath+"/positions?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	fp.err = errs.New(errs.CodeNotFound, "predmarket: market not found")
	other := pmBase + "/" + predmarket.NewMarketID().String() + "/positions?account_id=" + testOtherAcct.String()
	res = h.do(http.MethodGet, other, nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, calls, fp.count("Positions"), "a refused account never reaches the port")
	fp.err = nil

	op := pmOperator()
	h.as(&op)
	res = h.do(http.MethodGet, pmPath+"/positions?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "an operator with account:read_any reads any account; body=%s", res.Body.String())
	assert.Equal(t, testOtherAcct, fp.lastAccount)
	res.json(&list)
	require.Len(t, list.Items, 1)
	assert.Equal(t, "2000000", list.Items[0].Quantity)
}

func TestPredmarket_Oracle_NamesItsKeyOrSaysThereIsNone(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	res := h.do(http.MethodGet, pmBase+"/oracle", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var o api.PredictionOracle
	res.json(&o)
	assert.Equal(t, "pm-http-oracle", o.Source)
	assert.Equal(t, hex.EncodeToString(fp.oracle.PublicKey()), o.PublicKey)
	assert.Equal(t, api.PredictionOracleKind("SANDBOX"), o.Kind)

	fp.err = errs.New(errs.CodeProviderUnavailable, "no prediction market oracle is configured in this deployment")
	res = h.do(http.MethodGet, pmBase+"/oracle", nil)
	require.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeProviderUnavailable, res.problem().Code)
}

func TestPredmarket_IssueAndRedeem_ActForTheOwnerUnderANamespacedIntentKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path, action, call, key string
	}{
		{"/issuances", "ISSUE", "Issue", "pm-issue-key-0001"},
		{"/redemptions", "REDEEM", "Redeem", "pm-redeem-key-0001"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			t.Parallel()
			h, fp := newPredmarketHarness(t)
			res := h.do(http.MethodPost, pmPath+tc.path,
				map[string]any{"account_id": testAccountID.String(), "quantity": "3000000"}, "Idempotency-Key", tc.key)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			req := fp.lastPosition
			assert.Equal(t, pmMarketID, req.MarketID)
			assert.Equal(t, testAccountID, req.AccountID)
			assert.Equal(t, "3000000", req.Quantity.String())
			assert.Equal(t, "http:predmarket:"+map[string]string{"ISSUE": "issue", "REDEEM": "redeem"}[tc.action]+":"+tc.key,
				req.IdempotencyKey, "the client key is namespaced for the economic intent")
			assert.Equal(t, security.ActorUser, req.ActorType)
			assert.Equal(t, testUserID.String(), req.ActorID)

			raw := res.raw()
			assert.Equal(t, "SIMULATED", raw["value_domain"])
			assert.Equal(t, tc.action, raw["action"])
			assert.Equal(t, "3000000", raw["quantity"])
			assert.Equal(t, false, raw["replay"])
			assert.Equal(t, fp.action.Intent.ID.String(), raw["intent"].(map[string]any)["id"])
			assert.Equal(t, "COMPLETED", raw["intent"].(map[string]any)["status"])
			assert.Equal(t, "APPLIED", raw["effect"].(map[string]any)["status"])
			assert.Equal(t, fp.action.JournalTransactionID.String(), raw["journal_transaction_id"])
			assert.Equal(t, "decision-hash-1", raw["risk_decision_hash"])
			assert.Equal(t, 1, fp.count(tc.call))
		})
	}
}

func TestPredmarket_IssueAndRedeem_OnlyTheOwnerMayAct(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	// A customer naming someone else's account.
	res := h.do(http.MethodPost, pmPath+"/issuances",
		map[string]any{"account_id": testOtherAcct.String(), "quantity": "1000000"}, "Idempotency-Key", "pm-foreign-issue-1")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)

	// An operator: account:read_any is a read permission and never licenses
	// issuing sets on a customer's account.
	op := pmOperator()
	h.as(&op)
	res = h.do(http.MethodPost, pmPath+"/redemptions",
		map[string]any{"account_id": testAccountID.String(), "quantity": "1000000"}, "Idempotency-Key", "pm-operator-redeem-1")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Zero(t, fp.count("Issue")+fp.count("Redeem"), "a refused account never reaches the service")
}

func TestPredmarket_IssueAndRedeem_ValidationAndServiceRefusals(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	for i, bad := range []string{"1.5", "", "1e6", "+1000000"} {
		res := h.do(http.MethodPost, pmPath+"/issuances",
			map[string]any{"account_id": testAccountID.String(), "quantity": bad}, "Idempotency-Key", "pm-bad-qty-"+itoa(i)+"-x")
		require.Equal(t, http.StatusBadRequest, res.Code, "%q body=%s", bad, res.Body.String())
		assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
	}
	assert.Zero(t, fp.count("Issue"))

	// The service's refusals arrive with their own codes.
	for i, tc := range []struct {
		err    error
		status int
	}{
		{errs.New(errs.CodeRiskRejected, "risk rejected: RISK_INSUFFICIENT_BALANCE"), http.StatusUnprocessableEntity},
		{errs.New(errs.CodeInvalidStateTransition, "predmarket: market PMHTTP is CLOSED"), http.StatusConflict},
		{errs.New(errs.CodeValidationFailed, "predmarket: complete sets are issued and redeemed whole"), http.StatusBadRequest},
		{errs.New(errs.CodeReconciliationRequired, "predmarket: the market's escrow does not reconcile"), http.StatusConflict},
	} {
		fp.err = tc.err
		res := h.do(http.MethodPost, pmPath+"/issuances",
			map[string]any{"account_id": testAccountID.String(), "quantity": "3000000"}, "Idempotency-Key", "pm-refusal-"+itoa(i)+"-key")
		require.Equal(t, tc.status, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, errs.CodeOf(tc.err), res.problem().Code)
	}
}

// Every mutation runs under the Idempotency-Key contract: the same key and
// body replay the first answer without a second service call, and the same key
// with another body is refused.
func TestPredmarket_EveryMutationReplaysItsFirstAnswer(t *testing.T) {
	t.Parallel()
	for _, m := range pmMutations() {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, fp := newPredmarketHarness(t)
			if !m.customer {
				op := pmOperator()
				h.as(&op)
			}
			key := "pm-replay-" + m.name
			first := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Less(t, first.Code, 300, "body=%s", first.Body.String())
			second := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Less(t, second.Code, 300, "body=%s", second.Body.String())
			assert.JSONEq(t, first.Body.String(), second.Body.String(), "a replay answers exactly what the first call did")
			assert.Equal(t, 1, fp.count(m.call), "a replay never reaches the service")
			assert.Equal(t, "SIMULATED", first.raw()["value_domain"], "every answer states its value domain")

			reuse := h.do(http.MethodPost, m.path, m.other, "Idempotency-Key", key)
			require.Equal(t, http.StatusConflict, reuse.Code, "body=%s", reuse.Body.String())
			assert.Equal(t, errs.CodeInvalidIdempotencyReuse, reuse.problem().Code)
			assert.Equal(t, 1, fp.count(m.call))
		})
	}
}

func TestPredmarket_OperatorRoutes_RefuseCustomersAndStaleSessions(t *testing.T) {
	t.Parallel()
	for _, m := range pmMutations() {
		if m.customer {
			continue
		}
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, fp := newPredmarketHarness(t)
			res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "pm-customer-"+m.name)
			require.Equal(t, http.StatusForbidden, res.Code, "a customer reached %s: %s", m.name, res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)

			stale := pmOperator()
			stale.AuthTime = testNow.Add(-2 * stepUpMaxAge)
			h.as(&stale)
			res = h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "pm-stale-"+m.name)
			if m.name == "reconcile" {
				// It compares the books and records what it found; it moves
				// nothing, so it does not ask for a step-up.
				require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
				return
			}
			require.Equal(t, http.StatusForbidden, res.Code, "%s without a recent step-up: %s", m.name, res.Body.String())
			assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
			assert.Zero(t, fp.count(m.call))
		})
	}
}

func TestPredmarket_CreateMarket_TheCollateralIsTheSandboxDollar(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	op := pmOperator()
	h.as(&op)
	create := pmMutations()[0]
	res := h.do(http.MethodPost, create.path, create.body, "Idempotency-Key", "pm-create-0001")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	spec := fp.lastSpec
	assert.Equal(t, econ.SandboxDollarID(), spec.CollateralAssetID, "the collateral is not the client's choice")
	assert.Equal(t, gates.ModeSandbox, spec.Mode)
	assert.Equal(t, []string{"YES", "NO"}, spec.Outcomes)
	assert.Equal(t, 10*time.Minute, spec.DisputeWindow)
	assert.Equal(t, make([]byte, 32), []byte(spec.OraclePublicKey))
	assert.True(t, spec.OpenAt.Equal(testNow))
	assert.Equal(t, econ.Actor{Type: security.ActorOperator, ID: pmOperatorID.String()}, fp.lastActor,
		"the principal is the actor, so dual control compares real people")

	// The same key replays as 200; an identical definition under a new key is
	// the service's replay and is 200 too.
	res = h.do(http.MethodPost, create.path, create.body, "Idempotency-Key", "pm-create-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	fp.existing = true
	res = h.do(http.MethodPost, create.path, create.body, "Idempotency-Key", "pm-create-0002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 2, fp.count("Create"))
}

func TestPredmarket_CreateMarket_ValidationAndRefusals(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	op := pmOperator()
	h.as(&op)
	body := func(change func(map[string]any)) map[string]any {
		b := pmCreateBody("Which outcome will the sandbox election board certify?")
		change(b)
		return b
	}
	res := h.do(http.MethodPost, pmBase, body(func(b map[string]any) { b["oracle_public_key"] = "not-hex" }),
		"Idempotency-Key", "pm-create-badkey")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)

	res = h.do(http.MethodPost, pmBase, body(func(b map[string]any) { b["dispute_window_ms"] = int64(1) << 62 }),
		"Idempotency-Key", "pm-create-overflow")
	require.Equal(t, http.StatusBadRequest, res.Code, "a window no duration can hold is refused, not wrapped; body=%s", res.Body.String())
	assert.Zero(t, fp.count("Create"))

	fp.err = errs.New(errs.CodeInvalidIdempotencyReuse, "predmarket: a market with this symbol already exists with another definition")
	res = h.do(http.MethodPost, pmBase, body(func(map[string]any) {}), "Idempotency-Key", "pm-create-other")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, res.problem().Code)
}

// The service forbids the creator from resolving or voiding and a disputer
// from ruling; this layer re-implements none of it and passes the refusal on.
func TestPredmarket_DualControlRefusalsReachTheClient(t *testing.T) {
	t.Parallel()
	for _, m := range pmMutations() {
		switch m.name {
		case "resolve", "uphold", "void":
		default:
			continue
		}
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, fp := newPredmarketHarness(t)
			op := pmOperator()
			h.as(&op)
			fp.err = errs.New(errs.CodeForbidden, "predmarket: dual control: the creator of a market cannot resolve it")
			res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "pm-dual-"+m.name)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			p := res.problem()
			assert.Equal(t, errs.CodeForbidden, p.Code)
			assert.Contains(t, p.Detail, "dual control")
			assert.Equal(t, econ.Actor{Type: security.ActorOperator, ID: pmOperatorID.String()}, pmActorOf(fp, m.name))
		})
	}
}

func pmActorOf(fp *fakePredmarket, name string) econ.Actor {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	switch name {
	case "resolve":
		return fp.lastResolve.Actor
	case "uphold":
		return fp.lastRuling.Actor
	case "void":
		return fp.lastVoid.Actor
	}
	return fp.lastActor
}

func TestPredmarket_Observation_ReturnsAReportThePackageVerifies(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	op := pmOperator()
	h.as(&op)
	observe := pmMutations()[6]
	res := h.do(http.MethodPost, observe.path, observe.body, "Idempotency-Key", "pm-observe-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.PredictionOracleObservation
	res.json(&out)
	assert.Equal(t, pmMarketID.String(), out.Report.MarketId)
	assert.Equal(t, "YES", out.Report.Outcome)

	wire, err := json.Marshal(out.Report)
	require.NoError(t, err)
	report, err := predmarket.DecodeOracleReport(wire)
	require.NoError(t, err, "the wire form is the package's own")
	require.NoError(t, predmarket.VerifyReport(fp.oracle.PublicKey(), report), "the signature verifies against the oracle key")
	assert.Equal(t, hex.EncodeToString(report.Hash()), out.ReportHash)
	assert.Equal(t, `{"certified":"YES"}`, string(report.Evidence))

	fp.err = errs.New(errs.CodeProviderUnavailable, "no prediction market oracle is configured in this deployment")
	res = h.do(http.MethodPost, observe.path, observe.body, "Idempotency-Key", "pm-observe-0002")
	require.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
}

func TestPredmarket_ResolutionDisputeAndRulingShapes(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	op := pmOperator()
	h.as(&op)

	res := h.do(http.MethodPost, pmPath+"/resolve", map[string]any{"reason": "the board certified YES"}, "Idempotency-Key", "pm-resolve-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "the board certified YES", fp.lastResolve.Reason)
	assert.Equal(t, pmMarketID, fp.lastResolve.MarketID)
	var rr api.PredictionResolutionResult
	res.json(&rr)
	assert.Equal(t, "YES", rr.Resolution.OutcomeSymbol)
	assert.False(t, rr.Replay)
	fp.replay = true
	res = h.do(http.MethodPost, pmPath+"/resolve", map[string]any{}, "Idempotency-Key", "pm-resolve-0002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&rr)
	assert.True(t, rr.Replay, "an accepted report replayed says so")
	assert.Empty(t, fp.lastResolve.Reason)

	res = h.do(http.MethodPost, pmPath+"/dispute",
		map[string]any{"reason": "the certificate was published before the recount", "evidence": map[string]any{"notice": "recount"}},
		"Idempotency-Key", "pm-dispute-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, map[string]interface{}{"notice": "recount"}, fp.lastDispute.Evidence)
	var dr api.PredictionDisputeResult
	res.json(&dr)
	assert.Equal(t, fp.dispute.ID.String(), dr.Dispute.Id.String())

	// A dispute sent without its evidence reaches the service with none, so
	// the service's "a dispute carries its evidence" rule decides.
	res = h.do(http.MethodPost, pmPath+"/dispute", map[string]any{"reason": "no evidence attached here"},
		"Idempotency-Key", "pm-dispute-0002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Nil(t, fp.lastDispute.Evidence)

	res = h.do(http.MethodPost, pmPath+"/uphold", map[string]any{"reason": "the board's certificate stands"},
		"Idempotency-Key", "pm-uphold-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Nil(t, fp.lastRuling.Evidence)
	res.json(&rr)
	assert.Equal(t, api.PredictionResolutionKind("DISPUTE_UPHELD"), rr.Resolution.Kind)
}

func TestPredmarket_Settlement_ListsWhatThisCallPaid(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	op := pmOperator()
	h.as(&op)
	res := h.do(http.MethodPost, pmPath+"/settle", nil, "Idempotency-Key", "pm-settle-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, econ.Actor{Type: security.ActorOperator, ID: pmOperatorID.String()}, fp.lastActor)
	var sr api.PredictionSettlementResult
	res.json(&sr)
	assert.Equal(t, api.ValueDomainSimulated("SIMULATED"), sr.ValueDomain)
	assert.False(t, sr.Replay)
	assert.Equal(t, "5000000", sr.Settlement.TotalPaid)
	require.Len(t, sr.Lines, 2)
	assert.Equal(t, testAccountID.String(), sr.Lines[0].AccountId.String())
	assert.Equal(t, "3000000", sr.Lines[0].Amount)
	require.Len(t, sr.Lines[0].Shares, 2)
	assert.Equal(t, "YES", sr.Lines[0].Shares[0].OutcomeSymbol)
	assert.Equal(t, "3000000", sr.Lines[0].Shares[0].Quantity)
	assert.Equal(t, fp.lines[1].Intent.ID.String(), sr.Lines[1].IntentId.String())
	require.NotNil(t, sr.Lines[1].JournalTransactionId)
	assert.Equal(t, fp.lines[1].JournalTransactionID.String(), sr.Lines[1].JournalTransactionId.String())

	// Settling a settled market is the service's replay: nothing paid.
	fp.replay = true
	res = h.do(http.MethodPost, pmPath+"/settle", nil, "Idempotency-Key", "pm-settle-0002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&sr)
	assert.True(t, sr.Replay)
	assert.Empty(t, sr.Lines)
	assert.Equal(t, []any{}, res.raw()["lines"], "an empty list, never null")

	// A void passes its reason and evidence through.
	res = h.do(http.MethodPost, pmPath+"/void",
		map[string]any{"reason": "the event was cancelled by its organiser", "evidence": map[string]any{"notice": "cancelled"}},
		"Idempotency-Key", "pm-void-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "the event was cancelled by its organiser", fp.lastVoid.Reason)
	assert.Equal(t, map[string]interface{}{"notice": "cancelled"}, fp.lastVoid.Evidence)
}

func TestPredmarket_Reconcile_RecordsAsThePrincipal(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	op := pmOperator()
	h.as(&op)
	res := h.do(http.MethodPost, pmPath+"/reconcile", nil, "Idempotency-Key", "pm-reconcile-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, reconciliation.Actor{Type: security.ActorOperator, ID: pmOperatorID.String()}, fp.lastRecActor)
	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	assert.Equal(t, "PREDICTION_ESCROW", raw["kind"])
	assert.Equal(t, "FULL", raw["mode"])
	assert.Equal(t, "MATCHED", raw["status"])
	assert.Equal(t, map[string]any{"escrow": "5000000"}, raw["observed"])
	assert.Equal(t, map[string]any{"violations": []any{}}, raw["difference"])
}
