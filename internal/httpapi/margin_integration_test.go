//go:build integration

package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
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
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// marginWorld is the real margin service, its real dependencies and the
// HTTP surface over them, on the package's test database.
type marginWorld struct {
	t      *testing.T
	d      *db.DB
	clk    *clock.Fake
	h      *harness
	svc    *margin.Service
	ledger *ledger.Service
	prices *valuation.PriceStore
	usd    assets.Asset
	sol    assets.Asset
}

func newMarginWorld(t *testing.T) *marginWorld {
	t.Helper()
	return newMarginWorldWith(t, nil)
}

// newMarginWorldWith is newMarginWorld with the policy adjusted before the
// service is built (adjust may be nil).
func newMarginWorldWith(t *testing.T, adjust func(*margin.Policy)) *marginWorld {
	t.Helper()
	d := openTestDB(t)
	ctx := t.Context()
	w := &marginWorld{t: t, d: d, clk: clock.NewFake(time.Now().UTC().Truncate(time.Second))}

	// The deployment's one sandbox dollar (tests may provision it; the
	// composition root never does) and a simulated SOL the policy trades deep.
	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		w.usd, err = econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))
	var err error
	w.sol, err = assets.NewRepository().Create(ctx, d, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: "MSOL-" + uuid.NewString()[:8], Name: "margin httpapi itest SOL", Decimals: 9,
		RiskClass: assets.RiskMajor, Status: assets.StatusActive,
	})
	require.NoError(t, err)

	pol := margin.DefaultPolicy()
	pol.Version = "margin-httpapi-itest-" + uuid.NewString()
	pol.AssetRules = []margin.AssetRule{{AssetID: w.sol.ID.String(), RiskWeightBPS: 10_000, Liquidity: margin.LiqDeep}}
	if adjust != nil {
		adjust(&pol)
	}
	require.NoError(t, pol.Validate())

	outbox := event.NewOutbox(w.clk)
	aw := audit.NewWriterWithBuildVersion("httpapi-margin-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return false }, w.clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	w.ledger = ledger.NewService(w.clk, "httpapi-margin-itest")
	w.prices = valuation.NewPriceStore(w.clk)
	intents, effects := econ.NewIntentStore(w.clk, outbox, aw), econ.NewEffectStore(w.clk, outbox, aw)
	authorizer, riskStore := econ.NewAuthorizer(checker, kills), risk.NewStore()
	w.svc, err = margin.NewService(margin.Config{
		Policy: pol, RiskPolicy: risk.DefaultAdvancedPolicy(),
		LendingPoolName: "itest pool " + uuid.NewString(), InsuranceFundName: "itest fund " + uuid.NewString(),
	}, margin.Dependencies{
		Clock: w.clk, Ledger: w.ledger, Effects: effects, Intents: intents, Authorizer: authorizer, Registry: econ.NewRegistry(),
		Risk: riskStore, Capital: capital.NewService(w.clk, capital.OutboxEmitter{Outbox: outbox, Clock: w.clk}),
		Prices: w.prices, Router: router, Identity: compliance.NewAccountVerification(w.clk), KillSwitch: kills,
		Venue: margin.PaperVenue{}, Audit: aw,
	})
	require.NoError(t, err)

	ports, err := Wire(WireDeps{DB: d, Clock: w.clk, Env: "TEST", Ledger: w.ledger, Instruments: instruments.NewRepository(),
		Advanced: AdvancedDeps{
			Authorizer: authorizer, Intents: intents, Effects: effects, Risk: riskStore, Prices: w.prices, MaxPriceAge: time.Minute,
			Margin: w.svc.BuyingPowerReader(), MarginService: w.svc,
		}})
	require.NoError(t, err)
	require.NotNil(t, ports.Margin, "the margin service and the ledger wire the margin port")
	require.NotNil(t, ports.Advanced)

	userID, acct := seedAccount(t, d)
	w.h = newIntegrationHarness(t, d, acct, userID)
	w.h.server.opts.Ports.Margin = ports.Margin
	w.h.server.opts.Ports.Advanced = ports.Advanced
	return w
}

// customer creates a customer whose identity is VERIFIED (the margin
// service's identity reader is the production one) and returns its session.
func (w *marginWorld) customer() (accounts.AccountID, security.Principal) {
	w.t.Helper()
	userID, acct := seedAccount(w.t, w.d)
	verified, expires := w.clk.Now().Add(-time.Hour), w.clk.Now().Add(365*24*time.Hour)
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := compliance.NewRepository(audit.NewWriter()).Upsert(ctx, tx, compliance.Profile{
			UserID: userID, IdentityState: compliance.IdentityVerified, SanctionsState: compliance.SanctionsClear,
			VerifiedAt: &verified, ExpiresAt: &expires,
		}, compliance.Change{ActorType: security.ActorSystem, ActorID: "kyc-itest", Reason: "fixture"})
		return err
	}))
	return acct, security.Principal{
		SubjectID: userID.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{acct.String()}, SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
}

func (w *marginWorld) operator() security.Principal {
	return security.Principal{
		SubjectID: uuid.NewString(), ActorType: security.ActorOperator, Roles: []security.Role{security.RoleRisk},
		SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
}

// fund credits a wallet with sandbox dollars (SANDBOX_FUNDING).
func (w *marginWorld) fund(acct accounts.AccountID, units int64) {
	w.t.Helper()
	qty := money.QuantityFromInt64(units)
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := w.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "itest-margin-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: w.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, w.usd.ID), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, w.usd.ID), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

// mark moves the clock on and records the oracle's SOL price then (an
// observation's identity includes its time).
func (w *marginWorld) mark(after time.Duration, price string) {
	w.t.Helper()
	w.clk.Advance(after)
	p, err := money.PriceFromDecimalString(price, w.usd.ID.String(), "itest-oracle", w.clk.Now())
	require.NoError(w.t, err)
	_, err = w.prices.RecordPrice(w.t.Context(), w.d, valuation.PriceObservation{AssetID: w.sol.ID, QuoteAssetID: w.usd.ID,
		Mantissa: p.Mantissa, Scale: p.Scale, Source: "itest-oracle", ObservedAt: w.clk.Now()})
	require.NoError(w.t, err)
}

func (w *marginWorld) as(p security.Principal) *harness {
	w.h.as(&p)
	return w.h
}

func (w *marginWorld) post(p security.Principal, path string, body any, key string) *response {
	w.t.Helper()
	return w.as(p).do(http.MethodPost, path, body, "Idempotency-Key", key)
}

func (w *marginWorld) get(p security.Principal, path string) *response {
	w.t.Helper()
	return w.as(p).do(http.MethodGet, path, nil)
}

// TestIntegration_MarginOverHTTP drives margin end to end through HTTP over
// the real adapter, service and database: a funded, verified customer opens a
// margin account, posts collateral, borrows and buys on margin; the
// evaluation reads back exactly; buying power V2 reports the loan; a refusal
// is kept on the record; an operator marks the price down and runs the
// liquidation queue; and a second customer can read none of it.
func TestIntegration_MarginOverHTTP(t *testing.T) {
	w := newMarginWorld(t)
	acct, alice := w.customer()
	other, bob := w.customer()
	op := w.operator()
	w.fund(acct, 1_000_000_000) // 1,000 sandbox dollars

	// The operator funds the facilities: 1,000,000 to the pool, 100,000 to the fund.
	res := w.post(op, "/v1/advanced/margin/ops/facilities/seed",
		map[string]any{"pool_amount": "1000000000000", "fund_amount": "100000000000"}, "itest-seed-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var facilities api.MarginFacilities
	res.json(&facilities)
	assert.Equal(t, api.SIMULATED, facilities.ValueDomain)
	assert.Equal(t, w.usd.ID.String(), facilities.AssetId.String())
	assert.Equal(t, "1000000000000", facilities.LendingPool.Available)
	assert.Equal(t, "100000000000", facilities.InsuranceFund.Available)

	w.mark(time.Second, "100")
	t1 := w.clk.Now()

	// Open a CROSS account in PAPER mode; the numeraire is the sandbox dollar.
	res = w.post(alice, "/v1/advanced/margin/account",
		map[string]any{"account_id": acct.String(), "margin_mode": "CROSS", "mode": "PAPER"}, "itest-open-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var opened api.MarginAccount
	res.json(&opened)
	assert.Equal(t, api.MarginAccountStatus("NORMAL"), opened.Status)
	assert.Equal(t, w.usd.ID.String(), opened.NumeraireAssetId.String())

	// A LIVE margin account is the service's to refuse.
	res = w.post(alice, "/v1/advanced/margin/account",
		map[string]any{"account_id": acct.String(), "margin_mode": "CROSS", "mode": "LIVE"}, "itest-open-live-"+uuid.NewString())
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)

	// Post the 1,000 dollars as collateral.
	res = w.post(alice, "/v1/advanced/margin/collateral/post",
		map[string]any{"account_id": acct.String(), "asset_id": w.usd.ID.String(), "quantity": "1000000000"}, "itest-post-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var posted api.MarginCollateralResult
	res.json(&posted)
	require.NotNil(t, posted.Evaluation)
	assert.Equal(t, "1000000000", posted.Evaluation.Equity, "an account that owes nothing has its collateral as equity")
	assert.False(t, posted.Replayed)

	// Borrow 100 dollars into the wallet.
	res = w.post(alice, "/v1/advanced/margin/borrow", map[string]any{"account_id": acct.String(), "amount": "100000000"},
		"itest-borrow-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var borrowed api.MarginLoanResult
	res.json(&borrowed)
	require.NotNil(t, borrowed.LotId)
	borrowLot := borrowed.LotId.String()
	require.NotNil(t, borrowed.Evaluation)
	assert.Equal(t, "900000000", borrowed.Evaluation.Equity)

	// Buy 15 SOL on margin at 100 plus the disclosed 10 bps: 1,501.50 borrowed.
	buyKey := "itest-buy-" + uuid.NewString()
	buyBody := map[string]any{"account_id": acct.String(), "asset_id": w.sol.ID.String(), "quantity": "15000000000"}
	res = w.post(alice, "/v1/advanced/margin/leveraged-buys", buyBody, buyKey)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	firstBuy := res.Body.String()
	var bought api.MarginLeveragedBuyResult
	res.json(&bought)
	assert.Equal(t, "1501500000", bought.Cost)
	require.NotNil(t, bought.Mark)
	assert.Equal(t, "100", bought.Mark.Mantissa)
	require.NotNil(t, bought.ExecutionPrice)
	assert.Equal(t, "1001000", bought.ExecutionPrice.Mantissa)
	assert.Equal(t, 4, bought.ExecutionPrice.Scale)
	require.NotNil(t, bought.SlippageBps)
	assert.Equal(t, 10, *bought.SlippageBps)

	// The same key replays the same answer and buys nothing twice.
	res = w.post(alice, "/v1/advanced/margin/leveraged-buys", buyBody, buyKey)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.JSONEq(t, firstBuy, res.Body.String())

	// The evaluation reads back exactly, at the instant of the read:
	// collateral 1,000 + 15 × 100 × (1 − 25%) = 2,125; loans 100 + 1,501.50;
	// equity 523.50; initial 20% and maintenance 10% of the 1,500 exposure.
	require.Equal(t, t1, w.clk.Now())
	res = w.get(alice, "/v1/advanced/margin/account?account_id="+acct.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var detail api.MarginAccountDetail
	res.json(&detail)
	ev := detail.Evaluation
	assert.Equal(t, api.SIMULATED, detail.ValueDomain)
	assert.Equal(t, api.MarginAccountStatus("NORMAL"), detail.Status)
	assert.Equal(t, api.MarginAccountStatus("NORMAL"), ev.Health)
	assert.Equal(t, 6, ev.NumeraireDecimals)
	assert.Equal(t, "2125000000", ev.EligibleCollateral)
	assert.Equal(t, "1601500000", ev.BorrowLiabilities)
	assert.Equal(t, "0", ev.AccruedInterest)
	assert.Equal(t, "523500000", ev.Equity)
	assert.Equal(t, "2500000000", ev.MarketValue)
	assert.Equal(t, "898500000", ev.NetAssetValue)
	assert.Equal(t, "1500000000", ev.GrossExposure)
	assert.Equal(t, "300000000", ev.InitialMargin)
	assert.Equal(t, "150000000", ev.MaintenanceMargin)
	assert.Equal(t, "187500000", ev.WarningThreshold)
	assert.Equal(t, "373500000", ev.LiquidationBuffer)
	assert.Equal(t, "223500000", ev.MarginAvailable)
	assert.Equal(t, int64(28654), ev.EffectiveLeverageBps, "ceil(1,500 / 523.50 × 10000)")
	assert.Equal(t, int64(50000), ev.LeverageCapBps)
	assert.True(t, ev.PricesFresh)
	assert.Equal(t, "523.50", ev.Usd.Equity)
	assert.Equal(t, "1601.50", ev.Usd.BorrowLiabilities)
	assert.Equal(t, "373.50", ev.Usd.LiquidationBuffer)
	assert.Equal(t, "223.50", ev.Usd.MarginAvailable)
	assert.Equal(t, "300.00", ev.Usd.InitialMargin)
	var solHolding *api.MarginHolding
	for i := range ev.Holdings {
		if ev.Holdings[i].AssetId.String() == w.sol.ID.String() {
			solHolding = &ev.Holdings[i]
		}
	}
	require.NotNil(t, solHolding)
	assert.Equal(t, "15000000000", solHolding.Quantity)
	assert.Equal(t, "1500000000", solHolding.MarketValue)
	assert.Equal(t, "1125000000", solHolding.CollateralValue)
	assert.Equal(t, 2500, solHolding.Haircut.TotalBps)
	assert.Equal(t, 1000, solHolding.Haircut.VolatilityBps)
	assert.Equal(t, 1500, solHolding.Haircut.ConcentrationBps)
	require.NotNil(t, solHolding.Mark)
	assert.Equal(t, "100", solHolding.Mark.Mantissa)
	require.Len(t, detail.Loans, 2, "the borrow and the buy are two lots; the replay added none")
	principals := map[string]string{}
	for _, l := range detail.Loans {
		principals[l.LotId.String()] = l.Principal
	}
	assert.Equal(t, "100000000", principals[borrowLot])
	assert.Nil(t, detail.OpenCall)
	assert.Nil(t, detail.LatestCase)

	// Buying power V2 reads the same engine: the loan is a liability.
	res = w.get(alice, "/v1/advanced/buying-power?account_id="+acct.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var bp api.BuyingPowerV2
	res.json(&bp)
	assert.Equal(t, "1601.50", bp.BorrowLiabilities)
	assert.Equal(t, "300.00", bp.InitialMarginUsed)
	assert.Equal(t, "150.00", bp.MaintenanceMargin)

	// A refusal is kept on the record: the oversized borrow is refused by
	// Risk Kernel V2, and the refused intent and the decision persist.
	refusedKey := "itest-refused-" + uuid.NewString()
	refusedBody := map[string]any{"account_id": acct.String(), "amount": "1000000000000"}
	res = w.post(alice, "/v1/advanced/margin/borrow", refusedBody, refusedKey)
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	p := res.problem()
	assert.Equal(t, errs.CodeRiskRejected, p.Code)
	codes, _ := p.Fields["reason_codes"].([]any)
	require.NotEmpty(t, codes)
	assert.Contains(t, codes, risk.ReasonMarginInitial)
	var intentStatus, rejection string
	require.NoError(t, w.d.QueryRow(t.Context(), `SELECT status, coalesce(rejection_code,'') FROM economic_intents
		WHERE account_id = $1 AND idempotency_key = $2`, acct, refusedKey).Scan(&intentStatus, &rejection))
	assert.Equal(t, "REJECTED", intentStatus, "the refusal was committed, not rolled back")
	assert.Equal(t, codes[0], rejection, "the intent records the decision's first reason code")
	res = w.get(alice, "/v1/advanced/risk/decisions?account_id="+acct.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var decisions api.AdvancedRiskDecisionList
	res.json(&decisions)
	rejected := 0
	for _, dcs := range decisions.Items {
		if dcs.Verdict == "REJECT" {
			rejected++
		}
	}
	assert.Equal(t, 1, rejected, "the REJECT decision is on the record")
	again := w.post(alice, "/v1/advanced/margin/borrow", refusedBody, refusedKey)
	require.Equal(t, http.StatusUnprocessableEntity, again.Code)
	assert.Equal(t, errs.CodeRiskRejected, again.problem().Code)
	res = w.get(alice, "/v1/advanced/margin/account?account_id="+acct.String())
	require.Equal(t, http.StatusOK, res.Code)
	res.json(&detail)
	assert.Equal(t, "1601500000", detail.Evaluation.BorrowLiabilities, "the refused borrow lent nothing")

	// A stress run is stored, and reads back by id with its hash intact.
	res = w.post(alice, "/v1/advanced/margin/stress-runs", map[string]any{"account_id": acct.String()}, "itest-stress-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var stress api.MarginStressRun
	res.json(&stress)
	require.Len(t, stress.Results, len(margin.AllStressScenarios()))
	res = w.get(alice, "/v1/advanced/margin/stress-runs/"+stress.Id.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var stored api.MarginStressRun
	res.json(&stored)
	assert.Equal(t, stress.ReportHash, stored.ReportHash)
	assert.Equal(t, stress.Results, stored.Results)
	assert.Equal(t, stress.RunAt, stored.RunAt)

	// A day later (with a fresh mark), a repayment pays interest first: the
	// oldest lot's day of interest, then its principal.
	w.mark(24*time.Hour, "100")
	wantInterest := money.Quantity{}
	for _, period := range margin.DayPeriods(t1, w.clk.Now(), true, 366) {
		part, err := margin.InterestFor(money.QuantityFromInt64(100_000_000), margin.DefaultPolicy().BorrowRateBPS, period.Start, period.End)
		require.NoError(t, err)
		wantInterest = wantInterest.Add(part)
	}
	require.True(t, wantInterest.IsPositive())
	res = w.post(alice, "/v1/advanced/margin/repay", map[string]any{"account_id": acct.String(), "amount": "1000000"},
		"itest-repay-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var repaid api.MarginLoanResult
	res.json(&repaid)
	require.Len(t, repaid.Allocations, 1)
	assert.Equal(t, borrowLot, repaid.Allocations[0].LotId.String(), "the oldest lot is repaid first")
	assert.Equal(t, wantInterest.String(), repaid.Allocations[0].Interest, "interest before principal")
	assert.Equal(t, money.QuantityFromInt64(1_000_000).Sub(wantInterest).String(), repaid.Allocations[0].Principal)

	// The oracle marks SOL down to 60 and the operator marks the book to market.
	w.mark(time.Second, "60")
	res = w.post(op, "/v1/advanced/margin/ops/mark-to-market", map[string]any{}, "itest-mtm-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var marked api.MarginMarkToMarketResult
	res.json(&marked)
	assert.Empty(t, marked.Failures)
	require.Len(t, marked.Evaluated, 1, "the service marks the accounts of its own policy")
	assert.Equal(t, acct.String(), marked.Evaluated[0].AccountId.String())
	assert.Equal(t, api.MarginAccountStatus("MARGIN_CALL"), marked.Evaluated[0].Status)
	require.NotNil(t, marked.Evaluated[0].Call)
	assert.Equal(t, api.MarginCallStatus("OPEN"), marked.Evaluated[0].Call.Status)

	// The queue shows the account with its call and its live evaluation.
	res = w.get(op, "/v1/advanced/margin/ops/queue")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var queue api.MarginQueue
	res.json(&queue)
	require.Len(t, queue.Items, 1)
	entry := queue.Items[0]
	assert.Equal(t, acct.String(), entry.Account.AccountId.String())
	require.NotNil(t, entry.Evaluation)
	assert.True(t, entry.Evaluation.LiquidationEligible)
	require.NotNil(t, entry.OpenCall)

	// The operator runs the liquidation queue: one deterministic step.
	res = w.post(op, "/v1/advanced/margin/ops/queue/run", map[string]any{"limit": 10}, "itest-queue-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var run api.MarginQueueRunResult
	res.json(&run)
	assert.Empty(t, run.Failures)
	require.Len(t, run.Results, 1)
	step := run.Results[0]
	assert.True(t, step.Eligible)
	assert.Contains(t, []api.MarginPlanDecision{"SELL", "SELL_ALL"}, step.Decision)
	require.NotNil(t, step.Step)
	assert.Equal(t, api.MarginStepAction("SELL"), step.Step.Action)
	require.NotNil(t, step.Case)
	caseID := step.Case.Id.String()

	// The customer sees the case, its step at the 60 mark, and the call it
	// resolved.
	res = w.get(alice, "/v1/advanced/margin/cases/"+caseID)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var kase api.MarginCaseDetail
	res.json(&kase)
	require.Len(t, kase.Steps, 1)
	require.NotNil(t, kase.Steps[0].Mark)
	assert.Equal(t, "60", kase.Steps[0].Mark.Mantissa)
	assert.Equal(t, step.Step.Proceeds, kase.Steps[0].Proceeds)
	res = w.get(alice, "/v1/advanced/margin/calls?account_id="+acct.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var calls api.MarginCallPage
	res.json(&calls)
	require.Len(t, calls.Items, 1)
	assert.Equal(t, api.MarginCallStatus("LIQUIDATED"), calls.Items[0].Status, "the case took the call over")
	res = w.get(alice, "/v1/advanced/margin/cases?account_id="+acct.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var cases api.MarginCasePage
	res.json(&cases)
	require.Len(t, cases.Items, 1)
	assert.Equal(t, caseID, cases.Items[0].Id.String())
	res = w.get(op, "/v1/advanced/margin/ops/calls?status=LIQUIDATED")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var opCalls api.MarginCallPage
	res.json(&opCalls)
	require.Len(t, opCalls.Items, 1)
	assert.Equal(t, acct.String(), opCalls.Items[0].AccountId.String())

	// A second customer reads none of it: 403 on the account, NOT_FOUND on
	// every record by id, exactly as for an id that names nothing.
	for _, path := range []string{"/v1/advanced/margin/account", "/v1/advanced/margin/calls", "/v1/advanced/margin/cases",
		"/v1/advanced/margin/stress-runs"} {
		res = w.get(bob, path+"?account_id="+acct.String())
		require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", path, res.Body.String())
		assert.Equal(t, errs.CodeForbidden, res.problem().Code, path)
	}
	foreignCase := w.get(bob, "/v1/advanced/margin/cases/"+caseID)
	require.Equal(t, http.StatusNotFound, foreignCase.Code, "body=%s", foreignCase.Body.String())
	absentCase := w.get(bob, "/v1/advanced/margin/cases/"+econ.NewIntentID().String())
	require.Equal(t, http.StatusNotFound, absentCase.Code, "body=%s", absentCase.Body.String())
	assert.Equal(t, absentCase.problem().Detail, foreignCase.problem().Detail, "foreign and absent read alike")
	foreignRun := w.get(bob, "/v1/advanced/margin/stress-runs/"+stress.Id.String())
	require.Equal(t, http.StatusNotFound, foreignRun.Code, "body=%s", foreignRun.Body.String())
	assert.Equal(t, errs.CodeNotFound, foreignRun.problem().Code)
	res = w.post(bob, "/v1/advanced/margin/borrow", map[string]any{"account_id": acct.String(), "amount": "1"}, "itest-bob-"+uuid.NewString())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	res = w.get(bob, "/v1/advanced/margin/account?account_id="+other.String())
	require.Equal(t, http.StatusNotFound, res.Code, "bob has no margin account of his own; body=%s", res.Body.String())
	res = w.get(bob, "/v1/advanced/margin/calls?account_id="+other.String())
	require.Equal(t, http.StatusOK, res.Code, "an account without a margin account lists nothing; body=%s", res.Body.String())
	res.json(&calls)
	assert.Empty(t, calls.Items)
}

// --- covered deficits and MANUAL_REVIEW under dual control ------------------------

// fundUnits credits a wallet with any simulated asset (SANDBOX_FUNDING).
func (w *marginWorld) fundUnits(acct accounts.AccountID, asset assets.Asset, units int64) {
	w.t.Helper()
	qty := money.QuantityFromInt64(units)
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := w.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "itest-margin-fund-units:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: w.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset.ID), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, asset.ID), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

// withdrawUnits sends wallet value out of the platform (SANDBOX_WITHDRAWAL),
// e.g. borrowed cash the customer spent elsewhere.
func (w *marginWorld) withdrawUnits(acct accounts.AccountID, asset assets.Asset, units int64) {
	w.t.Helper()
	qty := money.QuantityFromInt64(units)
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := w.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxWithdrawal, IdempotencyKey: "itest-margin-withdraw:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_withdrawal", ID: ref}, EffectiveAt: w.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWithdrawn, asset.ID), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset.ID), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

// markAssetNow records the oracle's price of asset at the current instant.
func (w *marginWorld) markAssetNow(asset assets.Asset, price string) {
	w.t.Helper()
	p, err := money.PriceFromDecimalString(price, w.usd.ID.String(), "itest-oracle", w.clk.Now())
	require.NoError(w.t, err)
	_, err = w.prices.RecordPrice(w.t.Context(), w.d, valuation.PriceObservation{AssetID: asset.ID, QuoteAssetID: w.usd.ID,
		Mantissa: p.Mantissa, Scale: p.Scale, Source: "itest-oracle", ObservedAt: w.clk.Now()})
	require.NoError(w.t, err)
}

// liquidateUntil runs the operator's one-step liquidation until the decision
// is want, and returns that step's answer.
func (w *marginWorld) liquidateUntil(op security.Principal, acct accounts.AccountID, want api.MarginPlanDecision) api.MarginLiquidationResult {
	w.t.Helper()
	for i := 0; i < 12; i++ {
		res := w.post(op, "/v1/advanced/margin/ops/liquidations", map[string]any{"account_id": acct.String()},
			"itest-liquidate-"+uuid.NewString())
		require.Equal(w.t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		var out api.MarginLiquidationResult
		res.json(&out)
		if out.Decision == want {
			return out
		}
	}
	w.t.Fatalf("the account never reached %s", want)
	return api.MarginLiquidationResult{}
}

// TestIntegration_MarginCoveredDeficitIsRepaidOverHTTP: a customer whose
// borrowed cash left the platform is liquidated to bankruptcy; the deficit
// cannot be repaid before the insurance fund covers it; once covered, the
// owner repays part of it through HTTP, exactly once per key, never beyond
// what is owed, and the read reports every figure the service recorded.
func TestIntegration_MarginCoveredDeficitIsRepaidOverHTTP(t *testing.T) {
	w := newMarginWorld(t)
	acct, alice := w.customer()
	_, bob := w.customer()
	op := w.operator()
	w.fund(acct, 1_000_000_000)
	res := w.post(op, "/v1/advanced/margin/ops/facilities/seed",
		map[string]any{"pool_amount": "1000000000000", "fund_amount": "100000000000"}, "itest-seed-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	w.mark(time.Second, "100")
	for _, step := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/advanced/margin/account", map[string]any{"account_id": acct.String(), "margin_mode": "CROSS", "mode": "PAPER"}},
		{"/v1/advanced/margin/collateral/post", map[string]any{"account_id": acct.String(), "asset_id": w.usd.ID.String(), "quantity": "1000000000"}},
		{"/v1/advanced/margin/borrow", map[string]any{"account_id": acct.String(), "amount": "100000000"}},
		{"/v1/advanced/margin/leveraged-buys", map[string]any{"account_id": acct.String(), "asset_id": w.sol.ID.String(), "quantity": "15000000000"}},
	} {
		res = w.post(alice, step.path, step.body, "itest-setup-"+uuid.NewString())
		require.Equal(t, http.StatusOK, res.Code, "%s body=%s", step.path, res.Body.String())
	}
	deficitPath := "/v1/advanced/margin/deficit?account_id=" + acct.String()
	res = w.get(alice, deficitPath)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var status api.MarginDeficitStatus
	res.json(&status)
	assert.Nil(t, status.Case, "no case yet")
	assert.Nil(t, status.Deficit)
	require.NotNil(t, status.NumeraireDecimals)
	assert.Equal(t, 6, *status.NumeraireDecimals)

	// The borrowed cash leaves the platform, SOL collapses, the case runs to bankruptcy.
	w.withdrawUnits(acct, w.usd, 100_000_000)
	w.mark(time.Minute, "5")
	bankrupt := w.liquidateUntil(op, acct, "BANKRUPT")
	require.NotNil(t, bankrupt.Case)
	require.NotNil(t, bankrupt.Case.Deficit)
	deficit, err := money.ParseQuantity(*bankrupt.Case.Deficit)
	require.NoError(t, err)
	require.True(t, deficit.IsPositive())

	res = w.get(alice, deficitPath)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&status)
	require.NotNil(t, status.Case)
	assert.Equal(t, api.MarginCaseStatus("BANKRUPT"), status.Case.Status)
	assert.Nil(t, status.Outstanding, "an uncovered deficit is the pool's loan, not a debt to the fund")
	res = w.post(alice, "/v1/advanced/margin/deficit/repayments", map[string]any{"account_id": acct.String(), "amount": "1"},
		"itest-too-soon-"+uuid.NewString())
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidStateTransition, res.problem().Code)

	// The fund covers it; now the owner owes the fund exactly the deficit.
	res = w.post(op, "/v1/advanced/margin/ops/cases/"+bankrupt.Case.Id.String()+"/cover-deficit", nil, "itest-cover-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res = w.get(alice, deficitPath)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&status)
	require.NotNil(t, status.Deficit)
	require.NotNil(t, status.Outstanding)
	require.NotNil(t, status.Repaid)
	assert.Equal(t, deficit.String(), *status.Deficit)
	assert.Equal(t, deficit.String(), *status.Outstanding)
	assert.Equal(t, "0", *status.Repaid)
	assert.Empty(t, status.Repayments)

	// The owner is funded and repays half, exactly once per key.
	w.fund(acct, 10_000_000)
	w.fundUnits(acct, w.usd, marginQtyInt64(t, deficit))
	half, err := deficit.Div(money.QuantityFromInt64(2), money.RoundFloor)
	require.NoError(t, err)
	repayKey := "itest-repay-deficit-" + uuid.NewString()
	repayBody := map[string]any{"account_id": acct.String(), "amount": half.String()}
	res = w.post(alice, "/v1/advanced/margin/deficit/repayments", repayBody, repayKey)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var repaid api.MarginDeficitRepaymentResult
	res.json(&repaid)
	assert.Equal(t, api.SIMULATED, repaid.ValueDomain)
	assert.Equal(t, half.String(), repaid.Repaid)
	assert.Equal(t, deficit.Sub(half).String(), repaid.Outstanding)
	assert.Equal(t, bankrupt.Case.Id, repaid.CaseId)
	again := w.post(alice, "/v1/advanced/margin/deficit/repayments", repayBody, repayKey)
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, res.Body.String(), again.Body.String())
	var journals int
	require.NoError(t, w.d.QueryRow(t.Context(), `SELECT count(*) FROM margin_deficit_repayments WHERE case_id = $1`,
		bankrupt.Case.Id).Scan(&journals))
	assert.Equal(t, 1, journals, "a replay repays nothing twice")

	res = w.get(alice, deficitPath)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&status)
	assert.Equal(t, half.String(), *status.Repaid)
	assert.Equal(t, deficit.Sub(half).String(), *status.Outstanding)
	require.Len(t, status.Repayments, 1)
	assert.Equal(t, half.String(), status.Repayments[0].Amount)
	assert.Equal(t, repaid.JournalTransactionId, status.Repayments[0].JournalTransactionId)

	// Never beyond what is owed.
	res = w.post(alice, "/v1/advanced/margin/deficit/repayments", map[string]any{"account_id": acct.String(), "amount": deficit.String()},
		"itest-too-much-"+uuid.NewString())
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)

	// Scoping: a stranger reads nothing; an operator reads but repays for nobody.
	res = w.get(bob, deficitPath)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	res = w.get(op, deficitPath)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res = w.post(op, "/v1/advanced/margin/deficit/repayments", map[string]any{"account_id": acct.String(), "amount": "1"},
		"itest-op-repay-"+uuid.NewString())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
}

// TestIntegration_MarginManualReviewEndsOnlyWithTwoOperatorsOverHTTP: a case
// that used its one-sale budget stops in MANUAL_REVIEW and appears in the
// operators' queue and the console's counts; the proposer of a resolution
// cannot approve it; a second operator's approval resumes the case once, and
// the queue and the counts are empty again.
func TestIntegration_MarginManualReviewEndsOnlyWithTwoOperatorsOverHTTP(t *testing.T) {
	w := newMarginWorldWith(t, func(p *margin.Policy) { p.MaxLiquidationSteps = 1 })
	acct, alice := w.customer()
	proposer, approver := w.operator(), w.operator()
	res := w.post(proposer, "/v1/advanced/margin/ops/facilities/seed",
		map[string]any{"pool_amount": "1000000000000", "fund_amount": "100000000000"}, "itest-seed-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	// The console reads the same database.
	controller, err := killswitch.NewController(w.clk, noopKillAudit{}, NewApprovalVerifier(admin.NewService(w.clk, audit.NewWriter())))
	require.NoError(t, err)
	ops, err := Wire(WireDeps{DB: w.d, Clock: w.clk, Env: "TEST", KillSwitches: controller, Advanced: AdvancedDeps{
		Operator: OperatorDeps{Prices: w.prices, Audit: audit.NewWriter(), MaxMarkAge: time.Minute},
	}})
	require.NoError(t, err)
	require.NotNil(t, ops.Operator)
	w.h.server.opts.Ports.Operator = ops.Operator
	counts := func() api.AdvancedOpsDualControl {
		t.Helper()
		r := w.get(proposer, "/v1/advanced/ops/overview")
		require.Equal(t, http.StatusOK, r.Code, "body=%s", r.Body.String())
		var o api.AdvancedOpsOverview
		r.json(&o)
		return o.DualControl
	}
	before := counts()

	eth, err := assets.NewRepository().Create(t.Context(), w.d, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: "METH-" + uuid.NewString()[:8], Name: "margin httpapi itest ETH", Decimals: 9,
		RiskClass: assets.RiskMajor, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	w.fundUnits(acct, w.sol, 10_000_000_000)
	w.fundUnits(acct, eth, 10_000_000_000)
	w.mark(time.Second, "100")
	w.markAssetNow(eth, "100")
	for _, step := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/advanced/margin/account", map[string]any{"account_id": acct.String(), "margin_mode": "CROSS", "mode": "PAPER"}},
		{"/v1/advanced/margin/collateral/post", map[string]any{"account_id": acct.String(), "asset_id": w.sol.ID.String(), "quantity": "10000000000"}},
		{"/v1/advanced/margin/collateral/post", map[string]any{"account_id": acct.String(), "asset_id": eth.ID.String(), "quantity": "10000000000"}},
		{"/v1/advanced/margin/borrow", map[string]any{"account_id": acct.String(), "amount": "1000000000"}},
	} {
		res = w.post(alice, step.path, step.body, "itest-review-setup-"+uuid.NewString())
		require.Equal(t, http.StatusOK, res.Code, "%s body=%s", step.path, res.Body.String())
	}
	w.withdrawUnits(acct, w.usd, 1_000_000_000)
	w.mark(time.Minute, "10")
	w.markAssetNow(eth, "10")
	review := w.liquidateUntil(proposer, acct, "MANUAL_REVIEW")
	require.NotNil(t, review.Case)
	caseID := review.Case.Id.String()
	assert.Equal(t, api.MarginCaseStatus("MANUAL_REVIEW"), review.Case.Status)

	res = w.get(proposer, "/v1/advanced/margin/ops/reviews")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var queue api.MarginReviewQueue
	res.json(&queue)
	var item *api.MarginReviewItem
	for i := range queue.Items {
		if queue.Items[i].Case.Id.String() == caseID {
			item = &queue.Items[i]
		}
	}
	require.NotNil(t, item, "the case waits in the review queue")
	assert.Empty(t, item.Resolutions)
	assert.Equal(t, before.MarginCasesInReview+1, counts().MarginCasesInReview)

	// One operator proposes; nothing moves.
	res = w.post(proposer, "/v1/advanced/margin/ops/cases/"+caseID+"/resolutions",
		map[string]any{"outcome": "RESUME", "reason": "the venue is back; sell what is left"}, "itest-propose-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var resolution api.MarginCaseResolution
	res.json(&resolution)
	assert.True(t, resolution.Pending)
	assert.Equal(t, proposer.SubjectID, resolution.ProposedBy)
	assert.Equal(t, item.CurrentReviewTransitionId, resolution.ReviewTransitionId, "it answers the review the case is in now")
	now := counts()
	assert.Equal(t, before.MarginResolutionsPending+1, now.MarginResolutionsPending, "the console counts it as awaiting a second operator")

	decision := "/v1/advanced/margin/ops/resolutions/" + resolution.Id.String() + "/decision"
	approve := map[string]any{"decision": "APPROVED", "reason": "agreed: resume"}
	res = w.post(proposer, decision, approve, "itest-self-approve-"+uuid.NewString())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	res = w.post(alice, decision, approve, "itest-customer-approve-"+uuid.NewString())
	require.Equal(t, http.StatusForbidden, res.Code, "a customer decides nothing; body=%s", res.Body.String())

	approveKey := "itest-approve-" + uuid.NewString()
	res = w.post(approver, decision, approve, approveKey)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var decided api.MarginResolutionDecisionResult
	res.json(&decided)
	assert.Equal(t, api.SIMULATED, decided.ValueDomain)
	assert.False(t, decided.Replayed)
	assert.False(t, decided.Resolution.Pending)
	require.NotNil(t, decided.Resolution.DecidedBy)
	assert.Equal(t, approver.SubjectID, *decided.Resolution.DecidedBy)
	assert.Equal(t, api.MarginCaseStatus("IN_PROGRESS"), decided.Case.Status, "the case resumes with a fresh budget")
	again := w.post(approver, decision, approve, approveKey)
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, res.Body.String(), again.Body.String())
	res = w.post(approver, decision, approve, "itest-approve-again-"+uuid.NewString())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, true, res.raw()["replayed"], "the same decision by the same operator is the service's replay")
	res = w.post(proposer, decision, map[string]any{"decision": "REJECTED", "reason": "changed my mind"}, "itest-late-"+uuid.NewString())
	require.Equal(t, http.StatusConflict, res.Code, "a decided resolution takes no other decision; body=%s", res.Body.String())

	var exits int
	require.NoError(t, w.d.QueryRow(t.Context(), `SELECT count(*) FROM margin_liquidation_case_transitions
		WHERE case_id = $1 AND from_status = 'MANUAL_REVIEW'`, caseID).Scan(&exits))
	assert.Equal(t, 1, exits, "the review was left exactly once")
	after := counts()
	assert.Equal(t, before.MarginCasesInReview, after.MarginCasesInReview)
	assert.Equal(t, before.MarginResolutionsPending, after.MarginResolutionsPending)
}

func marginQtyInt64(t *testing.T, q money.Quantity) int64 {
	t.Helper()
	v, err := q.Int64()
	require.NoError(t, err)
	return v
}
