package httpapi

import (
	"context"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
)

var (
	testOpsMarkID      = valuation.NewPriceID()
	testOpsStableID    = uuid.MustParse("0193b2e0-0000-7000-8000-0000000000d1")
	testOpsStableAsset = uuid.MustParse("0193b2e0-0000-7000-8000-0000000000d2")
	testOpsBridgeID    = uuid.MustParse("0193b2e0-0000-7000-8000-0000000000d3")
	testOpsRecordID    = uuid.MustParse("0193b2e0-0000-7000-8000-0000000000d4")
)

// fakeOperatorPort is the operator console port. It records what reached it.
type fakeOperatorPort struct {
	stubErr
	mu       sync.Mutex
	overview OpsOverview
	recorded ReferenceMarkRecorded
	list     ReferenceMarkList
	chains   OpsChains
	chainErr error

	records     int
	lastRecord  ReferenceMarkCommand
	lastAsset   assets.AssetID
	lastLimit   int
	overviewHit int
}

func (f *fakeOperatorPort) Overview(context.Context) (OpsOverview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overviewHit++
	return f.overview, f.fail()
}

func (f *fakeOperatorPort) RecordMark(_ context.Context, r ReferenceMarkCommand) (ReferenceMarkRecorded, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records++
	f.lastRecord = r
	if err := f.fail(); err != nil {
		return ReferenceMarkRecorded{}, err
	}
	return f.recorded, nil
}

func (f *fakeOperatorPort) Marks(_ context.Context, asset assets.AssetID, limit int) (ReferenceMarkList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAsset, f.lastLimit = asset, limit
	return f.list, f.fail()
}

func (f *fakeOperatorPort) Chains(context.Context) (OpsChains, error) {
	if f.chainErr != nil {
		return OpsChains{}, f.chainErr
	}
	return f.chains, nil
}

// sampleOpsMark is a mark of 2500.125 sandbox dollars per unit, observed 90
// seconds before testNow.
func sampleOpsMark() ReferenceMark {
	observed := testNow.Add(-90 * time.Second)
	return ReferenceMark{
		ID: testOpsMarkID, AssetID: testAssetID, QuoteAssetID: econ.SandboxDollarID(), Mantissa: money.QuantityFromInt64(2_500_125),
		Scale: 3, Source: "ops-desk", ObservedAt: observed, ReceivedAt: testNow, Age: 90 * time.Second,
	}
}

func newOperatorHarness(t *testing.T) (*harness, *fakeOperatorPort) {
	t.Helper()
	h := newHarness(t)
	activated := testNow.Add(-time.Hour)
	opened := testNow.Add(-2 * time.Hour)
	oldest := 2 * time.Hour
	markAt := testNow.Add(-10 * time.Minute)
	markAge := 10 * time.Minute
	fo := &fakeOperatorPort{
		overview: OpsOverview{
			GeneratedAt: testNow, Environment: "TEST", MaxMarkAge: 5 * time.Minute,
			KillSwitches: []killswitch.Switch{{Kind: killswitch.Kind("CAPABILITY_DISABLE"), ScopeID: "MARGIN", Active: true,
				Severity: killswitch.Severity("SEVERE"), Reason: "drill", ActivatedBy: testUserID.String(), ActivatedAt: &activated}},
			StableAssets: []OpsStableAsset{{ID: testOpsStableID, Name: "sbx-usd-stable", AssetID: testOpsStableAsset, ChainID: "sbx-solana",
				Status: "PAUSED", LatestAlert: &OpsPegAlert{Kind: "PRICE_DEVIATION", Severity: "CRITICAL", BreakerTripped: true, ObservedAt: opened},
				LatestSupplyRecord: &OpsReconciliationRecord{ID: testOpsRecordID, Kind: "STABLE_RESERVE", Status: "MISMATCH", Material: true,
					BlocksNewRisk: true, OpenedAt: opened}}},
			BridgeRoutes: []OpsBridgeRoute{{ID: testOpsBridgeID, Name: "sol-to-eth", SourceChain: "sbx-solana",
				DestinationChain: "sbx-ethereum", Status: "ACTIVE"}},
			Reconciliation: []OpsReconciliationKind{{Kind: "STABLE_RESERVE", Unresolved: 1, Material: 1, BlocksNewRisk: 1,
				OldestOpened: &opened, OldestAge: &oldest}, {Kind: "BRIDGE"}},
			DualControl: OpsDualControl{TransferReversalsPending: 2, MarginCasesInReview: 1, MarginResolutionsPending: 1},
			Oracle: []OpsOracleAsset{{AssetID: testAssetID, Symbol: "SBX-SOL", ObservedAt: &markAt, Source: "ops-desk", Age: &markAge, Stale: true},
				{AssetID: testMarginSOL, Symbol: "SBX-ETH", Stale: true}},
		},
		recorded: ReferenceMarkRecorded{Mark: sampleOpsMark(), AssetSymbol: "SBX-SOL", QuoteSymbol: "SBX-USD", MaxAge: 5 * time.Minute},
		list: ReferenceMarkList{
			Asset:  assets.Asset{ID: testAssetID, Symbol: "SBX-SOL", Kind: assets.KindSimulated},
			Quote:  assets.Asset{ID: econ.SandboxDollarID(), Symbol: "SBX-USD", Kind: assets.KindSimulated},
			MaxAge: 5 * time.Minute, AsOf: testNow, Items: []ReferenceMark{sampleOpsMark()},
		},
		chainErr: errs.New(errs.CodeUnsupported, "no simulated chain host is configured in this deployment"),
	}
	ports := h.ports.ports()
	ports.Operator = fo
	h.server.opts.Ports = ports
	op := operatorPrincipal()
	h.as(&op)
	return h, fo
}

func opsMarkBody() map[string]any {
	return map[string]any{
		"asset_id": testAssetID.String(), "price": "2500.125", "source": "ops-desk",
		"observed_at": testNow.Add(-90 * time.Second).Format(time.RFC3339Nano),
	}
}

// Every route of the console is an operator's: a customer is refused before
// anything reaches the port.
func TestOperator_TheConsoleIsAnOperatorsOnly(t *testing.T) {
	t.Parallel()
	h, fo := newOperatorHarness(t)
	customer := customerPrincipal()
	h.as(&customer)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/advanced/ops/overview", nil},
		{http.MethodGet, "/v1/advanced/ops/marks?asset_id=" + testAssetID.String(), nil},
		{http.MethodPost, "/v1/advanced/ops/marks", opsMarkBody()},
		{http.MethodGet, "/v1/advanced/ops/chains", nil},
	} {
		res := h.do(c.method, c.path, c.body, "Idempotency-Key", "ops-customer-0001")
		require.Equal(t, http.StatusForbidden, res.Code, "%s %s body=%s", c.method, c.path, res.Body.String())
		assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	}
	assert.Zero(t, fo.overviewHit+fo.records)
	assert.True(t, fo.lastAsset.IsZero())
}

func TestOperator_TheOverviewRendersOnlyWhatTheAdapterRead(t *testing.T) {
	t.Parallel()
	h, _ := newOperatorHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/ops/overview", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.AdvancedOpsOverview
	res.json(&out)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)
	assert.Equal(t, "TEST", out.Environment)
	assert.Equal(t, int64(300_000), out.MaxMarkAgeMs)
	assert.False(t, out.ChainsConfigured)

	require.Len(t, out.KillSwitches, 1)
	assert.Equal(t, "CAPABILITY_DISABLE", out.KillSwitches[0].Kind)
	assert.Equal(t, "MARGIN", out.KillSwitches[0].ScopeId)

	require.Len(t, out.StableAssets, 1)
	s := out.StableAssets[0]
	assert.Equal(t, testOpsStableID, s.Id)
	assert.Equal(t, "PAUSED", s.Status)
	require.NotNil(t, s.LatestAlert)
	assert.True(t, s.LatestAlert.BreakerTripped)
	require.NotNil(t, s.LatestSupplyReconciliation)
	assert.Equal(t, "MISMATCH", s.LatestSupplyReconciliation.Status)

	require.Len(t, out.BridgeRoutes, 1)
	assert.Nil(t, out.BridgeRoutes[0].OpenReconciliation, "no open reconciliation is never invented")

	require.Len(t, out.Reconciliation, 2)
	require.NotNil(t, out.Reconciliation[0].OldestAgeMs)
	assert.Equal(t, int64(7_200_000), *out.Reconciliation[0].OldestAgeMs)
	assert.Nil(t, out.Reconciliation[1].OldestAgeMs, "a kind with nothing unresolved has no age")
	assert.Zero(t, out.Reconciliation[1].Unresolved)

	assert.Equal(t, int64(2), out.DualControl.TransferReversalsPending)
	assert.Equal(t, int64(1), out.DualControl.MarginResolutionsPending)

	require.Len(t, out.Oracle, 2)
	require.NotNil(t, out.Oracle[0].AgeMs)
	assert.Equal(t, int64(600_000), *out.Oracle[0].AgeMs)
	assert.True(t, out.Oracle[0].Stale)
	assert.Nil(t, out.Oracle[1].ObservedAt, "an asset with no mark at all shows none")
	assert.Nil(t, out.Oracle[1].AgeMs)
	assert.True(t, out.Oracle[1].Stale)
}

func TestOperator_AMarkCarriesThePriceAsTypedAndIsReplayedNotReExecuted(t *testing.T) {
	t.Parallel()
	h, fo := newOperatorHarness(t)
	res := h.do(http.MethodPost, "/v1/advanced/ops/marks", opsMarkBody(), "Idempotency-Key", "ops-mark-key-0001")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	require.Equal(t, 1, fo.records)
	got := fo.lastRecord
	assert.Equal(t, testAssetID, got.AssetID)
	assert.Equal(t, "2500.125", got.Price, "the decimal exactly as typed")
	assert.Equal(t, "ops-desk", got.Source)
	require.NotNil(t, got.ObservedAt)
	assert.True(t, got.ObservedAt.Equal(testNow.Add(-90*time.Second)))
	assert.Nil(t, got.QuoteAssetID)
	assert.Equal(t, security.ActorOperator, got.Actor.Type)
	assert.Equal(t, testUserID.String(), got.Actor.ID)

	var out api.AdvancedReferenceMarkRecorded
	res.json(&out)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)
	assert.Equal(t, "2500.125", out.Mark.Price)
	assert.Equal(t, "2500125", out.Mark.Mantissa)
	assert.Equal(t, 3, out.Mark.Scale)
	assert.Equal(t, int64(90_000), out.Mark.AgeMs)
	assert.False(t, out.Duplicate)
	assert.Equal(t, "SBX-USD", out.QuoteAssetSymbol)

	again := h.do(http.MethodPost, "/v1/advanced/ops/marks", opsMarkBody(), "Idempotency-Key", "ops-mark-key-0001")
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, res.Body.String(), again.Body.String())
	assert.Equal(t, 1, fo.records, "a replay must not reach the port")

	changed := opsMarkBody()
	changed["price"] = "2500.126"
	res = h.do(http.MethodPost, "/v1/advanced/ops/marks", changed, "Idempotency-Key", "ops-mark-key-0001")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, res.problem().Code)

	// The same observation recorded before answers 200: nothing new exists.
	fo.recorded.Duplicate = true
	res = h.do(http.MethodPost, "/v1/advanced/ops/marks", opsMarkBody(), "Idempotency-Key", "ops-mark-key-0002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, true, res.raw()["duplicate"])

	quote := opsMarkBody()
	quote["quote_asset_id"] = econ.SandboxDollarID().String()
	res = h.do(http.MethodPost, "/v1/advanced/ops/marks", quote, "Idempotency-Key", "ops-mark-key-0003")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.NotNil(t, fo.lastRecord.QuoteAssetID)
	assert.Equal(t, econ.SandboxDollarID(), *fo.lastRecord.QuoteAssetID)
}

func TestOperator_RecordingAMarkNeedsAFreshStepUp(t *testing.T) {
	t.Parallel()
	h, fo := newOperatorHarness(t)
	stale := operatorPrincipal()
	stale.AuthTime = testNow.Add(-2 * stepUpMaxAge)
	h.as(&stale)
	res := h.do(http.MethodPost, "/v1/advanced/ops/marks", opsMarkBody(), "Idempotency-Key", "ops-mark-stale-01")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
	assert.Zero(t, fo.records)
	// Reads need no step-up.
	res = h.do(http.MethodGet, "/v1/advanced/ops/overview", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
}

// The adapter's refusals reach the operator with their codes, and a refusal
// is replayed rather than decided again.
func TestOperator_TheAdaptersRefusalsKeepTheirCodes(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]error{
		"not simulated": errs.New(errs.CodeForbidden, "a sandbox reference mark values a SIMULATED asset only"),
		"future":        validationError("observed_at", "observed_at is in the future"),
		"over-precise":  errs.New(errs.CodePrecisionLoss, "price has 19 fractional digits"),
		"unknown asset": errs.New(errs.CodeNotFound, "asset not found"),
		"conflict":      errs.New(errs.CodeConflict, "a different price is already recorded for this observation identity"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, fo := newOperatorHarness(t)
			fo.err = refusal
			for i := 0; i < 2; i++ {
				res := h.do(http.MethodPost, "/v1/advanced/ops/marks", opsMarkBody(), "Idempotency-Key", "ops-refusal-0001")
				require.Equal(t, errs.HTTPStatus(errs.CodeOf(refusal)), res.Code, "body=%s", res.Body.String())
				assert.Equal(t, errs.CodeOf(refusal), res.problem().Code)
			}
			assert.Equal(t, 1, fo.records)
		})
	}
}

func TestOperator_AMarkRequestIsValidatedBeforeThePort(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(map[string]any){
		"bad asset":    func(b map[string]any) { b["asset_id"] = "not-a-uuid" },
		"bad quote":    func(b map[string]any) { b["quote_asset_id"] = "00000000-0000-0000-0000-000000000000" },
		"no price":     func(b map[string]any) { delete(b, "price") },
		"bad observed": func(b map[string]any) { b["observed_at"] = "yesterday" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, fo := newOperatorHarness(t)
			body := opsMarkBody()
			mutate(body)
			res := h.do(http.MethodPost, "/v1/advanced/ops/marks", body, "Idempotency-Key", "ops-bad-mark-0001")
			require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
			assert.Zero(t, fo.records)
		})
	}
}

func TestOperator_MarksAreListedWithTheirAge(t *testing.T) {
	t.Parallel()
	h, fo := newOperatorHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/ops/marks?asset_id="+testAssetID.String()+"&limit=3", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testAssetID, fo.lastAsset)
	assert.Equal(t, 3, fo.lastLimit)
	var out api.AdvancedReferenceMarkList
	res.json(&out)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)
	assert.Equal(t, "SIMULATED", out.AssetKind)
	assert.Equal(t, int64(300_000), out.MaxAgeMs)
	require.Len(t, out.Items, 1)
	assert.Equal(t, "2500.125", out.Items[0].Price)
	assert.Equal(t, int64(90_000), out.Items[0].AgeMs)
	assert.False(t, out.Items[0].Stale)
	assert.False(t, out.Items[0].Future)

	for _, q := range []string{"", "?asset_id=not-a-uuid"} {
		res = h.do(http.MethodGet, "/v1/advanced/ops/marks"+q, nil)
		require.Equal(t, http.StatusBadRequest, res.Code, "%q body=%s", q, res.Body.String())
	}
}

func TestOperator_ChainsAreUnsupportedWithoutAHostAndRenderedWithOne(t *testing.T) {
	t.Parallel()
	h, fo := newOperatorHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/ops/chains", nil)
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeUnsupported, res.problem().Code)

	produced := testNow.Add(-time.Minute)
	fo.chainErr = nil
	fo.chains = OpsChains{Network: "nodal-test", ObservedAt: testNow, Items: []OpsChainTip{
		{ChainID: "sbx-ethereum", Slot: 40, Height: 40, Hash: "4vJ9JU1bJJE96FWSJKvHsmmFADCg4gpZQff4P3bkLKi", Final: false, FinalizedHeight: 8,
			LastBlockProducedAt: &produced},
		{ChainID: "sbx-solana", Slot: 0, Height: 0, Hash: "11111111111111111111111111111111", Final: true},
	}}
	res = h.do(http.MethodGet, "/v1/advanced/ops/chains", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.AdvancedOpsChains
	res.json(&out)
	require.Len(t, out.Items, 2)
	assert.Equal(t, int64(40), out.Items[0].Height)
	assert.Equal(t, int64(8), out.Items[0].FinalizedHeight)
	require.NotNil(t, out.Items[0].LastBlockProducedAt)
	assert.Nil(t, out.Items[1].LastBlockProducedAt)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)
}

func TestOperator_AnUnwiredConsoleIsUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/advanced/ops/overview", nil},
		{http.MethodGet, "/v1/advanced/ops/marks?asset_id=" + testAssetID.String(), nil},
		{http.MethodPost, "/v1/advanced/ops/marks", opsMarkBody()},
		{http.MethodGet, "/v1/advanced/ops/chains", nil},
	} {
		res := h.do(c.method, c.path, c.body, "Idempotency-Key", "ops-unwired-0001")
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s %s body=%s", c.method, c.path, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
	}
}

// --- the adapter's rules, before any database ------------------------------------

func TestReferenceMark_APriceIsExactPositiveAndBounded(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]struct {
		mantissa string
		scale    int32
	}{
		"2500.125": {"2500125", 3}, "1": {"1", 0}, "0.000001": {"1", 6}, "0001.50": {"150", 2},
		"0.000000000000000001": {"1", 18}, "99999999999999999999999999999999999999": {"99999999999999999999999999999999999999", 0},
	} {
		q, scale, err := parseReferencePrice(raw)
		require.NoError(t, err, raw)
		assert.Equal(t, want.mantissa, q.String(), raw)
		assert.Equal(t, want.scale, scale, raw)
	}
	for raw, code := range map[string]errs.Code{
		"0": errs.CodeValidationFailed, "0.000": errs.CodeValidationFailed, "-1": errs.CodeValidationFailed,
		"1e5": errs.CodeValidationFailed, "1.": errs.CodeValidationFailed, ".5": errs.CodeValidationFailed,
		" 1": errs.CodeValidationFailed, "": errs.CodeValidationFailed, "1,5": errs.CodeValidationFailed,
		"0.0000000000000000001":                   errs.CodePrecisionLoss,
		"1.0000000000000000000":                   errs.CodePrecisionLoss,
		"100000000000000000000000000000000000000": errs.CodeValidationFailed, // 39 significant digits
	} {
		_, _, err := parseReferencePrice(raw)
		assert.Equal(t, code, errs.CodeOf(err), "%q", raw)
	}
}

func TestReferenceMark_TheSourceIsAPrintableLabel(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"ops-desk", "sbx-oracle:sol/usd", "Desk 2"} {
		assert.NoError(t, validReferenceSource(ok), ok)
	}
	for _, bad := range []string{"", " desk", "desk ", "a\nb", "a\x00b", strings.Repeat("x", 101)} {
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(validReferenceSource(bad)), "%q", bad)
	}
}

// The adapter refuses what no reader could use before it opens a
// transaction: no database is needed (and none is wired) to prove it.
func TestReferenceMark_TheAdapterRefusesUnusableObservationsBeforeTheDatabase(t *testing.T) {
	t.Parallel()
	a := operatorAdapter{clk: clock.NewFake(testNow), maxAge: 5 * time.Minute}
	op := econ.Actor{Type: security.ActorOperator, ID: testUserID.String()}
	future := testNow.Add(time.Second)
	old := testNow.Add(-5*time.Minute - time.Millisecond)
	other := assets.NewAssetID()
	usd := econ.SandboxDollarID()
	for name, c := range map[string]struct {
		cmd  ReferenceMarkCommand
		code errs.Code
	}{
		"agent":         {ReferenceMarkCommand{AssetID: testAssetID, Price: "1", Source: "s", Actor: econ.Actor{Type: security.ActorAgent, ID: "a"}}, errs.CodeForbidden},
		"customer":      {ReferenceMarkCommand{AssetID: testAssetID, Price: "1", Source: "s", Actor: econ.Actor{Type: security.ActorUser, ID: "u"}}, errs.CodeForbidden},
		"zero price":    {ReferenceMarkCommand{AssetID: testAssetID, Price: "0", Source: "s", Actor: op}, errs.CodeValidationFailed},
		"over-precise":  {ReferenceMarkCommand{AssetID: testAssetID, Price: "1.0000000000000000001", Source: "s", Actor: op}, errs.CodePrecisionLoss},
		"no source":     {ReferenceMarkCommand{AssetID: testAssetID, Price: "1", Actor: op}, errs.CodeValidationFailed},
		"the dollar":    {ReferenceMarkCommand{AssetID: usd, Price: "1", Source: "s", Actor: op}, errs.CodeValidationFailed},
		"other quote":   {ReferenceMarkCommand{AssetID: testAssetID, QuoteAssetID: &other, Price: "1", Source: "s", Actor: op}, errs.CodeValidationFailed},
		"in the future": {ReferenceMarkCommand{AssetID: testAssetID, Price: "1", Source: "s", ObservedAt: &future, Actor: op}, errs.CodeValidationFailed},
		"too old":       {ReferenceMarkCommand{AssetID: testAssetID, Price: "1", Source: "s", ObservedAt: &old, Actor: op}, errs.CodeValidationFailed},
	} {
		_, err := a.RecordMark(context.Background(), c.cmd)
		assert.Equal(t, c.code, errs.CodeOf(err), name)
	}
}

// TestProp_AReferencePriceIsReadExactly: every price the API renders is read
// back to the same mantissa × 10^-scale, with nothing rounded.
func TestProp_AReferencePriceIsReadExactly(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		scale := rapid.Int32Range(0, ReferenceMarkMaxScale).Draw(t, "scale")
		s := rapid.StringMatching(`[1-9][0-9]{0,37}`).Draw(t, "mantissa")
		m, ok := new(big.Int).SetString(s, 10)
		if !ok {
			t.Fatalf("not a number: %q", s)
		}
		mantissa := money.QuantityFromBigInt(m)
		rendered := mantissa.ToDecimalString(uint8(scale))
		got, gotScale, err := parseReferencePrice(rendered)
		if err != nil {
			t.Fatalf("parse %q: %v", rendered, err)
		}
		// Same value: got × 10^scale == mantissa × 10^gotScale.
		lhs := new(big.Int).Mul(got.BigInt(), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
		rhs := new(big.Int).Mul(mantissa.BigInt(), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(gotScale)), nil))
		if lhs.Cmp(rhs) != 0 {
			t.Fatalf("%q read as %s × 10^-%d, not %s × 10^-%d", rendered, got, gotScale, mantissa, scale)
		}
		if gotScale > ReferenceMarkMaxScale || !got.IsPositive() {
			t.Fatalf("%q read out of bounds: %s × 10^-%d", rendered, got, gotScale)
		}
	})
}

// FuzzParseReferencePrice: the price parser never panics, and whatever it
// accepts is positive, within the scale bound and renders back to itself.
func FuzzParseReferencePrice(f *testing.F) {
	for _, seed := range []string{"2500.125", "0", "1.", ".5", "1e5", "-1", "0.0000000000000000001", "00012.3400", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		q, scale, err := parseReferencePrice(raw)
		if err != nil {
			return
		}
		if !q.IsPositive() || scale < 0 || scale > ReferenceMarkMaxScale {
			t.Fatalf("accepted %q as %s × 10^-%d", raw, q, scale)
		}
		back, backScale, err := parseReferencePrice(q.ToDecimalString(uint8(scale)))
		if err != nil || !back.Equal(q) || backScale != scale {
			t.Fatalf("%q does not round-trip: %v %s %d", raw, err, back, backScale)
		}
	})
}
