package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

var (
	portfolioYesShare = assets.NewAssetID()
	portfolioRecord   = booksFixedID[reconciliation.RecordID]("0193b2e0-0000-7000-8000-0000000000e1")
	portfolioEffect   = booksFixedID[econ.EffectID]("0193b2e0-0000-7000-8000-0000000000e2")
)

// fakePortfolioPort is the portfolio's own read. It records the account it
// was asked about, so a test can prove the handler passed the scoped account,
// and it answers for that account, as the real read does.
type fakePortfolioPort struct {
	stubErr
	mu    sync.Mutex
	snap  PortfolioSnapshot
	calls int
	last  accounts.AccountID
}

func (f *fakePortfolioPort) Snapshot(_ context.Context, account accounts.AccountID) (PortfolioSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = account
	s := f.snap
	s.AccountID = account
	return s, f.fail()
}

func (f *fakePortfolioPort) asked() (int, accounts.AccountID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.last
}

// portfolioSampleSnapshot holds one of everything a balance can say: the
// sandbox dollar at par with reservations of two purposes, collateral and a
// loan; an equity share at a mark with shares awaiting settlement; and a
// prediction-market share no mark values. The risk records hold one blocking
// reconciliation record and one effect of unknown outcome, and the account
// holds shares in the prediction market of the predmarket fakes.
func portfolioSampleSnapshot() PortfolioSnapshot {
	usd := assets.Asset{
		ID: econ.SandboxDollarID(), Chain: assets.SandboxChain, Symbol: econ.SandboxDollarSymbol, Name: econ.SandboxDollarName,
		Decimals: econ.SandboxDollarDecimals, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
	}
	acme := assets.Asset{
		ID: eqShare, Chain: assets.SandboxChain, Symbol: "ACME", Name: "Acme (simulated share)", Decimals: 6,
		Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
	}
	yes := assets.Asset{
		ID: portfolioYesShare, Chain: "sbx-solana", Symbol: "PMHTTP-YES", Name: "PMHTTP outcome YES", Decimals: 6,
		Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
	}
	mark, err := money.NewPrice(qty("15000"), 2, econ.SandboxDollarID().String(), "sandbox-feed", testNow.Add(-30*time.Second))
	if err != nil {
		panic(err)
	}
	usdValue, usdOwed, acmeValue := qty("2296250000"), qty("100000000"), qty("750000000")
	return PortfolioSnapshot{
		AccountStatus: accounts.StatusActive, AsOf: testNow, Quote: usd, QuoteProvisioned: true, MaxMarkAge: 5 * time.Minute,
		Balances: []PortfolioBalance{
			{
				Asset: acme, Wallet: qty("3000000"), Reserved: qty("0"), WithdrawalHolds: qty("0"), Available: qty("3000000"),
				Elsewhere: []PortfolioLedgerBalance{{Code: ledger.CodeUnsettledReceivable, Quantity: qty("2000000")}},
				Total:     qty("5000000"),
				Valuation: PortfolioValuation{Status: PortfolioValuationMarked, Mark: &mark, Value: &acmeValue},
			},
			{
				Asset: yes, Wallet: qty("3000000"), Reserved: qty("0"), WithdrawalHolds: qty("0"), Available: qty("3000000"),
				Total: qty("3000000"),
				Valuation: PortfolioValuation{Status: PortfolioValuationUnvalued, Problem: errs.Newf(errs.CodeStaleMarketData,
					"no mark of PMHTTP-YES in the sandbox dollar within the maximum age (5m0s)")},
			},
			{
				Asset: usd, Wallet: qty("1000000000"), Reserved: qty("250000000"), WithdrawalHolds: qty("0"), Available: qty("750000000"),
				Reservations: []PortfolioReservation{
					{Purpose: PortfolioPurposeOpenOrders, Capability: "INTERNAL_MATCHING", Quantity: qty("200000000"), Count: 2},
					{Purpose: PortfolioPurposePendingTransfers, Capability: "P2P_TRANSFERS", Quantity: qty("50000000"), Count: 1},
				},
				Elsewhere: []PortfolioLedgerBalance{
					{Code: ledger.CodeMarginCollateral, Quantity: qty("500000000")},
					{Code: ledger.CodeDerivativesMargin, Quantity: qty("796250000")},
				},
				Owed:      []PortfolioLedgerBalance{{Code: ledger.CodeMarginLoan, Quantity: qty("100000000")}},
				Total:     qty("2296250000"),
				Valuation: PortfolioValuation{Status: PortfolioValuationPar, Value: &usdValue, OwedValue: &usdOwed},
			},
		},
		BlocksNewRisk: true,
		Reconciliation: []reconciliation.Record{{
			ID: portfolioRecord, Kind: reconciliation.KindEconomicEffect, ScopeType: "ECONOMIC_EFFECT", ScopeID: portfolioEffect.String(),
			AccountID: testAccountID, AssetID: econ.SandboxDollarID(), Status: reconciliation.StatusOpen, Material: true,
			BlocksNewRisk: true, OpenedAt: testNow.Add(-time.Hour),
		}},
		UnknownEffects: []econ.Effect{{
			ID: portfolioEffect, Key: "bridge:lock:1", Subsystem: econ.SubsystemBridge, Kind: "BRIDGE_SOURCE_FINALIZED",
			AccountID: testAccountID.String(), PostsLedger: true, Status: econ.EffectUnknown, ExternalRef: "devnet-tx-1",
			CreatedAt: testNow.Add(-2 * time.Hour), UpdatedAt: testNow.Add(-time.Hour),
		}},
		PredictionMarkets: []predmarket.MarketID{pmMarketID},
	}
}

// portfolioFakes are the doubles behind every section of the portfolio. All
// but the portfolio's own are the subsystems' own test doubles, so a section
// is checked against exactly what that subsystem's route renders.
type portfolioFakes struct {
	portfolio   *fakePortfolioPort
	advanced    *fakeAdvanced
	margin      *fakeMargin
	derivatives *fakeDerivatives
	predmarket  *fakePredmarket
	equities    *fakeEquities
	books       *fakeBooks
}

func newPortfolioHarness(t *testing.T) (*harness, *portfolioFakes) {
	t.Helper()
	h := newHarness(t)
	fx := &portfolioFakes{
		portfolio: &fakePortfolioPort{snap: portfolioSampleSnapshot()}, advanced: newFakeAdvanced(), margin: newFakeMargin(t),
		derivatives: newFakeDerivatives(), predmarket: newFakePredmarket(t), equities: newFakeEquities(), books: newFakeBooks(),
	}
	ports := h.ports.ports()
	ports.Portfolio, ports.Advanced, ports.Margin, ports.Derivatives = fx.portfolio, fx.advanced, fx.margin, fx.derivatives
	ports.PredictionMarkets, ports.Equities, ports.Books = fx.predmarket, fx.equities, fx.books
	h.server.opts.Ports = ports
	return h, fx
}

// portfolioOnlyHarness wires the portfolio's own read and nothing else.
func portfolioOnlyHarness(t *testing.T) (*harness, *fakePortfolioPort) {
	t.Helper()
	h := newHarness(t)
	fp := &fakePortfolioPort{snap: portfolioSampleSnapshot()}
	ports := h.ports.ports()
	ports.Portfolio = fp
	h.server.opts.Ports = ports
	return h, fp
}

func portfolioPath(acct accounts.AccountID) string {
	return "/v1/advanced/portfolio?account_id=" + acct.String()
}

// portfolioAt walks a decoded JSON document along keys and array indexes.
func portfolioAt(t *testing.T, doc any, path ...any) any {
	t.Helper()
	cur := doc
	for _, step := range path {
		switch k := step.(type) {
		case string:
			m, ok := cur.(map[string]any)
			require.True(t, ok, "expected an object before %q", k)
			v, present := m[k]
			require.True(t, present, "missing %q", k)
			cur = v
		case int:
			a, ok := cur.([]any)
			require.True(t, ok, "expected an array before [%d]", k)
			require.Greater(t, len(a), k, "array too short for [%d]", k)
			cur = a[k]
		}
	}
	return cur
}

// portfolioJSON is a decoded value encoded again, for comparing one part of a
// response with another response byte for byte.
func portfolioJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestPortfolio_IsScopedToTheCallersAccount(t *testing.T) {
	t.Parallel()
	h, fx := newPortfolioHarness(t)

	res := h.do(http.MethodGet, portfolioPath(testAccountID), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	calls, last := fx.portfolio.asked()
	assert.Equal(t, 1, calls)
	assert.Equal(t, testAccountID, last, "the scoped account is what the port is asked about")
	assert.Equal(t, testAccountID.String(), res.raw()["account_id"])

	res = h.do(http.MethodGet, portfolioPath(testOtherAcct), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	calls, _ = fx.portfolio.asked()
	assert.Equal(t, 1, calls, "a stranger's request reads nothing")

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, portfolioPath(testOtherAcct), nil)
	require.Equal(t, http.StatusOK, res.Code, "an operator with account:read_any reads any account; body=%s", res.Body.String())
	_, last = fx.portfolio.asked()
	assert.Equal(t, testOtherAcct, last)
	assert.Equal(t, testOtherAcct, fx.margin.lastAcct, "every composed read is scoped to the same account")
}

// A customer probing an account that does not exist gets exactly the refusal
// a foreign account gets, and the port is never asked: the answer cannot tell
// the two apart. Only an operator, who may read any account, learns that an
// account does not exist.
func TestPortfolio_AnAbsentAccountIsMaskedAsForbiddenAndNotFoundOnlyToAnOperator(t *testing.T) {
	t.Parallel()
	h, fp := portfolioOnlyHarness(t)
	absent := accounts.NewAccountID()

	foreign := h.do(http.MethodGet, portfolioPath(testOtherAcct), nil)
	missing := h.do(http.MethodGet, portfolioPath(absent), nil)
	require.Equal(t, http.StatusForbidden, foreign.Code, "body=%s", foreign.Body.String())
	require.Equal(t, foreign.Code, missing.Code, "body=%s", missing.Body.String())
	fp1, mp1 := foreign.problem(), missing.problem()
	assert.Equal(t, fp1.Code, mp1.Code)
	assert.Equal(t, fp1.Detail, mp1.Detail)
	assert.Equal(t, fp1.Title, mp1.Title)
	calls, _ := fp.asked()
	assert.Zero(t, calls, "scoping refuses before anything is read")

	fp.err = errs.New(errs.CodeNotFound, "account not found")
	op := operatorPrincipal()
	h.as(&op)
	res := h.do(http.MethodGet, portfolioPath(absent), nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)
}

func TestPortfolio_RefusesAMalformedOrMissingAccountID(t *testing.T) {
	t.Parallel()
	h, fp := portfolioOnlyHarness(t)
	for _, path := range []string{"/v1/advanced/portfolio?account_id=not-a-uuid", "/v1/advanced/portfolio"} {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusBadRequest, res.Code, "%s body=%s", path, res.Body.String())
		assert.Equal(t, errs.CodeValidationFailed, res.problem().Code, path)
	}
	calls, _ := fp.asked()
	assert.Zero(t, calls)
}

// Every figure of the portfolio's own read renders as an exact base-unit
// string with its asset and decimals, in the order the read gave.
func TestPortfolio_RendersTheBalancesExactly(t *testing.T) {
	t.Parallel()
	h, _ := portfolioOnlyHarness(t)
	res := h.do(http.MethodGet, portfolioPath(testAccountID), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	doc := res.raw()

	assert.Equal(t, "SIMULATED", doc["value_domain"])
	assert.Equal(t, econ.SandboxDollarID().String(), doc["quote_asset_id"])
	assert.Equal(t, "SBX-USD", doc["quote_symbol"])
	assert.EqualValues(t, 6, doc["quote_decimals"])
	assert.Equal(t, true, doc["quote_provisioned"])
	assert.EqualValues(t, 300000, doc["max_mark_age_ms"])
	assert.Equal(t, "2026-09-06T12:00:00Z", doc["as_of"])

	balances := doc["balances"].([]any)
	require.Len(t, balances, 3)
	for i := range balances {
		assert.Equal(t, "SIMULATED", portfolioAt(t, doc, "balances", i, "value_domain"))
	}

	acme := balances[0].(map[string]any)
	assert.Equal(t, eqShare.String(), acme["asset_id"])
	assert.Equal(t, "ACME", acme["symbol"])
	assert.Equal(t, "nodal-sandbox", acme["chain"])
	assert.EqualValues(t, 6, acme["decimals"])
	assert.Equal(t, "3000000", acme["wallet"])
	assert.Equal(t, "0", acme["reserved"])
	assert.Equal(t, "3000000", acme["available"])
	assert.Equal(t, "5000000", acme["total"])
	assert.Equal(t, []any{map[string]any{"code": "UNSETTLED_RECEIVABLE", "quantity": "2000000"}}, acme["held_elsewhere"])
	assert.Equal(t, []any{}, acme["owed"])
	assert.Equal(t, []any{}, acme["reservations"])
	assert.Equal(t, map[string]any{
		"status": "MARKED", "value": "750000000",
		"mark": map[string]any{
			"price": "150.00", "mantissa": "15000", "scale": float64(2), "source": "sandbox-feed",
			"observed_at": "2026-09-06T11:59:30Z", "age_ms": float64(30000),
		},
	}, acme["valuation"])

	yes := balances[1].(map[string]any)
	assert.Equal(t, "sbx-solana", yes["chain"], "the same unit on another chain is another asset")
	assert.Equal(t, map[string]any{
		"status": "UNVALUED",
		"problem": map[string]any{
			"code": "STALE_MARKET_DATA", "detail": "no mark of PMHTTP-YES in the sandbox dollar within the maximum age (5m0s)",
		},
	}, yes["valuation"], "an asset no mark values carries no value at all, not a zero")

	sbx := balances[2].(map[string]any)
	assert.Equal(t, "1000000000", sbx["wallet"])
	assert.Equal(t, "250000000", sbx["reserved"])
	assert.Equal(t, "0", sbx["withdrawal_holds"])
	assert.Equal(t, "750000000", sbx["available"])
	assert.Equal(t, "2296250000", sbx["total"])
	assert.Equal(t, []any{
		map[string]any{"purpose": "OPEN_ORDERS", "capability": "INTERNAL_MATCHING", "quantity": "200000000", "reservations": float64(2)},
		map[string]any{"purpose": "PENDING_TRANSFERS", "capability": "P2P_TRANSFERS", "quantity": "50000000", "reservations": float64(1)},
	}, sbx["reservations"])
	assert.Equal(t, []any{
		map[string]any{"code": "MARGIN_COLLATERAL", "quantity": "500000000"},
		map[string]any{"code": "DERIVATIVES_MARGIN", "quantity": "796250000"},
	}, sbx["held_elsewhere"])
	assert.Equal(t, []any{map[string]any{"code": "MARGIN_LOAN", "quantity": "100000000"}}, sbx["owed"])
	assert.Equal(t, map[string]any{"status": "PAR", "value": "2296250000", "owed_value": "100000000"}, sbx["valuation"])

	risk := doc["risk"].(map[string]any)
	assert.Equal(t, "ACTIVE", risk["account_status"])
	assert.Equal(t, true, risk["blocks_new_risk"])
	assert.Equal(t, []any{map[string]any{
		"id": portfolioRecord.String(), "kind": "ECONOMIC_EFFECT", "status": "OPEN", "material": true, "blocks_new_risk": true,
		"scope_type": "ECONOMIC_EFFECT", "asset_id": econ.SandboxDollarID().String(), "opened_at": "2026-09-06T11:00:00Z",
	}}, risk["reconciliation"])
	assert.Equal(t, false, risk["reconciliation_truncated"])
	effects := risk["unknown_effects"].([]any)
	require.Len(t, effects, 1)
	effect := effects[0].(map[string]any)
	assert.Equal(t, portfolioEffect.String(), effect["id"])
	assert.Equal(t, "UNKNOWN", effect["status"])
	assert.Equal(t, "BRIDGE", effect["subsystem"])
	assert.Equal(t, "devnet-tx-1", effect["external_ref"])
}

// A deployment that has not provisioned the sandbox dollar still answers the
// portfolio: it says so, names the unit by its well-known identity, and every
// balance is UNVALUED with that reason.
func TestPortfolio_WithoutTheSandboxDollarNothingIsValued(t *testing.T) {
	t.Parallel()
	h, fp := portfolioOnlyHarness(t)
	snap := portfolioSampleSnapshot()
	snap.QuoteProvisioned = false
	problem := errs.New(errs.CodeUnsupported, "the sandbox dollar is not provisioned in this deployment, so there is nothing to value the asset in")
	for i := range snap.Balances {
		snap.Balances[i].Valuation = PortfolioValuation{Status: PortfolioValuationUnvalued, Problem: problem}
	}
	fp.snap = snap
	res := h.do(http.MethodGet, portfolioPath(testAccountID), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	doc := res.raw()
	assert.Equal(t, false, doc["quote_provisioned"])
	assert.Equal(t, econ.SandboxDollarID().String(), doc["quote_asset_id"])
	for i := range doc["balances"].([]any) {
		assert.Equal(t, "UNVALUED", portfolioAt(t, doc, "balances", i, "valuation", "status"))
		assert.Equal(t, "UNSUPPORTED", portfolioAt(t, doc, "balances", i, "valuation", "problem", "code"))
	}
}

// Every section that is a subsystem's read is exactly what that subsystem's
// own route answers, compared byte for byte with the route's own response,
// and each was asked for the bounded, filtered page the portfolio documents.
func TestPortfolio_SectionsAreTheSubsystemsOwnReads(t *testing.T) {
	t.Parallel()
	h, fx := newPortfolioHarness(t)
	res := h.do(http.MethodGet, portfolioPath(testAccountID), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	doc := res.raw()

	// What each composed read was asked for.
	assert.Equal(t, "OPEN", string(fx.derivatives.lastStatus))
	assert.Equal(t, portfolioListBound, fx.derivatives.lastLimit)
	assert.True(t, fx.books.lastWorking)
	assert.Equal(t, portfolioListBound, fx.books.lastLimit)
	assert.Equal(t, EquitySettlementsPending, fx.equities.lastState)
	assert.Equal(t, portfolioListBound, fx.equities.lastLimit)
	assert.Equal(t, portfolioDecisionLimit, fx.advanced.lastLimit)
	assert.Equal(t, testAccountID, fx.predmarket.lastAccount)

	acct := testAccountID.String()
	same := func(route string, path ...any) {
		t.Helper()
		own := h.do(http.MethodGet, route, nil)
		require.Equal(t, http.StatusOK, own.Code, "%s body=%s", route, own.Body.String())
		assert.JSONEq(t, own.Body.String(), portfolioJSON(t, portfolioAt(t, doc, path...)), "%v is %s's own answer", path, route)
	}
	for _, section := range []string{"buying_power"} {
		assert.Equal(t, "PRESENT", portfolioAt(t, doc, section, "state"))
	}
	same("/v1/advanced/buying-power?account_id="+acct, "buying_power", "buying_power")
	same("/v1/advanced/margin/account?account_id="+acct, "positions", "margin", "account")
	same("/v1/advanced/derivatives/positions?status=OPEN&limit=50&account_id="+acct, "positions", "derivatives", "positions")
	same("/v1/advanced/prediction-markets/"+pmMarketID.String()+"/positions?account_id="+acct,
		"positions", "prediction_markets", "markets", 0, "positions")
	same("/v1/advanced/books/orders?scope=WORKING&limit=50&account_id="+acct, "positions", "order_books", "orders")

	// The equities double mints a fresh journal id for its settled sample on
	// every call, so that one field is set aside before comparing.
	settlements := h.do(http.MethodGet, "/v1/advanced/equities/settlements?state=PENDING&limit=50&account_id="+acct, nil)
	require.Equal(t, http.StatusOK, settlements.Code, "body=%s", settlements.Body.String())
	withoutJournal := func(page any) string {
		t.Helper()
		for _, item := range portfolioAt(t, page, "items").([]any) {
			delete(item.(map[string]any), "journal_transaction_id")
		}
		return portfolioJSON(t, page)
	}
	assert.JSONEq(t, withoutJournal(settlements.raw()), withoutJournal(portfolioAt(t, doc, "positions", "equities", "pending_settlements")))

	decisions := h.do(http.MethodGet, "/v1/advanced/risk/decisions?limit=20&account_id="+acct, nil)
	require.Equal(t, http.StatusOK, decisions.Code, "body=%s", decisions.Body.String())
	assert.JSONEq(t, portfolioJSON(t, decisions.raw()["items"]), portfolioJSON(t, portfolioAt(t, doc, "risk", "decisions", "items")))

	// The market a holding is in, as the market list renders it.
	markets := h.do(http.MethodGet, "/v1/advanced/prediction-markets?status=SETTLED&limit=200", nil)
	require.Equal(t, http.StatusOK, markets.Code, "body=%s", markets.Body.String())
	assert.JSONEq(t, portfolioJSON(t, portfolioAt(t, markets.raw(), "items", 0)),
		portfolioJSON(t, portfolioAt(t, doc, "positions", "prediction_markets", "markets", 0, "market")))
	assert.Equal(t, false, portfolioAt(t, doc, "positions", "prediction_markets", "truncated"))

	// The listing whose shares the account holds, as the listings route
	// renders it, with the share balances from the balances read.
	listings := h.do(http.MethodGet, "/v1/advanced/equities/listings?limit=200", nil)
	require.Equal(t, http.StatusOK, listings.Code, "body=%s", listings.Body.String())
	holdings := portfolioAt(t, doc, "positions", "equities", "holdings").([]any)
	require.Len(t, holdings, 1)
	assert.JSONEq(t, portfolioJSON(t, portfolioAt(t, listings.raw(), "items", 0)), portfolioJSON(t, portfolioAt(t, holdings[0], "listing")))
	assert.Equal(t, "3000000", portfolioAt(t, holdings[0], "settled"))
	assert.Equal(t, "2000000", portfolioAt(t, holdings[0], "unsettled"))
	assert.Equal(t, false, portfolioAt(t, doc, "positions", "equities", "listings_truncated"))

	for _, section := range []string{"margin", "derivatives", "prediction_markets", "equities", "order_books"} {
		assert.Equal(t, "PRESENT", portfolioAt(t, doc, "positions", section, "state"), section)
		assert.Equal(t, "SIMULATED", portfolioAt(t, doc, "positions", section, "value_domain"), section)
	}
	assert.Equal(t, "PRESENT", portfolioAt(t, doc, "risk", "decisions", "state"))
	assert.Equal(t, "SIMULATED", portfolioAt(t, doc, "buying_power", "value_domain"))
}

// A deployment that runs none of the subsystems still answers the portfolio,
// and says, section by section, that each one is not configured here: none
// is left out, and none is drawn as an empty answer.
func TestPortfolio_UnconfiguredSubsystemsAreReportedNotOmitted(t *testing.T) {
	t.Parallel()
	h, _ := portfolioOnlyHarness(t)
	res := h.do(http.MethodGet, portfolioPath(testAccountID), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	doc := res.raw()

	sections := map[string][]any{
		"buying power":       {"buying_power"},
		"margin":             {"positions", "margin"},
		"derivatives":        {"positions", "derivatives"},
		"prediction markets": {"positions", "prediction_markets"},
		"equities":           {"positions", "equities"},
		"order books":        {"positions", "order_books"},
		"risk decisions":     {"risk", "decisions"},
	}
	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		section := portfolioAt(t, doc, sections[name]...).(map[string]any)
		assert.Equal(t, "NOT_CONFIGURED", section["state"], name)
		assert.NotContains(t, section, "problem", name)
		for _, payload := range []string{"buying_power", "account", "positions", "pending_settlements", "orders"} {
			assert.NotContains(t, section, payload, "%s carries no %s it did not read", name, payload)
		}
	}
	assert.Equal(t, []any{}, portfolioAt(t, doc, "positions", "prediction_markets", "markets"),
		"the markets the snapshot found are not read without the subsystem")
	assert.Equal(t, []any{}, portfolioAt(t, doc, "positions", "equities", "holdings"))
	assert.Equal(t, []any{}, portfolioAt(t, doc, "risk", "decisions", "items"))
	// The portfolio's own reads are whole regardless.
	assert.Len(t, doc["balances"], 3)
	assert.Len(t, portfolioAt(t, doc, "risk", "unknown_effects"), 1)
}

// A subsystem's refusal is a state of its section, with the code its own
// route answers with; a margin read's NOT_FOUND is an account without a
// margin account. Other sections are not affected by a refusal.
func TestPortfolio_ARefusalIsTheStateOfItsSectionOnly(t *testing.T) {
	t.Parallel()
	h, fx := newPortfolioHarness(t)
	fx.margin.err = errs.New(errs.CodeNotFound, "the account has no margin account")
	fx.derivatives.err = errs.New(errs.CodeStaleMarketData, "the contract has no mark within its oracle bound")
	fx.advanced.err = errs.New(errs.CodeUnsupported, "the sandbox dollar is not provisioned in this deployment")
	fx.predmarket.err = errs.New(errs.CodeProviderUnavailable, "the prediction market read could not be completed in time")

	res := h.do(http.MethodGet, portfolioPath(testAccountID), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	doc := res.raw()

	margin := portfolioAt(t, doc, "positions", "margin").(map[string]any)
	assert.Equal(t, "NONE", margin["state"])
	assert.NotContains(t, margin, "problem", "having no margin account is not a refusal")
	assert.NotContains(t, margin, "account")

	unavailable := map[string]struct {
		path []any
		code string
	}{
		"derivatives":        {[]any{"positions", "derivatives"}, "STALE_MARKET_DATA"},
		"buying power":       {[]any{"buying_power"}, "UNSUPPORTED"},
		"risk decisions":     {[]any{"risk", "decisions"}, "UNSUPPORTED"},
		"prediction markets": {[]any{"positions", "prediction_markets"}, "PROVIDER_UNAVAILABLE"},
	}
	for name, want := range unavailable {
		section := portfolioAt(t, doc, want.path...).(map[string]any)
		assert.Equal(t, "UNAVAILABLE", section["state"], name)
		assert.Equal(t, want.code, portfolioAt(t, section, "problem", "code"), name)
		assert.NotEmpty(t, portfolioAt(t, section, "problem", "detail"), name)
	}
	assert.Equal(t, []any{}, portfolioAt(t, doc, "positions", "prediction_markets", "markets"),
		"a section that refused shows nothing it did not read")
	assert.Equal(t, "PRESENT", portfolioAt(t, doc, "positions", "equities", "state"))
	assert.Equal(t, "PRESENT", portfolioAt(t, doc, "positions", "order_books", "state"))
}

// An internal fault in a composed read is not a state of a section: the
// whole read fails, so the fault is visible where faults are recorded.
func TestPortfolio_AnInternalFaultFailsTheWholeRead(t *testing.T) {
	t.Parallel()
	h, fx := newPortfolioHarness(t)
	fx.books.err = errs.New(errs.CodeInternal, "the order book read broke")
	res := h.do(http.MethodGet, portfolioPath(testAccountID), nil)
	require.Equal(t, http.StatusInternalServerError, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInternal, res.problem().Code)
	assert.NotContains(t, res.Body.String(), "order book read broke", "an internal detail never leaves the server")
}

// A snapshot of an account other than the one asked about is a fault, never
// shown.
func TestPortfolio_ASnapshotOfAnotherAccountIsAFault(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ports := h.ports.ports()
	ports.Portfolio = portfolioOtherAccount{}
	h.server.opts.Ports = ports
	res := h.do(http.MethodGet, portfolioPath(testAccountID), nil)
	require.Equal(t, http.StatusInternalServerError, res.Code, "body=%s", res.Body.String())
	assert.NotContains(t, res.Body.String(), testOtherAcct.String())
}

type portfolioOtherAccount struct{}

func (portfolioOtherAccount) Snapshot(context.Context, accounts.AccountID) (PortfolioSnapshot, error) {
	s := portfolioSampleSnapshot()
	s.AccountID = testOtherAcct
	return s, nil
}

func TestPortfolio_UnwiredPortIsUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res := h.do(http.MethodGet, portfolioPath(testAccountID), nil)
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
}

func TestPortfolio_AnAgentNeverReachesIt(t *testing.T) {
	t.Parallel()
	h, fp := portfolioOnlyHarness(t)
	agent := agentPrincipal()
	h.as(&agent)
	res := h.do(http.MethodGet, portfolioPath(testAccountID), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	calls, _ := fp.asked()
	assert.Zero(t, calls)
}

// The portfolio runs other routes' handlers in-process, past the
// authorization middleware that guards each route. That is sound only while
// each of them demands exactly what the portfolio does: the same permissions,
// no step-up, no mutation. A composed read held to a stricter policy would
// make the portfolio a way around it, and this fails first.
func TestPortfolio_ComposesOnlyReadsUnderItsOwnPolicy(t *testing.T) {
	t.Parallel()
	own, ok := policyFor("GetAdvancedPortfolio")
	require.True(t, ok)
	require.ElementsMatch(t, []security.Permission{security.PermAccountRead, security.PermAccountReadAny}, own.AnyOf)
	require.False(t, own.StepUp)
	require.False(t, own.Mutating)
	for _, op := range []string{
		"GetAdvancedBuyingPower", "GetAdvancedRiskDecisions", "GetAdvancedMarginAccount", "GetAdvancedDerivativesPositions",
		"GetAdvancedPredictionMarketsMarketIdPositions", "GetAdvancedEquitiesListings", "GetAdvancedEquitiesSettlements",
		"GetAdvancedBooksOrders",
	} {
		pol, ok := policyFor(op)
		require.True(t, ok, op)
		assert.ElementsMatch(t, own.AnyOf, pol.AnyOf, op)
		assert.Equal(t, own.StepUp, pol.StepUp, op)
		assert.Equal(t, own.Mutating, pol.Mutating, op)
		assert.Equal(t, own.AllowAgent, pol.AllowAgent, op)
	}
}
