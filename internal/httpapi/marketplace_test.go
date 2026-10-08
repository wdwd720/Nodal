package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
)

// mktID decodes a fixed UUIDv7 into a marketplace id, whose kind is
// unexported in its package.
func mktID[T any, P marketplaceTextID[T]](s string) T {
	out, err := marketplaceParseID[T, P](uuid.MustParse(s), "test")
	if err != nil {
		panic(err)
	}
	return out
}

var (
	testMktListing     = mktID[marketplace.ListingID]("0193b2e0-0000-7000-8000-00000000a001")
	testMktDraftOnly   = mktID[marketplace.ListingID]("0193b2e0-0000-7000-8000-00000000a002")
	testMktApproved    = mktID[marketplace.VersionID]("0193b2e0-0000-7000-8000-00000000b001")
	testMktDraft       = mktID[marketplace.VersionID]("0193b2e0-0000-7000-8000-00000000b002")
	testMktInstall     = mktID[marketplace.InstallID]("0193b2e0-0000-7000-8000-00000000c001")
	testMktFollow      = mktID[marketplace.FollowRunID]("0193b2e0-0000-7000-8000-00000000d001")
	testMktEvaluation  = mktID[marketplace.EvaluationID]("0193b2e0-0000-7000-8000-00000000e001")
	testMktRun         = mktRunID("0193b2e0-0000-7000-8000-00000000f001")
	testMktInstrument  = "0193b2e0-0000-7000-8000-0000000000a1"
	testMktCreatorUser = "0193b2e0-0000-7000-8000-0000000000c1"
	testMktPrice       = valuation.PriceID(econ.NewIntentID())
)

func mktRunID(s string) sandboxrt.RunID {
	r, err := sandboxrt.ParseRunID(s)
	if err != nil {
		panic(err)
	}
	return r
}

// fakeMarketplace is the marketplace port. It counts every call by method so a
// test can prove what reached the service and that a replay did not, and it
// records the scoped arguments each call was made with.
type fakeMarketplace struct {
	stubErr
	mu    sync.Mutex
	calls map[string]int

	listing     marketplace.Listing
	draftOnly   marketplace.Listing
	approved    marketplace.Version
	draft       marketplace.Version
	install     marketplace.Install
	summaries   []MarketplaceListingSummary
	queue       []MarketplaceQueueItem
	installView MarketplaceInstallView
	follow      MarketplaceFollowView
	run         MarketplaceRunView
	evaluation  MarketplaceEvaluation
	perf        marketplace.PerformanceSnapshot
	validation  marketplace.Validation
	next        string
	existed     bool

	lastAcct     accounts.AccountID
	lastLimit    int
	lastInstall  *marketplace.InstallID
	lastStatuses []marketplace.Status
	lastActor    econ.Actor
	lastMode     gates.Mode
	lastCreate   MarketplaceCreateListing
	lastFollow   MarketplaceFollowRequest
	lastSimulate MarketplaceSimulate
	lastFork     MarketplaceFork
	lastReason   string
	lastCode     string
	lastTo       marketplace.Moderation
}

func (f *fakeMarketplace) count(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
}

func (f *fakeMarketplace) callsTo(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fakeMarketplace) scoped(acct accounts.AccountID, limit int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAcct, f.lastLimit = acct, limit
}

func (f *fakeMarketplace) view(lid marketplace.ListingID) (MarketplaceListingView, error) {
	switch lid {
	case f.listing.ID:
		return MarketplaceListingView{Listing: f.listing, Versions: []MarketplaceVersionView{
			f.versionView(f.draft), f.versionView(f.approved),
		}}, nil
	case f.draftOnly.ID:
		d := f.draft
		d.ListingID = f.draftOnly.ID
		return MarketplaceListingView{Listing: f.draftOnly, Versions: []MarketplaceVersionView{f.versionView(d)}}, nil
	}
	return MarketplaceListingView{}, marketplaceListingNotFound(lid)
}

func (f *fakeMarketplace) versionView(v marketplace.Version) MarketplaceVersionView {
	vv := MarketplaceVersionView{Version: v, Reviews: []MarketplaceReview{}, Evaluations: []MarketplaceEvaluation{},
		Performance: []marketplace.PerformanceSnapshot{},
		Provenance: marketplace.Provenance{VersionID: v.ID, StrategyRef: v.StrategyRef(), SourceCommit: v.SourceCommit,
			SourceDigest: v.SourceDigest, ManifestHash: v.ManifestHash}}
	if v.Status == marketplace.StatusApproved {
		val := f.validation
		vv.Validation = &val
		vv.Reviews = []MarketplaceReview{{Decision: marketplace.DecisionApprove, ReasonCode: "MEETS_POLICY", Note: "looked at it",
			Reviewer: econ.Actor{Type: security.ActorOperator, ID: "op-reviewer-1"}, ReviewedAt: testNow}}
		vv.Evaluations = []MarketplaceEvaluation{f.evaluation}
		vv.Performance = []marketplace.PerformanceSnapshot{f.perf}
		vv.Provenance.ArtifactDigest = f.validation.ArtifactDigest
		vv.Provenance.ToolchainVersion = f.validation.ToolchainVersion
	}
	return vv
}

func (f *fakeMarketplace) Catalog(_ context.Context, _ string, limit int) ([]MarketplaceListingSummary, string, error) {
	f.count("Catalog")
	f.scoped(accounts.AccountID{}, limit)
	return f.summaries, f.next, f.fail()
}

func (f *fakeMarketplace) AccountListings(_ context.Context, acct accounts.AccountID, _ string, limit int) ([]MarketplaceListingSummary, string, error) {
	f.count("AccountListings")
	f.scoped(acct, limit)
	return f.summaries, f.next, f.fail()
}

func (f *fakeMarketplace) Listing(_ context.Context, lid marketplace.ListingID) (MarketplaceListingView, error) {
	f.count("Listing")
	if err := f.fail(); err != nil {
		return MarketplaceListingView{}, err
	}
	return f.view(lid)
}

func (f *fakeMarketplace) ListingRecord(_ context.Context, lid marketplace.ListingID) (marketplace.Listing, error) {
	v, err := f.view(lid)
	return v.Listing, err
}

func (f *fakeMarketplace) VersionRecord(_ context.Context, vid marketplace.VersionID) (marketplace.Listing, marketplace.Version, error) {
	switch vid {
	case f.approved.ID:
		return f.listing, f.approved, nil
	case f.draft.ID:
		return f.listing, f.draft, nil
	}
	return marketplace.Listing{}, marketplace.Version{}, marketplaceVersionNotFound(vid)
}

func (f *fakeMarketplace) ReviewQueue(_ context.Context, statuses []marketplace.Status, _ string, limit int) ([]MarketplaceQueueItem, string, error) {
	f.count("ReviewQueue")
	f.mu.Lock()
	f.lastStatuses, f.lastLimit = statuses, limit
	f.mu.Unlock()
	return f.queue, f.next, f.fail()
}

func (f *fakeMarketplace) Installs(_ context.Context, acct accounts.AccountID, _ string, limit int) ([]MarketplaceInstallView, string, error) {
	f.count("Installs")
	f.scoped(acct, limit)
	return []MarketplaceInstallView{f.installView}, f.next, f.fail()
}

func (f *fakeMarketplace) InstallRecord(_ context.Context, iid marketplace.InstallID) (marketplace.Install, error) {
	if iid != f.install.ID {
		return marketplace.Install{}, marketplaceInstallNotFound(iid)
	}
	return f.install, nil
}

func (f *fakeMarketplace) Follows(_ context.Context, acct accounts.AccountID, install *marketplace.InstallID, _ string, limit int) ([]MarketplaceFollowView, string, error) {
	f.count("Follows")
	f.scoped(acct, limit)
	f.mu.Lock()
	f.lastInstall = install
	f.mu.Unlock()
	return []MarketplaceFollowView{f.follow}, f.next, f.fail()
}

func (f *fakeMarketplace) Runs(_ context.Context, acct accounts.AccountID, _ string, limit int) ([]MarketplaceRunView, string, error) {
	f.count("Runs")
	f.scoped(acct, limit)
	return []MarketplaceRunView{f.run}, f.next, f.fail()
}

func (f *fakeMarketplace) CreateListing(_ context.Context, r MarketplaceCreateListing) (MarketplaceListingView, error) {
	f.count("CreateListing")
	f.mu.Lock()
	f.lastCreate, f.lastActor = r, r.Actor
	f.mu.Unlock()
	if err := f.fail(); err != nil {
		return MarketplaceListingView{}, err
	}
	return f.view(f.listing.ID)
}

func (f *fakeMarketplace) NewVersion(_ context.Context, r MarketplaceNewVersion) (marketplace.Version, error) {
	f.count("NewVersion")
	f.mu.Lock()
	f.lastActor = r.Actor
	f.mu.Unlock()
	return f.draft, f.fail()
}

func (f *fakeMarketplace) Fork(_ context.Context, r MarketplaceFork) (MarketplaceListingView, error) {
	f.count("Fork")
	f.mu.Lock()
	f.lastFork, f.lastActor = r, r.Actor
	f.mu.Unlock()
	if err := f.fail(); err != nil {
		return MarketplaceListingView{}, err
	}
	return f.view(f.listing.ID)
}

func (f *fakeMarketplace) versionOp(method string, actor econ.Actor, v marketplace.Version) (marketplace.Version, error) {
	f.count(method)
	f.mu.Lock()
	f.lastActor = actor
	f.mu.Unlock()
	return v, f.fail()
}

func (f *fakeMarketplace) Submit(_ context.Context, _ marketplace.VersionID, actor econ.Actor) (marketplace.Version, error) {
	v := f.draft
	v.Status = marketplace.StatusSubmitted
	return f.versionOp("Submit", actor, v)
}

func (f *fakeMarketplace) Simulate(_ context.Context, r MarketplaceSimulate) (MarketplaceEvaluation, error) {
	f.count("Simulate")
	f.mu.Lock()
	f.lastSimulate, f.lastActor = r, r.Actor
	f.mu.Unlock()
	return f.evaluation, f.fail()
}

func (f *fakeMarketplace) Validate(_ context.Context, _ marketplace.VersionID, actor econ.Actor) (marketplace.Version, marketplace.Validation, error) {
	v := f.draft
	v.Status = marketplace.StatusValidated
	out, err := f.versionOp("Validate", actor, v)
	return out, f.validation, err
}

func (f *fakeMarketplace) StartReview(_ context.Context, _ marketplace.VersionID, actor econ.Actor) (marketplace.Version, error) {
	v := f.draft
	v.Status = marketplace.StatusInReview
	return f.versionOp("StartReview", actor, v)
}

func (f *fakeMarketplace) Approve(_ context.Context, _ marketplace.VersionID, actor econ.Actor, code, _ string) (marketplace.Version, error) {
	f.mu.Lock()
	f.lastCode = code
	f.mu.Unlock()
	return f.versionOp("Approve", actor, f.approved)
}

func (f *fakeMarketplace) Reject(_ context.Context, _ marketplace.VersionID, actor econ.Actor, code, _ string) (marketplace.Version, error) {
	f.mu.Lock()
	f.lastCode = code
	f.mu.Unlock()
	v := f.draft
	v.Status, v.RejectionCode = marketplace.StatusRejected, code
	return f.versionOp("Reject", actor, v)
}

func (f *fakeMarketplace) Moderate(_ context.Context, _ marketplace.VersionID, actor econ.Actor, to marketplace.Moderation, reason string) (marketplace.Version, error) {
	f.mu.Lock()
	f.lastTo, f.lastReason = to, reason
	f.mu.Unlock()
	v := f.approved
	v.Moderation = to
	return f.versionOp("Moderate", actor, v)
}

func (f *fakeMarketplace) Deprecate(_ context.Context, _ marketplace.VersionID, actor econ.Actor, reason string) (marketplace.Version, error) {
	f.mu.Lock()
	f.lastReason = reason
	f.mu.Unlock()
	v := f.approved
	v.Status = marketplace.StatusDeprecated
	return f.versionOp("Deprecate", actor, v)
}

func (f *fakeMarketplace) InstallVersion(_ context.Context, r MarketplaceInstallRequest) (MarketplaceInstallView, bool, error) {
	f.count("InstallVersion")
	f.mu.Lock()
	f.lastMode, f.lastActor, f.lastAcct = r.Mode, r.Actor, r.Account
	existed := f.existed
	f.mu.Unlock()
	if err := f.fail(); err != nil {
		return MarketplaceInstallView{}, false, err
	}
	return f.installView, existed, nil
}

func (f *fakeMarketplace) RemoveInstall(_ context.Context, _ marketplace.InstallID, actor econ.Actor, reason string) (MarketplaceInstallView, error) {
	f.count("RemoveInstall")
	f.mu.Lock()
	f.lastActor, f.lastReason = actor, reason
	f.mu.Unlock()
	out := f.installView
	out.Install.Status = marketplace.InstallRemoved
	return out, f.fail()
}

func (f *fakeMarketplace) Follow(_ context.Context, r MarketplaceFollowRequest) (MarketplaceFollowView, error) {
	f.count("Follow")
	f.mu.Lock()
	f.lastFollow, f.lastActor = r, r.Actor
	f.mu.Unlock()
	return f.follow, f.fail()
}

func (f *fakeMarketplace) RecordPerformance(_ context.Context, _ marketplace.InstallID, actor econ.Actor) (marketplace.PerformanceSnapshot, error) {
	f.count("RecordPerformance")
	f.mu.Lock()
	f.lastActor = actor
	f.mu.Unlock()
	return f.perf, f.fail()
}

func testMktManifest() sandboxrt.Manifest {
	return sandboxrt.Manifest{AllowedIntentKinds: []econ.IntentKind{econ.KindReduceExposure, econ.KindAcquireExposure},
		AllowedInstrumentIDs: []string{testMktInstrument}, MaxNotionalPerIntent: "1000.00", MaxIntentsPerRun: 2,
		MinIntervalSeconds: 3600}.Canonicalize()
}

// newFakeMarketplace holds a listing of testAccountID with an APPROVED,
// CLEARED version and a DRAFT, a second listing with a DRAFT only, an install
// of the approved version by testAccountID, and a follow run whose one intent
// the risk kernel allowed and paper execution refused.
func newFakeMarketplace() *fakeMarketplace {
	m := testMktManifest()
	hash, err := m.Hash()
	if err != nil {
		panic(err)
	}
	creator := econ.Actor{Type: security.ActorUser, ID: testMktCreatorUser}
	listing := marketplace.Listing{ID: testMktListing, CreatorAccountID: testAccountID, Creator: creator, Title: "Momentum", CreatedAt: testNow}
	draftOnly := listing
	draftOnly.ID, draftOnly.Title = testMktDraftOnly, "Unfinished"
	approved := marketplace.Version{
		ID: testMktApproved, ListingID: testMktListing, Number: 1, Status: marketplace.StatusApproved, Moderation: marketplace.ModerationCleared,
		Description: "buys strength", RiskClass: marketplace.RiskModerate, SupportedInstrumentIDs: []string{testMktInstrument},
		Manifest: m, ManifestHash: hash, SourceDigest: []byte{0x5a, 0x01}, SourceCommit: "abc123",
		BacktestEvidenceRef: "evidence://backtests/momentum", DataDependencies: []string{"closes"},
		Fees:   marketplace.FeeModel{PerformanceFeeBPS: 2000, ManagementFeeBPS: 100, HighWaterMark: true},
		Config: map[string]string{"order_units": "1000000"}, CreatedBy: creator, CreatedAt: testNow, UpdatedAt: testNow,
	}
	draft := approved
	draft.ID, draft.Number, draft.Status, draft.Moderation = testMktDraft, 2, marketplace.StatusDraft, marketplace.ModerationUnreviewed
	summary := MarketplaceVersionSummary{ID: approved.ID, ListingID: approved.ListingID, Number: 1, Status: approved.Status,
		Moderation: approved.Moderation, RiskClass: approved.RiskClass, Description: approved.Description, CreatedAt: testNow, UpdatedAt: testNow}
	install := marketplace.Install{ID: testMktInstall, AccountID: testAccountID, VersionID: testMktApproved, Mode: gates.ModePaper,
		Status: marketplace.InstallActive, InstalledBy: econ.Actor{Type: security.ActorUser, ID: testUserID.String()}, PaperCapital: "10000.00",
		CreatedAt: testNow}
	perf := marketplace.PerformanceSnapshot{
		Performance: marketplace.Performance{Points: 3, FirstAsOf: testNow.Add(-2 * time.Hour), LastAsOf: testNow, ReturnBPS: -125,
			MaxDrawdownBPS: 310, VolatilityBPS: 42, TurnoverBPS: 15, HistoryDigest: []byte{0xab, 0xcd}},
		InstallID: testMktInstall, VersionID: testMktApproved, ComputedAt: testNow,
	}
	intentID := econ.NewIntentID()
	effectID := econ.NewEffectID()
	decision := risk.AdvancedDecisionRecord{ID: testDecisionID(), IntentID: intentID.String(), AccountID: testAccountID.String(),
		IntentKind: "ACQUIRE_EXPOSURE", Stage: risk.StagePreTrade, Verdict: risk.Allow, ReasonCodes: []string{}, InputHash: "ih",
		PolicyVersion: "advanced-sandbox-v1", PolicyHash: "ph", EvaluatorVersion: risk.AdvancedEvaluatorVersion, DecisionHash: "dh-1",
		EvaluatedAt: testNow}
	follow := MarketplaceFollowView{
		Run: marketplace.FollowRun{
			ID: testMktFollow, InstallID: testMktInstall, VersionID: testMktApproved, AccountID: testAccountID, SandboxRunID: testMktRun,
			Trigger: marketplace.TriggerExplicit, AsOf: testNow, InputDigest: []byte{0x01}, RunDigest: []byte{0x02},
			Termination: sandboxrt.TerminationOK, ProposalsCount: 1, AcceptedCount: 1, IntentsSubmitted: 1, IntentsFailed: 1,
			PaperCash: "9849.75000000", Equity: "10000.25000000", Turnover: "0", EvidenceRef: "proof://marketplace/follow/x.json",
			EvidenceSHA256: []byte{0xee}, CreatedAt: testNow,
			Marks: []marketplace.FollowMark{{InstrumentID: testMktInstrument, PriceID: testMktPrice, Mark: "150.25", ObservedAt: testNow}},
			Intents: []marketplace.FollowIntent{{ProposalSeq: 1, IntentID: intentID, Kind: econ.KindAcquireExposure,
				Outcome: marketplace.OutcomeDispatchFailed, DecisionHash: "dh-1", EffectID: &effectID, Notional: "150.250000",
				ReferencePrice: "150.25"}},
		},
		Decisions: map[econ.IntentID]risk.AdvancedDecisionRecord{intentID: decision},
		Effects:   map[econ.EffectID]MarketplaceEffectState{effectID: {Status: econ.EffectFailed, FailureCode: "UNSUPPORTED"}},
	}
	follo := testMktFollow
	return &fakeMarketplace{
		calls:   map[string]int{},
		listing: listing, draftOnly: draftOnly, approved: approved, draft: draft, install: install,
		summaries:   []MarketplaceListingSummary{{Listing: listing, Latest: summary}},
		queue:       []MarketplaceQueueItem{{Listing: listing, Version: summary}},
		installView: MarketplaceInstallView{Install: install, Listing: listing, Version: summary, LatestPerformance: &perf},
		follow:      follow,
		run: MarketplaceRunView{
			Run: sandboxrt.RunRecord{ID: testMktRun, ArtifactDigest: []byte{0xaa}, InputDigest: []byte{0x01}, LimitsVersion: "sandbox-limits/v1",
				Termination: sandboxrt.TerminationInvalidOutput, ExitCode: 0, ArtifactVerified: true, WallMS: 7, OutputBytes: 40,
				OutputDigest: []byte{0x03}, RunDigest: []byte{0x02}, StrategyRef: approved.StrategyRef(), CreatedAt: testNow},
			FollowRunID: &follo,
			Violations:  []sandboxrt.Violation{{Code: sandboxrt.ViolationOutputRecordType, Seq: 1, Detail: "a transfer record is not a proposal"}},
		},
		evaluation: MarketplaceEvaluation{Evaluation: marketplace.Evaluation{ID: testMktEvaluation, VersionID: testMktApproved,
			Kind: marketplace.EvaluationSimulation, DatasetVersion: "ds-2026-10", SnapshotsDigest: []byte{0x0a}, ResultsDigest: []byte{0x0b},
			RunsOK: 1, ProposalsCount: 1, AcceptedCount: 1, EvidenceRef: "proof://marketplace/evaluation/x.json", EvidenceSHA256: []byte{0x0c},
			Runs: []marketplace.EvaluationRun{{Seq: 1, SandboxRunID: testMktRun, AsOf: testNow, InputDigest: "01", RunDigest: "02",
				Termination: sandboxrt.TerminationOK, Proposals: 1, Accepted: 1}}},
			SnapshotsCount: 1, CreatedAt: testNow},
		perf: perf,
		validation: marketplace.Validation{VersionID: testMktApproved, Outcome: marketplace.ValidationPassed, ArtifactDigest: []byte{0xaa, 0xbb},
			ScanCodes: []string{}, ToolchainVersion: "go1.27.1", SDKVersion: "nodalsdk/v1@sha256:00", RuntimeVersion: "wazero/v1.12.0",
			BuildLog: []byte("# strategy\n./main.go:3:1: note: built\n")},
	}
}

func newMarketplaceHarness(t *testing.T) (*harness, *fakeMarketplace) {
	t.Helper()
	h := newHarness(t)
	fm := newFakeMarketplace()
	ports := h.ports.ports()
	ports.Marketplace = fm
	h.server.opts.Ports = ports
	return h, fm
}

func marketplaceStranger() security.Principal {
	p := customerPrincipal()
	p.SubjectID = accounts.NewUserID().String()
	p.AccountIDs = []string{testOtherAcct.String()}
	return p
}

// testMktVersionInput is a complete version: manifest lists out of order (the
// server canonicalizes them), one source file and every optional field.
func testMktVersionInput() map[string]any {
	return map[string]any{
		"description": "buys strength", "risk_class": "MODERATE", "source_commit": "abc123",
		"supported_instrument_ids": []string{testMktInstrument},
		"manifest": map[string]any{
			"allowed_intent_kinds":   []string{"REDUCE_EXPOSURE", "ACQUIRE_EXPOSURE", "ACQUIRE_EXPOSURE"},
			"allowed_instrument_ids": []string{testMktInstrument}, "max_notional_per_intent": "1000.00",
			"max_intents_per_run": 2, "min_interval_seconds": 3600,
		},
		"source":                map[string]string{"main.go": "package main\n\nfunc main() {}\n"},
		"backtest_evidence_ref": "evidence://backtests/momentum", "data_dependencies": []string{"closes"},
		"fees":   map[string]any{"performance_fee_bps": 2000, "management_fee_bps": 100, "high_water_mark": true},
		"config": map[string]string{"order_units": "1000000"},
	}
}

func testMktSnapshot() map[string]any {
	return map[string]any{
		"as_of": testNow.Format(time.RFC3339Nano),
		"instruments": []map[string]any{{
			"instrument_id": testMktInstrument, "kind": "SPOT_PAIR", "base_decimals": 6, "quote_decimals": 6,
			"mark": "150.25", "best_bid": "150.20", "best_ask": "150.30", "closes": []string{"140.00", "152.50"},
		}},
		"portfolio": map[string]any{"balance": "10000.25", "positions": []map[string]any{{"instrument_id": testMktInstrument, "quantity": "2000000"}}},
	}
}

func TestMarketplace_CatalogIsReadableByAnyCustomerAndNamesTheCreatorsAccountToItsOwnerOnly(t *testing.T) {
	t.Parallel()
	h, fm := newMarketplaceHarness(t)
	fm.next = "cursor-2"
	res := h.do(http.MethodGet, "/v1/advanced/strategies/catalog?limit=5000", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var page api.StrategyListingPage
	res.json(&page)
	require.Len(t, page.Items, 1)
	assert.Equal(t, maxPageLimit, fm.lastLimit, "the limit is clamped")
	require.NotNil(t, page.Items[0].CreatorAccountId, "the creator's own account sees it named")
	assert.Equal(t, testAccountID.String(), page.Items[0].CreatorAccountId.String())
	assert.Equal(t, testMktListing.String()+"@1", page.Items[0].LatestVersion.StrategyRef)
	next, err := page.NextCursor.Get()
	require.NoError(t, err)
	assert.Equal(t, "cursor-2", next)

	stranger := marketplaceStranger()
	h.as(&stranger)
	res = h.do(http.MethodGet, "/v1/advanced/strategies/catalog", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var public api.StrategyListingPage
	res.json(&public)
	require.Len(t, public.Items, 1)
	assert.Nil(t, public.Items[0].CreatorAccountId, "another customer's account id is never handed to a stranger")
	assert.NotContains(t, res.Body.String(), testAccountID.String())
}

func TestMarketplace_AccountReadsAreScopedToTheCallersAccount(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"/v1/advanced/strategies/listings", "/v1/advanced/strategies/installs",
		"/v1/advanced/strategies/follows", "/v1/advanced/strategies/runs",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			h, fm := newMarketplaceHarness(t)
			res := h.do(http.MethodGet, path+"?account_id="+testAccountID.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, testAccountID, fm.lastAcct)

			res = h.do(http.MethodGet, path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)

			op := operatorPrincipal()
			h.as(&op)
			res = h.do(http.MethodGet, path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "an operator with account:read_any reads any account; body=%s", res.Body.String())
			assert.Equal(t, testOtherAcct, fm.lastAcct)
		})
	}
}

func TestMarketplace_ListingDetailShowsAStrangerOnlyItsPublicVersions(t *testing.T) {
	t.Parallel()
	h, _ := newMarketplaceHarness(t)
	path := "/v1/advanced/strategies/listings/" + testMktListing.String()

	res := h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var owner api.StrategyListingDetail
	res.json(&owner)
	require.Len(t, owner.Versions, 2, "the creator's account sees every version, the DRAFT included")
	require.NotNil(t, owner.CreatorAccountId)
	require.NotNil(t, owner.Versions[1].CreatedByActorId)
	assert.Equal(t, testMktCreatorUser, *owner.Versions[1].CreatedByActorId)
	require.Len(t, owner.Versions[1].Reviews, 1)
	require.NotNil(t, owner.Versions[1].Reviews[0].ReviewerActorId)

	stranger := marketplaceStranger()
	h.as(&stranger)
	res = h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var public api.StrategyListingDetail
	res.json(&public)
	require.Len(t, public.Versions, 1, "a stranger sees the approved version only")
	v := public.Versions[0]
	assert.Equal(t, api.StrategyStatusApproved, v.Status)
	assert.Nil(t, public.CreatorAccountId)
	assert.Nil(t, v.CreatedByActorId, "who wrote it is the creator's and the operators' to see")
	require.Len(t, v.Reviews, 1)
	assert.Equal(t, "OPERATOR", v.Reviews[0].ReviewerActorType, "that an operator reviewed it is public")
	assert.Nil(t, v.Reviews[0].ReviewerActorId)
	require.Len(t, v.Performance, 1)
	assert.Nil(t, v.Performance[0].InstallId, "performance on a listing comes from other accounts' installs, which are not named")
	assert.NotContains(t, res.Body.String(), testMktDraft.String())
	assert.NotContains(t, res.Body.String(), testMktCreatorUser)

	// A listing with no public version is NOT_FOUND, exactly as an absent id.
	res = h.do(http.MethodGet, "/v1/advanced/strategies/listings/"+testMktDraftOnly.String(), nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	hidden := res.problem()
	absentID := mktID[marketplace.ListingID]("0193b2e0-0000-7000-8000-00000000a0ff")
	res = h.do(http.MethodGet, "/v1/advanced/strategies/listings/"+absentID.String(), nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	absent := res.problem()
	assert.Equal(t, absent.Code, hidden.Code)
	assert.Equal(t, absent.Detail, hidden.Detail)

	// An operator who may read any account sees all of it.
	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, "/v1/advanced/strategies/listings/"+testMktDraftOnly.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
}

// A write that names a record of another account is told it does not exist,
// and nothing reaches the service.
func TestMarketplace_WritesOnAnotherAccountsRecordsAreNotFound(t *testing.T) {
	t.Parallel()
	other := map[string]any{"account_id": testOtherAcct.String()}
	cases := []struct {
		name, path, method string
		body               any
	}{
		{"new version", "/v1/advanced/strategies/listings/" + testMktListing.String() + "/versions", "NewVersion",
			map[string]any{"account_id": testOtherAcct.String(), "version": testMktVersionInput()}},
		{"submit", "/v1/advanced/strategies/versions/" + testMktDraft.String() + "/submit", "Submit", other},
		{"simulate", "/v1/advanced/strategies/versions/" + testMktApproved.String() + "/simulations", "Simulate",
			map[string]any{"account_id": testOtherAcct.String(), "dataset_version": "ds-1", "snapshots": []any{testMktSnapshot()}}},
		{"fork a draft", "/v1/advanced/strategies/versions/" + testMktDraft.String() + "/forks", "Fork",
			map[string]any{"account_id": testOtherAcct.String(), "title": "Mine now", "version": testMktVersionInput()}},
		{"install a draft", "/v1/advanced/strategies/installs", "InstallVersion",
			map[string]any{"account_id": testOtherAcct.String(), "version_id": testMktDraft.String(), "mode": "PAPER"}},
		{"follow", "/v1/advanced/strategies/installs/" + testMktInstall.String() + "/follows", "Follow", other},
		{"remove", "/v1/advanced/strategies/installs/" + testMktInstall.String() + "/remove", "RemoveInstall", other},
		{"performance", "/v1/advanced/strategies/installs/" + testMktInstall.String() + "/performance", "RecordPerformance", other},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, fm := newMarketplaceHarness(t)
			stranger := marketplaceStranger()
			h.as(&stranger)
			res := h.do(http.MethodPost, tc.path, tc.body, "Idempotency-Key", "stranger-key-0001")
			require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeNotFound, res.problem().Code)
			assert.Zero(t, fm.callsTo(tc.method), "nothing reaches the service")
		})
	}
}

// The public version may be forked and installed by anyone; the record checks
// pass and the service decides.
func TestMarketplace_APublicVersionCanBeForkedAndInstalledByAnotherCustomer(t *testing.T) {
	t.Parallel()
	h, fm := newMarketplaceHarness(t)
	stranger := marketplaceStranger()
	h.as(&stranger)
	res := h.do(http.MethodPost, "/v1/advanced/strategies/versions/"+testMktApproved.String()+"/forks",
		map[string]any{"account_id": testOtherAcct.String(), "title": "My fork", "version": map[string]any{
			"description": "a fork", "risk_class": "AGGRESSIVE", "source_commit": "fork-1"}},
		"Idempotency-Key", "fork-key-000001")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testOtherAcct, fm.lastFork.Account)
	assert.Equal(t, testMktApproved, fm.lastFork.ParentID)
	assert.Nil(t, fm.lastFork.Version.Source, "a fork without source takes the parent's")
	assert.Empty(t, fm.lastFork.Version.Manifest.Schema, "a fork without a manifest takes the parent's")

	res = h.do(http.MethodPost, "/v1/advanced/strategies/installs",
		map[string]any{"account_id": testOtherAcct.String(), "version_id": testMktApproved.String(), "mode": "PAPER"},
		"Idempotency-Key", "install-key-0001")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testOtherAcct, fm.lastAcct)
}

func TestMarketplace_WritesActOnlyForAnAccountTheCallerOwns(t *testing.T) {
	t.Parallel()
	h, fm := newMarketplaceHarness(t)
	res := h.do(http.MethodPost, "/v1/advanced/strategies/listings",
		map[string]any{"account_id": testOtherAcct.String(), "title": "Momentum", "version": testMktVersionInput()},
		"Idempotency-Key", "create-key-0001")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)

	// account:read_any is a read permission; it never lets an operator write
	// for a customer.
	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodPost, "/v1/advanced/strategies/listings",
		map[string]any{"account_id": testAccountID.String(), "title": "Momentum", "version": testMktVersionInput()},
		"Idempotency-Key", "create-key-0002")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Zero(t, fm.callsTo("CreateListing"))
}

func TestMarketplace_CreateTranslatesTheVersionExactly(t *testing.T) {
	t.Parallel()
	h, fm := newMarketplaceHarness(t)
	res := h.do(http.MethodPost, "/v1/advanced/strategies/listings",
		map[string]any{"account_id": testAccountID.String(), "title": "Momentum", "version": testMktVersionInput()},
		"Idempotency-Key", "create-key-0003")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	c := fm.lastCreate
	assert.Equal(t, testAccountID, c.Account)
	assert.Equal(t, econ.Actor{Type: security.ActorUser, ID: testUserID.String()}, c.Actor, "the actor is the principal")
	assert.Equal(t, "Momentum", c.Title)
	m := c.Version.Manifest
	assert.Equal(t, sandboxrt.ManifestSchema, m.Schema)
	assert.Equal(t, []econ.IntentKind{econ.KindAcquireExposure, econ.KindReduceExposure}, m.AllowedIntentKinds,
		"lists are sorted and de-duplicated so the hash covers exactly what is returned")
	assert.Equal(t, "1000.00", m.MaxNotionalPerIntent)
	assert.Equal(t, []byte("package main\n\nfunc main() {}\n"), c.Version.Source["main.go"])
	assert.Equal(t, money.BPS(2000), c.Version.Fees.PerformanceFeeBPS)
	assert.Equal(t, "1000000", c.Version.Config["order_units"])
	assert.Equal(t, []string{testMktInstrument}, c.Version.SupportedInstrumentIDs)

	var detail api.StrategyListingDetail
	res.json(&detail)
	assert.Equal(t, api.SIMULATED, detail.ValueDomain)
	require.NotEmpty(t, detail.Versions)
	assert.Equal(t, "1000.00", detail.Versions[0].Manifest.MaxNotionalPerIntent)
}

func TestMarketplace_ValidationErrors(t *testing.T) {
	t.Parallel()
	bigFile := strings.Repeat("x", 65537)
	manyFiles := map[string]string{}
	for i := 0; i < 33; i++ {
		manyFiles["f"+strings.Repeat("a", i+1)+".go"] = "package main\n"
	}
	withSource := func(src map[string]string) map[string]any {
		v := testMktVersionInput()
		v["source"] = src
		return map[string]any{"account_id": testAccountID.String(), "title": "Momentum", "version": v}
	}
	snapshots := make([]any, 21)
	for i := range snapshots {
		snapshots[i] = testMktSnapshot()
	}
	badDecimals := testMktSnapshot()
	badDecimals["instruments"].([]map[string]any)[0]["base_decimals"] = 19
	cases := []struct {
		name, path string
		body       any
		field      string
	}{
		{"oversized source file", "/v1/advanced/strategies/listings", withSource(map[string]string{"main.go": bigFile}), "version.source"},
		{"too many source files", "/v1/advanced/strategies/listings", withSource(manyFiles), "version.source"},
		{"too many snapshots", "/v1/advanced/strategies/versions/" + testMktApproved.String() + "/simulations",
			map[string]any{"account_id": testAccountID.String(), "dataset_version": "ds-1", "snapshots": snapshots}, "snapshots"},
		{"unknown install mode", "/v1/advanced/strategies/installs",
			map[string]any{"account_id": testAccountID.String(), "version_id": testMktApproved.String(), "mode": "REAL_MONEY"}, "mode"},
		{"decimals out of range", "/v1/advanced/strategies/versions/" + testMktApproved.String() + "/simulations",
			map[string]any{"account_id": testAccountID.String(), "dataset_version": "ds-1", "snapshots": []any{badDecimals}},
			"snapshot.instruments"},
		{"not a version 7 id", "/v1/advanced/strategies/versions/" + uuid.NewString() + "/submit",
			map[string]any{"account_id": testAccountID.String()}, "versionId"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, fm := newMarketplaceHarness(t)
			res := h.do(http.MethodPost, tc.path, tc.body, "Idempotency-Key", "validation-key-1")
			require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
			p := res.problem()
			assert.Equal(t, errs.CodeValidationFailed, p.Code)
			assert.Equal(t, tc.field, p.Fields["field"])
			fm.mu.Lock()
			defer fm.mu.Unlock()
			assert.Empty(t, fm.calls, "a malformed request reaches no service")
		})
	}

	h, _ := newMarketplaceHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	res := h.do(http.MethodPost, "/v1/advanced/strategies/versions/"+testMktApproved.String()+"/moderation",
		map[string]any{"to": "HIDDEN", "reason": "x"}, "Idempotency-Key", "validation-key-2")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	res = h.do(http.MethodGet, "/v1/advanced/strategies/review-queue?status=PUBLISHED", nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
}

// mktMutation is one write route with a body, a different body for the same
// key, and the port method it must call exactly once.
type mktMutation struct {
	name, path, method string
	body, other        any
	operator           bool
	status             int
}

func mktMutations() []mktMutation {
	acct := map[string]any{"account_id": testAccountID.String()}
	// The idempotency hash canonicalizes JSON, so a different body must differ
	// in content, not in whitespace.
	acctOther := map[string]any{"account_id": testAccountID.String(), "note": "a different request"}
	version := map[string]any{"account_id": testAccountID.String(), "version": testMktVersionInput()}
	other := testMktVersionInput()
	other["description"] = "different"
	snap := testMktSnapshot()
	v := "/v1/advanced/strategies/versions/"
	i := "/v1/advanced/strategies/installs/" + testMktInstall.String()
	return []mktMutation{
		{"create listing", "/v1/advanced/strategies/listings", "CreateListing",
			map[string]any{"account_id": testAccountID.String(), "title": "Momentum", "version": testMktVersionInput()},
			map[string]any{"account_id": testAccountID.String(), "title": "Other", "version": testMktVersionInput()}, false, http.StatusCreated},
		{"new version", "/v1/advanced/strategies/listings/" + testMktListing.String() + "/versions", "NewVersion",
			version, map[string]any{"account_id": testAccountID.String(), "version": other}, false, http.StatusCreated},
		{"fork", v + testMktApproved.String() + "/forks", "Fork",
			map[string]any{"account_id": testAccountID.String(), "title": "Fork", "version": testMktVersionInput()},
			map[string]any{"account_id": testAccountID.String(), "title": "Fork 2", "version": testMktVersionInput()}, false, http.StatusCreated},
		{"submit", v + testMktDraft.String() + "/submit", "Submit", acct, acctOther, false, http.StatusOK},
		{"simulate", v + testMktApproved.String() + "/simulations", "Simulate",
			map[string]any{"account_id": testAccountID.String(), "dataset_version": "ds-1", "snapshots": []any{snap}},
			map[string]any{"account_id": testAccountID.String(), "dataset_version": "ds-2", "snapshots": []any{snap}}, false, http.StatusCreated},
		{"install", "/v1/advanced/strategies/installs", "InstallVersion",
			map[string]any{"account_id": testAccountID.String(), "version_id": testMktApproved.String(), "mode": "PAPER"},
			map[string]any{"account_id": testAccountID.String(), "version_id": testMktApproved.String(), "mode": "SANDBOX"}, false, http.StatusCreated},
		{"remove install", i + "/remove", "RemoveInstall", acct,
			map[string]any{"account_id": testAccountID.String(), "reason": "changed my mind"}, false, http.StatusOK},
		{"follow", i + "/follows", "Follow", acct, acctOther, false, http.StatusCreated},
		{"performance", i + "/performance", "RecordPerformance", acct, acctOther, false, http.StatusCreated},
		{"validate", v + testMktDraft.String() + "/validate", "Validate", nil, "{}", true, http.StatusOK},
		{"start review", v + testMktDraft.String() + "/review", "StartReview", nil, "{}", true, http.StatusOK},
		{"approve", v + testMktDraft.String() + "/approve", "Approve",
			map[string]any{"reason_code": "MEETS_POLICY"}, map[string]any{"reason_code": "OTHER_REASON"}, true, http.StatusOK},
		{"reject", v + testMktDraft.String() + "/reject", "Reject",
			map[string]any{"reason_code": "UNSAFE"}, map[string]any{"reason_code": "UNSAFE", "note": "x"}, true, http.StatusOK},
		{"moderate", v + testMktApproved.String() + "/moderation", "Moderate",
			map[string]any{"to": "FLAGGED", "reason": "reports"}, map[string]any{"to": "TAKEN_DOWN", "reason": "reports"}, true, http.StatusOK},
		{"deprecate", v + testMktApproved.String() + "/deprecate", "Deprecate",
			map[string]any{"reason": "superseded"}, map[string]any{"reason": "replaced"}, true, http.StatusOK},
	}
}

// Every mutation runs once per key: a replay returns the recorded answer
// without a second service call, and the same key with a different body is
// refused.
func TestMarketplace_EveryMutationReplaysWithoutASecondServiceCall(t *testing.T) {
	t.Parallel()
	for _, m := range mktMutations() {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, fm := newMarketplaceHarness(t)
			if m.operator {
				op := operatorPrincipal()
				h.as(&op)
			}
			key := "replay-" + strings.ReplaceAll(m.name, " ", "-")
			first := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Equal(t, m.status, first.Code, "body=%s", first.Body.String())
			require.Equal(t, 1, fm.callsTo(m.method))

			replay := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Contains(t, []int{http.StatusOK, m.status}, replay.Code, "body=%s", replay.Body.String())
			assert.JSONEq(t, first.Body.String(), replay.Body.String(), "a replay is the recorded answer")
			assert.Equal(t, 1, fm.callsTo(m.method), "a replay never reaches the service")

			reuse := h.do(http.MethodPost, m.path, m.other, "Idempotency-Key", key)
			require.Equal(t, http.StatusConflict, reuse.Code, "body=%s", reuse.Body.String())
			assert.Equal(t, errs.CodeInvalidIdempotencyReuse, reuse.problem().Code)
			assert.Equal(t, 1, fm.callsTo(m.method))
		})
	}
}

// A deployment without the marketplace says so on every route.
func TestMarketplace_UnwiredPortIsUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	op := operatorPrincipal()
	op.AccountIDs = []string{testAccountID.String()}
	h.as(&op)
	gets := []string{
		"/v1/advanced/strategies/catalog",
		"/v1/advanced/strategies/listings?account_id=" + testAccountID.String(),
		"/v1/advanced/strategies/listings/" + testMktListing.String(),
		"/v1/advanced/strategies/review-queue",
		"/v1/advanced/strategies/installs?account_id=" + testAccountID.String(),
		"/v1/advanced/strategies/follows?account_id=" + testAccountID.String(),
		"/v1/advanced/strategies/runs?account_id=" + testAccountID.String(),
	}
	for _, path := range gets {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", path, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, path)
	}
	for _, m := range mktMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "unwired-"+strings.ReplaceAll(m.name, " ", "-"))
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", m.path, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, m.path)
	}
}

func TestMarketplace_FollowReportsEveryIntentWithItsRiskDecisionAndExactFigures(t *testing.T) {
	t.Parallel()
	h, fm := newMarketplaceHarness(t)
	res := h.do(http.MethodPost, "/v1/advanced/strategies/installs/"+testMktInstall.String()+"/follows",
		map[string]any{"account_id": testAccountID.String()}, "Idempotency-Key", "follow-key-00001")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())

	// The request names the install and nothing about the market or the
	// portfolio: the service builds the run's input itself.
	assert.Equal(t, testMktInstall, fm.lastFollow.InstallID)
	assert.Equal(t, testUserID.String(), fm.lastFollow.Actor.ID, "the follow is the account owner's")

	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	assert.Equal(t, "9849.75000000", raw["paper_cash"], "the paper book's cash, every digit")
	assert.Equal(t, "10000.25000000", raw["equity"])
	assert.Equal(t, false, raw["replayed"])
	marks := raw["marks"].([]any)
	require.Len(t, marks, 1)
	mark := marks[0].(map[string]any)
	assert.Equal(t, "150.25", mark["mark"])
	assert.Equal(t, testMktInstrument, mark["instrument_id"])
	assert.Equal(t, testMktPrice.String(), mark["price_id"], "the price store's observation, by id")
	intents := raw["intents"].([]any)
	require.Len(t, intents, 1)
	it := intents[0].(map[string]any)
	assert.Equal(t, "150.250000", it["notional"], "a notional keeps every digit the service computed")
	assert.Equal(t, "150.25", it["reference_price"])
	assert.Equal(t, "DISPATCH_FAILED", it["outcome"])
	assert.Equal(t, "FAILED", it["effect_status"])
	assert.Equal(t, "UNSUPPORTED", it["effect_failure_code"])
	decision := it["risk_decision"].(map[string]any)
	assert.Equal(t, "ALLOW", decision["verdict"])
	assert.Equal(t, "dh-1", decision["decision_hash"])

	// A follow at an instant already followed is the recorded run: 200, replayed.
	fm.follow.Run.Replayed = true
	res = h.do(http.MethodPost, "/v1/advanced/strategies/installs/"+testMktInstall.String()+"/follows",
		map[string]any{"account_id": testAccountID.String()}, "Idempotency-Key", "follow-key-00002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, true, res.raw()["replayed"])

	// A body that still carries a snapshot carries nothing the service reads.
	res = h.do(http.MethodPost, "/v1/advanced/strategies/installs/"+testMktInstall.String()+"/follows",
		map[string]any{"account_id": testAccountID.String(), "snapshot": testMktSnapshot()}, "Idempotency-Key", "follow-key-00003")
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, MarketplaceFollowRequest{InstallID: testMktInstall, Actor: fm.lastFollow.Actor, CorrelationID: fm.lastFollow.CorrelationID},
		fm.lastFollow, "the port's request has no field a snapshot could reach")

	// The history read: one page, with the same intent and decision.
	res = h.do(http.MethodGet, "/v1/advanced/strategies/follows?account_id="+testAccountID.String()+"&install_id="+testMktInstall.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.NotNil(t, fm.lastInstall)
	assert.Equal(t, testMktInstall, *fm.lastInstall)
	var page api.StrategyFollowRunPage
	res.json(&page)
	assert.Equal(t, api.SIMULATED, page.ValueDomain)
	require.Len(t, page.Items, 1)
	assert.Nil(t, page.Items[0].Replayed, "replayed belongs to the answer to a follow request only")
	require.NotNil(t, page.Items[0].Intents[0].RiskDecision)
	assert.Equal(t, api.ALLOW, page.Items[0].Intents[0].RiskDecision.Verdict)
	require.NotNil(t, page.Items[0].PaperCash)
	assert.Len(t, page.Items[0].Marks, 1)
}

// A refusal of the sandbox budget reaches the client as 429 with its stable
// reason and the time the window will have room.
func TestMarketplace_ASimulationBeyondTheSandboxBudgetIsTooManyRequests(t *testing.T) {
	t.Parallel()
	h, fm := newMarketplaceHarness(t)
	retry := 90 * time.Second
	refusal := errs.New(errs.CodeRateLimited, "sandboxrt: the sandbox budget refuses this: the account has started 240 of its 240 sandbox runs this window").
		WithField("reason", sandboxrt.BudgetWindowRuns)
	refusal.RetryAfter = &retry
	fm.err = refusal
	res := h.do(http.MethodPost, "/v1/advanced/strategies/versions/"+testMktApproved.String()+"/simulations",
		map[string]any{"account_id": testAccountID.String(), "dataset_version": "ds-1", "snapshots": []any{testMktSnapshot()}},
		"Idempotency-Key", "budget-key-00001")
	require.Equal(t, http.StatusTooManyRequests, res.Code, "body=%s", res.Body.String())
	p := res.problem()
	assert.Equal(t, errs.CodeRateLimited, p.Code)
	assert.Equal(t, sandboxrt.BudgetWindowRuns, p.Fields["reason"])
	assert.Equal(t, testAccountID, fm.lastSimulate.Account, "the simulation is charged to the creator's account")
}

func TestMarketplace_InstallPassesTheRequestedModeToTheServiceAndReportsAnExistingInstall(t *testing.T) {
	t.Parallel()
	h, fm := newMarketplaceHarness(t)
	fm.err = errs.New(errs.CodeForbidden, `marketplace: installs are PAPER only; "LIVE" would allocate value the marketplace may never allocate`)
	res := h.do(http.MethodPost, "/v1/advanced/strategies/installs",
		map[string]any{"account_id": testAccountID.String(), "version_id": testMktApproved.String(), "mode": "LIVE"},
		"Idempotency-Key", "install-live-0001")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	p := res.problem()
	assert.Equal(t, errs.CodeForbidden, p.Code)
	assert.Contains(t, p.Detail, "PAPER only")
	assert.Equal(t, gates.ModeLive, fm.lastMode, "the mode is the service's to refuse, not the handler's to rewrite")

	fm.err, fm.existed = nil, true
	res = h.do(http.MethodPost, "/v1/advanced/strategies/installs",
		map[string]any{"account_id": testAccountID.String(), "version_id": testMktApproved.String(), "mode": "PAPER"},
		"Idempotency-Key", "install-paper-001")
	require.Equal(t, http.StatusOK, res.Code, "an existing install is not a new one; body=%s", res.Body.String())
	var in api.StrategyInstall
	res.json(&in)
	assert.Equal(t, api.AdvancedModePAPER, in.Mode)
	assert.Equal(t, api.SIMULATED, in.ValueDomain)
	require.NotNil(t, in.LatestPerformance)
	assert.Equal(t, int64(-125), in.LatestPerformance.ReturnBps)
	require.NotNil(t, in.LatestPerformance.InstallId, "an account's own install is named on its own performance")
}

// The service's dual-control refusal reaches the client with its code; and a
// customer cannot reach an operator decision at all.
func TestMarketplace_ReviewRefusalsReachTheClient(t *testing.T) {
	t.Parallel()
	h, fm := newMarketplaceHarness(t)
	path := "/v1/advanced/strategies/versions/" + testMktDraft.String() + "/approve"
	res := h.do(http.MethodPost, path, map[string]any{"reason_code": "MEETS_POLICY"}, "Idempotency-Key", "approve-key-0001")
	require.Equal(t, http.StatusForbidden, res.Code, "a customer holds no operator permission; body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	assert.Zero(t, fm.callsTo("Approve"))

	op := operatorPrincipal()
	h.as(&op)
	fm.err = errs.New(errs.CodeForbidden, "marketplace: the creator of a strategy cannot review it")
	res = h.do(http.MethodPost, path, map[string]any{"reason_code": "MEETS_POLICY"}, "Idempotency-Key", "approve-key-0002")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	p := res.problem()
	assert.Equal(t, errs.CodeForbidden, p.Code)
	assert.Equal(t, "marketplace: the creator of a strategy cannot review it", p.Detail)
	assert.Equal(t, econ.Actor{Type: security.ActorOperator, ID: testUserID.String()}, fm.lastActor, "the reviewer is the operator principal")

	stale := operatorPrincipal()
	stale.AuthTime = testNow.Add(-2 * stepUpMaxAge)
	h.as(&stale)
	res = h.do(http.MethodPost, path, map[string]any{"reason_code": "MEETS_POLICY"}, "Idempotency-Key", "approve-key-0003")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
}

func TestMarketplace_ReviewQueueDefaultsToTheStatusesAwaitingAnOperator(t *testing.T) {
	t.Parallel()
	h, fm := newMarketplaceHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/strategies/review-queue", nil)
	require.Equal(t, http.StatusForbidden, res.Code, "a customer is not a reviewer; body=%s", res.Body.String())

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, "/v1/advanced/strategies/review-queue", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, []marketplace.Status{marketplace.StatusSubmitted, marketplace.StatusValidated, marketplace.StatusInReview}, fm.lastStatuses)
	var page api.StrategyReviewQueuePage
	res.json(&page)
	require.Len(t, page.Items, 1)
	assert.Equal(t, testAccountID.String(), page.Items[0].CreatorAccountId.String())

	res = h.do(http.MethodGet, "/v1/advanced/strategies/review-queue?status=REJECTED", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, []marketplace.Status{marketplace.StatusRejected}, fm.lastStatuses)
}

func TestMarketplace_ReadsRenderExactFiguresInTheSimulatedDomain(t *testing.T) {
	t.Parallel()
	h, _ := newMarketplaceHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/strategies/listings/"+testMktListing.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	versions := raw["versions"].([]any)
	approved := versions[1].(map[string]any)
	assert.Equal(t, "SIMULATED", approved["value_domain"])
	manifest := approved["manifest"].(map[string]any)
	assert.Equal(t, "1000.00", manifest["max_notional_per_intent"])
	assert.Equal(t, []any{"ACQUIRE_EXPOSURE", "REDUCE_EXPOSURE"}, manifest["allowed_intent_kinds"])
	assert.Equal(t, "5a01", approved["source_digest"])
	fees := approved["fees"].(map[string]any)
	assert.EqualValues(t, 2000, fees["performance_fee_bps"], "basis points are integers")
	assert.EqualValues(t, 100, fees["management_fee_bps"])
	validation := approved["validation"].(map[string]any)
	assert.Equal(t, "PASSED", validation["outcome"])
	assert.Equal(t, "aabb", validation["artifact_digest"])
	assert.Equal(t, "# strategy\n./main.go:3:1: note: built\n", validation["build_log"], "the creator reads what the compiler said")
	provenance := approved["provenance"].(map[string]any)
	assert.Equal(t, "aabb", provenance["artifact_digest"])
	assert.Equal(t, testMktListing.String()+"@1", provenance["strategy_ref"])
	perf := approved["performance"].([]any)[0].(map[string]any)
	assert.Equal(t, "SIMULATED", perf["value_domain"])
	assert.Equal(t, "abcd", perf["history_digest"])
	evaluation := approved["evaluations"].([]any)[0].(map[string]any)
	assert.Equal(t, "SIMULATED", evaluation["value_domain"])
	assert.Equal(t, "ds-2026-10", evaluation["dataset_version"])
	assert.Nil(t, evaluation["runs"], "a listing lists evaluations, not each one's runs")

	res = h.do(http.MethodGet, "/v1/advanced/strategies/runs?account_id="+testAccountID.String()+"&limit=1000", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var runs api.StrategySandboxRunPage
	res.json(&runs)
	require.Len(t, runs.Items, 1)
	r := runs.Items[0]
	assert.Equal(t, api.StrategyRunInvalidOutput, r.Termination)
	assert.Equal(t, api.StrategyRunSourceFollow, r.Source)
	require.Len(t, r.Violations, 1)
	assert.Equal(t, "OUTPUT_RECORD_TYPE_UNKNOWN", r.Violations[0].Code)
	assert.Equal(t, 1, r.Violations[0].RecordSeq)

	res = h.do(http.MethodPost, "/v1/advanced/strategies/versions/"+testMktApproved.String()+"/simulations",
		map[string]any{"account_id": testAccountID.String(), "dataset_version": "ds-1", "snapshots": []any{testMktSnapshot()}},
		"Idempotency-Key", "simulate-key-001")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var ev api.StrategyEvaluation
	res.json(&ev)
	assert.Equal(t, api.SIMULATED, ev.ValueDomain)
	require.NotNil(t, ev.Runs, "the answer to a simulation carries each run")
	assert.Len(t, *ev.Runs, 1)
}

func TestMarketplace_RunsPageIsBounded(t *testing.T) {
	t.Parallel()
	h, fm := newMarketplaceHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/strategies/runs?account_id="+testAccountID.String()+"&limit=200", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, marketplaceMaxRunPage, fm.lastLimit, "each run carries its violations, so a page is bounded tighter")
}

// The builder that stands in where there is no toolchain refuses rather than
// pretends; the paper executor and the risk sections refuse to be wired
// without what they read.
func TestMarketplace_HonestStandInsRefuse(t *testing.T) {
	t.Parallel()
	build, err := UnavailableStrategyBuilder{Reason: "no go command"}.Build(context.Background(), sandboxrt.Source{"main.go": []byte("package main")})
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "no go command")
	assert.Empty(t, build.Wasm)
	assert.Empty(t, build.ArtifactDigest, "no build result is fabricated")

	receipt, err := UnwiredPaperExecutor{}.ExecutePaper(context.Background(), nil, marketplace.PaperExecution{})
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	assert.Empty(t, receipt.ExternalRef, "no fill is reported")

	_, err = NewMarketplacePaperExecutor(MarketplacePaperExecutorConfig{})
	require.Error(t, err, "an executor without its venue would fill nothing honestly")
	_, err = NewMarketplacePaperExecutor(MarketplacePaperExecutorConfig{Intents: &econ.IntentStore{}, Venue: orders.PaperConfig{
		Instruments: instruments.NewRepository(), Assets: assets.NewRepository()}, Schedule: fees.Schedule{Version: "v1"}})
	require.Error(t, err, "nor without the venue's own collaborators")

	_, err = NewMarketplaceRiskSections(MarketplaceSectionsConfig{})
	require.Error(t, err, "risk sections without their sources would vouch for nothing")
}

// A stranger reading a public version sees its validation but not the
// compiler's output, which can quote the source.
func TestMarketplace_TheBuildLogIsTheCreatorsToRead(t *testing.T) {
	t.Parallel()
	h, _ := newMarketplaceHarness(t)
	stranger := marketplaceStranger()
	h.as(&stranger)
	res := h.do(http.MethodGet, "/v1/advanced/strategies/listings/"+testMktListing.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	versions := res.raw()["versions"].([]any)
	require.Len(t, versions, 1, "the public version only")
	validation := versions[0].(map[string]any)["validation"].(map[string]any)
	assert.Equal(t, "PASSED", validation["outcome"])
	_, shown := validation["build_log"]
	assert.False(t, shown)

	res = h.do(http.MethodGet, "/v1/advanced/strategies/installs?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	install := res.raw()["items"].([]any)[0].(map[string]any)
	assert.Equal(t, "10000.00", install["paper_capital"], "an install names the paper capital its book started with")
}
