//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// pmWorld is the real prediction market service over the integration
// database, every dependency real and on one fake clock, so a test can walk a
// market past its close time.
type pmWorld struct {
	t      *testing.T
	d      *db.DB
	clk    *clock.Fake
	ledger *ledger.Service
	oracle *predmarket.SandboxOracle
	ports  Ports
}

func newPMWorld(t *testing.T, d *db.DB) *pmWorld {
	t.Helper()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("httpapi-predmarket-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return false }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	seed := sha256.Sum256([]byte("httpapi-predmarket-itest-oracle"))
	oracle, err := predmarket.NewSandboxOracle("itest-election-board", seed[:])
	require.NoError(t, err)
	led := ledger.NewService(clk, "httpapi-predmarket-itest")
	authz := econ.NewAuthorizer(checker, kills)
	intents, effects := econ.NewIntentStore(clk, out, aw), econ.NewEffectStore(clk, out, aw)
	svc, err := predmarket.NewService(predmarket.Config{
		Clock: clk, Intents: intents, Effects: effects, Authorizer: authz, Registry: econ.NewRegistry(), KillSwitches: kills,
		Ledger: led, Capital: capital.NewService(clk, capital.OutboxEmitter{Outbox: out, Clock: clk}), Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Instruments: instruments.NewRepository(),
		Assets: assets.NewRepository(), Reconciliation: reconciliation.NewRepository(clk, out, aw), Audit: aw, SandboxOracle: oracle,
	})
	require.NoError(t, err)
	ports, err := Wire(WireDeps{
		DB: d, Clock: clk, Env: "TEST", Assets: assets.NewRepository(), Instruments: instruments.NewRepository(),
		Advanced: AdvancedDeps{
			Authorizer: authz, Intents: intents, Effects: effects, Risk: risk.NewStore(),
			PredictionMarkets: svc, PredictionOracle: oracle,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, ports.PredictionMarkets, "the service wires the port")
	require.NoError(t, d.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))
	return &pmWorld{t: t, d: d, clk: clk, ledger: led, oracle: oracle, ports: ports}
}

// user creates a platform user whose identity provider asserted an email, so
// the account it owns is NODAL_IDENTITY-verified for Risk V2.
func (w *pmWorld) user() accounts.UserID {
	w.t.Helper()
	sum := sha256.Sum256([]byte(uuid.NewString()))
	u, err := accounts.NewRepository().CreateUser(w.t.Context(), w.d, "httpapi-predmarket-itest", "sub-"+uuid.NewString(), sum[:])
	require.NoError(w.t, err)
	return u.ID
}

func (w *pmWorld) customer(units int64) (accounts.UserID, accounts.AccountID) {
	w.t.Helper()
	u := w.user()
	a, err := accounts.NewRepository().CreateAccount(w.t.Context(), w.d, u, accounts.KindCustomer)
	require.NoError(w.t, err)
	if units > 0 {
		amount := money.QuantityFromInt64(units * 1_000_000)
		w.post(ledger.Posting{
			Kind: ledger.KindSandboxFunding, EffectiveAt: w.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(a.ID, ledger.CodeWallet, econ.SandboxDollarID()), Side: ledger.Debit, Quantity: amount},
				{Account: ledger.CustomerAccount(a.ID, ledger.CodeCapital, econ.SandboxDollarID()), Side: ledger.Credit, Quantity: amount},
			},
		})
	}
	return u, a.ID
}

func (w *pmWorld) post(p ledger.Posting) {
	w.t.Helper()
	ref := uuid.NewString()
	p.IdempotencyKey = "pm-itest:" + ref
	p.Reference = ledger.FinancialEventReference{Type: "pm_itest_" + strings.ToLower(string(p.Kind)), ID: ref}
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.ledger.Post(ctx, tx, p)
		return err
	}))
}

// trade is a TEST-ONLY stand-in for the internal order book, which is not
// merged: the per-party balanced SIM_TRADE a matched trade of outcome shares
// against collateral posts. It lets the holders at settlement differ from the
// holders at issuance, which is what makes the payout check meaningful.
func (w *pmWorld) trade(seller, buyer accounts.AccountID, share assets.AssetID, shares, price int64) {
	w.t.Helper()
	s, p := money.QuantityFromInt64(shares), money.QuantityFromInt64(price)
	usd := econ.SandboxDollarID()
	w.post(ledger.Posting{
		Kind: ledger.KindSimTrade, EffectiveAt: w.clk.Now(),
		Entries: []ledger.Entry{
			{Account: ledger.CustomerAccount(seller, ledger.CodeWallet, share), Side: ledger.Credit, Quantity: s},
			{Account: ledger.CustomerAccount(seller, ledger.CodeTradingOutflow, share), Side: ledger.Debit, Quantity: s},
			{Account: ledger.CustomerAccount(buyer, ledger.CodeWallet, share), Side: ledger.Debit, Quantity: s},
			{Account: ledger.CustomerAccount(buyer, ledger.CodeTradingInflow, share), Side: ledger.Credit, Quantity: s},
			{Account: ledger.CustomerAccount(buyer, ledger.CodeWallet, usd), Side: ledger.Credit, Quantity: p},
			{Account: ledger.CustomerAccount(buyer, ledger.CodeTradingOutflow, usd), Side: ledger.Debit, Quantity: p},
			{Account: ledger.CustomerAccount(seller, ledger.CodeWallet, usd), Side: ledger.Debit, Quantity: p},
			{Account: ledger.CustomerAccount(seller, ledger.CodeTradingInflow, usd), Side: ledger.Credit, Quantity: p},
		},
	})
}

func (w *pmWorld) wallet(acct accounts.AccountID, asset assets.AssetID) string {
	w.t.Helper()
	b, err := w.ledger.Balance(w.t.Context(), w.d, ledger.CustomerAccount(acct, ledger.CodeWallet, asset))
	require.NoError(w.t, err)
	return b.String()
}

func pmPrincipal(user accounts.UserID, actor security.ActorType, role security.Role, owned ...accounts.AccountID) security.Principal {
	ids := make([]string, 0, len(owned))
	for _, a := range owned {
		ids = append(ids, a.String())
	}
	return security.Principal{
		SubjectID: user.String(), ActorType: actor, Roles: []security.Role{role}, AccountIDs: ids,
		SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
}

// TestIntegration_PredictionMarketLifecycleOverHTTP walks one market through
// its whole life over HTTP, against the real adapter and database: an operator
// defines and opens it, two customers issue complete sets (one also redeems
// one), shares change hands, the market closes, the sandbox oracle's verified
// report resolves it -- after the creator's attempt is refused by dual
// control -- and a second operator settles it. Each winning share pays exactly
// one collateral unit, the escrow is emptied, the collateral the customers
// were funded with is conserved to the base unit, a second settlement is a
// replay that pays nothing, and a stranger cannot read a customer's shares.
func TestIntegration_PredictionMarketLifecycleOverHTTP(t *testing.T) {
	d := openTestDB(t)
	w := newPMWorld(t, d)
	creator, resolver := w.user(), w.user()
	c1User, c1 := w.customer(100)
	c2User, c2 := w.customer(100)
	strangerUser, stranger := w.customer(0)

	h := newIntegrationHarness(t, d, c1, c1User)
	h.server.opts.Ports.PredictionMarkets = w.ports.PredictionMarkets
	opA := pmPrincipal(creator, security.ActorOperator, security.RoleAdmin)
	opB := pmPrincipal(resolver, security.ActorOperator, security.RoleAdmin)
	cust1 := pmPrincipal(c1User, security.ActorUser, security.RoleCustomer, c1)
	cust2 := pmPrincipal(c2User, security.ActorUser, security.RoleCustomer, c2)
	other := pmPrincipal(strangerUser, security.ActorUser, security.RoleCustomer, stranger)
	keys := 0
	key := func() string { keys++; return "pm-itest-" + uuid.NewString()[:8] + "-" + itoa(keys) }

	// 1. The operator defines the market and opens it.
	h.as(&opA)
	openAt := w.clk.Now()
	closeAt := openAt.Add(time.Hour)
	symbol := "ITPM-" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:10])
	res := h.do(http.MethodPost, "/v1/advanced/prediction-markets", map[string]any{
		"symbol": symbol, "question": "Which outcome will the sandbox election board certify?",
		"outcomes": []string{"YES", "NO"}, "mode": "SANDBOX", "open_at": openAt, "close_at": closeAt,
		"dispute_window_ms": 0, "oracle_source": w.oracle.Source(), "oracle_public_key": hex.EncodeToString(w.oracle.PublicKey()),
	}, "Idempotency-Key", key())
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var market api.PredictionMarket
	res.json(&market)
	assert.Equal(t, "DRAFT", market.Status)
	assert.Equal(t, econ.SandboxDollarID().String(), market.CollateralAssetId.String())
	assert.Equal(t, econ.SandboxDollarSymbol, market.CollateralSymbol)
	assert.Equal(t, "1000000", market.UnitQuantity)
	require.Len(t, market.Outcomes, 2)
	yes, no := market.Outcomes[0], market.Outcomes[1]
	require.Equal(t, "YES", yes.Symbol)
	yesShare, err := assets.ParseAssetID(yes.ShareAssetId.String())
	require.NoError(t, err)
	noShare, err := assets.ParseAssetID(no.ShareAssetId.String())
	require.NoError(t, err)
	base := "/v1/advanced/prediction-markets/" + market.Id.String()

	res = h.do(http.MethodPost, base+"/open", nil, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&market)
	require.Equal(t, "OPEN", market.Status)

	// The market is listed as OPEN to a customer.
	h.as(&cust1)
	res = h.do(http.MethodGet, "/v1/advanced/prediction-markets?status=OPEN&limit=200", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var list api.PredictionMarketList
	res.json(&list)
	listed := false
	for _, m := range list.Items {
		listed = listed || m.Id == market.Id
	}
	assert.True(t, listed, "the open market is listed")

	// 2. Two customers issue complete sets; the first also redeems one.
	issueKey := key()
	res = h.do(http.MethodPost, base+"/issuances", map[string]any{"account_id": c1.String(), "quantity": "3000000"}, "Idempotency-Key", issueKey)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var issued api.PredictionSetResult
	res.json(&issued)
	assert.Equal(t, api.ValueDomainSimulated("SIMULATED"), issued.ValueDomain)
	assert.Equal(t, "COMPLETED", issued.Intent.Status)
	assert.Equal(t, "3000000", issued.Quantity)
	require.NotNil(t, issued.JournalTransactionId)
	require.NotNil(t, issued.Effect)
	assert.Equal(t, api.EconomicEffectStatus("APPLIED"), issued.Effect.Status)
	first := res.Body.String()

	// The same key replays the first answer; nothing is issued twice.
	res = h.do(http.MethodPost, base+"/issuances", map[string]any{"account_id": c1.String(), "quantity": "3000000"}, "Idempotency-Key", issueKey)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.JSONEq(t, first, res.Body.String())
	assert.Equal(t, "97000000", w.wallet(c1, econ.SandboxDollarID()), "3 sets cost 3 units, once")

	res = h.do(http.MethodPost, base+"/redemptions", map[string]any{"account_id": c1.String(), "quantity": "1000000"}, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	h.as(&cust2)
	res = h.do(http.MethodPost, base+"/issuances", map[string]any{"account_id": c2.String(), "quantity": "2000000"}, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	// A set is whole: a fraction of one is the service's refusal.
	res = h.do(http.MethodPost, base+"/issuances", map[string]any{"account_id": c2.String(), "quantity": "1500000"}, "Idempotency-Key", key())
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)

	// The escrow holds exactly the outstanding complete sets.
	res = h.do(http.MethodGet, base+"/books", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var books api.PredictionEscrowBooks
	res.json(&books)
	assert.Equal(t, "4000000", books.Escrow)
	assert.Equal(t, "4000000", books.Collateral)
	assert.True(t, books.Reconciles, "violations: %v", books.Violations)
	for _, o := range books.Outcomes {
		assert.Equal(t, "4000000", o.SharesOutstanding, o.Symbol)
		assert.Equal(t, "4000000", o.HeldByCustomers, o.Symbol)
	}

	// Shares change hands (test-only stand-in for the order book): the second
	// customer sells one YES share to the first for 0.40 units.
	w.trade(c2, c1, yesShare, 1_000_000, 400_000)

	h.as(&cust1)
	res = h.do(http.MethodGet, base+"/positions?account_id="+c1.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var positions api.PredictionPositionList
	res.json(&positions)
	held := map[string]string{}
	for _, p := range positions.Items {
		held[p.OutcomeSymbol] = p.Quantity
	}
	assert.Equal(t, map[string]string{"YES": "3000000", "NO": "2000000"}, held)

	// A stranger cannot read the customer's shares.
	h.as(&other)
	res = h.do(http.MethodGet, base+"/positions?account_id="+c1.String(), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	// Nor issue sets on it.
	res = h.do(http.MethodPost, base+"/issuances", map[string]any{"account_id": c1.String(), "quantity": "1000000"}, "Idempotency-Key", key())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())

	// 3. Closing waits for the close time; then the market closes and asks for
	// an oracle report.
	h.as(&opA)
	res = h.do(http.MethodPost, base+"/close", nil, "Idempotency-Key", key())
	require.Equal(t, http.StatusConflict, res.Code, "nothing closes before its close time; body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidStateTransition, res.problem().Code)
	w.clk.Set(closeAt.Add(time.Minute))
	res = h.do(http.MethodPost, base+"/close", nil, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&market)
	require.Equal(t, "CLOSED", market.Status)

	h.as(&opB)
	res = h.do(http.MethodPost, base+"/request-resolution", nil, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&market)
	require.Equal(t, "RESOLVING", market.Status)

	// 4. The sandbox oracle observes YES, and its report verifies.
	h.as(&opA)
	res = h.do(http.MethodPost, base+"/oracle-observations", map[string]any{
		"outcome": "YES", "observed_at": closeAt.Add(30 * time.Second), "evidence": `{"certified":"YES","board":"sandbox"}`,
	}, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var observation api.PredictionOracleObservation
	res.json(&observation)
	wire, err := json.Marshal(observation.Report)
	require.NoError(t, err)
	report, err := predmarket.DecodeOracleReport(wire)
	require.NoError(t, err)
	require.NoError(t, predmarket.VerifyReport(w.oracle.PublicKey(), report))

	// The market's creator may not resolve it: the service refuses, and the
	// refusal reaches the client with its code.
	res = h.do(http.MethodPost, base+"/resolve", map[string]any{}, "Idempotency-Key", key())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	refusal := res.problem()
	assert.Equal(t, errs.CodeForbidden, refusal.Code)
	assert.Contains(t, refusal.Detail, "creator")

	// Another operator resolves it from the verified report.
	h.as(&opB)
	res = h.do(http.MethodPost, base+"/resolve", map[string]any{"reason": "the board certified YES"}, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var resolved api.PredictionResolutionResult
	res.json(&resolved)
	assert.Equal(t, "RESOLVED", resolved.Market.Status)
	assert.Equal(t, api.PredictionResolutionKind("ORACLE"), resolved.Resolution.Kind)
	assert.Equal(t, "YES", resolved.Resolution.OutcomeSymbol)
	require.NotNil(t, resolved.Resolution.ReportHash)
	assert.Equal(t, observation.ReportHash, *resolved.Resolution.ReportHash, "the accepted report is the one the oracle signed")
	assert.Equal(t, resolver.String(), resolved.Resolution.AcceptedBy.ActorId)

	// 5. Settlement: every winning share pays one unit, from the escrow.
	walletsBefore := map[accounts.AccountID]string{
		c1: w.wallet(c1, econ.SandboxDollarID()), c2: w.wallet(c2, econ.SandboxDollarID()),
	}
	assert.Equal(t, "97600000", walletsBefore[c1], "100 - 3 + 1 - 0.40")
	assert.Equal(t, "98400000", walletsBefore[c2], "100 - 2 + 0.40")
	res = h.do(http.MethodPost, base+"/settle", nil, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var settled api.PredictionSettlementResult
	res.json(&settled)
	assert.False(t, settled.Replay)
	assert.Equal(t, "SETTLED", settled.Market.Status)
	assert.Equal(t, "PAYOUT", settled.Settlement.Kind)
	assert.Equal(t, "4000000", settled.Settlement.EscrowBefore)
	assert.Equal(t, "4000000", settled.Settlement.TotalPaid)
	assert.Equal(t, "0", settled.Settlement.Residual)
	require.NotNil(t, settled.Settlement.WinningOutcomeSymbol)
	assert.Equal(t, "YES", *settled.Settlement.WinningOutcomeSymbol)
	paid := map[string]string{}
	for _, l := range settled.Lines {
		paid[l.AccountId.String()] = l.Amount
		require.NotNil(t, l.JournalTransactionId, "every payout is a journal transaction")
	}
	assert.Equal(t, map[string]string{c1.String(): "3000000", c2.String(): "1000000"}, paid,
		"one unit per winning share held at settlement")
	order := []string{settled.Lines[0].AccountId.String(), settled.Lines[1].AccountId.String()}
	assert.True(t, sort.StringsAreSorted(order), "payout lines are in account order")

	assert.Equal(t, "100600000", w.wallet(c1, econ.SandboxDollarID()), "97.60 + 3 winning shares")
	assert.Equal(t, "99400000", w.wallet(c2, econ.SandboxDollarID()), "98.40 + 1 winning share")
	sum := money.QuantityFromInt64(0)
	for _, acct := range []accounts.AccountID{c1, c2} {
		q, err := money.ParseQuantity(w.wallet(acct, econ.SandboxDollarID()))
		require.NoError(t, err)
		sum = sum.Add(q)
	}
	assert.Equal(t, "200000000", sum.String(), "the 200 units the customers were funded with are conserved")
	for _, acct := range []accounts.AccountID{c1, c2} {
		assert.Equal(t, "0", w.wallet(acct, yesShare), "every share is retired")
		assert.Equal(t, "0", w.wallet(acct, noShare), "every share is retired")
	}

	// 6. A second settlement is a replay: the record, and nothing paid.
	res = h.do(http.MethodPost, base+"/settle", nil, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var again api.PredictionSettlementResult
	res.json(&again)
	assert.True(t, again.Replay)
	assert.Empty(t, again.Lines)
	assert.Equal(t, settled.Settlement.Id, again.Settlement.Id)
	assert.Equal(t, "100600000", w.wallet(c1, econ.SandboxDollarID()))
	assert.Equal(t, "99400000", w.wallet(c2, econ.SandboxDollarID()))

	// 7. The books are empty and reconcile, and the reconciliation records it.
	res = h.do(http.MethodGet, base+"/books", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&books)
	assert.Equal(t, "0", books.Escrow)
	assert.True(t, books.Reconciles, "violations: %v", books.Violations)
	for _, o := range books.Outcomes {
		assert.Equal(t, "0", o.SharesOutstanding, o.Symbol)
	}
	res = h.do(http.MethodPost, base+"/reconcile", nil, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var rec api.PredictionEscrowReconciliation
	res.json(&rec)
	assert.Equal(t, "PREDICTION_ESCROW", rec.Kind)
	assert.Equal(t, "MATCHED", rec.Status)
	assert.False(t, rec.Material)

	// 8. What a customer reads afterwards.
	h.as(&cust1)
	res = h.do(http.MethodGet, base, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var detail api.PredictionMarketDetail
	res.json(&detail)
	assert.Equal(t, "SETTLED", detail.Market.Status)
	require.Len(t, detail.Resolutions, 1)
	require.NotNil(t, detail.Settlement)
	assert.Equal(t, settled.Settlement.Id, detail.Settlement.Id)
	res = h.do(http.MethodGet, base+"/positions?account_id="+c1.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&positions)
	assert.Empty(t, positions.Items, "settled shares are retired")
}

// TestIntegration_PredictionMarketRefusalsReachTheClient: a refused issuance
// keeps its REJECTED economic intent on record, neither the disputer nor the
// author of the disputed resolution may rule on a dispute, and the creator may
// not propose to void the market.
func TestIntegration_PredictionMarketRefusalsReachTheClient(t *testing.T) {
	d := openTestDB(t)
	w := newPMWorld(t, d)
	creator, resolver, disputer, ruler := w.user(), w.user(), w.user(), w.user()
	cUser, cAcct := w.customer(1)
	h := newIntegrationHarness(t, d, cAcct, cUser)
	h.server.opts.Ports.PredictionMarkets = w.ports.PredictionMarkets
	opA := pmPrincipal(creator, security.ActorOperator, security.RoleAdmin)
	opB := pmPrincipal(resolver, security.ActorOperator, security.RoleAdmin)
	opC := pmPrincipal(disputer, security.ActorOperator, security.RoleAdmin)
	opD := pmPrincipal(ruler, security.ActorOperator, security.RoleAdmin)
	cust := pmPrincipal(cUser, security.ActorUser, security.RoleCustomer, cAcct)
	key := func() string { return "pm-itest-" + uuid.NewString() }

	h.as(&opA)
	closeAt := w.clk.Now().Add(time.Hour)
	res := h.do(http.MethodPost, "/v1/advanced/prediction-markets", map[string]any{
		"symbol":   "ITPMR-" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:10]),
		"question": "Will the sandbox recount change the certified outcome?", "outcomes": []string{"YES", "NO"},
		"mode": "SANDBOX", "open_at": w.clk.Now(), "close_at": closeAt, "dispute_window_ms": 600000,
		"oracle_source": w.oracle.Source(), "oracle_public_key": hex.EncodeToString(w.oracle.PublicKey()),
	}, "Idempotency-Key", key())
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var market api.PredictionMarket
	res.json(&market)
	base := "/v1/advanced/prediction-markets/" + market.Id.String()
	res = h.do(http.MethodPost, base+"/open", nil, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	// The customer holds one unit and asks for five sets: Risk V2 refuses,
	// and the refused intent stays on record as REJECTED.
	h.as(&cust)
	res = h.do(http.MethodPost, base+"/issuances", map[string]any{"account_id": cAcct.String(), "quantity": "5000000"}, "Idempotency-Key", key())
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeRiskRejected, res.problem().Code)
	var rejected int
	require.NoError(t, d.QueryRow(t.Context(), `SELECT count(*) FROM economic_intents
		WHERE account_id = $1 AND status = 'REJECTED' AND kind = 'CREATE_PREDICTION_POSITION'`, cAcct).Scan(&rejected))
	assert.Equal(t, 1, rejected, "the refusal is kept on record")
	res = h.do(http.MethodPost, base+"/issuances", map[string]any{"account_id": cAcct.String(), "quantity": "1000000"}, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	// Close, resolve YES, then a third operator disputes it.
	w.clk.Set(closeAt.Add(time.Minute))
	h.as(&opA)
	require.Equal(t, http.StatusOK, h.do(http.MethodPost, base+"/close", nil, "Idempotency-Key", key()).Code)
	require.Equal(t, http.StatusOK, h.do(http.MethodPost, base+"/request-resolution", nil, "Idempotency-Key", key()).Code)
	res = h.do(http.MethodPost, base+"/oracle-observations", map[string]any{
		"outcome": "YES", "observed_at": closeAt.Add(10 * time.Second), "evidence": `{"certified":"YES"}`,
	}, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	h.as(&opB)
	res = h.do(http.MethodPost, base+"/resolve", map[string]any{}, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	// Settlement waits for the dispute window.
	res = h.do(http.MethodPost, base+"/settle", nil, "Idempotency-Key", key())
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidStateTransition, res.problem().Code)

	h.as(&opC)
	res = h.do(http.MethodPost, base+"/dispute", map[string]any{
		"reason": "the certificate was published before the recount", "evidence": map[string]any{"notice": "recount ordered"},
	}, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var disputed api.PredictionDisputeResult
	res.json(&disputed)
	assert.Equal(t, "DISPUTED", disputed.Market.Status)

	// The operator who raised the dispute cannot rule on it ...
	res = h.do(http.MethodPost, base+"/uphold", map[string]any{"reason": "my own dispute is wrong after all"}, "Idempotency-Key", key())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.problem().Detail, "dispute")
	// ... nor can the operator whose resolution is disputed ...
	h.as(&opB)
	res = h.do(http.MethodPost, base+"/uphold", map[string]any{"reason": "my own resolution stands, I say"}, "Idempotency-Key", key())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.problem().Detail, "disputed")
	// ... and the creator cannot propose to void the market.
	h.as(&opA)
	res = h.do(http.MethodPost, base+"/void-proposals", map[string]any{
		"reason": "the creator wants out of this market", "evidence": map[string]any{"why": "regret"},
	}, "Idempotency-Key", key())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.problem().Detail, "creator")

	// An independent operator upholds it.
	h.as(&opD)
	res = h.do(http.MethodPost, base+"/uphold", map[string]any{"reason": "the recount confirmed the certificate"}, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var upheld api.PredictionResolutionResult
	res.json(&upheld)
	assert.Equal(t, "RESOLVED", upheld.Market.Status)
	assert.Equal(t, api.PredictionResolutionKind("DISPUTE_UPHELD"), upheld.Resolution.Kind)
}
