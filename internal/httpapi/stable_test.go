package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stableasset"
)

// stableTestID makes an id of a type whose constructor its package keeps to
// itself, by decoding a fresh UUIDv7 into it.
func stableTestID[P interface {
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
	stableTestAssetID  = stableTestID[*stableasset.StableAssetID]()
	stableTestUnits    = assets.NewAssetID()
	stableTestTBill    = assets.NewAssetID()
	stableTestOperator = accounts.NewUserID()
	stableBase         = "/v1/advanced/stable-assets"
	stablePath         = stableBase + "/" + stableTestAssetID.String()
)

// stableOperator is an operator who owns no account and authenticated
// strongly a minute ago.
func stableOperator() security.Principal {
	p := operatorPrincipal()
	p.SubjectID = stableTestOperator.String()
	return p
}

// fakeStablePort is the stable asset port. It records what each call was
// asked, so a test can prove the handler passed the scoped account, the
// principal and the client's key rather than raw client input.
type fakeStablePort struct {
	stubErr
	mu    sync.Mutex
	calls map[string]int

	view          StableAssetView
	detail        StableAssetDetailView
	reserve       StableReserveView
	chain         StableSnapshotChainView
	holdings      StableHoldingsView
	issuance      StableIssuanceView
	redemption    StableRedemptionView
	issueOutcome  StableIssuanceOutcome
	redeemOutcome StableRedemptionOutcome
	monitor       StableMonitorView
	supply        stableasset.SupplyReconciliation
	taken         StableSnapshotTakenView
	report        stableasset.SettleReport
	nextCursor    string

	lastAccount accounts.AccountID
	lastFilter  *stableasset.StableAssetID
	lastCursor  string
	lastLimit   int
	lastIssue   StableIssueCommand
	lastRedeem  StableRedeemCommand
	lastDef     stableasset.Definition
	lastActor   econ.Actor
	lastStatus  stableasset.Status
	lastReason  string
}

func (f *fakeStablePort) called(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
}

func (f *fakeStablePort) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeStablePort) Assets(_ context.Context, limit int) ([]StableAssetView, error) {
	f.called("Assets")
	f.mu.Lock()
	f.lastLimit = limit
	f.mu.Unlock()
	return []StableAssetView{f.view}, f.fail()
}

func (f *fakeStablePort) Asset(context.Context, stableasset.StableAssetID) (StableAssetDetailView, error) {
	f.called("Asset")
	return f.detail, f.fail()
}

func (f *fakeStablePort) Reserve(context.Context, stableasset.StableAssetID) (StableReserveView, error) {
	f.called("Reserve")
	return f.reserve, f.fail()
}

func (f *fakeStablePort) Snapshots(_ context.Context, _ stableasset.StableAssetID, limit int) (StableSnapshotChainView, error) {
	f.called("Snapshots")
	f.mu.Lock()
	f.lastLimit = limit
	f.mu.Unlock()
	return f.chain, f.fail()
}

func (f *fakeStablePort) Holdings(_ context.Context, account accounts.AccountID) (StableHoldingsView, error) {
	f.called("Holdings")
	f.mu.Lock()
	f.lastAccount = account
	f.mu.Unlock()
	v := f.holdings
	v.AccountID = account
	return v, f.fail()
}

func (f *fakeStablePort) Issuances(_ context.Context, account accounts.AccountID, asset *stableasset.StableAssetID, cursor string, limit int) (StableIssuancePage, error) {
	f.called("Issuances")
	f.mu.Lock()
	f.lastAccount, f.lastFilter, f.lastCursor, f.lastLimit = account, asset, cursor, limit
	f.mu.Unlock()
	return StableIssuancePage{Items: []StableIssuanceView{f.issuance}, NextCursor: f.nextCursor}, f.fail()
}

func (f *fakeStablePort) Issuance(context.Context, stableasset.MintRequestID) (StableIssuanceView, error) {
	f.called("Issuance")
	return f.issuance, f.fail()
}

func (f *fakeStablePort) Redemptions(_ context.Context, account accounts.AccountID, asset *stableasset.StableAssetID, cursor string, limit int) (StableRedemptionPage, error) {
	f.called("Redemptions")
	f.mu.Lock()
	f.lastAccount, f.lastFilter, f.lastCursor, f.lastLimit = account, asset, cursor, limit
	f.mu.Unlock()
	return StableRedemptionPage{Items: []StableRedemptionView{f.redemption}, NextCursor: f.nextCursor}, f.fail()
}

func (f *fakeStablePort) Redemption(context.Context, stableasset.BurnRequestID) (StableRedemptionView, error) {
	f.called("Redemption")
	return f.redemption, f.fail()
}

func (f *fakeStablePort) Issue(_ context.Context, c StableIssueCommand) (StableIssuanceOutcome, error) {
	f.called("Issue")
	f.mu.Lock()
	f.lastIssue = c
	f.mu.Unlock()
	return f.issueOutcome, f.fail()
}

func (f *fakeStablePort) Redeem(_ context.Context, c StableRedeemCommand) (StableRedemptionOutcome, error) {
	f.called("Redeem")
	f.mu.Lock()
	f.lastRedeem = c
	f.mu.Unlock()
	return f.redeemOutcome, f.fail()
}

func (f *fakeStablePort) Register(_ context.Context, def stableasset.Definition, by econ.Actor) (StableAssetDetailView, error) {
	f.called("Register")
	f.mu.Lock()
	f.lastDef, f.lastActor = def, by
	f.mu.Unlock()
	return f.detail, f.fail()
}

func (f *fakeStablePort) SetStatus(_ context.Context, _ stableasset.StableAssetID, to stableasset.Status, by econ.Actor, reason string) (StableAssetDetailView, error) {
	f.called("SetStatus:" + string(to))
	f.mu.Lock()
	f.lastStatus, f.lastActor, f.lastReason = to, by, reason
	f.mu.Unlock()
	return f.detail, f.fail()
}

func (f *fakeStablePort) RunMonitor(_ context.Context, _ stableasset.StableAssetID, by econ.Actor) (StableMonitorView, error) {
	f.called("RunMonitor")
	f.mu.Lock()
	f.lastActor = by
	f.mu.Unlock()
	return f.monitor, f.fail()
}

func (f *fakeStablePort) ReconcileSupply(_ context.Context, _ stableasset.StableAssetID, by econ.Actor) (stableasset.SupplyReconciliation, error) {
	f.called("ReconcileSupply")
	f.mu.Lock()
	f.lastActor = by
	f.mu.Unlock()
	return f.supply, f.fail()
}

func (f *fakeStablePort) TakeSnapshot(context.Context, stableasset.StableAssetID) (StableSnapshotTakenView, error) {
	f.called("TakeSnapshot")
	return f.taken, f.fail()
}

func (f *fakeStablePort) ProcessRedemptions(_ context.Context, _ stableasset.StableAssetID, limit int) (stableasset.SettleReport, error) {
	f.called("ProcessRedemptions")
	f.mu.Lock()
	f.lastLimit = limit
	f.mu.Unlock()
	return f.report, f.fail()
}

func (f *fakeStablePort) ResolveUnknown(_ context.Context, limit int) (stableasset.SettleReport, error) {
	f.called("ResolveUnknown")
	f.mu.Lock()
	f.lastLimit = limit
	f.mu.Unlock()
	return f.report, f.fail()
}

func stableTestPrice(mantissa string, scale int32, source string) money.Price {
	return money.Price{Mantissa: qty(mantissa), Scale: scale, QuoteAsset: econ.SandboxDollarID().String(), Source: source, At: testNow}
}

func stableTestPriceDoc(mantissa string, scale int32, source string) stableasset.PriceDoc {
	return stableasset.PriceDoc{Mantissa: mantissa, Scale: scale, Quote: econ.SandboxDollarID().String(), Source: source, At: testNow}
}

// stableTestSnapshotKey signs the snapshot chains the tests build.
var stableTestSnapshotKey = ed25519.NewKeyFromSeed(func() []byte { s := sha256.Sum256([]byte("httpapi stable snapshots")); return s[:] }())

// stableTestChain builds n snapshots exactly as the service signs them:
// canonical JSON, hash-linked, signed over the domain-separated hash.
func stableTestChain(t *testing.T, key ed25519.PrivateKey, n int) []stableasset.SignedSnapshot {
	t.Helper()
	pub, ok := key.Public().(ed25519.PublicKey)
	require.True(t, ok)
	out := make([]stableasset.SignedSnapshot, 0, n)
	prev := ""
	for i := 0; i < n; i++ {
		body := stableasset.ReserveSnapshot{
			Version: "nodal-sandbox/stable-reserve-snapshot/v1", Disclaimer: stableasset.SnapshotDisclaimer,
			StableAssetID: stableTestAssetID.String(), AssetID: stableTestUnits.String(), Chain: string(devnethost.ChainSolana),
			Mint: "mint", Seq: strconv.Itoa(i + 1), TakenAt: testNow.Add(time.Duration(i) * time.Minute), PrevHash: prev,
			LedgerSupply: "600000000", ChainSupply: "600000000", PendingMint: "0", PendingBurn: "0",
			Peg: stableTestPriceDoc("1000000", 6, "TARGET_PEG"),
			Reserves: []stableasset.SnapshotReserve{{
				AssetID: econ.SandboxDollarID().String(), Kind: string(stableasset.ReserveCashEquivalent), Balance: "600000000",
				Decimals: "6", FactorBPS: "10000", Eligible: "600.000000",
			}},
			Composition: []stableasset.CompositionLine{
				{Kind: string(stableasset.ReserveCashEquivalent), Eligible: "600.000000", ShareBPS: "10000"},
				{Kind: string(stableasset.ReserveTokenized), Eligible: "0", ShareBPS: "0"},
			},
			SupplyValue: "600.000000000000", EligibleReserve: "600.000000", CoverageBPS: "10000",
		}
		canonical, err := audit.CanonicalJSON(body)
		require.NoError(t, err)
		hash := sha256.Sum256(canonical)
		sig := ed25519.Sign(key, append([]byte("nodal-sandbox/stable-reserve-snapshot/signature/v1\x00"), hash[:]...))
		sn, err := stableasset.ParseSignedSnapshot(canonical, hash[:], sig, pub)
		require.NoError(t, err)
		sn.EvidenceRef = "proof://stableasset/snapshots/" + strconv.Itoa(i+1)
		out = append(out, sn)
		prev = hex.EncodeToString(hash[:])
	}
	return out
}

// newFakeStablePort holds a realistic stable asset: 600 units issued against
// 600 sandbox dollars in reserve and a tokenized line priced at 0.99, one
// issuance and one redemption of the test account, and a snapshot chain.
func newFakeStablePort(t *testing.T) *fakeStablePort {
	t.Helper()
	usd := econ.SandboxDollarID()
	asset := stableasset.PlatformStableAsset{
		ID: stableTestAssetID, Name: "httpapi-test-dollar", AssetID: stableTestUnits, FacilityID: econ.NewFacilityID(),
		Network: "httpapi-sbx", Chain: devnethost.ChainSolana, Mint: stableasset.MintAddress("httpapi-sbx", "httpapi-test-dollar"),
		Decimals: 6, Status: stableasset.StatusActive, IssuanceCap: qty("1000000000000"),
		MintAuthorityMode: stableasset.MintAuthorityPlatformSigner,
		MintAuthority:     devnet.NewKeypairFromSeed(sha256.Sum256([]byte("httpapi issuer"))).PublicKey(),
		BurnAuthorityMode: stableasset.BurnAuthorityHolderSigned, RedemptionMode: stableasset.RedemptionInstant,
		PegQuoteAssetID: usd, Peg: stableTestPrice("1000000", 6, "TARGET_PEG"), OracleSource: "httpapi-oracle",
		MaxPriceAge: time.Hour, PegDeviationThreshold: 50, Attestable: true,
		Reserves: []stableasset.EligibleReserve{
			{AssetID: usd, Kind: stableasset.ReserveCashEquivalent, FactorBPS: 10_000, AcceptedForMint: true, Decimals: 6},
			{AssetID: stableTestTBill, Kind: stableasset.ReserveTokenized, FactorBPS: 9_000, Decimals: 6},
		},
		CreatedBy: econ.Actor{Type: security.ActorOperator, ID: stableTestOperator.String()}, CreatedAt: testNow, UpdatedAt: testNow,
	}
	labels := map[assets.AssetID]StableAssetLabel{
		usd: {Symbol: econ.SandboxDollarSymbol, Decimals: 6}, stableTestTBill: {Symbol: "SBX-TBILL", Decimals: 6},
		stableTestUnits: {Symbol: "SBX-PUSD", Decimals: 6},
	}
	view := StableAssetView{Asset: asset, Label: labels[stableTestUnits], PegCurrency: "USD",
		Reserves: map[assets.AssetID]StableAssetLabel{usd: labels[usd], stableTestTBill: labels[stableTestTBill]}}
	priceDoc := stableTestPriceDoc("1000000", 6, "PAR")
	tbillPrice := stableTestPriceDoc("990000", 6, "httpapi-oracle")
	ledgerView := stableasset.ReserveLedger{
		StableAssetID: stableTestAssetID, AsOf: testNow, IssuedSupply: qty("600000000"),
		CashEquivalentReserve: []stableasset.ReserveHolding{{AssetID: usd, Kind: stableasset.ReserveCashEquivalent, Quantity: qty("600000000"),
			Decimals: 6, FactorBPS: 10_000, Price: &priceDoc, Eligible: stableasset.Value{Coef: qty("6000000000000000"), Exp: -13}}},
		TokenizedReserve: []stableasset.ReserveHolding{{AssetID: stableTestTBill, Kind: stableasset.ReserveTokenized, Quantity: qty("5000000"),
			Decimals: 6, FactorBPS: 9_000, Price: &tbillPrice, Eligible: stableasset.Value{Coef: qty("44550000000000"), Exp: -16}}},
		PendingMint: qty("0"), ReservedNotFinalSupply: qty("0"), Receivable: []stableasset.ReserveHolding{}, PendingBurn: qty("0"),
		PendingRedemption: []stableasset.ReserveHolding{}, SupplyValue: stableasset.Value{Coef: qty("600000000000000"), Exp: -12},
		EligibleReserve: stableasset.Value{Coef: qty("6044550000000000"), Exp: -13}, CoverageBPS: 10_074,
		Unverified: []assets.AssetID{},
	}
	rec := stableasset.MintRecord{
		ID: stableTestID[*stableasset.MintRequestID](), StableAssetID: stableTestAssetID, AccountID: testAccountID,
		IntentID: econ.NewIntentID(), EffectID: econ.NewEffectID(), Mode: gates.ModeSandbox, Quantity: qty("600000000"),
		ReserveAssetID: usd, ReserveQuantity: qty("600000000"), ReservationID: stableTestID[*capital.ReservationID](),
		Recipient: devnet.NewKeypairFromSeed(sha256.Sum256([]byte("httpapi holder"))).PublicKey(),
		Plan:      stableasset.MintPlan{ReservePrice: priceDoc, Peg: stableTestPriceDoc("1000000", 6, "TARGET_PEG")},
		PlanHash:  sha256.Sum256([]byte("plan")), Chain: devnethost.ChainSolana, TxID: devnet.TxID("mint-tx-1"),
		Status: stableasset.MintIssued, CreatedAt: testNow, UpdatedAt: testNow,
	}
	jid := ledger.NewTransactionID()
	slot := int64(42)
	rec.JournalTransactionID, rec.FinalSlot = &jid, &slot
	burn := stableasset.BurnRecord{
		ID: stableTestID[*stableasset.BurnRequestID](), StableAssetID: stableTestAssetID, AccountID: testAccountID,
		IntentID: econ.NewIntentID(), EffectID: econ.NewEffectID(), Mode: gates.ModeSandbox, Quantity: qty("200000000"),
		PayoutAssetID: usd, PayoutQuantity: qty("200000000"), ReservationID: stableTestID[*capital.ReservationID](),
		Holder: rec.Recipient, Plan: stableasset.BurnPlan{RedemptionMode: string(stableasset.RedemptionInstant)},
		PlanHash: sha256.Sum256([]byte("burn plan")), Chain: devnethost.ChainSolana, TxID: devnet.TxID("burn-tx-1"),
		Status: stableasset.BurnRedeemed, CreatedAt: testNow, UpdatedAt: testNow,
	}
	issuance := StableIssuanceView{Record: rec, Stable: labels[stableTestUnits], Reserve: labels[usd],
		Reconciliation: StableRequestReconciliation{EffectStatus: econ.EffectApplied}}
	redemption := StableRedemptionView{Record: burn, Stable: labels[stableTestUnits], Payout: labels[usd],
		Reconciliation: StableRequestReconciliation{EffectStatus: econ.EffectApplied}}
	snaps := stableTestChain(t, stableTestSnapshotKey, 3)
	pub, _ := stableTestSnapshotKey.Public().(ed25519.PublicKey)
	verified := StableSnapshotVerification{Checked: 3, FailedIndex: -1, PublicKey: pub}
	supply := stableasset.SupplyReconciliation{
		StableAssetID: stableTestAssetID, LedgerOutstanding: qty("600000000"), CustomerWalletSum: qty("500000000"),
		HeldElsewhere: qty("100000000"), ChainSupply: qty("600000000"), ExpectedChainSupply: qty("600000000"),
		SupplyValue: ledgerView.SupplyValue, EligibleReserve: ledgerView.EligibleReserve, CoverageBPS: 10_074,
		SupplyMatches: true, WalletsMatch: true, Covered: true, RecordID: id.New[id.Any]().String(),
	}
	return &fakeStablePort{
		calls: map[string]int{}, view: view,
		detail: StableAssetDetailView{
			StableAssetView: view,
			Transitions: []StableAssetTransition{
				{From: "NONE", To: stableasset.StatusActive, Actor: asset.CreatedBy, Reason: "registered", OccurredAt: testNow},
			},
			Alerts: []StableAlertView{{
				ID: id.New[id.Any](), Kind: stableasset.AlertPriceStale, Severity: stableasset.SeverityWarning,
				Observed: json.RawMessage(`{"reason":"no fresh market price from the oracle source"}`), ObservedAt: testNow,
			}},
		},
		reserve: StableReserveView{Asset: view, Ledger: ledgerView, Labels: labels},
		chain:   StableSnapshotChainView{StableAssetID: stableTestAssetID, Total: 3, Offset: 0, Items: snaps, Verification: verified},
		holdings: StableHoldingsView{
			HolderAddress: rec.Recipient.String(), AsOf: testNow,
			Items: []StableHoldingView{{Asset: asset, Label: labels[stableTestUnits], ChainBalance: qty("400000000"),
				Availability: capital.Availability{WalletBalance: qty("400000000"), Reserved: qty("0"), Held: qty("0"), Available: qty("400000000")}}},
			Payment: []StablePaymentView{{AssetID: usd, Label: labels[usd],
				Availability: capital.Availability{WalletBalance: qty("600000000"), Reserved: qty("100000000"), Held: qty("0"), Available: qty("500000000")}}},
		},
		issuance: issuance, redemption: redemption,
		issueOutcome:  StableIssuanceOutcome{View: issuance, Settled: true},
		redeemOutcome: StableRedemptionOutcome{View: redemption, Settled: true},
		monitor: StableMonitorView{Asset: view, Labels: labels, Report: stableasset.MonitorReport{
			StableAssetID: stableTestAssetID, ObservedAt: testNow, PriceStale: true, Reserve: ledgerView, Supply: supply,
			Alerts: []stableasset.AlertKind{stableasset.AlertPriceStale}, Status: stableasset.StatusActive,
		}},
		supply: supply,
		taken:  StableSnapshotTakenView{Snapshot: snaps[2], Index: 2, Verification: verified},
		report: stableasset.SettleReport{Examined: 2, Issued: 1, Failed: 1},
	}
}

func newStableHarness(t *testing.T) (*harness, *fakeStablePort) {
	t.Helper()
	h := newHarness(t)
	fp := newFakeStablePort(t)
	ports := h.ports.ports()
	ports.Stable = fp
	h.server.opts.Ports = ports
	return h, fp
}

// stableMutation is one POST route with a valid body, and a different valid
// body for the same route (a body the handler refuses never reaches the
// idempotency check).
type stableMutation struct {
	name     string
	path     string
	body     any
	other    any
	call     string
	customer bool
	stepUp   bool
}

func stableIssueBody(q string) map[string]any {
	return map[string]any{"account_id": testAccountID.String(), "quantity": q, "reserve_asset_id": econ.SandboxDollarID().String(), "mode": "SANDBOX"}
}

func stableRedeemBody(q string) map[string]any {
	return map[string]any{"account_id": testAccountID.String(), "quantity": q, "mode": "SANDBOX"}
}

func stableDefinitionBody(name string) map[string]any {
	return map[string]any{
		"name": name, "symbol": "SBX-PUSD", "peg_currency": "USD", "decimals": 6, "issuance_cap": "1000000000000",
		"redemption_mode": "INSTANT", "peg": "1.000000", "oracle_source": "httpapi-oracle", "max_price_age_ms": 3600000,
		"peg_deviation_threshold_bps": 50, "attestable": true,
		"reserves": []map[string]any{
			{"asset_id": econ.SandboxDollarID().String(), "kind": "CASH_EQUIVALENT", "factor_bps": 10000, "accepted_for_mint": true},
			{"asset_id": stableTestTBill.String(), "kind": "TOKENIZED", "factor_bps": 9000, "accepted_for_mint": false},
		},
	}
}

func stableMutations() []stableMutation {
	noBody := map[string]any{"different": "body"}
	reason := func(r string) map[string]any { return map[string]any{"reason": r} }
	return []stableMutation{
		{"issue", stablePath + "/issuances", stableIssueBody("600000000"), stableIssueBody("500000000"), "Issue", true, false},
		{"redeem", stablePath + "/redemptions", stableRedeemBody("200000000"), stableRedeemBody("100000000"), "Redeem", true, false},
		{"register", stableBase, stableDefinitionBody("httpapi-test-dollar"), stableDefinitionBody("httpapi-other-dollar"), "Register", false, true},
		{"pause", stablePath + "/pause", reason("the peg is being investigated"), reason("another reason"), "SetStatus:PAUSED", false, true},
		{"resume", stablePath + "/resume", reason("the market price is back at the peg"), reason("another reason"), "SetStatus:ACTIVE", false, true},
		{"monitor", stablePath + "/monitor-runs", nil, noBody, "RunMonitor", false, false},
		{"reconcile", stablePath + "/reconciliations", nil, noBody, "ReconcileSupply", false, false},
		{"snapshot", stablePath + "/snapshots", nil, noBody, "TakeSnapshot", false, false},
		{"queue", stablePath + "/redemption-queue/run", map[string]any{"limit": 10}, map[string]any{"limit": 20}, "ProcessRedemptions", false, true},
		{"resolve", stableBase + "/ops/resolve-unknown", map[string]any{"limit": 10}, map[string]any{"limit": 20}, "ResolveUnknown", false, true},
	}
}

// stableReads are the GET routes, each with valid parameters.
func stableReads() []string {
	return []string{
		stableBase,
		stablePath,
		stablePath + "/reserve",
		stablePath + "/snapshots",
		stableBase + "/holdings?account_id=" + testAccountID.String(),
		stableBase + "/issuances?account_id=" + testAccountID.String(),
		stableBase + "/issuances/" + stableTestID[*stableasset.MintRequestID]().String(),
		stableBase + "/redemptions?account_id=" + testAccountID.String(),
		stableBase + "/redemptions/" + stableTestID[*stableasset.BurnRequestID]().String(),
	}
}

func TestStable_UnwiredPortIsUnsupportedOnEveryRoute(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	op := stableOperator()
	h.as(&op)
	for _, path := range stableReads() {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", path, res.Body.String())
		p := res.problem()
		assert.Equal(t, errs.CodeUnsupported, p.Code, path)
		assert.Contains(t, p.Detail, "no sandbox chain is configured", path)
	}
	for i, m := range stableMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "stable-unwired-"+itoa(i)+"-key")
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", m.name, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, m.name)
	}
}

func TestStable_ListAndDetail_RenderTheRegistryExactly(t *testing.T) {
	t.Parallel()
	h, fp := newStableHarness(t)
	res := h.do(http.MethodGet, stableBase+"?limit=5000", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, maxPageLimit, fp.lastLimit, "the limit is clamped")
	var list api.StableAssetList
	res.json(&list)
	assert.Equal(t, api.SIMULATED, list.ValueDomain)
	require.Len(t, list.Items, 1)
	a := list.Items[0]
	assert.Equal(t, api.SIMULATED, a.ValueDomain)
	assert.Equal(t, "SBX-PUSD", a.Symbol)
	assert.Equal(t, "USD", a.PegCurrency)
	assert.Equal(t, "1000000000000", a.IssuanceCap, "base units as an exact integer string")
	assert.Equal(t, "1000000", a.Peg.Mantissa)
	assert.Equal(t, 6, a.Peg.Scale)
	assert.Equal(t, "1.000000", a.Peg.Decimal, "the peg written out exactly")
	assert.Equal(t, econ.SandboxDollarID().String(), a.Peg.Quote)
	assert.Equal(t, api.StableAssetStatusActive, a.Status)
	assert.Equal(t, api.StableRedemptionModeInstant, a.RedemptionMode)
	assert.Equal(t, int64(3_600_000), a.MaxPriceAgeMs)
	require.Len(t, a.Reserves, 2)
	assert.Equal(t, econ.SandboxDollarSymbol, a.Reserves[0].Symbol)
	assert.Equal(t, api.StableReserveKindCashEquivalent, a.Reserves[0].Kind)
	assert.True(t, a.Reserves[0].AcceptedForMint)
	assert.Equal(t, 9000, a.Reserves[1].FactorBps)

	res = h.do(http.MethodGet, stablePath, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	transitions := raw["transitions"].([]any)
	require.Len(t, transitions, 1)
	assert.Equal(t, "NONE", transitions[0].(map[string]any)["from_status"])
	alerts := raw["alerts"].([]any)
	require.Len(t, alerts, 1)
	alert := alerts[0].(map[string]any)
	assert.Equal(t, "PRICE_STALE", alert["kind"])
	assert.Equal(t, map[string]any{"reason": "no fresh market price from the oracle source"}, alert["observed"])

	res = h.do(http.MethodGet, stableBase+"/not-a-uuid", nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	fp.err = errs.New(errs.CodeNotFound, "stable asset not found")
	res = h.do(http.MethodGet, stablePath, nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
}

func TestStable_Reserve_StatesEveryFigureExactly(t *testing.T) {
	t.Parallel()
	h, fp := newStableHarness(t)
	res := h.do(http.MethodGet, stablePath+"/reserve", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var r api.StableReserve
	res.json(&r)
	assert.Equal(t, api.SIMULATED, r.ValueDomain)
	assert.Equal(t, "600000000", r.IssuedSupply)
	assert.Equal(t, "600.000000000000", r.SupplyValue, "an exact value, never rounded")
	assert.Equal(t, "604.4550000000000", r.EligibleReserve)
	assert.Equal(t, int64(10_074), r.CoverageBps)
	assert.True(t, r.Covered)
	require.Len(t, r.CashEquivalentReserve, 1)
	assert.Equal(t, "600.0000000000000", r.CashEquivalentReserve[0].Eligible)
	require.NotNil(t, r.CashEquivalentReserve[0].Price)
	assert.Equal(t, "PAR", r.CashEquivalentReserve[0].Price.Source)
	require.Len(t, r.TokenizedReserve, 1)
	assert.Equal(t, "SBX-TBILL", r.TokenizedReserve[0].Symbol)
	assert.Equal(t, "0.990000", r.TokenizedReserve[0].Price.Decimal)
	assert.Equal(t, []any{}, res.raw()["receivable"], "an empty list, never null")

	// A reserve line with no fresh price has no price key and counts zero.
	fp.reserve.Ledger.TokenizedReserve[0].Price = nil
	fp.reserve.Ledger.TokenizedReserve[0].Eligible = stableasset.Value{Coef: qty("0")}
	fp.reserve.Ledger.Unverified = []assets.AssetID{stableTestTBill}
	res = h.do(http.MethodGet, stablePath+"/reserve", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	line := raw["tokenized_reserve"].([]any)[0].(map[string]any)
	_, present := line["price"]
	assert.False(t, present)
	assert.Equal(t, "0", line["eligible"])
	assert.Equal(t, []any{stableTestTBill.String()}, raw["unverified_reserve_asset_ids"])

	// A line naming an asset the adapter did not label is a fault, not a blank.
	delete(fp.reserve.Labels, stableTestTBill)
	res = h.do(http.MethodGet, stablePath+"/reserve", nil)
	require.Equal(t, http.StatusInternalServerError, res.Code, "body=%s", res.Body.String())
}

func TestStable_Snapshots_CarryTheChainsVerificationAndExactBodies(t *testing.T) {
	t.Parallel()
	h, fp := newStableHarness(t)
	res := h.do(http.MethodGet, stablePath+"/snapshots?limit=2", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 2, fp.lastLimit)
	var c api.StableSnapshotChain
	res.json(&c)
	assert.Equal(t, api.SIMULATED, c.ValueDomain)
	assert.True(t, c.Verification.Verified)
	assert.Equal(t, 3, c.Verification.Checked)
	assert.Nil(t, c.Verification.FailedIndex)
	pub, _ := stableTestSnapshotKey.Public().(ed25519.PublicKey)
	assert.Equal(t, hex.EncodeToString(pub), c.Verification.PublicKey)
	assert.Equal(t, stableasset.SnapshotDisclaimer, c.Verification.Disclaimer)
	require.Len(t, c.Items, 3)
	for i, sn := range c.Items {
		assert.True(t, sn.Verified)
		assert.Equal(t, int64(i+1), sn.Seq)
		sum := sha256.Sum256([]byte(sn.CanonicalBody))
		assert.Equal(t, hex.EncodeToString(sum[:]), sn.Hash, "the hash covers exactly the canonical body")
		assert.Equal(t, "600000000", sn.Body.LedgerSupply)
		assert.Equal(t, "10000", sn.Body.Composition[0].ShareBps)
	}
	assert.Empty(t, c.Items[0].PrevHash)
	assert.Equal(t, c.Items[0].Hash, c.Items[1].PrevHash, "hash-linked")

	// The chain's verdict marks every snapshot from the first failure on.
	fp.chain.Offset = 1
	fp.chain.Items = fp.chain.Items[1:]
	fp.chain.Verification.FailedIndex, fp.chain.Verification.Reason = 2, stableasset.SnapshotSignatureInvalid
	res = h.do(http.MethodGet, stablePath+"/snapshots", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&c)
	assert.False(t, c.Verification.Verified)
	require.NotNil(t, c.Verification.FailedIndex)
	assert.Equal(t, 2, *c.Verification.FailedIndex)
	require.NotNil(t, c.Verification.Reason)
	assert.Equal(t, api.StableSnapshotFailureSignatureInvalid, *c.Verification.Reason)
	require.Len(t, c.Items, 2)
	assert.True(t, c.Items[0].Verified, "seq 2 is before the failure")
	assert.False(t, c.Items[1].Verified, "seq 3 is the failure")
}

// The adapter's verification is the package's own verifier against the
// configured key: a whole chain verifies, and the first snapshot that does
// not -- a wrong key, a broken signature, a broken link -- is found.
func TestStable_VerifyChain_FindsTheFirstSnapshotThatDoesNotVerify(t *testing.T) {
	t.Parallel()
	snaps := stableTestChain(t, stableTestSnapshotKey, 3)
	pub, _ := stableTestSnapshotKey.Public().(ed25519.PublicKey)
	v, err := stableVerify(snaps, pub)
	require.NoError(t, err)
	assert.True(t, v.Verified())
	assert.Equal(t, 3, v.Checked)

	other := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	otherPub, _ := other.Public().(ed25519.PublicKey)
	v, err = stableVerify(snaps, otherPub)
	require.NoError(t, err)
	assert.Equal(t, 0, v.FailedIndex)
	assert.Equal(t, stableasset.SnapshotKeyMismatch, v.Reason)

	broken := stableTestChain(t, stableTestSnapshotKey, 3)
	broken[1].Signature[0] ^= 0xff
	v, err = stableVerify(broken, pub)
	require.NoError(t, err)
	assert.Equal(t, 1, v.FailedIndex)
	assert.Equal(t, stableasset.SnapshotSignatureInvalid, v.Reason)
	assert.True(t, v.VerifiedAt(0))
	assert.False(t, v.VerifiedAt(1))
	assert.False(t, v.VerifiedAt(2))

	unlinked := append([]stableasset.SignedSnapshot{}, snaps[0], snaps[2])
	v, err = stableVerify(unlinked, pub)
	require.NoError(t, err)
	assert.Equal(t, 1, v.FailedIndex)
	assert.Equal(t, stableasset.SnapshotChainBroken, v.Reason)

	// A key that cannot be a verification key is a fault, not a finding.
	_, err = stableVerify(snaps, ed25519.PublicKey{1, 2, 3})
	require.Error(t, err)
}

func TestStable_Holdings_AreScopedToTheCallersAccount(t *testing.T) {
	t.Parallel()
	h, fp := newStableHarness(t)
	res := h.do(http.MethodGet, stableBase+"/holdings?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testAccountID, fp.lastAccount)
	var hv api.StableHoldings
	res.json(&hv)
	assert.Equal(t, api.SIMULATED, hv.ValueDomain)
	require.Len(t, hv.Items, 1)
	assert.Equal(t, "400000000", hv.Items[0].Wallet)
	assert.Equal(t, "400000000", hv.Items[0].Available)
	assert.Equal(t, "400000000", hv.Items[0].ChainBalance)
	require.Len(t, hv.PaymentAssets, 1)
	assert.Equal(t, "100000000", hv.PaymentAssets[0].Reserved)
	assert.Equal(t, "500000000", hv.PaymentAssets[0].Available)

	calls := fp.count("Holdings")
	res = h.do(http.MethodGet, stableBase+"/holdings?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	assert.Equal(t, calls, fp.count("Holdings"), "a refused account never reaches the port")

	op := stableOperator()
	h.as(&op)
	res = h.do(http.MethodGet, stableBase+"/holdings?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "an operator with account:read_any reads any account; body=%s", res.Body.String())
	assert.Equal(t, testOtherAcct, fp.lastAccount)
}

func TestStable_RequestLists_AreScopedAndPaged(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"issuances", "redemptions"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			h, fp := newStableHarness(t)
			fp.nextCursor = "next-page"
			path := stableBase + "/" + kind + "?account_id=" + testAccountID.String() + "&stable_asset_id=" + stableTestAssetID.String() +
				"&cursor=abc&limit=7"
			res := h.do(http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, testAccountID, fp.lastAccount)
			require.NotNil(t, fp.lastFilter)
			assert.Equal(t, stableTestAssetID, *fp.lastFilter)
			assert.Equal(t, "abc", fp.lastCursor)
			assert.Equal(t, 7, fp.lastLimit)
			raw := res.raw()
			assert.Equal(t, "SIMULATED", raw["value_domain"])
			assert.Equal(t, "next-page", raw["next_cursor"])
			item := raw["items"].([]any)[0].(map[string]any)
			assert.Equal(t, "SIMULATED", item["value_domain"])
			assert.Equal(t, "NOT_REQUIRED", item["reconciliation"].(map[string]any)["state"])

			fp.nextCursor = ""
			res = h.do(http.MethodGet, stableBase+"/"+kind+"?account_id="+testAccountID.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Nil(t, fp.lastFilter)
			next, present := res.raw()["next_cursor"]
			assert.True(t, present, "the last page says so with null")
			assert.Nil(t, next)

			res = h.do(http.MethodGet, stableBase+"/"+kind+"?account_id="+testAccountID.String()+"&stable_asset_id=nope", nil)
			require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())

			calls := fp.count(map[string]string{"issuances": "Issuances", "redemptions": "Redemptions"}[kind])
			res = h.do(http.MethodGet, stableBase+"/"+kind+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, calls, fp.count(map[string]string{"issuances": "Issuances", "redemptions": "Redemptions"}[kind]))

			op := stableOperator()
			h.as(&op)
			res = h.do(http.MethodGet, stableBase+"/"+kind+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		})
	}
}

func TestStable_RequestByID_AnswersNotFoundForAnotherAccount(t *testing.T) {
	t.Parallel()
	h, fp := newStableHarness(t)
	issuancePath := stableBase + "/issuances/" + fp.issuance.Record.ID.String()
	redemptionPath := stableBase + "/redemptions/" + fp.redemption.Record.ID.String()

	res := h.do(http.MethodGet, issuancePath, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var is api.StableIssuance
	res.json(&is)
	assert.Equal(t, "600000000", is.Quantity)
	assert.Equal(t, "600000000", is.ReserveQuantity)
	assert.Equal(t, "1.000000", is.ReservePrice.Decimal)
	assert.Equal(t, api.StableIssuanceStatusIssued, is.Status)
	assert.Equal(t, hex.EncodeToString(fp.issuance.Record.PlanHash[:]), is.PlanHash)
	require.NotNil(t, is.FinalSlot)
	assert.Equal(t, int64(42), *is.FinalSlot)
	require.NotNil(t, is.JournalTransactionId)

	res = h.do(http.MethodGet, redemptionPath, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var rd api.StableRedemption
	res.json(&rd)
	assert.Equal(t, "200000000", rd.PayoutQuantity)
	assert.Equal(t, api.StableRedemptionModeInstant, rd.RedemptionMode)

	// Another account's request answers exactly like a missing one.
	fp.issuance.Record.AccountID, fp.redemption.Record.AccountID = testOtherAcct, testOtherAcct
	foreign := h.do(http.MethodGet, issuancePath, nil)
	require.Equal(t, http.StatusNotFound, foreign.Code, "body=%s", foreign.Body.String())
	fp.err = errs.New(errs.CodeNotFound, "mint request not found")
	missing := h.do(http.MethodGet, issuancePath, nil)
	require.Equal(t, http.StatusNotFound, missing.Code, "body=%s", missing.Body.String())
	assert.Equal(t, foreign.problem().Detail, missing.problem().Detail, "absent and not yours are one answer")
	fp.err = nil
	res = h.do(http.MethodGet, redemptionPath, nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())

	// An operator with account:read_any reads it.
	op := stableOperator()
	h.as(&op)
	res = h.do(http.MethodGet, issuancePath, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res = h.do(http.MethodGet, stableBase+"/issuances/not-a-uuid", nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
}

func TestStable_Issue_ActsForTheOwnerAsThePrincipal(t *testing.T) {
	t.Parallel()
	h, fp := newStableHarness(t)
	res := h.do(http.MethodPost, stablePath+"/issuances", stableIssueBody("600000000"), "Idempotency-Key", "stable-issue-key-0001")
	require.Equal(t, http.StatusOK, res.Code, "a final issuance is 200; body=%s", res.Body.String())
	c := fp.lastIssue
	assert.Equal(t, stableTestAssetID, c.StableAssetID)
	assert.Equal(t, testAccountID, c.AccountID)
	assert.Equal(t, "600000000", c.Quantity.String())
	assert.Equal(t, econ.SandboxDollarID(), c.ReserveAssetID)
	assert.Equal(t, gates.ModeSandbox, c.Mode)
	assert.Equal(t, "stable-issue-key-0001", c.IdempotencyKey)
	assert.Equal(t, econ.Actor{Type: security.ActorUser, ID: testUserID.String()}, c.Actor, "the actor is the principal")
	var out api.StableIssuanceResult
	res.json(&out)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)
	assert.True(t, out.Settled)
	assert.False(t, out.Replay)
	assert.Equal(t, api.StableIssuanceStatusIssued, out.Issuance.Status)

	res = h.do(http.MethodPost, stablePath+"/redemptions", stableRedeemBody("200000000"), "Idempotency-Key", "stable-redeem-key-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "200000000", fp.lastRedeem.Quantity.String())
	assert.Equal(t, testAccountID, fp.lastRedeem.AccountID)
	var rout api.StableRedemptionResult
	res.json(&rout)
	assert.Equal(t, api.StableRedemptionStatusRedeemed, rout.Redemption.Status)
}

// An unknown submission outcome is neither a success nor a failure: 202, the
// request in SUBMISSION_UNKNOWN, its effect UNKNOWN, awaiting reconciliation.
// Replaying the key answers exactly that again and submits nothing.
func TestStable_UnknownOutcome_IsAcceptedAndShownUnknown(t *testing.T) {
	t.Parallel()
	h, fp := newStableHarness(t)
	unknownSince := testNow.Add(time.Second)
	fp.issueOutcome.Settled = false
	fp.issueOutcome.View.Record.Status = stableasset.MintSubmissionUnknown
	fp.issueOutcome.View.Record.JournalTransactionID, fp.issueOutcome.View.Record.FinalSlot = nil, nil
	fp.issueOutcome.View.Reconciliation = StableRequestReconciliation{EffectStatus: econ.EffectUnknown, UnknownSince: &unknownSince}
	res := h.do(http.MethodPost, stablePath+"/issuances", stableIssueBody("600000000"), "Idempotency-Key", "stable-unknown-0001")
	require.Equal(t, http.StatusAccepted, res.Code, "body=%s", res.Body.String())
	var out api.StableIssuanceResult
	res.json(&out)
	assert.False(t, out.Settled)
	assert.Equal(t, api.StableIssuanceStatusSubmissionUnknown, out.Issuance.Status)
	assert.Equal(t, api.StableReconciliationAwaiting, out.Issuance.Reconciliation.State)
	assert.Equal(t, api.StableEffectStatusUnknown, out.Issuance.Reconciliation.EffectStatus)
	require.NotNil(t, out.Issuance.Reconciliation.UnknownSince)
	assert.Nil(t, out.Issuance.Reconciliation.ResolvedAt)
	assert.Nil(t, out.Issuance.JournalTransactionId, "nothing was posted")

	again := h.do(http.MethodPost, stablePath+"/issuances", stableIssueBody("600000000"), "Idempotency-Key", "stable-unknown-0001")
	require.Equal(t, http.StatusAccepted, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, res.Body.String(), again.Body.String())
	assert.Equal(t, 1, fp.count("Issue"), "a replay never reaches the service")

	// Settled from the chain later: RESOLVED, with when.
	resolvedAt := testNow.Add(time.Minute)
	fp.issuance.Reconciliation = StableRequestReconciliation{EffectStatus: econ.EffectApplied, UnknownSince: &unknownSince, ResolvedAt: &resolvedAt}
	res = h.do(http.MethodGet, stableBase+"/issuances/"+fp.issuance.Record.ID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var is api.StableIssuance
	res.json(&is)
	assert.Equal(t, api.StableReconciliationResolved, is.Reconciliation.State)
	require.NotNil(t, is.Reconciliation.ResolvedAt)
	assert.True(t, is.Reconciliation.ResolvedAt.Equal(resolvedAt))
}

func TestStable_IssueAndRedeem_OnlyTheOwnerMayAct(t *testing.T) {
	t.Parallel()
	h, fp := newStableHarness(t)
	foreign := stableIssueBody("600000000")
	foreign["account_id"] = testOtherAcct.String()
	res := h.do(http.MethodPost, stablePath+"/issuances", foreign, "Idempotency-Key", "stable-foreign-issue-1")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)

	// An operator: account:read_any never licenses a customer's command, and
	// the operator role does not hold sandbox:stable anyway.
	op := stableOperator()
	h.as(&op)
	res = h.do(http.MethodPost, stablePath+"/redemptions", stableRedeemBody("1000000"), "Idempotency-Key", "stable-operator-redeem-1")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Zero(t, fp.count("Issue")+fp.count("Redeem"), "a refused account never reaches the service")
}

func TestStable_IssueAndRedeem_ValidationAndServiceRefusals(t *testing.T) {
	t.Parallel()
	h, fp := newStableHarness(t)
	for i, bad := range []map[string]any{
		stableIssueBody("1.5"), stableIssueBody(""), stableIssueBody("1e6"),
		func() map[string]any { b := stableIssueBody("100"); b["mode"] = "LIVE"; return b }(),
		func() map[string]any { b := stableIssueBody("100"); b["reserve_asset_id"] = "not-a-uuid"; return b }(),
	} {
		res := h.do(http.MethodPost, stablePath+"/issuances", bad, "Idempotency-Key", "stable-bad-"+itoa(i)+"-key")
		require.Equal(t, http.StatusBadRequest, res.Code, "%v body=%s", bad, res.Body.String())
		assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
	}
	res := h.do(http.MethodPost, stableBase+"/not-a-uuid/issuances", stableIssueBody("100"), "Idempotency-Key", "stable-bad-path-key")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Zero(t, fp.count("Issue"))

	// The service's refusals arrive with their own codes, and a replay of the
	// key reproduces the refusal rather than asking again.
	for i, tc := range []struct {
		err    error
		status int
	}{
		{errs.New(errs.CodeAssetRestricted, "stableasset: the stable asset is PAUSED and is not issuing").WithField("reason", stableasset.RefusalAssetNotActive), http.StatusUnprocessableEntity},
		{errs.New(errs.CodeRiskMaxPosition, "stableasset: the fully reserved admission rule refused the mint").WithField("reason", stableasset.RefusalCapExceeded), http.StatusUnprocessableEntity},
		{errs.New(errs.CodeRiskRejected, "risk rejected: RISK_IDENTITY_UNVERIFIED"), http.StatusUnprocessableEntity},
		{errs.New(errs.CodeInsufficientBuyingPower, "capital: insufficient available balance"), http.StatusUnprocessableEntity},
		{errs.New(errs.CodeInvalidIdempotencyReuse, "econ: the intent key names another request"), http.StatusConflict},
	} {
		fp.err = tc.err
		key := "stable-refusal-" + itoa(i) + "-key"
		res := h.do(http.MethodPost, stablePath+"/issuances", stableIssueBody("600000000"), "Idempotency-Key", key)
		require.Equal(t, tc.status, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, errs.CodeOf(tc.err), res.problem().Code)
		calls := fp.count("Issue")
		fp.err = nil
		res = h.do(http.MethodPost, stablePath+"/issuances", stableIssueBody("600000000"), "Idempotency-Key", key)
		require.Equal(t, tc.status, res.Code, "a recorded refusal is replayed; body=%s", res.Body.String())
		assert.Equal(t, calls, fp.count("Issue"))
	}
}

// Every mutation runs under the Idempotency-Key contract: the same key and
// body replay the first answer without a second service call, and the same key
// with another body is refused.
func TestStable_EveryMutationReplaysItsFirstAnswer(t *testing.T) {
	t.Parallel()
	for _, m := range stableMutations() {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, fp := newStableHarness(t)
			if !m.customer {
				op := stableOperator()
				h.as(&op)
			}
			key := "stable-replay-" + m.name
			first := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Less(t, first.Code, 300, "body=%s", first.Body.String())
			second := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Equal(t, first.Code, second.Code, "body=%s", second.Body.String())
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

func TestStable_OperatorRoutes_RefuseCustomersAndStaleSessions(t *testing.T) {
	t.Parallel()
	for _, m := range stableMutations() {
		if m.customer {
			continue
		}
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, fp := newStableHarness(t)
			res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "stable-customer-"+m.name)
			require.Equal(t, http.StatusForbidden, res.Code, "a customer reached %s: %s", m.name, res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)

			stale := stableOperator()
			stale.AuthTime = testNow.Add(-2 * stepUpMaxAge)
			h.as(&stale)
			res = h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "stable-stale-"+m.name)
			if !m.stepUp {
				// It observes and records; it moves no value, so it does not
				// ask for a step-up.
				require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
				return
			}
			require.Equal(t, http.StatusForbidden, res.Code, "%s without a recent step-up: %s", m.name, res.Body.String())
			assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
			assert.Zero(t, fp.count(m.call))
		})
	}
}

func TestStable_Register_PegsInTheSandboxDollarAsThePrincipal(t *testing.T) {
	t.Parallel()
	h, fp := newStableHarness(t)
	op := stableOperator()
	h.as(&op)
	res := h.do(http.MethodPost, stableBase, stableDefinitionBody("httpapi-test-dollar"), "Idempotency-Key", "stable-register-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	def := fp.lastDef
	assert.Equal(t, econ.SandboxDollarID(), def.PegQuoteAssetID, "the peg's quote is not the caller's choice")
	assert.Equal(t, "1000000", def.PegMantissa.String())
	assert.Equal(t, int32(6), def.PegScale)
	assert.Equal(t, uint8(6), def.Decimals)
	assert.Equal(t, "1000000000000", def.IssuanceCap.String())
	assert.Equal(t, time.Hour, def.MaxPriceAge)
	assert.Equal(t, money.BPS(50), def.PegDeviationThreshold)
	assert.Equal(t, stableasset.RedemptionInstant, def.RedemptionMode)
	require.Len(t, def.Reserves, 2)
	assert.Equal(t, stableasset.ReserveSpec{AssetID: econ.SandboxDollarID(), Kind: stableasset.ReserveCashEquivalent, FactorBPS: 10_000, AcceptedForMint: true}, def.Reserves[0])
	assert.Equal(t, econ.Actor{Type: security.ActorOperator, ID: stableTestOperator.String()}, fp.lastActor)

	for i, change := range []func(map[string]any){
		func(b map[string]any) { b["peg"] = "one dollar" },
		func(b map[string]any) { b["peg"] = "-1" },
		func(b map[string]any) { b["decimals"] = 300 },
		func(b map[string]any) { b["issuance_cap"] = "1.5" },
		func(b map[string]any) { b["redemption_mode"] = "SOMETIMES" },
		func(b map[string]any) { b["max_price_age_ms"] = int64(1) << 62 },
		func(b map[string]any) { b["max_price_age_ms"] = 0 },
		func(b map[string]any) {
			b["reserves"] = []map[string]any{{"asset_id": "nope", "kind": "TOKENIZED", "factor_bps": 1, "accepted_for_mint": false}}
		},
		func(b map[string]any) {
			b["reserves"] = []map[string]any{{"asset_id": stableTestTBill.String(), "kind": "BONDS", "factor_bps": 1, "accepted_for_mint": false}}
		},
	} {
		b := stableDefinitionBody("httpapi-test-dollar")
		change(b)
		res := h.do(http.MethodPost, stableBase, b, "Idempotency-Key", "stable-register-bad-"+itoa(i))
		require.Equal(t, http.StatusBadRequest, res.Code, "%d body=%s", i, res.Body.String())
		assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
	}
	assert.Equal(t, 1, fp.count("Register"), "a refused definition never reaches the service")

	fp.err = errs.New(errs.CodeConflict, "stableasset: a different stable asset is registered under this name")
	res = h.do(http.MethodPost, stableBase, stableDefinitionBody("httpapi-test-dollar"), "Idempotency-Key", "stable-register-0002")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeConflict, res.problem().Code)
}

func TestStable_OperatorPasses_RenderTheirReportsAndBounds(t *testing.T) {
	t.Parallel()
	h, fp := newStableHarness(t)
	op := stableOperator()
	h.as(&op)

	res := h.do(http.MethodPost, stablePath+"/pause", map[string]any{"reason": "the peg is being investigated"}, "Idempotency-Key", "stable-pause-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, stableasset.StatusPaused, fp.lastStatus)
	assert.Equal(t, "the peg is being investigated", fp.lastReason)
	assert.Equal(t, econ.Actor{Type: security.ActorOperator, ID: stableTestOperator.String()}, fp.lastActor)

	res = h.do(http.MethodPost, stablePath+"/monitor-runs", nil, "Idempotency-Key", "stable-monitor-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var m api.StableMonitorReport
	res.json(&m)
	assert.True(t, m.PriceStale)
	assert.Nil(t, m.MarketPrice)
	assert.Equal(t, []api.StableAlertKind{api.StableAlertKindPriceStale}, m.Alerts)
	assert.Equal(t, "600000000", m.Reserve.IssuedSupply)
	assert.Equal(t, "100000000", m.Supply.HeldElsewhere)

	res = h.do(http.MethodPost, stablePath+"/reconciliations", nil, "Idempotency-Key", "stable-reconcile-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var s api.StableSupplyReconciliation
	res.json(&s)
	assert.Equal(t, api.SIMULATED, s.ValueDomain)
	assert.Equal(t, "500000000", s.CustomerWalletSum)
	assert.Equal(t, "100000000", s.HeldElsewhere, "units held elsewhere are outstanding supply too")
	assert.Equal(t, "600000000", s.LedgerOutstanding)
	assert.True(t, s.WalletsMatch)
	assert.True(t, s.Matched)
	require.NotNil(t, s.RecordId)

	res = h.do(http.MethodPost, stablePath+"/snapshots", nil, "Idempotency-Key", "stable-snapshot-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var sn api.StableSnapshotResult
	res.json(&sn)
	assert.Equal(t, int64(3), sn.Snapshot.Seq)
	assert.True(t, sn.Snapshot.Verified)
	assert.True(t, sn.Verification.Verified)

	res = h.do(http.MethodPost, stablePath+"/redemption-queue/run", map[string]any{"limit": 7}, "Idempotency-Key", "stable-queue-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 7, fp.lastLimit)
	res = h.do(http.MethodPost, stableBase+"/ops/resolve-unknown", nil, "Idempotency-Key", "stable-resolve-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 0, fp.lastLimit, "no limit asks for the service's default")
	var rep api.StableSettleReport
	res.json(&rep)
	assert.Equal(t, 2, rep.Examined)
	assert.Equal(t, 1, rep.Issued)
	assert.Equal(t, 1, rep.Failed)

	for i, bad := range []int{0, 501} {
		res = h.do(http.MethodPost, stableBase+"/ops/resolve-unknown", map[string]any{"limit": bad}, "Idempotency-Key", "stable-resolve-bad-"+itoa(i))
		require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	}
}
