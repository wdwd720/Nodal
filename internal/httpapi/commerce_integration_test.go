//go:build integration

package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/idempotency"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// JOURNEY C over HTTP (gola.md PART XVII).
//
// The domain suite in internal/commerce proves the accounting and the database
// refusals. This proves the thing that suite cannot: that the same guarantees
// survive the HTTP surface -- the authorization gate, the tenant check, the
// persisted idempotency contract and the generated binder -- with the REAL
// commerce service behind them rather than a double.

// commerceCaps reports a fixed capability set as ACTIVE for both the compiler
// and the domain service.
type commerceCaps map[valuedomain.CapabilityKey]bool

func (c commerceCaps) Active(context.Context) (map[valuedomain.CapabilityKey]bool, error) {
	return c, nil
}

// ActiveConversionCapabilities makes the same set serve the ledger's resolver,
// so a test that activates a capability activates it everywhere rather than in
// one layer and not the other.
func (c commerceCaps) ActiveConversionCapabilities(context.Context) (map[valuedomain.CapabilityKey]bool, error) {
	return c, nil
}

// verifiedAt reports a fixed financial verification level for every account.
type verifiedAt valuedomain.VerificationLevel

func (v verifiedAt) Level(context.Context, accounts.AccountID) (valuedomain.VerificationLevel, error) {
	return valuedomain.VerificationLevel(v), nil
}

// fixedJurisdiction answers with one jurisdiction, as a deployment that has
// made a determination would.
type fixedJurisdiction string

func (j fixedJurisdiction) Jurisdiction(context.Context, accounts.AccountID) (string, error) {
	return string(j), nil
}

// commercePolicy is what a deployment that HAS decided to run a marketplace
// looks like: internal commerce permitted in one named jurisdiction, behind
// the MARKETPLACE gate, with everything else denied.
func commercePolicy(t *testing.T) *legalrouter.Router {
	t.Helper()
	r, err := legalrouter.New(legalrouter.Policy{
		Version: "httpapi-itest-commerce-v1",
		Rules: []legalrouter.Rule{
			{
				Match: legalrouter.Key{
					Product:      legalrouter.ProductInternalCommerce,
					Jurisdiction: "US-CA",
				},
				Outcome:            legalrouter.Allow,
				ReasonCode:         "MARKETPLACE_APPROVED_FOR_TEST",
				ApprovalReference:  "ITEST-COMMERCE-001",
				RequiredCapability: commerce.CapMarketplace,
			},
			{Outcome: legalrouter.Deny, ReasonCode: "NO_APPROVAL_ON_RECORD"},
		},
	})
	require.NoError(t, err)
	return r
}

type commerceHarness struct {
	*harness
	db     *db.DB
	credit *credit.Service
	asset  assets.AssetID

	sellerUser, buyerUser       accounts.UserID
	sellerAccount, buyerAccount accounts.AccountID
}

// newCommerceHarness builds the real router over the real commerce service,
// with the marketplace approved and gated. newClosedCommerceHarness is the
// same thing on a fresh deployment's defaults.
func newCommerceHarness(t *testing.T) *commerceHarness {
	return newCommerceHarnessWith(t, commercePolicy(t),
		commerceCaps{commerce.CapMarketplace: true}, "US-CA")
}

func newCommerceHarnessWith(
	t *testing.T,
	router *legalrouter.Router,
	caps commerceCaps,
	jurisdiction string,
) *commerceHarness {
	t.Helper()
	d := openTestDB(t)

	clk := clock.System()
	led := ledger.NewService(clk, "httpapi-commerce-itest")
	credits := credit.NewService(led, clk)
	svc := commerce.NewService(led, credits, audit.NewWriter(), clk)
	svc.SetCapabilityResolver(caps)

	sellerUser, sellerAccount := seedAccount(t, d)
	buyerUser, buyerAccount := seedAccount(t, d)

	fx := newFixtures()
	fx.idem = nil
	ports := fx.ports()
	ports.Idempotency = idempotencyAdapter{
		store: idempotency.NewStore(func() time.Time { return time.Now().UTC() }), db: d,
	}
	deps := NativeEconomyDeps{
		Commerce:     svc,
		Capabilities: caps,
		Verification: verifiedAt(valuedomain.VerificationNodalIdentity),
		Jurisdiction: fixedJurisdiction(jurisdiction),
		LegalRouter:  router,
		Clock:        clk,
	}
	ports.Commerce = commerceAdapter{svc: svc, db: d, clk: clk, deps: deps}

	h := &harness{t: t, ports: fx}
	srv, err := New(Options{
		Env: config.EnvTest, Clock: clk, CookieName: "cp_session",
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

	ch := &commerceHarness{
		harness: h, db: d, credit: credits,
		sellerUser: sellerUser, buyerUser: buyerUser,
		sellerAccount: sellerAccount, buyerAccount: buyerAccount,
		asset: commerceCreditAsset(t, d),
	}
	return ch
}

// commerceCreditAsset returns THE Credit asset, creating it if this database
// has none. Migration 00711 permits exactly one.
func commerceCreditAsset(t *testing.T, d *db.DB) assets.AssetID {
	t.Helper()
	ctx := context.Background()
	var existing assets.AssetID
	if err := d.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&existing); err == nil {
		return existing
	}
	created, err := assets.NewRepository().Create(ctx, d, assets.Asset{
		Chain: assets.InternalChain, Kind: assets.KindCredit,
		ValueDomain: valuedomain.InternalCredit,
		Symbol:      "CREDIT", Name: "Nodal Credit", Decimals: 6,
		RiskClass: assets.RiskUnsupported, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	return created.ID
}

func (h *commerceHarness) principal(user accounts.UserID, account accounts.AccountID) *security.Principal {
	return &security.Principal{
		SubjectID:  user.String(),
		ActorType:  security.ActorUser,
		Roles:      []security.Role{security.RoleCustomer},
		AccountIDs: []string{account.String()},
		SessionID:  testSessionID,
		AuthTime:   time.Now().UTC(),
		AMR:        []string{"pwd", "mfa"},
	}
}

func (h *commerceHarness) asSeller() *harness {
	return h.harness.as(h.principal(h.sellerUser, h.sellerAccount))
}

func (h *commerceHarness) asBuyer() *harness {
	return h.harness.as(h.principal(h.buyerUser, h.buyerAccount))
}

// fund mints Credits directly through the domain service. There is no HTTP
// route that mints Credits and there must not be one: minting is a funding
// event, not a request.
func (h *commerceHarness) fund(account accounts.AccountID, qty int64) {
	h.t.Helper()
	require.NoError(h.t, h.db.InTx(h.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := h.credit.Issue(ctx, tx, credit.IssueRequest{
				AccountID: account, Quantity: money.QuantityFromInt64(qty),
				Origin:   valuedomain.OriginPurchased,
				Finality: valuedomain.FinalitySettled,
				Reference: credit.Reference{
					Type: "test_issue", ID: id.New[id.Any]().String(),
				},
				IdempotencyKey: "issue-" + id.New[id.Any]().String(),
				Reason:         "httpapi commerce itest",
				EffectiveAt:    time.Now().UTC(),
			})
			return err
		}))
}

func idemKey() string { return "commerce-itest-" + id.New[id.Any]().String() }

// TestIntegration_CommerceJourneyOverHTTP walks JOURNEY C through the real
// router: register, list, publish, buy, read back.
func TestIntegration_CommerceJourneyOverHTTP(t *testing.T) {
	h := newCommerceHarness(t)
	h.fund(h.buyerAccount, 10_000)

	// 1. The seller registers.
	res := h.asSeller().do(http.MethodPost, "/v1/internal-sellers", map[string]any{
		"account_id":   h.sellerAccount.String(),
		"display_name": "Test Creator",
	}, "Idempotency-Key", idemKey())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	seller := res.raw()
	assert.Equal(t, "ACTIVE", seller["status"])

	// 2. A product in DRAFT. The response already names the provenance a sale
	//    will produce, because a seller is entitled to know that before listing.
	res = h.asSeller().do(http.MethodPost, "/v1/internal-products", map[string]any{
		"account_id":       h.sellerAccount.String(),
		"kind":             "DATA",
		"title":            "Order-book snapshots, 2026",
		"description":      "one year of L2 snapshots",
		"price":            "1000",
		"platform_fee_bps": 1000,
	}, "Idempotency-Key", idemKey())
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	product := res.raw()
	productID := product["product_id"].(string)
	assert.Equal(t, "DRAFT", product["status"])
	assert.Equal(t, "DATA_SALE_EARNING", product["earning_origin"])
	assert.Equal(t, "100", product["platform_fee"])
	assert.Equal(t, "900", product["seller_proceeds"])
	assert.Equal(t, false, product["terms_frozen"])

	// A draft is not for sale.
	res = h.asBuyer().do(http.MethodPost, "/v1/internal-products/"+productID+"/orders", map[string]any{
		"account_id":     h.buyerAccount.String(),
		"expected_price": "1000",
	}, "Idempotency-Key", idemKey())
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())

	// 3. Publish. Terms freeze.
	res = h.asSeller().do(http.MethodPost, "/v1/internal-products/"+productID+"/status",
		map[string]any{"status": "ACTIVE"}, "Idempotency-Key", idemKey())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	published := res.raw()
	assert.Equal(t, "ACTIVE", published["status"])
	assert.Equal(t, true, published["terms_frozen"])
	assert.NotEmpty(t, published["published_at"])

	// 4. It appears in the catalogue.
	res = h.asBuyer().do(http.MethodGet, "/v1/internal-products?kind=DATA", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var page struct {
		Items []map[string]any `json:"items"`
	}
	res.json(&page)
	require.NotEmpty(t, page.Items)
	found := false
	for _, it := range page.Items {
		if it["product_id"] == productID {
			found = true
		}
	}
	assert.True(t, found, "a published product must be listed")

	// 5. The purchase.
	buyKey := idemKey()
	res = h.asBuyer().do(http.MethodPost, "/v1/internal-products/"+productID+"/orders", map[string]any{
		"account_id":     h.buyerAccount.String(),
		"expected_price": "1000",
	}, "Idempotency-Key", buyKey)
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	order := res.raw()
	assert.Equal(t, "1000", order["price"])
	assert.Equal(t, "100", order["platform_fee"])
	assert.Equal(t, "900", order["seller_proceeds"])
	assert.Equal(t, "DATA_SALE_EARNING", order["earning_origin"])
	assert.NotEmpty(t, order["journal_transaction_id"])

	// 6. A retry with the same key replays rather than charging twice. The
	//    persisted idempotency contract and the domain's own key both hold;
	//    this asserts the outer one, which is what a flaky client hits.
	replay := h.asBuyer().do(http.MethodPost, "/v1/internal-products/"+productID+"/orders", map[string]any{
		"account_id":     h.buyerAccount.String(),
		"expected_price": "1000",
	}, "Idempotency-Key", buyKey)
	require.Equal(t, http.StatusOK, replay.Code, "body=%s", replay.Body.String())
	assert.Equal(t, order["order_id"], replay.raw()["order_id"])

	// 7. Both sides can read the order from their own side of it.
	res = h.asBuyer().do(http.MethodGet, "/v1/internal-orders?account_id="+h.buyerAccount.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&page)
	require.Len(t, page.Items, 1)
	assert.Equal(t, order["order_id"], page.Items[0]["order_id"])

	res = h.asSeller().do(http.MethodGet,
		"/v1/internal-orders?account_id="+h.sellerAccount.String()+"&role=SELLER", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&page)
	require.Len(t, page.Items, 1)
	assert.Equal(t, order["order_id"], page.Items[0]["order_id"])

	// 8. And the provenance actually landed: the seller holds 900 Credits of
	//    DATA_SALE_EARNING, which the payout engine treats differently from the
	//    PURCHASED Credits the buyer spent.
	lots, err := h.credit.Lots(t.Context(), h.db, h.sellerAccount)
	require.NoError(t, err)
	require.Len(t, lots, 1)
	assert.Equal(t, valuedomain.OriginDataSaleEarning, lots[0].Origin)
	assert.Equal(t, "900", lots[0].Quantity.String())
	require.NoError(t, h.credit.VerifyProvenance(t.Context(), h.db, h.sellerAccount))
	require.NoError(t, h.credit.VerifyProvenance(t.Context(), h.db, h.buyerAccount))
}

// TestIntegration_CommerceRefusesCrossTenantRequests: every one of these routes
// names an account, and naming somebody else's is a cross-tenant request
// however it is dressed up.
func TestIntegration_CommerceRefusesCrossTenantRequests(t *testing.T) {
	h := newCommerceHarness(t)
	h.fund(h.buyerAccount, 5_000)

	// Set up a genuine listing as the seller.
	require.Equal(t, http.StatusOK, h.asSeller().do(http.MethodPost, "/v1/internal-sellers", map[string]any{
		"account_id": h.sellerAccount.String(), "display_name": "Test Creator",
	}, "Idempotency-Key", idemKey()).Code)
	created := h.asSeller().do(http.MethodPost, "/v1/internal-products", map[string]any{
		"account_id": h.sellerAccount.String(), "kind": "RESEARCH",
		"title": "A note", "price": "500",
	}, "Idempotency-Key", idemKey())
	require.Equal(t, http.StatusCreated, created.Code, "body=%s", created.Body.String())
	productID := created.raw()["product_id"].(string)
	require.Equal(t, http.StatusOK, h.asSeller().do(http.MethodPost,
		"/v1/internal-products/"+productID+"/status",
		map[string]any{"status": "ACTIVE"}, "Idempotency-Key", idemKey()).Code)

	t.Run("registering somebody else as a seller", func(t *testing.T) {
		res := h.asBuyer().do(http.MethodPost, "/v1/internal-sellers", map[string]any{
			"account_id": h.sellerAccount.String(), "display_name": "Not mine",
		}, "Idempotency-Key", idemKey())
		require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	})

	t.Run("attributing earnings to an account you do not own", func(t *testing.T) {
		res := h.asBuyer().do(http.MethodPost, "/v1/internal-sellers", map[string]any{
			"account_id":        h.buyerAccount.String(),
			"display_name":      "Mine, paying elsewhere",
			"payout_account_id": h.sellerAccount.String(),
		}, "Idempotency-Key", idemKey())
		require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	})

	t.Run("listing a product under somebody else's account", func(t *testing.T) {
		res := h.asBuyer().do(http.MethodPost, "/v1/internal-products", map[string]any{
			"account_id": h.sellerAccount.String(), "kind": "DATA",
			"title": "Not mine", "price": "100",
		}, "Idempotency-Key", idemKey())
		require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	})

	t.Run("moving somebody else's product", func(t *testing.T) {
		res := h.asBuyer().do(http.MethodPost, "/v1/internal-products/"+productID+"/status",
			map[string]any{"status": "WITHDRAWN"}, "Idempotency-Key", idemKey())
		require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	})

	t.Run("buying with somebody else's Credits", func(t *testing.T) {
		res := h.asSeller().do(http.MethodPost, "/v1/internal-products/"+productID+"/orders",
			map[string]any{
				"account_id": h.buyerAccount.String(), "expected_price": "500",
			}, "Idempotency-Key", idemKey())
		require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	})

	t.Run("reading somebody else's orders", func(t *testing.T) {
		res := h.asBuyer().do(http.MethodGet,
			"/v1/internal-orders?account_id="+h.sellerAccount.String(), nil)
		require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	})

	// The product is still ACTIVE and unsold: none of the above did anything.
	res := h.asBuyer().do(http.MethodGet, "/v1/internal-products/"+productID, nil)
	require.Equal(t, http.StatusOK, res.Code)
	assert.Equal(t, "ACTIVE", res.raw()["status"])
	res = h.asSeller().do(http.MethodGet,
		"/v1/internal-orders?account_id="+h.sellerAccount.String()+"&role=SELLER", nil)
	require.Equal(t, http.StatusOK, res.Code)
	var page struct {
		Items []map[string]any `json:"items"`
	}
	res.json(&page)
	assert.Empty(t, page.Items)
}

// TestIntegration_APriceTheBuyerDidNotAgreeToIsAConflictOverHTTP: the domain
// refusal has to survive as a 409 with a machine-readable code, not as a 500.
func TestIntegration_APriceTheBuyerDidNotAgreeToIsAConflictOverHTTP(t *testing.T) {
	h := newCommerceHarness(t)
	h.fund(h.buyerAccount, 5_000)

	require.Equal(t, http.StatusOK, h.asSeller().do(http.MethodPost, "/v1/internal-sellers", map[string]any{
		"account_id": h.sellerAccount.String(), "display_name": "Test Creator",
	}, "Idempotency-Key", idemKey()).Code)
	created := h.asSeller().do(http.MethodPost, "/v1/internal-products", map[string]any{
		"account_id": h.sellerAccount.String(), "kind": "COMPUTE",
		"title": "GPU hours", "price": "1200",
	}, "Idempotency-Key", idemKey())
	require.Equal(t, http.StatusCreated, created.Code)
	productID := created.raw()["product_id"].(string)
	require.Equal(t, http.StatusOK, h.asSeller().do(http.MethodPost,
		"/v1/internal-products/"+productID+"/status",
		map[string]any{"status": "ACTIVE"}, "Idempotency-Key", idemKey()).Code)

	res := h.asBuyer().do(http.MethodPost, "/v1/internal-products/"+productID+"/orders",
		map[string]any{"account_id": h.buyerAccount.String(), "expected_price": "900"},
		"Idempotency-Key", idemKey())
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	p := res.problem()
	assert.Equal(t, errs.CodeConflict, p.Code)

	// Nothing moved, and the honest price still works.
	res = h.asBuyer().do(http.MethodPost, "/v1/internal-products/"+productID+"/orders",
		map[string]any{"account_id": h.buyerAccount.String(), "expected_price": "1200"},
		"Idempotency-Key", idemKey())
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
}

// TestIntegration_AFreshDeploymentSellsNothing is PART LXIII at the HTTP
// surface. With the conservative policy (no LegalRouter configured), an
// unknown jurisdiction and no capability active, a listing can still be
// created -- publishing moves nothing -- and a purchase is refused, naming the
// policy version that refused it.
func TestIntegration_AFreshDeploymentSellsNothing(t *testing.T) {
	h := newCommerceHarnessWith(t, nil, commerceCaps{}, "")
	h.fund(h.buyerAccount, 5_000)

	require.Equal(t, http.StatusOK, h.asSeller().do(http.MethodPost, "/v1/internal-sellers", map[string]any{
		"account_id": h.sellerAccount.String(), "display_name": "Test Creator",
	}, "Idempotency-Key", idemKey()).Code)
	created := h.asSeller().do(http.MethodPost, "/v1/internal-products", map[string]any{
		"account_id": h.sellerAccount.String(), "kind": "DATA",
		"title": "A dataset", "price": "1000",
	}, "Idempotency-Key", idemKey())
	require.Equal(t, http.StatusCreated, created.Code, "body=%s", created.Body.String())
	productID := created.raw()["product_id"].(string)
	require.Equal(t, http.StatusOK, h.asSeller().do(http.MethodPost,
		"/v1/internal-products/"+productID+"/status",
		map[string]any{"status": "ACTIVE"}, "Idempotency-Key", idemKey()).Code)

	res := h.asBuyer().do(http.MethodPost, "/v1/internal-products/"+productID+"/orders",
		map[string]any{"account_id": h.buyerAccount.String(), "expected_price": "1000"},
		"Idempotency-Key", idemKey())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	p := res.problem()
	require.Equal(t, errs.CodeForbidden, p.Code)
	require.Contains(t, res.Body.String(), "legal-router-v1-conservative",
		"the refusal must name the fail-closed policy that produced it")

	// Nothing moved.
	lots, err := h.credit.Lots(t.Context(), h.db, h.sellerAccount)
	require.NoError(t, err)
	require.Empty(t, lots, "a refused purchase must mint no provenance")
}

// TestIntegration_TheGateAndThePolicyMustBothAgree: the policy permitting is
// not enough, and neither is the gate. Each alone refuses, and the refusal
// says which.
func TestIntegration_TheGateAndThePolicyMustBothAgree(t *testing.T) {
	noPolicy := func(*testing.T) *legalrouter.Router { return nil }
	type setup struct {
		what       string
		router     func(*testing.T) *legalrouter.Router
		caps       commerceCaps
		juris      string
		wantSold   bool
		wantStatus int
		wantCode   errs.Code
	}
	// The two refusals are deliberately DIFFERENT answers. A gate that is off
	// is "not yet, here" -- 422 CAPABILITY_NOT_APPROVED, with the capability
	// named, so an operator knows exactly what to activate. A policy that
	// denies is "not this, by you, under this policy" -- 403, with the policy
	// version and rule index, so it can be traced to a line somebody wrote.
	// Collapsing them would lose the distinction that makes either useful.
	for _, tc := range []setup{
		{"policy and gate agree", commercePolicy, commerceCaps{commerce.CapMarketplace: true},
			"US-CA", true, http.StatusCreated, ""},
		{"gate off", commercePolicy, commerceCaps{},
			"US-CA", false, http.StatusUnprocessableEntity, errs.CodeCapabilityNotApproved},
		{"policy denies this jurisdiction", commercePolicy, commerceCaps{commerce.CapMarketplace: true},
			"US-NY", false, http.StatusForbidden, errs.CodeForbidden},
		{"no policy at all", noPolicy, commerceCaps{commerce.CapMarketplace: true},
			"US-CA", false, http.StatusForbidden, errs.CodeForbidden},
	} {
		t.Run(tc.what, func(t *testing.T) {
			h := newCommerceHarnessWith(t, tc.router(t), tc.caps, tc.juris)
			h.fund(h.buyerAccount, 5_000)

			require.Equal(t, http.StatusOK, h.asSeller().do(http.MethodPost, "/v1/internal-sellers",
				map[string]any{"account_id": h.sellerAccount.String(), "display_name": "Creator"},
				"Idempotency-Key", idemKey()).Code)
			created := h.asSeller().do(http.MethodPost, "/v1/internal-products", map[string]any{
				"account_id": h.sellerAccount.String(), "kind": "RESEARCH",
				"title": "A note", "price": "500",
			}, "Idempotency-Key", idemKey())
			require.Equal(t, http.StatusCreated, created.Code)
			productID := created.raw()["product_id"].(string)
			require.Equal(t, http.StatusOK, h.asSeller().do(http.MethodPost,
				"/v1/internal-products/"+productID+"/status",
				map[string]any{"status": "ACTIVE"}, "Idempotency-Key", idemKey()).Code)

			res := h.asBuyer().do(http.MethodPost, "/v1/internal-products/"+productID+"/orders",
				map[string]any{"account_id": h.buyerAccount.String(), "expected_price": "500"},
				"Idempotency-Key", idemKey())
			require.Equal(t, tc.wantStatus, res.Code, "body=%s", res.Body.String())
			if tc.wantSold {
				return
			}
			require.Equal(t, tc.wantCode, res.problem().Code)
			require.Contains(t, res.Body.String(), "policy_version",
				"a refusal must name the policy version that produced it")
			require.Contains(t, res.Body.String(), "policy_rule_index",
				"a refusal must be traceable to the rule that produced it")

			// Nothing moved either way.
			lots, lerr := h.credit.Lots(t.Context(), h.db, h.sellerAccount)
			require.NoError(t, lerr)
			require.Empty(t, lots, "a refused purchase must mint no provenance")
		})
	}
}

// mutableJurisdiction is a resolver an operator (or a determination landing
// mid-session) can change under a live session.
type mutableJurisdiction struct{ value *string }

func (j mutableJurisdiction) Jurisdiction(context.Context, accounts.AccountID) (string, error) {
	return *j.value, nil
}

// TestIntegration_JurisdictionTurningBlockedMidSessionStopsTheNextPurchase is
// PART LXXII item 30.
//
// The buyer does nothing wrong and nothing changes about their session: their
// cookie, their roles and their account are identical before and after. What
// changes is the deployment's answer to "where is this person". A system that
// resolved jurisdiction once at sign-in would keep selling; this one resolves
// it per command, so the very next purchase is refused.
func TestIntegration_JurisdictionTurningBlockedMidSessionStopsTheNextPurchase(t *testing.T) {
	d := openTestDB(t)
	jurisdiction := "US-CA"
	caps := commerceCaps{commerce.CapMarketplace: true}

	clk := clock.System()
	led := ledger.NewService(clk, "httpapi-commerce-itest")
	credits := credit.NewService(led, clk)
	svc := commerce.NewService(led, credits, audit.NewWriter(), clk)
	svc.SetCapabilityResolver(caps)

	sellerUser, sellerAccount := seedAccount(t, d)
	buyerUser, buyerAccount := seedAccount(t, d)

	fx := newFixtures()
	fx.idem = nil
	ports := fx.ports()
	ports.Idempotency = idempotencyAdapter{
		store: idempotency.NewStore(func() time.Time { return time.Now().UTC() }), db: d,
	}
	ports.Commerce = commerceAdapter{svc: svc, db: d, clk: clk, deps: NativeEconomyDeps{
		Commerce:     svc,
		Capabilities: caps,
		Verification: verifiedAt(valuedomain.VerificationNodalIdentity),
		Jurisdiction: mutableJurisdiction{value: &jurisdiction},
		LegalRouter:  commercePolicy(t),
		Clock:        clk,
	}}

	h := &harness{t: t, ports: fx}
	srv, err := New(Options{
		Env: config.EnvTest, Clock: clk, CookieName: "cp_session",
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

	ch := &commerceHarness{
		harness: h, db: d, credit: credits,
		sellerUser: sellerUser, buyerUser: buyerUser,
		sellerAccount: sellerAccount, buyerAccount: buyerAccount,
		asset: commerceCreditAsset(t, d),
	}
	ch.fund(buyerAccount, 10_000)

	require.Equal(t, http.StatusOK, ch.asSeller().do(http.MethodPost, "/v1/internal-sellers",
		map[string]any{"account_id": sellerAccount.String(), "display_name": "Creator"},
		"Idempotency-Key", idemKey()).Code)
	created := ch.asSeller().do(http.MethodPost, "/v1/internal-products", map[string]any{
		"account_id": sellerAccount.String(), "kind": "DATA",
		"title": "A dataset", "price": "1000",
	}, "Idempotency-Key", idemKey())
	require.Equal(t, http.StatusCreated, created.Code, "body=%s", created.Body.String())
	productID := created.raw()["product_id"].(string)
	require.Equal(t, http.StatusOK, ch.asSeller().do(http.MethodPost,
		"/v1/internal-products/"+productID+"/status",
		map[string]any{"status": "ACTIVE"}, "Idempotency-Key", idemKey()).Code)

	// One purchase succeeds while the jurisdiction is permitted.
	first := ch.asBuyer().do(http.MethodPost, "/v1/internal-products/"+productID+"/orders",
		map[string]any{"account_id": buyerAccount.String(), "expected_price": "1000"},
		"Idempotency-Key", idemKey())
	require.Equal(t, http.StatusCreated, first.Code, "body=%s", first.Body.String())

	// The determination changes. Same session, same cookie, same principal.
	jurisdiction = "US-NY"

	second := ch.asBuyer().do(http.MethodPost, "/v1/internal-products/"+productID+"/orders",
		map[string]any{"account_id": buyerAccount.String(), "expected_price": "1000"},
		"Idempotency-Key", idemKey())
	require.Equal(t, http.StatusForbidden, second.Code,
		"a jurisdiction that turns blocked must stop the next command; body=%s", second.Body.String())
	require.Contains(t, second.Body.String(), "policy_version")

	// Exactly one sale happened.
	var orders int
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT count(*) FROM internal_commerce_orders WHERE buyer_account_id = $1`,
		buyerAccount).Scan(&orders))
	require.Equal(t, 1, orders)
}
