//go:build integration

package httpapi

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// itestMomentum is a well-behaved strategy: for every instrument it compares a
// short and a long moving average of the closes and proposes buying when the
// short one is above the long one, logging its signal.
const itestMomentum = `package main

import (
	nodalsdk "nodal.dev/sdk"
)

func main() {
	nodalsdk.Run(func(s *nodalsdk.Session) error {
		units, ok := s.Config("order_units")
		if !ok {
			units = "1000000"
		}
		for _, ins := range s.Instruments() {
			if len(ins.Closes) < 5 {
				continue
			}
			short, err := nodalsdk.SMA(ins.Closes, 3, 6, nodalsdk.RoundHalfEven)
			if err != nil {
				return err
			}
			long, err := nodalsdk.SMA(ins.Closes, 5, 6, nodalsdk.RoundHalfEven)
			if err != nil {
				return err
			}
			signal := "flat"
			if short.Cmp(long) > 0 {
				signal = "long"
				if err := s.EmitIntent("ACQUIRE_EXPOSURE", ins.ID, "BUY", units, ""); err != nil {
					return err
				}
			}
			if err := s.Log("signal", map[string]string{
				"instrument": ins.ID, "short": short.String(), "long": long.String(), "signal": signal,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}
`

// marketplaceItest is the world of one marketplace integration test: the real
// services over the real database, wired by Wire exactly as cmd/api wires them.
type marketplaceItest struct {
	t          *testing.T
	d          *db.DB
	h          *harness
	prices     *valuation.PriceStore
	toolchain  bool
	instrument string
	base, quot assets.AssetID
}

func newMarketplaceItest(t *testing.T) *marketplaceItest {
	t.Helper()
	d := openTestDB(t)
	ctx := t.Context()
	clk := clock.System()
	outbox := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("httpapi-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return false }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	auth := econ.NewAuthorizer(checker, kills)
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	runner, err := sandboxrt.NewRunner(context.Background(), sandboxrt.RunnerConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.Close(context.Background()) })
	sandbox := sandboxrt.NewService(clk, auth, runner, aw)

	w := &marketplaceItest{t: t, d: d, prices: valuation.NewPriceStore(clk)}
	// A real build when the toolchain is here, as cmd/api decides; the
	// refusing builder otherwise.
	var builder marketplace.ArtifactBuilder = UnavailableStrategyBuilder{Reason: "no go command on PATH in this test environment"}
	if goBin, lookErr := exec.LookPath("go"); lookErr == nil {
		goBin, err = filepath.Abs(goBin)
		require.NoError(t, err)
		cache := t.TempDir()
		if base, cacheErr := os.UserCacheDir(); cacheErr == nil {
			cache = filepath.Join(base, "nodal", "strategy-gocache")
			require.NoError(t, os.MkdirAll(cache, 0o700))
		}
		b, err := sandboxrt.NewBuilder(sandboxrt.BuilderConfig{GoBinary: goBin, CacheDir: cache, TempDir: t.TempDir(), Timeout: 10 * time.Minute})
		require.NoError(t, err)
		builder, w.toolchain = b, true
	}
	sections, err := NewMarketplaceRiskSections(MarketplaceSectionsConfig{
		Clock: clk, Accounts: accounts.NewRepository(), Instruments: instruments.NewRepository(),
		Identity: compliance.NewAccountVerification(clk), Prices: w.prices, KillSwitches: kills, MaxPriceAge: 5 * time.Minute,
	})
	require.NoError(t, err)
	svc, err := marketplace.NewService(marketplace.Config{
		Clock: clk, Intents: econ.NewIntentStore(clk, outbox, aw), Effects: econ.NewEffectStore(clk, outbox, aw), Auth: auth,
		Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Sandbox: sandbox, Builder: builder,
		Sections: sections, Executor: UnwiredPaperExecutor{}, Audit: aw,
	})
	require.NoError(t, err)
	ports, err := Wire(WireDeps{DB: d, Clock: clk, Env: "TEST", Instruments: instruments.NewRepository(),
		Advanced: AdvancedDeps{Marketplace: MarketplaceDeps{Service: svc, Sandbox: sandbox}}})
	require.NoError(t, err)
	require.NotNil(t, ports.Marketplace, "the marketplace and its sandbox wire the port")

	userID, acct := seedAccount(t, d)
	w.h = newIntegrationHarness(t, d, acct, userID)
	w.h.server.opts.Ports.Marketplace = ports.Marketplace

	// One simulated spot pair the strategies trade.
	repo := assets.NewRepository()
	mk := func(symbol string) assets.AssetID {
		a, err := repo.Create(ctx, d, assets.Asset{Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
			Symbol: symbol, Name: symbol + " (sandbox)", Decimals: 6, RiskClass: assets.RiskSettlement, Status: assets.StatusActive})
		require.NoError(t, err)
		return a.ID
	}
	suffix := uuid.NewString()[:8]
	w.quot, w.base = mk("HTQ"+suffix), mk("HTB"+suffix)
	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		ins, err := instruments.NewRepository().CreateSpotPair(ctx, tx, instruments.SpotPairSpec{Base: w.base, Quote: w.quot, Settlement: w.quot,
			CanonicalName: "HTB" + suffix + "/HTQ" + suffix, RiskClass: assets.RiskMajor, Status: assets.StatusActive,
			ActiveFrom: time.Now().Add(-time.Hour)})
		w.instrument = ins.ID.String()
		return err
	}))
	return w
}

// customer is a customer principal owning a new account; verified gives its
// owner a VERIFIED compliance profile.
func (w *marketplaceItest) customer(verified bool) (security.Principal, accounts.AccountID) {
	w.t.Helper()
	userID, acct := seedAccount(w.t, w.d)
	if verified {
		_, err := w.d.Pool().Exec(w.t.Context(), `INSERT INTO compliance_profiles (user_id, identity_state, verified_at) VALUES ($1, 'VERIFIED', now())`, userID)
		require.NoError(w.t, err)
	}
	return security.Principal{
		SubjectID: userID.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{acct.String()}, SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}, acct
}

// operator is an operator principal with a fresh step-up; with an account it
// also owns that customer account (an ADMIN holds the customer surface too).
func (w *marketplaceItest) operator(role security.Role, acct *accounts.AccountID) security.Principal {
	p := security.Principal{
		SubjectID: uuid.NewString(), ActorType: security.ActorOperator, Roles: []security.Role{role},
		SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
	if acct != nil {
		p.AccountIDs = []string{acct.String()}
	}
	return p
}

func (w *marketplaceItest) as(p security.Principal) *harness {
	w.h.as(&p)
	return w.h
}

func (w *marketplaceItest) versionBody(acct accounts.AccountID, title string) map[string]any {
	return map[string]any{
		"account_id": acct.String(), "title": title,
		"version": map[string]any{
			"description": "buys strength when the short average is above the long one", "risk_class": "MODERATE",
			"source_commit": "itest-momentum", "supported_instrument_ids": []string{w.instrument},
			"manifest": map[string]any{
				"allowed_intent_kinds": []string{"ACQUIRE_EXPOSURE", "REDUCE_EXPOSURE"}, "allowed_instrument_ids": []string{w.instrument},
				"max_notional_per_intent": "1000.00", "max_intents_per_run": 2, "min_interval_seconds": 60,
			},
			"source":                map[string]string{"main.go": itestMomentum},
			"backtest_evidence_ref": "evidence://itest/momentum", "data_dependencies": []string{"closes", "marks"},
			"fees":   map[string]any{"performance_fee_bps": 2000, "management_fee_bps": 100, "high_water_mark": true},
			"config": map[string]string{"order_units": "1000000"},
		},
	}
}

// snapshot is a market in which the instrument trends up, so momentum buys
// one whole unit at the mark.
func (w *marketplaceItest) snapshot(asOf time.Time) map[string]any {
	return map[string]any{
		"as_of": asOf.UTC().Format(time.RFC3339Nano),
		"instruments": []map[string]any{{
			"instrument_id": w.instrument, "kind": "SPOT_PAIR", "base_decimals": 6, "quote_decimals": 6,
			"mark": "150.25", "best_bid": "150.20", "best_ask": "150.30",
			"closes": []string{"140.00", "142.50", "145.00", "147.50", "150.00", "152.50"},
		}},
		"portfolio": map[string]any{"balance": "10000.00", "positions": []any{}},
	}
}

// TestIntegration_MarketplaceLifecycleThroughHTTP drives the marketplace
// through HTTP over the real adapter and database: a creator publishes and
// submits; the creator cannot approve it -- a customer is refused at the
// route, and an operator who created a listing is refused by the service's
// dual control; an operator validates (a real build when the toolchain is
// here), reviews and approves; a second customer installs it in PAPER and
// paper-follows; the intents the follow produced and their Risk Kernel V2
// decisions are listed; and a stranger cannot read another account's installs.
func TestIntegration_MarketplaceLifecycleThroughHTTP(t *testing.T) {
	w := newMarketplaceItest(t)
	creator, creatorAcct := w.customer(false)

	// --- the creator publishes and submits --------------------------------
	res := w.as(creator).do(http.MethodPost, "/v1/advanced/strategies/listings", w.versionBody(creatorAcct, "Momentum"),
		"Idempotency-Key", "itest-create-"+uuid.NewString())
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var created api.StrategyListingDetail
	res.json(&created)
	require.Len(t, created.Versions, 1)
	v1 := created.Versions[0]
	assert.Equal(t, api.StrategyStatusDraft, v1.Status)
	assert.Equal(t, "1000.00", v1.Manifest.MaxNotionalPerIntent)
	assert.Equal(t, api.SIMULATED, created.ValueDomain)
	require.NotNil(t, created.CreatorAccountId)
	assert.Equal(t, creatorAcct.String(), created.CreatorAccountId.String())
	listingPath := "/v1/advanced/strategies/listings/" + created.Id.String()
	versionPath := "/v1/advanced/strategies/versions/" + v1.Id.String()

	// Nobody else sees a DRAFT: the listing is NOT_FOUND to a stranger.
	follower, followerAcct := w.customer(true)
	res = w.as(follower).do(http.MethodGet, listingPath, nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())

	res = w.as(creator).do(http.MethodPost, versionPath+"/submit", map[string]any{"account_id": creatorAcct.String()},
		"Idempotency-Key", "itest-submit-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var submitted api.StrategyVersion
	res.json(&submitted)
	assert.Equal(t, api.StrategyStatusSubmitted, submitted.Status)

	// --- the creator cannot approve their own strategy ---------------------
	res = w.as(creator).do(http.MethodPost, versionPath+"/approve", map[string]any{"reason_code": "SELF_APPROVAL"},
		"Idempotency-Key", "itest-self-approve-"+uuid.NewString())
	require.Equal(t, http.StatusForbidden, res.Code, "a customer holds no reviewer permission; body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)

	// An operator who is also a creator is refused by the service itself.
	_, adminAcct := w.customer(false)
	adminCreator := w.operator(security.RoleAdmin, &adminAcct)
	res = w.as(adminCreator).do(http.MethodPost, "/v1/advanced/strategies/listings", w.versionBody(adminAcct, "Operator's own"),
		"Idempotency-Key", "itest-admin-create-"+uuid.NewString())
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var own api.StrategyListingDetail
	res.json(&own)
	ownVersion := "/v1/advanced/strategies/versions/" + own.Versions[0].Id.String()
	res = w.as(adminCreator).do(http.MethodPost, ownVersion+"/submit", map[string]any{"account_id": adminAcct.String()},
		"Idempotency-Key", "itest-admin-submit-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	for _, step := range []struct {
		path string
		body any
	}{{ownVersion + "/review", nil}, {ownVersion + "/approve", map[string]any{"reason_code": "SELF_APPROVAL"}}} {
		res = w.as(adminCreator).do(http.MethodPost, step.path, step.body, "Idempotency-Key", "itest-self-review-"+uuid.NewString())
		require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", step.path, res.Body.String())
		p := res.problem()
		assert.Equal(t, errs.CodeForbidden, p.Code)
		assert.Equal(t, "marketplace: the creator of a strategy cannot review it", p.Detail, "the service's dual control reaches the client")
	}

	// --- an operator validates, reviews and approves ------------------------
	reviewer := w.operator(security.RoleOperations, nil)
	res = w.as(reviewer).do(http.MethodPost, versionPath+"/validate", nil, "Idempotency-Key", "itest-validate-"+uuid.NewString())
	if !w.toolchain {
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
		t.Log("no Go toolchain in this environment: validation answered UNSUPPORTED; the rest of the lifecycle needs a build")
		return
	}
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var validated api.StrategyValidationResult
	res.json(&validated)
	assert.Equal(t, api.StrategyStatusValidated, validated.Version.Status)
	assert.Equal(t, api.StrategyValidationPassed, validated.Validation.Outcome)
	require.NotNil(t, validated.Validation.ArtifactDigest)
	assert.Len(t, *validated.Validation.ArtifactDigest, 64, "the artifact is named by the sha256 of its bytes")
	assert.Empty(t, validated.Validation.ScanCodes)

	// The review queue shows it to the operator, oldest first.
	res = w.as(reviewer).do(http.MethodGet, "/v1/advanced/strategies/review-queue?status=VALIDATED&limit=200", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var queue api.StrategyReviewQueuePage
	res.json(&queue)
	inQueue := false
	for _, it := range queue.Items {
		inQueue = inQueue || it.Version.Id == v1.Id
	}
	assert.True(t, inQueue, "the validated version waits in the review queue")

	res = w.as(reviewer).do(http.MethodPost, versionPath+"/review", nil, "Idempotency-Key", "itest-review-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	approveKey := "itest-approve-" + uuid.NewString()
	approveBody := map[string]any{"reason_code": "MEETS_POLICY", "note": "manifest is narrow; momentum only"}
	res = w.as(reviewer).do(http.MethodPost, versionPath+"/approve", approveBody, "Idempotency-Key", approveKey)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var approved api.StrategyVersion
	res.json(&approved)
	assert.Equal(t, api.StrategyStatusApproved, approved.Status)
	assert.Equal(t, api.StrategyModerationCleared, approved.Moderation)
	replay := w.as(reviewer).do(http.MethodPost, versionPath+"/approve", approveBody, "Idempotency-Key", approveKey)
	require.Equal(t, http.StatusOK, replay.Code, "body=%s", replay.Body.String())
	assert.JSONEq(t, res.Body.String(), replay.Body.String(), "an approval replays; it is not decided twice")

	// --- it is public now ---------------------------------------------------
	res = w.as(follower).do(http.MethodGet, "/v1/advanced/strategies/catalog?limit=200", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var catalog api.StrategyListingPage
	res.json(&catalog)
	listed := false
	for _, it := range catalog.Items {
		if it.Id == created.Id {
			listed = true
			assert.Nil(t, it.CreatorAccountId, "the creator's account is not handed to another customer")
			assert.Equal(t, api.StrategyStatusApproved, it.LatestVersion.Status)
		}
	}
	require.True(t, listed, "the approved listing is in the catalogue")

	res = w.as(follower).do(http.MethodGet, listingPath, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var public api.StrategyListingDetail
	res.json(&public)
	require.Len(t, public.Versions, 1)
	pv := public.Versions[0]
	require.NotNil(t, pv.Validation)
	assert.Equal(t, api.StrategyValidationPassed, pv.Validation.Outcome)
	require.Len(t, pv.Reviews, 1)
	assert.Equal(t, api.StrategyReviewApprove, pv.Reviews[0].Decision)
	assert.Equal(t, "MEETS_POLICY", pv.Reviews[0].ReasonCode)
	assert.Nil(t, pv.Reviews[0].ReviewerActorId)
	assert.Equal(t, *validated.Validation.ArtifactDigest, *pv.Provenance.ArtifactDigest, "provenance names the built artifact")
	assert.Equal(t, created.Id.String()+"@1", pv.Provenance.StrategyRef)

	// --- a second customer installs in PAPER and paper-follows -------------
	res = w.as(follower).do(http.MethodPost, "/v1/advanced/strategies/installs",
		map[string]any{"account_id": followerAcct.String(), "version_id": v1.Id.String(), "mode": "LIVE"},
		"Idempotency-Key", "itest-install-live-"+uuid.NewString())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.problem().Detail, "PAPER only", "the service refuses any mode but PAPER, and says so")

	res = w.as(follower).do(http.MethodPost, "/v1/advanced/strategies/installs",
		map[string]any{"account_id": followerAcct.String(), "version_id": v1.Id.String(), "mode": "PAPER"},
		"Idempotency-Key", "itest-install-"+uuid.NewString())
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var install api.StrategyInstall
	res.json(&install)
	assert.Equal(t, api.AdvancedModePAPER, install.Mode)
	assert.Equal(t, api.StrategyInstallActive, install.Status)
	installPath := "/v1/advanced/strategies/installs/" + install.Id.String()

	// A fresh reference price for the pair, as the risk kernel's oracle
	// section requires for a risk-increasing intent.
	_, err := w.prices.RecordPrice(t.Context(), w.d, valuation.PriceObservation{AssetID: w.base, QuoteAssetID: w.quot,
		Mantissa: money.QuantityFromInt64(15025), Scale: 2, Source: "itest-mark", ObservedAt: time.Now().UTC().Add(-time.Second)})
	require.NoError(t, err)

	asOf := time.Now().UTC().Add(-2 * time.Second)
	followKey := "itest-follow-" + uuid.NewString()
	followBody := map[string]any{"account_id": followerAcct.String(), "snapshot": w.snapshot(asOf)}
	res = w.as(follower).do(http.MethodPost, installPath+"/follows", followBody, "Idempotency-Key", followKey)
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var run api.StrategyFollowRun
	res.json(&run)
	assert.Equal(t, api.SIMULATED, run.ValueDomain)
	assert.Equal(t, api.StrategyRunOk, run.Termination)
	assert.Equal(t, "10000.00", run.Equity, "equity is the exact value of the snapshot the run saw")
	assert.Zero(t, run.ViolationsCount)
	require.Len(t, run.Intents, 1, "momentum proposes one buy, which the manifest permits")
	intent := run.Intents[0]
	assert.Equal(t, "ACQUIRE_EXPOSURE", intent.Kind)
	assert.Regexp(t, `^150\.25(0*)$`, intent.Notional, "one whole unit at the mark, exactly, at the scale the service computed")
	require.NotNil(t, intent.RiskDecision, "every intent a strategy proposes faces Risk Kernel V2")
	assert.Equal(t, api.ALLOW, intent.RiskDecision.Verdict,
		"sections read from the database: an active account, a verified owner, an open pair and a fresh price; reasons=%v",
		intent.RiskDecision.ReasonCodes)
	// No paper venue executes economic intents in this build: the allowed
	// intent is handed to execution, which refuses, and the record says so.
	assert.Equal(t, api.StrategyOutcomeDispatchFailed, intent.Outcome)
	require.NotNil(t, intent.EffectStatus)
	assert.Equal(t, "FAILED", *intent.EffectStatus)
	require.NotNil(t, intent.EffectFailureCode)
	assert.Equal(t, "UNSUPPORTED", *intent.EffectFailureCode)

	// The same key replays the answer; the same snapshot under a new key is the
	// recorded run, and nothing runs again.
	replay = w.as(follower).do(http.MethodPost, installPath+"/follows", followBody, "Idempotency-Key", followKey)
	require.Equal(t, http.StatusOK, replay.Code, "a replay returns the recorded answer; body=%s", replay.Body.String())
	assert.JSONEq(t, res.Body.String(), replay.Body.String())
	again := w.as(follower).do(http.MethodPost, installPath+"/follows", followBody, "Idempotency-Key", "itest-follow-"+uuid.NewString())
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	var recorded api.StrategyFollowRun
	again.json(&recorded)
	require.NotNil(t, recorded.Replayed)
	assert.True(t, *recorded.Replayed)
	assert.Equal(t, run.Id, recorded.Id)

	// --- the follower's reads observe it -------------------------------------
	res = w.as(follower).do(http.MethodGet, "/v1/advanced/strategies/follows?account_id="+followerAcct.String()+"&install_id="+install.Id.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var follows api.StrategyFollowRunPage
	res.json(&follows)
	require.Len(t, follows.Items, 1)
	listedRun := follows.Items[0]
	assert.Equal(t, run.Id, listedRun.Id)
	require.Len(t, listedRun.Intents, 1)
	require.NotNil(t, listedRun.Intents[0].RiskDecision)
	assert.Equal(t, intent.RiskDecision.DecisionHash, listedRun.Intents[0].RiskDecision.DecisionHash)
	assert.Equal(t, intent.IntentId, *listedRun.Intents[0].RiskDecision.IntentId)

	res = w.as(follower).do(http.MethodGet, "/v1/advanced/strategies/runs?account_id="+followerAcct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var runs api.StrategySandboxRunPage
	res.json(&runs)
	require.Len(t, runs.Items, 1)
	assert.Equal(t, run.SandboxRunId, runs.Items[0].Id)
	assert.Equal(t, api.StrategyRunOk, runs.Items[0].Termination)
	assert.Equal(t, api.StrategyRunSourceFollow, runs.Items[0].Source)
	assert.True(t, runs.Items[0].ArtifactVerified)
	assert.Empty(t, runs.Items[0].Violations)

	res = w.as(follower).do(http.MethodPost, installPath+"/performance", map[string]any{"account_id": followerAcct.String()},
		"Idempotency-Key", "itest-performance-"+uuid.NewString())
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var perf api.StrategyPerformanceSnapshot
	res.json(&perf)
	assert.Equal(t, 1, perf.FollowRunsCount)
	assert.Equal(t, int64(0), perf.ReturnBps, "one point has no return")

	res = w.as(follower).do(http.MethodGet, "/v1/advanced/strategies/installs?account_id="+followerAcct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var installs api.StrategyInstallPage
	res.json(&installs)
	require.Len(t, installs.Items, 1)
	require.NotNil(t, installs.Items[0].LatestPerformance)
	assert.Equal(t, perf.HistoryDigest, installs.Items[0].LatestPerformance.HistoryDigest)

	// The listing now carries the paper performance of an install, unnamed.
	res = w.as(creator).do(http.MethodGet, listingPath, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var withPerf api.StrategyListingDetail
	res.json(&withPerf)
	require.Len(t, withPerf.Versions[0].Performance, 1)
	assert.Nil(t, withPerf.Versions[0].Performance[0].InstallId)

	// --- a stranger cannot read or act on another account's installs ---------
	res = w.as(creator).do(http.MethodGet, "/v1/advanced/strategies/installs?account_id="+followerAcct.String(), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	res = w.as(creator).do(http.MethodGet, "/v1/advanced/strategies/follows?account_id="+followerAcct.String(), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	res = w.as(creator).do(http.MethodPost, installPath+"/follows",
		map[string]any{"account_id": creatorAcct.String(), "snapshot": w.snapshot(time.Now().UTC().Add(-time.Second))},
		"Idempotency-Key", "itest-stranger-follow-"+uuid.NewString())
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)

	// The creator's own install list is theirs alone and empty.
	res = w.as(creator).do(http.MethodGet, "/v1/advanced/strategies/installs?account_id="+creatorAcct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var none api.StrategyInstallPage
	res.json(&none)
	assert.Empty(t, none.Items)
}
