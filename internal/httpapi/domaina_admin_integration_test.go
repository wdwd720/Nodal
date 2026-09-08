//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The operator's hands on the internal economy, proved over HTTP against the
// real admin service, the real domain services and a real database
// (gola.md Stage 17).
//
// The property under test is the asymmetry: STOPPING the internal economy is
// one operator's decision, RESTARTING it takes two. Everything else here
// exists to make that assertion mean something — a halt that does not halt, or
// a resume that a single operator can push through, would both pass a test
// that only checked status codes.

type domainAHarness struct {
	*harness
	db  *db.DB
	clk *clock.Fake

	nativeAssets  *nativeasset.Service
	nativeMarkets *nativemarket.Service
	commerce      *commerce.Service
	credits       *credit.Service

	creator accounts.AccountID
	buyer   accounts.AccountID
	asset   nativeasset.Asset
	market  nativemarket.Market
}

// seedGlobalRiskPolicy records the GLOBAL risk policy these tests evaluate
// against, once for the whole package.
//
// It records the compiled-in default -- what `go run ./scripts/riskpolicy`
// gives a fresh deployment -- rather than a relaxed fixture policy, because a
// fixture that loosens the limits it is meant to exercise is the test
// equivalent of turning the control off.
func seedGlobalRiskPolicy(t *testing.T, d *db.DB, at time.Time) {
	t.Helper()
	globalRiskPolicy.Do(func() {
		ctx := security.WithPrincipal(context.Background(), security.Principal{
			SubjectID: "domaina-itest", ActorType: security.ActorSystem, AuthTime: at,
		})
		if _, _, err := risk.NewStore().EffectivePolicy(ctx, d, "", "", at); err == nil {
			return // an earlier run against this database already recorded it
		}
		require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := risk.NewStore().RecordPolicy(ctx, tx, risk.PolicyRecord{
					Scope:       risk.ScopeGlobal,
					Version:     "domaina-itest-global",
					Rules:       json.RawMessage(risk.DefaultGlobalPolicyJSON),
					EffectiveAt: at.Add(-time.Hour),
					ActorType:   security.ActorSystem,
					ActorID:     "domaina-itest",
					Reason:      "the compiled-in default limits, as a fresh deployment gets them",
				})
				return err
			}))
	})
}

var globalRiskPolicy sync.Once

func newDomainAHarness(t *testing.T, d *db.DB) *domainAHarness {
	t.Helper()
	clk := clock.NewFake(testNow)
	led := ledger.NewService(clk, "domaina-admin-itest")
	led.SetCapabilityResolver(commerceCaps{valuedomain.CapNativeMarketTrading: true})
	credits := credit.NewService(led, clk)

	assetSvc := nativeasset.NewService(clk, nil)
	seedGlobalRiskPolicy(t, d, testNow)
	marketSvc := nativemarket.NewService(led, credits, valuation.NewPriceStore(clk), audit.NewWriter(),
		instruments.NewRepository(), nativemarket.NewRiskGate(risk.NewStore(), clk), clk)
	commerceSvc := commerce.NewService(led, credits, audit.NewWriter(), clk)
	commerceSvc.SetCapabilityResolver(commerceCaps{commerce.CapMarketplace: true})

	adminSvc := admin.NewService(clk, audit.NewWriter())

	fx := newFixtures()
	ports := fx.ports()
	ports.AdminActions = adminActionsAdapter{
		svc: adminSvc, rm: NewReadModel(d), db: d, q: d,
		executors: DomainAExecutors(DomainAExecutorDeps{
			NativeAssets:  assetSvc,
			NativeMarkets: marketSvc,
			Commerce:      commerceSvc,
			Credits:       credits,
		}),
	}

	// The customer-facing Domain A ports, so a test can make a real request
	// over HTTP and not only drive the admin plane. Without them every customer
	// route on this harness answers UNSUPPORTED, which reads as a refusal and
	// is not one -- and a test asserting a refusal would pass for the wrong
	// reason. That is how F-37 stayed hidden: no test drove these routes.
	payoutSvc := payout.NewService(led, credits, payout.NewEngine(credits), payout.NewRegistry(true), clk)
	economy := NativeEconomyDeps{
		NativeAssets: assetSvc,
		Payouts:      payoutSvc,
		LegalRouter:  mustRouter(t, legalrouter.DevelopmentPolicy()),
		Capabilities: commerceCaps{valuedomain.CapNativeMarketTrading: true},
		Verification: verifiedAt(valuedomain.VerificationNodalIdentity),
		Jurisdiction: fixedJurisdiction("US-CA"),
		Clock:        clk,
	}
	ports.NativeAssets = nativeAssetsAdapter{deps: economy, db: d}
	ports.Payouts = payoutsAdapter{deps: economy, db: d, clk: clk}
	ports.NativeMarkets = nativeMarketsAdapter{db: d, deps: NativeEconomyDeps{
		NativeMarkets: marketSvc,
		LegalRouter:   mustRouter(t, legalrouter.DevelopmentPolicy()),
		Capabilities:  commerceCaps{valuedomain.CapNativeMarketTrading: true},
		Verification:  verifiedAt(valuedomain.VerificationNodalIdentity),
		Jurisdiction:  fixedJurisdiction("US-CA"),
		Clock:         clk,
	}}

	h := &harness{t: t, ports: fx}
	srv, err := New(Options{
		Env: config.EnvTest, Clock: clk, CookieName: "cp_session", SessionTTL: time.Hour,
		Authenticator: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if h.princip == nil {
					next.ServeHTTP(w, r)
					return
				}
				next.ServeHTTP(w, r.WithContext(security.WithPrincipal(r.Context(), *h.princip)))
			})
		},
		Ports: ports,
	})
	require.NoError(t, err)
	h.server = srv

	dh := &domainAHarness{
		harness: h, db: d, clk: clk,
		nativeAssets: assetSvc, nativeMarkets: marketSvc, commerce: commerceSvc, credits: credits,
		creator: seedCustomerAccount(t, d), buyer: seedCustomerAccount(t, d),
	}
	dh.launchMarket(t, commerceCreditAsset(t, d))
	return dh
}

func seedCustomerAccount(t *testing.T, d *db.DB) accounts.AccountID {
	t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(t.Context(), d, "domaina-admin-itest", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	a, err := repo.CreateAccount(t.Context(), d, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	return a.ID
}

func qq(s string) money.Quantity {
	q, err := money.ParseQuantity(s)
	if err != nil {
		panic(err)
	}
	return q
}

// launchMarket takes an asset all the way to a live market, which is the state
// every control below acts on.
func (h *domainAHarness) launchMarket(t *testing.T, creditAsset assets.AssetID) {
	t.Helper()
	// The TAIL of a UUIDv7, not the head: the head is time-ordered, so two
	// assets created in the same millisecond would collide on the symbol and
	// the registry would refuse the second with SYMBOL_TAKEN.
	raw := strings.ReplaceAll(id.New[id.Any]().String(), "-", "")
	suffix := raw[len(raw)-6:]
	require.NoError(t, h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			a, _, err := h.nativeAssets.CreateDraft(ctx, tx, nativeasset.CreateRequest{
				CreatorAccountID: h.creator,
				Name:             "Adminable " + suffix,
				Symbol:           "AD" + suffix[:4],
				Description:      "a test asset for the admin workflows",
				Supply: nativeasset.SupplyModel{
					MaxSupply:         qq("1000000000000000"),
					CreatorAllocation: qq("100000000000000"),
				},
			})
			if err != nil {
				return err
			}
			h.asset = a
			if _, err := h.nativeAssets.SetStatus(ctx, tx, a.AssetID, nativeasset.StatusPendingReview, "submitted"); err != nil {
				return err
			}
			if _, err := h.nativeAssets.SetModeration(ctx, tx, a.AssetID, nativeasset.ModerationApproved, "fixture"); err != nil {
				return err
			}
			if _, err := h.nativeAssets.Activate(ctx, tx, a.AssetID, "fixture"); err != nil {
				return err
			}
			m, err := h.nativeMarkets.Create(ctx, tx, nativemarket.CreateRequest{
				AssetID: a.AssetID, CreditAssetID: creditAsset, CreatorID: h.creator,
				PoolSupply: a.Supply.PoolSupply(), CreatorAllocation: a.Supply.CreatorAllocation,
				VirtualCreditReserve: qq("30000000000"),
				Fees:                 nativemarket.Fees{PlatformBPS: 100, CreatorBPS: 50},
				IdempotencyKey:       "mint-" + suffix,
				EffectiveAt:          h.clk.Now(),
			})
			if err != nil {
				return err
			}
			m, err = h.nativeMarkets.SetStatus(ctx, tx, m.ID, nativemarket.StatusActive, "fixture")
			if err != nil {
				return err
			}
			h.market = m
			return nil
		}))
}

func (h *domainAHarness) marketStatus(t *testing.T) nativemarket.Status {
	t.Helper()
	m, err := h.nativeMarkets.Market(t.Context(), h.db, h.market.ID)
	require.NoError(t, err)
	return m.Status
}

// execute runs an approved action over HTTP.
func execute(h *harness, actionID string) *response {
	return h.do(http.MethodPost, "/v1/admin/actions/"+actionID+"/execute",
		map[string]any{}, "Idempotency-Key", newKey())
}

// proposeWithParams is propose() with a params body, which the moderation and
// payout kinds need.
func proposeWithParams(t *testing.T, h *harness, kind admin.Kind, targetType, targetID string, params any) wireAction {
	t.Helper()
	body := map[string]any{
		"kind": string(kind), "target_type": targetType, "target_id": targetID,
		"reason": "stage 17 operator workflow verification",
	}
	if params != nil {
		body["params"] = params
	}
	res := h.do(http.MethodPost, "/v1/admin/actions", body, "Idempotency-Key", newKey())
	require.Equal(t, http.StatusCreated, res.Code, "propose failed; body=%s", res.Body.String())
	var a wireAction
	res.json(&a)
	return a
}

// ---------------------------------------------------------------------------
// Stopping: one operator
// ---------------------------------------------------------------------------

// TestIntegration_OneOperatorCanHaltAMarket. A control that needs two
// signatures to stop an incident is a control nobody reaches for at 3am.
func TestIntegration_OneOperatorCanHaltAMarket(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	ops := seedOperator(t, d, security.RoleOperations)

	require.Equal(t, nativemarket.StatusActive, h.marketStatus(t))

	action := proposeWithParams(t, h.as(&ops), admin.KindNativeMarketHalt,
		"native_market", h.market.ID.String(), nil)
	res := execute(h.as(&ops), action.ID)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	require.Equal(t, nativemarket.StatusHalted, h.marketStatus(t),
		"the halt must actually halt, not merely record an approval")

	// And a trade is refused now, which is what halting is FOR.
	err := h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, qerr := h.nativeMarkets.Quote(ctx, tx, nativemarket.QuoteRequest{
				MarketID: h.market.ID, AccountID: h.buyer,
				Side: nativemarket.Buy, Amount: qq("1000000"),
			})
			return qerr
		})
	require.Error(t, err, "a halted market must not quote")
}

// TestIntegration_EveryStoppingControlIsOneOperator walks the whole set.
func TestIntegration_EveryStoppingControlIsOneOperator(t *testing.T) {
	for _, tc := range []struct {
		kind admin.Kind
		want nativemarket.Status
	}{
		{admin.KindNativeMarketCloseOnly, nativemarket.StatusCloseOnly},
		{admin.KindNativeMarketHalt, nativemarket.StatusHalted},
		{admin.KindNativeMarketFreeze, nativemarket.StatusFrozen},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			d := openTestDB(t)
			h := newDomainAHarness(t, d)
			ops := seedOperator(t, d, security.RoleOperations)

			action := proposeWithParams(t, h.as(&ops), tc.kind, "native_market", h.market.ID.String(), nil)
			res := execute(h.as(&ops), action.ID)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			require.Equal(t, tc.want, h.marketStatus(t))
		})
	}
}

// ---------------------------------------------------------------------------
// Restarting: two
// ---------------------------------------------------------------------------

// TestIntegration_ResumingAMarketTakesTwoPeople. Restarting is the direction
// that adds exposure, so that is where the second pair of eyes belongs — and
// the approve-side permission is held by no standing role, so an operator
// cannot do it alone however senior they are.
func TestIntegration_ResumingAMarketTakesTwoPeople(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	ops := seedOperator(t, d, security.RoleOperations)

	halt := proposeWithParams(t, h.as(&ops), admin.KindNativeMarketHalt,
		"native_market", h.market.ID.String(), nil)
	require.Equal(t, http.StatusOK, execute(h.as(&ops), halt.ID).Code)
	require.Equal(t, nativemarket.StatusHalted, h.marketStatus(t))

	// One operator proposes a resume and cannot execute it: it is not approved.
	resume := proposeWithParams(t, h.as(&ops), admin.KindNativeMarketResume,
		"native_market", h.market.ID.String(), nil)
	res := execute(h.as(&ops), resume.ID)
	require.NotEqual(t, http.StatusOK, res.Code, "an unapproved resume must not execute; body=%s", res.Body.String())
	require.Equal(t, nativemarket.StatusHalted, h.marketStatus(t))

	// The proposer cannot approve their own resume, even holding a live
	// break-glass elevation — which is the only way anybody holds
	// native_market:resume at all.
	self := elevate(ops, testNow.Add(time.Hour))
	res = decide(h.as(&self), resume.ID, "approve", "approving my own")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	require.Equal(t, nativemarket.StatusHalted, h.marketStatus(t))

	// A DIFFERENT elevated principal can.
	approver := elevate(seedOperator(t, d, security.RoleOperations), testNow.Add(time.Hour))
	res = decide(h.as(&approver), resume.ID, "approve", "second pair of eyes")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	res = execute(h.as(&ops), resume.ID)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.Equal(t, nativemarket.StatusActive, h.marketStatus(t))
}

// TestIntegration_ApprovalDoesNotOverrideTheMarketsOwnTransitionTable. FROZEN
// cannot go straight back to ACTIVE: an economic incident is stepped down
// through HALTED or CLOSE_ONLY, and no number of signatures shortens that.
func TestIntegration_ApprovalDoesNotOverrideTheMarketsOwnTransitionTable(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	ops := seedOperator(t, d, security.RoleOperations)

	freeze := proposeWithParams(t, h.as(&ops), admin.KindNativeMarketFreeze,
		"native_market", h.market.ID.String(), nil)
	require.Equal(t, http.StatusOK, execute(h.as(&ops), freeze.ID).Code)
	require.Equal(t, nativemarket.StatusFrozen, h.marketStatus(t))

	resume := proposeWithParams(t, h.as(&ops), admin.KindNativeMarketResume,
		"native_market", h.market.ID.String(), nil)
	approver := elevate(seedOperator(t, d, security.RoleOperations), testNow.Add(time.Hour))
	require.Equal(t, http.StatusOK, decide(h.as(&approver), resume.ID, "approve", "approved").Code)

	res := execute(h.as(&ops), resume.ID)
	require.NotEqual(t, http.StatusOK, res.Code,
		"FROZEN → ACTIVE must be refused by the market, whatever was approved; body=%s", res.Body.String())
	require.Equal(t, nativemarket.StatusFrozen, h.marketStatus(t))

	// Stepping down works, and then the same approval path does.
	closeOnly := proposeWithParams(t, h.as(&ops), admin.KindNativeMarketCloseOnly,
		"native_market", h.market.ID.String(), nil)
	require.Equal(t, http.StatusOK, execute(h.as(&ops), closeOnly.ID).Code)
	require.Equal(t, nativemarket.StatusCloseOnly, h.marketStatus(t))
}

// ---------------------------------------------------------------------------
// Moderation and commerce
// ---------------------------------------------------------------------------

// TestIntegration_AModerationVerdictIsRecordedAndDoesNotTrade. A content
// decision must not be an economic one: recording APPROVED does not start
// trading, and recording REJECTED does not by itself halt a live market.
func TestIntegration_AModerationVerdictIsRecordedAndDoesNotTrade(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	compliance := seedOperator(t, d, security.RoleCompliance)

	before := h.marketStatus(t)
	action := proposeWithParams(t, h.as(&compliance), admin.KindNativeAssetModerationVerdict,
		"native_asset", h.asset.AssetID.String(),
		map[string]any{"state": "FLAGGED", "notes": "reported by three users"})
	res := execute(h.as(&compliance), action.ID)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	got, err := h.nativeAssets.Get(t.Context(), h.db, h.asset.AssetID)
	require.NoError(t, err)
	assert.Equal(t, nativeasset.ModerationFlagged, got.Moderation)
	assert.Contains(t, got.ModerationNotes, "three users")
	assert.Equal(t, before, h.marketStatus(t),
		"a verdict records a judgement; halting the market is a separate act")
}

// TestIntegration_AnUnknownModerationStateIsRefused: params are re-verified by
// Execute, and the executor still refuses a state nobody declared rather than
// writing it.
func TestIntegration_AnUnknownModerationStateIsRefused(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	compliance := seedOperator(t, d, security.RoleCompliance)

	action := proposeWithParams(t, h.as(&compliance), admin.KindNativeAssetModerationVerdict,
		"native_asset", h.asset.AssetID.String(),
		map[string]any{"state": "PROBABLY_FINE", "notes": "n/a"})
	res := execute(h.as(&compliance), action.ID)
	require.NotEqual(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	got, err := h.nativeAssets.Get(t.Context(), h.db, h.asset.AssetID)
	require.NoError(t, err)
	assert.Equal(t, nativeasset.ModerationApproved, got.Moderation, "nothing was written")
}

// TestIntegration_SuspendingASellerStopsNewOrdersAndKeepsPastEarnings.
// Suspension stops new activity. Clawing back a completed sale is a ledger
// correction, which is a different action with a different approval, and
// conflating the two would let a moderation decision move money.
func TestIntegration_SuspendingASellerStopsNewOrdersAndKeepsPastEarnings(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	ops := seedOperator(t, d, security.RoleOperations)

	// A real sale first.
	var product commerce.Product
	require.NoError(t, h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, err := h.commerce.RegisterSeller(ctx, tx, commerce.Seller{
				AccountID: h.creator, DisplayName: "Creator",
			}); err != nil {
				return err
			}
			p, err := h.commerce.CreateProduct(ctx, tx, commerce.Product{
				SellerAccountID: h.creator, Kind: commerce.KindData,
				Title: "A dataset", Price: qq("1000"),
			})
			if err != nil {
				return err
			}
			product, err = h.commerce.Publish(ctx, tx, p.ID)
			return err
		}))
	require.NoError(t, h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := h.credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: h.buyer, Quantity: qq("5000"),
				Origin: valuedomain.OriginPurchased, Finality: valuedomain.FinalitySettled,
				Reference:      credit.Reference{Type: "test_fund", ID: id.New[id.Any]().String()},
				IdempotencyKey: "fund-" + id.New[id.Any]().String(),
				Reason:         "fixture", EffectiveAt: h.clk.Now(),
			})
			return err
		}))
	require.NoError(t, h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := h.commerce.Purchase(ctx, tx, commerce.PurchaseRequest{
				ProductID: product.ID, BuyerAccountID: h.buyer, ExpectedPrice: qq("1000"),
				IdempotencyKey: "buy-" + id.New[id.Any]().String(), EffectiveAt: h.clk.Now(),
			})
			return err
		}))

	lotsBefore, err := h.credits.Lots(t.Context(), h.db, h.creator)
	require.NoError(t, err)
	require.Len(t, lotsBefore, 1)

	// Now suspend.
	action := proposeWithParams(t, h.as(&ops), admin.KindCommerceSellerSuspend,
		"account", h.creator.String(), nil)
	res := execute(h.as(&ops), action.ID)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	sel, err := h.commerce.Seller(t.Context(), h.db, h.creator)
	require.NoError(t, err)
	assert.Equal(t, commerce.SellerSuspended, sel.Status)
	assert.NotEmpty(t, sel.SuspendedReason, "a suspension must record why")

	// New orders stop.
	err = h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, perr := h.commerce.Purchase(ctx, tx, commerce.PurchaseRequest{
				ProductID: product.ID, BuyerAccountID: h.buyer, ExpectedPrice: qq("1000"),
				IdempotencyKey: "buy-" + id.New[id.Any]().String(), EffectiveAt: h.clk.Now(),
			})
			return perr
		})
	require.Error(t, err)
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// What was already earned is untouched.
	lotsAfter, err := h.credits.Lots(t.Context(), h.db, h.creator)
	require.NoError(t, err)
	require.Equal(t, len(lotsBefore), len(lotsAfter))
	require.Equal(t, lotsBefore[0].Remaining.String(), lotsAfter[0].Remaining.String())
	require.NoError(t, h.credits.VerifyProvenance(t.Context(), h.db, h.creator))
}

// TestIntegration_WithdrawingAProductIsTerminal.
func TestIntegration_WithdrawingAProductIsTerminal(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	ops := seedOperator(t, d, security.RoleOperations)

	var product commerce.Product
	require.NoError(t, h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, err := h.commerce.RegisterSeller(ctx, tx, commerce.Seller{
				AccountID: h.creator, DisplayName: "Creator",
			}); err != nil {
				return err
			}
			p, err := h.commerce.CreateProduct(ctx, tx, commerce.Product{
				SellerAccountID: h.creator, Kind: commerce.KindResearch,
				Title: "A note", Price: qq("500"),
			})
			if err != nil {
				return err
			}
			product, err = h.commerce.Publish(ctx, tx, p.ID)
			return err
		}))

	action := proposeWithParams(t, h.as(&ops), admin.KindCommerceProductWithdraw,
		"internal_product", product.ID.String(), nil)
	require.Equal(t, http.StatusOK, execute(h.as(&ops), action.ID).Code)

	got, err := h.commerce.Product(t.Context(), h.db, product.ID)
	require.NoError(t, err)
	assert.Equal(t, commerce.StatusWithdrawn, got.Status)

	// It cannot come back: WITHDRAWN is terminal, and an admin action does not
	// change that.
	err = h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, serr := h.commerce.SetStatus(ctx, tx, product.ID, commerce.StatusActive)
			return serr
		})
	require.Error(t, err)
	require.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
}

// TestIntegration_AnExecutorRefusesOutsideExecute. Every executor reads its
// target from the action Execute is applying, never from its caller. Called
// with no such action -- which is what a second, unlocked path into the same
// effect would look like -- it must refuse rather than guess a target.
func TestIntegration_AnExecutorRefusesOutsideExecute(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)

	table := DomainAExecutors(DomainAExecutorDeps{
		NativeAssets: h.nativeAssets, NativeMarkets: h.nativeMarkets, Commerce: h.commerce,
	})
	require.NotEmpty(t, table)
	for kind, fn := range table {
		_, err := fn(t.Context(), nil, json.RawMessage(`{}`))
		require.Error(t, err, "%s ran outside Execute", kind)
		require.Equal(t, errs.CodeInternal, errs.CodeOf(err), "%s", kind)
	}
	require.Equal(t, nativemarket.StatusActive, h.marketStatus(t), "nothing moved")
}

// TestIntegration_ADeploymentWithoutTheInternalEconomyRegistersNoExecutors: a
// nil service leaves its kinds unregistered, so the API answers UNSUPPORTED
// rather than reporting a success that never happened.
func TestIntegration_ADeploymentWithoutTheInternalEconomyRegistersNoExecutors(t *testing.T) {
	require.Empty(t, DomainAExecutors(DomainAExecutorDeps{}))

	partial := DomainAExecutors(DomainAExecutorDeps{NativeMarkets: nativemarket.NewService(
		ledger.NewService(clock.NewFake(testNow), "x"),
		credit.NewService(ledger.NewService(clock.NewFake(testNow), "x"), clock.NewFake(testNow)),
		valuation.NewPriceStore(clock.NewFake(testNow)), audit.NewWriter(), instruments.NewRepository(),
		nativemarket.NewRiskGate(risk.NewStore(), clock.NewFake(testNow)), clock.NewFake(testNow),
	)})
	require.Len(t, partial, 4, "the four market controls and nothing else")
	for _, k := range []admin.Kind{
		admin.KindNativeMarketHalt, admin.KindNativeMarketCloseOnly,
		admin.KindNativeMarketFreeze, admin.KindNativeMarketResume,
	} {
		require.Contains(t, partial, k)
	}
}

// --- helpers for the launch chain -------------------------------------------

// newAsset creates one DRAFT asset with a unique name and symbol.
func (h *domainAHarness) newAsset(t *testing.T) nativeasset.Asset {
	t.Helper()
	raw := strings.ReplaceAll(id.New[id.Any]().String(), "-", "")
	suffix := raw[len(raw)-6:]
	var out nativeasset.Asset
	require.NoError(t, h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			a, _, err := h.nativeAssets.CreateDraft(ctx, tx, nativeasset.CreateRequest{
				CreatorAccountID: h.creator,
				Name:             "Launchable " + suffix,
				Symbol:           "LA" + suffix[:4],
				Description:      "an asset for the launch chain",
				Supply: nativeasset.SupplyModel{
					MaxSupply:         qq("1000000000000000"),
					CreatorAllocation: qq("100000000000000"),
				},
			})
			out = a
			return err
		}))
	return out
}

func (h *domainAHarness) newDraftAsset(t *testing.T) nativeasset.Asset {
	t.Helper()
	return h.newAsset(t)
}

// newSubmittedAsset is a draft its creator has submitted for review, which is
// the state a launch may act on.
func (h *domainAHarness) newSubmittedAsset(t *testing.T) nativeasset.Asset {
	t.Helper()
	a := h.newAsset(t)
	require.NoError(t, h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := h.nativeAssets.SetStatus(ctx, tx, a.AssetID, nativeasset.StatusPendingReview,
				"submitted for review by its creator")
			return err
		}))
	return a
}

func (h *domainAHarness) assetStatus(t *testing.T, assetID assets.AssetID) nativeasset.Status {
	t.Helper()
	a, err := h.nativeAssets.Get(t.Context(), h.db, assetID)
	require.NoError(t, err)
	return a.Status
}

func (h *domainAHarness) marketOf(t *testing.T, assetID assets.AssetID) nativemarket.MarketID {
	t.Helper()
	m, err := h.nativeMarkets.MarketByAsset(t.Context(), h.db, assetID)
	require.NoError(t, err)
	return m.ID
}

// fundCredits issues Credits so a buyer can trade.
func (h *domainAHarness) fundCredits(t *testing.T, account accounts.AccountID, amount string) {
	t.Helper()
	require.NoError(t, h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := h.credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: account, Quantity: qq(amount),
				Origin: valuedomain.OriginPromotional, Finality: valuedomain.FinalityUnfunded,
				Reference:      credit.Reference{Type: "test_issue", ID: id.New[id.Any]().String()},
				IdempotencyKey: "fund-" + id.New[id.Any]().String(),
				Reason:         "launch chain test", EffectiveAt: h.clk.Now(),
			})
			return err
		}))
}

// buyOnMarket spends Credits on the market, which is what proves a launched
// market is usable rather than merely present.
func (h *domainAHarness) buyOnMarket(t *testing.T, marketID nativemarket.MarketID, buyer accounts.AccountID, credits string) nativemarket.ExecuteResult {
	t.Helper()
	var out nativemarket.ExecuteResult
	require.NoError(t, h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = h.nativeMarkets.Execute(ctx, tx, nativemarket.ExecuteRequest{
				MarketID: marketID, AccountID: buyer, Side: nativemarket.Buy,
				Amount: qq(credits), MinOutput: qq("1"),
				IdempotencyKey: "buy-" + id.New[id.Any]().String(),
				EffectiveAt:    h.clk.Now(),
			})
			return err
		}))
	return out
}

// TestIntegration_LaunchingAMarketTakesTwoPeopleAndThenItTrades is F-28.
//
// The chain from "a creator made an asset" to "a market trades it" had no
// middle in any deployment. `POST /native-assets` produced a DRAFT;
// `nativeasset.Activate` and `nativemarket.Create` were reachable only from
// tests; and the moderation executor's own comment said "activating a market is
// a separate act" — an act nothing implemented. Every existing test built its
// market by calling the services directly, which is exactly why nobody noticed
// that no operator could.
//
// This drives the whole chain through the real surfaces: the creator submits,
// moderation approves, two operators launch, and then somebody buys.
func TestIntegration_LaunchingAMarketTakesTwoPeopleAndThenItTrades(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	ops := seedOperator(t, d, security.RoleOperations)

	asset := h.newSubmittedAsset(t)

	// A moderation verdict is one operator — a COMPLIANCE one, because
	// native_asset:moderate is a content permission and OPERATIONS does not
	// hold it. It does NOT start trading.
	mod := seedOperator(t, d, security.RoleCompliance)
	verdict := proposeWithParams(t, h.as(&mod), admin.KindNativeAssetModerationVerdict,
		"native_asset", asset.AssetID.String(), map[string]any{"state": "APPROVED", "notes": "reviewed"})
	require.Equal(t, http.StatusOK, execute(h.as(&mod), verdict.ID).Code)
	require.Equal(t, nativeasset.StatusPendingReview, h.assetStatus(t, asset.AssetID),
		"approving content must not by itself start an economy")

	params := map[string]any{
		"virtual_credit_reserve": "30000000000",
		"platform_fee_bps":       100,
		"creator_fee_bps":        50,
	}
	launch := proposeWithParams(t, h.as(&ops), admin.KindNativeMarketLaunch,
		"native_asset", asset.AssetID.String(), params)

	// One operator cannot launch. Launching MINTS: every unit that will ever
	// exist is created by this action, and the economics lock behind it.
	res := execute(h.as(&ops), launch.ID)
	require.NotEqual(t, http.StatusOK, res.Code, "an unapproved launch must not mint; body=%s", res.Body.String())
	require.Equal(t, nativeasset.StatusPendingReview, h.assetStatus(t, asset.AssetID))

	// Nor can the proposer approve their own.
	self := elevate(ops, testNow.Add(time.Hour))
	require.Equal(t, http.StatusForbidden, decide(h.as(&self), launch.ID, "approve", "approving my own").Code)

	approver := elevate(seedOperator(t, d, security.RoleOperations), testNow.Add(time.Hour))
	require.Equal(t, http.StatusOK, decide(h.as(&approver), launch.ID, "approve", "second pair of eyes").Code)

	res = execute(h.as(&ops), launch.ID)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.Equal(t, nativeasset.StatusActive, h.assetStatus(t, asset.AssetID))

	marketID := h.marketOf(t, asset.AssetID)
	m, err := h.nativeMarkets.Market(t.Context(), h.db, marketID)
	require.NoError(t, err)
	require.Equal(t, nativemarket.StatusActive, m.Status)
	require.Equal(t, "30000000000", m.Curve.VirtualCreditReserve.String(),
		"the market opens with the economics that were APPROVED, not ones supplied at execution")
	require.EqualValues(t, 100, int(m.Fees.PlatformBPS))
	require.EqualValues(t, 50, int(m.Fees.CreatorBPS))

	// And it trades. A launch that produced an unusable market would satisfy
	// every assertion above.
	buyer := seedCustomerAccount(t, d)
	h.fundCredits(t, buyer, "5000000000")
	fill := h.buyOnMarket(t, marketID, buyer, "1000000000")
	require.True(t, fill.Fill.AssetsOut.IsPositive(), "the first buyer must receive units")
}

// TestIntegration_ADraftCannotBeLaunched: PENDING_REVIEW means the CREATOR
// submitted it, which is what freezes the economics for review. An operator who
// could launch a draft would be launching something its creator was still
// editing.
func TestIntegration_ADraftCannotBeLaunched(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	ops := seedOperator(t, d, security.RoleOperations)

	draft := h.newDraftAsset(t)
	launch := proposeWithParams(t, h.as(&ops), admin.KindNativeMarketLaunch,
		"native_asset", draft.AssetID.String(), map[string]any{
			"virtual_credit_reserve": "30000000000", "platform_fee_bps": 100, "creator_fee_bps": 50,
		})
	approver := elevate(seedOperator(t, d, security.RoleOperations), testNow.Add(time.Hour))
	require.Equal(t, http.StatusOK, decide(h.as(&approver), launch.ID, "approve", "ok").Code)

	res := execute(h.as(&ops), launch.ID)
	require.NotEqual(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.Contains(t, res.Body.String(), "submitted for review")
	require.Equal(t, nativeasset.StatusDraft, h.assetStatus(t, draft.AssetID))
}

// TestIntegration_ALaunchWithNoOpeningPriceIsRefused: virtual_credit_reserve
// sets the opening price and there is no default worth guessing. A launch that
// defaulted it would price somebody's asset for them.
func TestIntegration_ALaunchWithNoOpeningPriceIsRefused(t *testing.T) {
	d := openTestDB(t)
	h := newDomainAHarness(t, d)
	ops := seedOperator(t, d, security.RoleOperations)

	asset := h.newSubmittedAsset(t)
	require.NoError(t, h.db.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := h.nativeAssets.SetModeration(ctx, tx, asset.AssetID, nativeasset.ModerationApproved, "fixture")
			return err
		}))

	for _, bad := range []map[string]any{
		{"platform_fee_bps": 100, "creator_fee_bps": 50},
		{"virtual_credit_reserve": "0", "platform_fee_bps": 100, "creator_fee_bps": 50},
		{"virtual_credit_reserve": "not a number", "platform_fee_bps": 100, "creator_fee_bps": 50},
	} {
		launch := proposeWithParams(t, h.as(&ops), admin.KindNativeMarketLaunch,
			"native_asset", asset.AssetID.String(), bad)
		approver := elevate(seedOperator(t, d, security.RoleOperations), testNow.Add(time.Hour))
		require.Equal(t, http.StatusOK, decide(h.as(&approver), launch.ID, "approve", "ok").Code)
		res := execute(h.as(&ops), launch.ID)
		require.NotEqual(t, http.StatusOK, res.Code, "params %v must be refused; body=%s", bad, res.Body.String())
	}
	require.Equal(t, nativeasset.StatusPendingReview, h.assetStatus(t, asset.AssetID))
}
