//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
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
	"github.com/nodal/controlplane/internal/derivatives"
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
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// derivativesCapitalEmitter forwards capital's reservation events onto the
// transactional outbox, as the composition root's emitter does.
type derivativesCapitalEmitter struct {
	outbox *event.Outbox
	clk    clock.Clock
}

func (e derivativesCapitalEmitter) Emit(ctx context.Context, tx pgx.Tx, topic string, payload any) error {
	if _, ok := event.Lookup(event.Topic(topic)); !ok {
		return nil
	}
	ev, ok := payload.(capital.ReservationEvent)
	if !ok {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return e.outbox.Enqueue(ctx, tx, topic, event.Envelope{
		ID: event.NewEventID().String(), Type: topic, SchemaVersion: event.Topic(topic).Version(), Source: "capital",
		AggregateType: event.AggregateReservation, AggregateID: ev.ReservationID, OccurredAt: e.clk.Now(), Payload: body,
	})
}

// derivativesWorld is a perpetual on the sandbox dollar, the real service
// and adapter over the real database, and the principals that act on it.
type derivativesWorld struct {
	t        *testing.T
	d        *db.DB
	clk      *clock.Fake
	h        *harness
	led      *ledger.Service
	perp     instruments.InstrumentID
	a, b     accounts.AccountID
	custA    security.Principal
	custB    security.Principal
	operator security.Principal
	stranger security.Principal
}

// derivativesStart is on both the hourly variation-margin grid and the
// eight-hour funding grid.
var derivativesStart = time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)

func newDerivativesWorld(t *testing.T) *derivativesWorld {
	t.Helper()
	d := openTestDB(t)
	ctx := t.Context()
	clk := clock.NewFake(derivativesStart)
	outbox := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("httpapi-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return false }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	// The deployment's default legal policy, as cmd/api builds it.
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	led := ledger.NewService(clk, "httpapi-itest")
	intents, effects := econ.NewIntentStore(clk, outbox, aw), econ.NewEffectStore(clk, outbox, aw)
	riskStore := risk.NewStore()
	authorizer := econ.NewAuthorizer(checker, kills)

	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))
	svc, err := derivatives.NewService(derivatives.Config{
		Clock: clk, Intents: intents, Effects: effects, Authorizer: authorizer, Registry: econ.NewRegistry(), Ledger: led,
		Capital: capital.NewService(clk, derivativesCapitalEmitter{outbox: outbox, clk: clk}), RiskStore: riskStore,
		RiskPolicy: risk.DefaultAdvancedPolicy(), Policy: derivatives.DefaultPolicy(), Prices: valuation.NewPriceStore(clk),
		Instruments: instruments.NewRepository(), Router: router, KillSwitches: kills,
		Reconciliation: reconciliation.NewRepository(clk, outbox, aw), USDAssetID: econ.SandboxDollarID(),
		// Its own clearing house, so its books hold this test's flows only.
		ClearingHouseName: "derivatives clearing house httpapi-itest " + uuid.NewString(),
	})
	require.NoError(t, err)

	ports, err := Wire(WireDeps{DB: d, Clock: clk, Env: "TEST", Instruments: instruments.NewRepository(), Advanced: AdvancedDeps{
		Authorizer: authorizer, Intents: intents, Effects: effects, Risk: riskStore,
		Prices: valuation.NewPriceStore(clk), MaxPriceAge: time.Minute,
		Derivatives: DerivativesDeps{Service: svc, ValuationAssetID: econ.SandboxDollarID()},
	}})
	require.NoError(t, err)
	require.NotNil(t, ports.Derivatives, "a service and a valuation asset wire the port")

	w := &derivativesWorld{t: t, d: d, clk: clk, led: led}
	userA, acctA := w.verifiedCustomer()
	userB, acctB := w.verifiedCustomer()
	userC, acctC := w.verifiedCustomer()
	w.a, w.b = acctA, acctB
	w.h = newIntegrationHarness(t, d, acctA, userA)
	w.h.server.opts.Ports.Derivatives = ports.Derivatives
	w.h.server.opts.Ports.Advanced = ports.Advanced
	w.custA = *w.h.princip
	w.custB = customerOf(userB, acctB)
	w.stranger = customerOf(userC, acctC)
	w.operator = security.Principal{
		SubjectID: uuid.NewString(), ActorType: security.ActorOperator, Roles: []security.Role{security.RoleOperations},
		SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
	w.perp = w.listPerpetual()
	// 100,000 sandbox dollars each (6 decimals).
	w.fund(acctA, 100_000_000_000)
	w.fund(acctB, 100_000_000_000)
	return w
}

func customerOf(user accounts.UserID, acct accounts.AccountID) security.Principal {
	return security.Principal{
		SubjectID: user.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{acct.String()}, SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
}

// verifiedCustomer is a customer account whose owner's identity is VERIFIED:
// the risk kernel reads it from compliance_profiles, never from a request.
func (w *derivativesWorld) verifiedCustomer() (accounts.UserID, accounts.AccountID) {
	w.t.Helper()
	user, acct := seedAccount(w.t, w.d)
	_, err := w.d.Pool().Exec(w.t.Context(), `INSERT INTO compliance_profiles (user_id, identity_state, verified_at)
		VALUES ($1, 'VERIFIED', now())`, user)
	require.NoError(w.t, err)
	return user, acct
}

// listPerpetual lists a perpetual on a sandbox underlying of risk class
// MAJOR (10% initial, 5% maintenance): one contract is 0.01 of the
// underlying (8 decimals), prices have 2 decimals on a $0.50 tick, so one
// tick of one contract is 5,000 base units of the sandbox dollar.
func (w *derivativesWorld) listPerpetual() instruments.InstrumentID {
	w.t.Helper()
	return w.listContract(instruments.TypePerpetual, nil)
}

// listFuture lists a dated future on the same terms as listPerpetual,
// expiring at expiry.
func (w *derivativesWorld) listFuture(expiry time.Time) instruments.InstrumentID {
	w.t.Helper()
	return w.listContract(instruments.TypeFuture, &expiry)
}

func (w *derivativesWorld) listContract(kind instruments.InstrumentType, expiry *time.Time) instruments.InstrumentID {
	w.t.Helper()
	ctx := w.t.Context()
	sym := "DVBTC" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:6])
	btc, err := assets.NewRepository().Create(ctx, w.d, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: sym, Name: sym + " (sandbox)", Decimals: 8, RiskClass: assets.RiskMajor, Status: assets.StatusActive,
	})
	require.NoError(w.t, err)
	usd := econ.SandboxDollarID()
	repo := instruments.NewRepository()
	var perp instruments.InstrumentID
	require.NoError(w.t, w.d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		spot, err := repo.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: btc.ID, Quote: usd, Settlement: usd, CanonicalName: sym + "/SBX-USD " + uuid.NewString(),
			RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: derivativesStart.Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		under := spot.ID
		terms := instruments.Terms{
			Kind: kind, Symbol: sym + "-" + string(kind), VenueCode: "NODAL-SANDBOX", SettlementAssetID: usd,
			UnderlyingInstrumentID: &under, ContractMultiplier: money.QuantityFromInt64(1_000_000), PriceScale: 2,
			TickSize: money.QuantityFromInt64(50), LotSize: money.QuantityFromInt64(1), MinQuantity: money.QuantityFromInt64(1),
			SettlementStyle: instruments.SettleCash, TradingCalendar: derivatives.Calendar24x7, OracleMaxAgeMS: 60_000,
			OracleSources: []string{"itest-operator"}, MarginModel: instruments.MarginLinear, Expiry: expiry,
			CorporateActionBehavior: instruments.CorporateActionsNone, Transferability: instruments.NonTransferable,
		}
		if kind == instruments.TypePerpetual {
			terms.FundingIntervalMS = 8 * 3600 * 1000
		}
		ins, _, err := repo.CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
			Terms: terms, CanonicalName: terms.Symbol, RiskClass: assets.RiskSpeculative, Status: assets.StatusActive,
			ActiveFrom: derivativesStart.Add(-time.Hour), ActorType: "OPERATOR", ActorID: "itest-operator",
		})
		perp = ins.ID
		return err
	}))
	return perp
}

func (w *derivativesWorld) fund(acct accounts.AccountID, baseUnits int64) {
	w.t.Helper()
	q := money.QuantityFromInt64(baseUnits)
	usd := econ.SandboxDollarID()
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := w.led.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "itest-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: w.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, usd), Side: ledger.Debit, Quantity: q},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, usd), Side: ledger.Credit, Quantity: q},
			},
		})
		return err
	}))
}

// as runs the next requests as p, with a strong authentication of right now.
func (w *derivativesWorld) as(p security.Principal) *harness {
	p.AuthTime = time.Now().UTC()
	return w.h.as(&p)
}

func (w *derivativesWorld) post(p security.Principal, path string, body any) *response {
	w.t.Helper()
	return w.as(p).do(http.MethodPost, path, body, "Idempotency-Key", "itest-"+uuid.NewString())
}

func (w *derivativesWorld) get(p security.Principal, path string) *response {
	w.t.Helper()
	return w.as(p).do(http.MethodGet, path, nil)
}

func (w *derivativesWorld) mark(markDecimal, indexDecimal string) api.DerivativeMark {
	w.t.Helper()
	return w.markOn(w.perp, markDecimal, indexDecimal)
}

func (w *derivativesWorld) markOn(ins instruments.InstrumentID, markDecimal, indexDecimal string) api.DerivativeMark {
	w.t.Helper()
	res := w.post(w.operator, derivBase+"/contracts/"+ins.String()+"/marks", map[string]any{
		"mark": markDecimal, "index": indexDecimal, "source": "itest-operator", "observed_at": w.clk.Now().Format(time.RFC3339Nano),
	})
	require.Equal(w.t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var m api.DerivativeMark
	res.json(&m)
	return m
}

func (w *derivativesWorld) positions(p security.Principal, acct accounts.AccountID) api.DerivativePositionPage {
	w.t.Helper()
	res := w.get(p, derivBase+"/positions?account_id="+acct.String())
	require.Equal(w.t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var page api.DerivativePositionPage
	res.json(&page)
	return page
}

func (w *derivativesWorld) flows(p security.Principal, acct accounts.AccountID) []api.DerivativeFlow {
	w.t.Helper()
	res := w.get(p, derivBase+"/flows?account_id="+acct.String())
	require.Equal(w.t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var page api.DerivativeFlowPage
	res.json(&page)
	return page.Items
}

func (w *derivativesWorld) accruals(p security.Principal, acct accounts.AccountID) []api.DerivativeFundingAccrual {
	w.t.Helper()
	res := w.get(p, derivBase+"/funding-accruals?account_id="+acct.String())
	require.Equal(w.t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var page api.DerivativeFundingAccrualPage
	res.json(&page)
	return page.Items
}

// TestIntegration_DerivativesThroughHTTP drives a perpetual end to end
// through the API over the real adapter, service and database: an operator
// lists it and marks it, two customers open opposite positions, the mark
// moves and the operator settles variation margin and funding, both
// customers read their positions and flows to the base unit, a stale mark
// refuses a risk-increasing open and prices no close (each refusal recorded,
// and findable) until the operator marks the contract again, and a stranger
// reads nothing of theirs.
//
// The arithmetic every figure below follows (tick value 5,000 base units;
// 10 contracts; disclosed paper slippage 10 bps against the customer):
//
//	open at mark 65,000.00: A buys at 65,065.00 = 130,130 ticks, cost 6,506,500,000;
//	  B sells at 64,935.00 = 129,870 ticks, cost -6,493,500,000;
//	  initial margin at the mark = 10% of 6,500,000,000 + 3 intervals x 0.75% buffer
//	  = 650,000,000 + 146,250,000 = 796,250,000 each.
//	mark 66,000.00 (132,000 ticks, 660,000,000 per contract), variation margin:
//	  A receives 6,600,000,000 - 6,506,500,000 = 93,500,000;
//	  B pays -6,600,000,000 + 6,493,500,000 = -106,500,000.
//	funding [08:00, 16:00): samples (mark - index) / index = 0, 1/999, 1/999;
//	  premium = round(2/2997 x 10^8) = 66,733 (not clamped);
//	  per contract = floor(660,000,000 x 66,733 / 10^8) = 440,437;
//	  A (long) pays 4,404,370; B (short) receives 4,404,370.
//	close A at a fresh 66,000.00 mark, selling at 65,934.00 = 131,868 ticks:
//	  realized 6,593,400,000 - 6,506,500,000 = 86,900,000; A pays
//	  6,593,400,000 - 6,600,000,000 = -6,600,000 against its settled value.
func TestIntegration_DerivativesThroughHTTP(t *testing.T) {
	w := newDerivativesWorld(t)

	// --- the operator lists the perpetual and capitalizes the clearing house.
	res := w.get(w.operator, derivBase+"/contracts?kind=PERPETUAL&limit=200")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var list api.DerivativeContractList
	res.json(&list)
	assert.Equal(t, api.SIMULATED, list.ValueDomain)
	var listed *api.DerivativeContract
	for i := range list.Items {
		if list.Items[i].Id.String() == w.perp.String() {
			listed = &list.Items[i]
		}
	}
	require.NotNil(t, listed, "the perpetual is listed")
	assert.Equal(t, api.DerivativeKindPerpetual, listed.Kind)
	assert.False(t, listed.Marked, "not marked yet")
	require.NotNil(t, listed.Terms.FundingIntervalMs)
	assert.Equal(t, int64(8*3600*1000), *listed.Terms.FundingIntervalMs)
	assert.Equal(t, econ.SandboxDollarSymbol, listed.SettlementAsset.Symbol)
	assert.Equal(t, 6, listed.SettlementAsset.Decimals)

	res = w.post(w.operator, derivBase+"/clearing-house/funding", map[string]any{"quantity": "1000000000000"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, econ.SandboxDollarID().String(), res.raw()["asset"].(map[string]any)["id"])

	// --- 08:00 the operator marks it; two customers open opposite positions.
	m1 := w.mark("65000.00", "65000.00")
	assert.Equal(t, "6500000", m1.MarkMantissa)
	assert.Equal(t, 2, m1.PriceScale)
	require.True(t, m1.HasIndex)
	assert.Equal(t, "6500000", *m1.IndexMantissa)

	res = w.get(w.custA, derivBase+"/contracts/"+w.perp.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var detail api.DerivativeContract
	res.json(&detail)
	require.True(t, detail.Marked)
	assert.Equal(t, m1.Id, detail.LatestMark.Id)

	openA := map[string]any{"account_id": w.a.String(), "instrument_id": w.perp.String(), "side": "LONG", "quantity": "10", "mode": "PAPER"}
	keyA := "itest-open-a-" + uuid.NewString()
	resA := w.as(w.custA).do(http.MethodPost, derivBase+"/positions", openA, "Idempotency-Key", keyA)
	require.Equal(t, http.StatusCreated, resA.Code, "body=%s", resA.Body.String())
	var tradeA api.DerivativeTradeResult
	resA.json(&tradeA)
	assert.Equal(t, "COMPLETED", tradeA.Intent.Status)
	require.NotNil(t, tradeA.Fill)
	assert.Equal(t, "6506500", tradeA.Fill.PriceMantissa, "65,000.00 moved 10 bps against the buyer")
	assert.Equal(t, "6506500000", tradeA.Fill.Value)
	require.NotNil(t, tradeA.Position)
	assert.Equal(t, "796250000", tradeA.Position.MarginPosted)
	require.NotNil(t, tradeA.RiskDecisionId)
	positionA := tradeA.Position.Id

	// The same key again is a replay of the same answer, not a second trade.
	again := w.as(w.custA).do(http.MethodPost, derivBase+"/positions", openA, "Idempotency-Key", keyA)
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, resA.Body.String(), again.Body.String())

	openB := map[string]any{"account_id": w.b.String(), "instrument_id": w.perp.String(), "side": "SHORT", "quantity": "10", "mode": "PAPER"}
	resB := w.post(w.custB, derivBase+"/positions", openB)
	require.Equal(t, http.StatusCreated, resB.Code, "body=%s", resB.Body.String())
	var tradeB api.DerivativeTradeResult
	resB.json(&tradeB)
	assert.Equal(t, "6493500", tradeB.Fill.PriceMantissa, "65,000.00 moved 10 bps against the seller")
	assert.Equal(t, "-10", tradeB.Position.Quantity)
	positionB := tradeB.Position.Id

	// --- the mark moves; 09:00:30 the operator settles [08:00, 09:00).
	w.clk.Set(derivativesStart.Add(30 * time.Minute))
	w.mark("66000.00", "65934.00")
	w.clk.Set(derivativesStart.Add(time.Hour + 30*time.Second))
	w.mark("66000.00", "65934.00")
	vmPath := derivBase + "/contracts/" + w.perp.String() + "/variation-margin-runs"
	interval := map[string]any{"interval_start": derivativesStart.Format(time.RFC3339)}
	res = w.post(w.operator, vmPath, interval)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var vm api.DerivativeRunResult
	res.json(&vm)
	assert.False(t, vm.Replay)
	require.NotNil(t, vm.Paid)
	assert.Equal(t, "106500000", *vm.Paid, "B pays")
	assert.Equal(t, "93500000", *vm.Received, "A receives")
	assert.Equal(t, "0", *vm.Shortfall)
	assert.Equal(t, 2, *vm.Positions)

	// A second run of the interval, under a new key, is the service's replay.
	res = w.post(w.operator, vmPath, interval)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, true, res.raw()["replay"])
	assert.NotContains(t, res.raw(), "paid")

	// A customer cannot run it, and a run needs a recent strong authentication.
	res = w.post(w.custA, vmPath, interval)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())

	// --- 16:00 the operator charges funding for [08:00, 16:00).
	w.clk.Set(derivativesStart.Add(8 * time.Hour))
	res = w.post(w.operator, derivBase+"/contracts/"+w.perp.String()+"/funding-runs", interval)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var fr api.DerivativeRunResult
	res.json(&fr)
	premium, err := derivatives.PremiumIndexE8([]derivatives.IndexSample{
		{Mark: money.QuantityFromInt64(6_500_000), MarkScale: 2, Index: money.QuantityFromInt64(6_500_000), IndexScale: 2},
		{Mark: money.QuantityFromInt64(6_600_000), MarkScale: 2, Index: money.QuantityFromInt64(6_593_400), IndexScale: 2},
		{Mark: money.QuantityFromInt64(6_600_000), MarkScale: 2, Index: money.QuantityFromInt64(6_593_400), IndexScale: 2},
	})
	require.NoError(t, err)
	require.Equal(t, int64(66_733), premium, "the worked example above")
	assert.Equal(t, int64(66_733), *fr.RateE8)
	assert.False(t, *fr.Clamped)
	assert.Equal(t, "440437", *fr.PerContractAmount)
	assert.Equal(t, "4404370", *fr.Paid)
	assert.Equal(t, "4404370", *fr.Received, "a balanced book's funding conserves exactly")

	// --- both customers read their book, valued at a fresh mark.
	w.mark("66000.00", "65934.00")
	pageA := w.positions(w.custA, w.a)
	require.Equal(t, api.DerivativeBookValued, pageA.BookStatus, "refusal=%+v", pageA.BookRefusal)
	require.Len(t, pageA.Items, 1)
	pa := pageA.Items[0]
	assert.Equal(t, positionA, pa.Id)
	assert.Equal(t, api.DerivativePositionOpen, pa.Status)
	assert.Equal(t, api.DerivativeSideLong, *pa.Side)
	assert.Equal(t, "10", pa.Quantity)
	assert.Equal(t, "6506500000", pa.CostValue)
	assert.Equal(t, "6600000000", pa.SettledValue, "variation margin moved the baseline to the mark")
	assert.Equal(t, "-4404370", pa.FundingPnl)
	assert.Equal(t, "796250000", pa.MarginPosted)
	assert.Equal(t, "0", pa.RealizedPnl)
	require.Equal(t, api.DerivativeValuationValued, pa.ValuationStatus)
	assert.Equal(t, "6506500", pa.Valuation.AverageEntryPriceMantissa)
	assert.Equal(t, "93500000", pa.Valuation.UnrealizedPnl)
	assert.Equal(t, "0", pa.Valuation.EquityAdjustment, "variation margin has already paid it")
	assert.Equal(t, "6600000000", pa.Valuation.Requirement.Notional)
	assert.Equal(t, "808500000", pa.Valuation.Requirement.Initial, "10% + the funding buffer of 148,500,000")
	assert.Equal(t, "330000000", pa.Valuation.Requirement.Maintenance)
	assert.Equal(t, "148500000", pa.Valuation.Requirement.FundingBuffer)
	require.NotNil(t, pageA.Book)
	assert.Equal(t, "6600.00", pageA.Book.GrossNotional)
	assert.Equal(t, "808.50", pageA.Book.InitialMargin)
	assert.Equal(t, "330.00", pageA.Book.MaintenanceMargin)
	assert.Equal(t, "93.50", pageA.Book.UnrealizedPnl)
	assert.Equal(t, econ.SandboxDollarSymbol, pageA.Book.QuoteAsset.Symbol)

	pageB := w.positions(w.custB, w.b)
	require.Len(t, pageB.Items, 1)
	pb := pageB.Items[0]
	assert.Equal(t, positionB, pb.Id)
	assert.Equal(t, api.DerivativeSideShort, *pb.Side)
	assert.Equal(t, "-6493500000", pb.CostValue)
	assert.Equal(t, "-6600000000", pb.SettledValue)
	assert.Equal(t, "4404370", pb.FundingPnl)
	assert.Equal(t, "796250000", pb.MarginPosted)
	require.Equal(t, api.DerivativeValuationValued, pb.ValuationStatus)
	assert.Equal(t, "6493500", pb.Valuation.AverageEntryPriceMantissa)
	assert.Equal(t, "-106500000", pb.Valuation.UnrealizedPnl)

	flowsA := w.flows(w.custA, w.a)
	require.Len(t, flowsA, 2)
	assert.Equal(t, api.DerivativeFlowFunding, flowsA[0].Kind, "newest first")
	assert.Equal(t, "-4404370", flowsA[0].Amount)
	assert.Equal(t, api.DerivativeFlowVariationMargin, flowsA[1].Kind)
	assert.Equal(t, "93500000", flowsA[1].Amount)
	for _, f := range flowsA {
		require.NotNil(t, f.JournalTransactionId, "every flow names the journal that posted it")
		assert.Equal(t, "0", f.Shortfall)
		assert.Equal(t, positionA, f.PositionId)
	}
	flowsB := w.flows(w.custB, w.b)
	require.Len(t, flowsB, 2)
	assert.Equal(t, "4404370", flowsB[0].Amount)
	assert.Equal(t, "-106500000", flowsB[1].Amount)

	accA := w.accruals(w.custA, w.a)
	require.Len(t, accA, 1)
	assert.Equal(t, api.DerivativeFundingPay, accA[0].Direction)
	assert.Equal(t, "4404370", accA[0].Amount)
	assert.Equal(t, "10", accA[0].Quantity)
	assert.Equal(t, int64(66_733), accA[0].RateE8)
	assert.Equal(t, "6600000", accA[0].MarkMantissa)
	require.NotNil(t, accA[0].FlowId)
	assert.Equal(t, flowsA[0].Id, *accA[0].FlowId)
	accB := w.accruals(w.custB, w.b)
	require.Len(t, accB, 1)
	assert.Equal(t, api.DerivativeFundingReceive, accB[0].Direction)
	assert.Equal(t, "-10", accB[0].Quantity)

	// --- a stranger reads nothing of A's.
	res = w.get(w.stranger, derivBase+"/positions?account_id="+w.a.String())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	res = w.get(w.stranger, derivBase+"/flows?account_id="+w.a.String())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	res = w.get(w.stranger, derivBase+"/positions/"+positionA.String())
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)
	absent := w.get(w.stranger, derivBase+"/positions/"+uuid.Must(uuid.NewV7()).String())
	require.Equal(t, http.StatusNotFound, absent.Code)
	assert.Equal(t, absent.problem().Detail, res.problem().Detail, "foreign and absent read the same")
	res = w.post(w.stranger, derivBase+"/positions/"+positionA.String()+"/close", map[string]any{"mode": "PAPER"})
	require.Equal(t, http.StatusNotFound, res.Code, "a stranger cannot close it either; body=%s", res.Body.String())
	// An operator reads it, and still cannot close it: OPERATIONS does not
	// hold sandbox:trade at all (an ADMIN, who does, is told the position does
	// not exist -- TestDerivatives_CloseAndExerciseAreOwnerOnlyAndMasked).
	res = w.get(w.operator, derivBase+"/positions/"+positionA.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res = w.post(w.operator, derivBase+"/positions/"+positionA.String()+"/close", map[string]any{"mode": "PAPER"})
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)

	// --- 16:05 the mark is stale: new risk is refused and recorded, and so is a
	// close, which has no fresh price to be filled at.
	w.clk.Set(derivativesStart.Add(8*time.Hour + 5*time.Minute))
	more := map[string]any{"account_id": w.a.String(), "instrument_id": w.perp.String(), "side": "LONG", "quantity": "1", "mode": "PAPER"}
	res = w.post(w.custA, derivBase+"/positions", more)
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	refusal := res.problem()
	assert.Equal(t, errs.CodeRiskRejected, refusal.Code)
	assert.NotEmpty(t, refusal.Fields["reasons"])
	assert.NotEmpty(t, refusal.Fields["risk_decision_id"])
	intentID, _ := refusal.Fields["intent_id"].(string)
	require.NotEmpty(t, intentID, "the refusal names the intent it recorded")
	res = w.get(w.custA, "/v1/advanced/intents/"+intentID)
	require.Equal(t, http.StatusOK, res.Code, "the rejected intent can be found; body=%s", res.Body.String())
	var rejected api.EconomicIntent
	res.json(&rejected)
	assert.Equal(t, "REJECTED", rejected.Status)
	require.NotNil(t, rejected.RejectionCode)
	assert.Equal(t, "RISK_REJECTED", *rejected.RejectionCode)

	pageA = w.positions(w.custA, w.a)
	assert.Equal(t, api.DerivativeBookRefused, pageA.BookStatus, "a stale mark makes the book unvaluable")
	require.NotNil(t, pageA.BookRefusal)
	assert.Equal(t, string(errs.CodeStaleMarketData), pageA.BookRefusal.Code)
	assert.Equal(t, api.DerivativeValuationRefused, pageA.Items[0].ValuationStatus)
	assert.Nil(t, pageA.Items[0].Valuation, "no figure stands in for the one the service could not compute")

	res = w.post(w.custA, derivBase+"/positions/"+positionA.String()+"/close", map[string]any{"mode": "PAPER"})
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "a stale mark prices no close; body=%s", res.Body.String())
	staleClose := res.problem()
	assert.Equal(t, errs.CodeStaleMarketData, staleClose.Code)
	assert.Contains(t, staleClose.Fields["reasons"], "MARK_STALE")
	closeIntent, _ := staleClose.Fields["intent_id"].(string)
	require.NotEmpty(t, closeIntent, "the refusal names the intent it recorded")
	res = w.get(w.custA, "/v1/advanced/intents/"+closeIntent)
	require.Equal(t, http.StatusOK, res.Code, "the refused close can be found; body=%s", res.Body.String())
	res.json(&rejected)
	assert.Equal(t, "REJECTED", rejected.Status)
	require.NotNil(t, rejected.RejectionCode)
	assert.Equal(t, "STALE_MARKET_DATA", *rejected.RejectionCode)

	// The operator marks it again, and the close is priced at the fresh mark.
	w.mark("66000.00", "65934.00")
	res = w.post(w.custA, derivBase+"/positions/"+positionA.String()+"/close", map[string]any{"mode": "PAPER"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var closed api.DerivativeTradeResult
	res.json(&closed)
	assert.Equal(t, "6593400", closed.Fill.PriceMantissa, "the fresh mark, 10 bps against the seller")
	assert.Equal(t, "86900000", closed.Fill.RealizedPnl)
	assert.Equal(t, "-6600000", closed.Fill.AmountSettled)
	assert.Equal(t, api.DerivativePositionClosed, closed.Position.Status)
	assert.Equal(t, "0", closed.Position.Quantity)
	assert.Equal(t, "0", closed.Position.MarginPosted, "the margin came back")
	assert.Equal(t, "86900000", closed.Position.RealizedPnl)

	flowsA = w.flows(w.custA, w.a)
	require.Len(t, flowsA, 3)
	assert.Equal(t, api.DerivativeFlowRealized, flowsA[0].Kind)
	assert.Equal(t, "-6600000", flowsA[0].Amount)

	res = w.get(w.custA, derivBase+"/positions/"+positionA.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "NOT_APPLICABLE", res.raw()["valuation_status"], "a closed position has no valuation")

	// --- the clearing house's books agree with the register.
	res = w.post(w.operator, derivBase+"/clearing-house/reconciliations", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var rep api.DerivativeClearingReport
	res.json(&rep)
	assert.True(t, rep.Matched, "%+v", rep)
	assert.Equal(t, int64(5), rep.Flows)
	// 93.5 - 106.5 - 4.40437 + 4.40437 - 6.6 sandbox dollars, customer side.
	assert.Equal(t, "-19600000", rep.RegisterNet)
	assert.Equal(t, "19600000", rep.Result)
	assert.Equal(t, "0", rep.Difference)
	assert.False(t, rep.InternallyMatched, "B's short is still open against the clearing house")
}

// TestIntegration_ADerivativeSettlesByTheRuleOrByATwoOperatorOverrideThroughHTTP:
// a dated future whose feed stalled five minutes before expiry cannot settle
// by the rule; the contract says so and what an override may propose; one
// operator proposes a price with evidence, cannot approve it itself, and a
// second operator's approval settles the contract at exactly that price --
// all over the real adapter, service and database.
func TestIntegration_ADerivativeSettlesByTheRuleOrByATwoOperatorOverrideThroughHTTP(t *testing.T) {
	w := newDerivativesWorld(t)
	expiry := derivativesStart.Add(2 * time.Hour)
	fut := w.listFuture(expiry)
	w.markOn(fut, "65000.00", "65000.00")
	for acct, side := range map[accounts.AccountID]string{w.a: "LONG", w.b: "SHORT"} {
		owner := w.custA
		if acct == w.b {
			owner = w.custB
		}
		res := w.post(owner, derivBase+"/positions", map[string]any{
			"account_id": acct.String(), "instrument_id": fut.String(), "side": side, "quantity": "2", "mode": "PAPER"})
		require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	}
	w.clk.Set(expiry.Add(-5 * time.Minute))
	ref := w.markOn(fut, "66000.00", "66000.00")
	w.clk.Set(expiry.Add(time.Hour))
	settle := derivBase + "/contracts/" + fut.String() + "/settlement"

	res := w.post(w.operator, settle, map[string]any{})
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "no fresh mark at expiry; body=%s", res.Body.String())
	assert.Equal(t, errs.CodeStaleMarketData, res.problem().Code)

	res = w.get(w.operator, derivBase+"/contracts/"+fut.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var c api.DerivativeContract
	res.json(&c)
	require.NotNil(t, c.SettlementPreview)
	assert.True(t, c.SettlementPreview.Expired)
	assert.False(t, c.SettlementPreview.Settleable)
	require.NotNil(t, c.SettlementPreview.ReferenceMark)
	assert.Equal(t, ref.Id, c.SettlementPreview.ReferenceMark.Id)
	assert.Equal(t, "6600000", *c.SettlementPreview.ReferencePriceMantissa)
	assert.Equal(t, "5940000", *c.SettlementPreview.OverrideMinPriceMantissa)
	assert.Equal(t, "7260000", *c.SettlementPreview.OverrideMaxPriceMantissa)
	require.NotNil(t, c.SettlementOverrides)
	assert.Empty(t, *c.SettlementOverrides)

	evidence := strings.Repeat("5e", 32)
	res = w.post(w.operator, settle, map[string]any{"method": "PROPOSE_OVERRIDE", "price": "67000.00",
		"reason": "the feed stalled five minutes before expiry", "evidence_ref": "s3://sandbox-evidence/bulletin.pdf",
		"evidence_sha256": evidence})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var proposed api.DerivativeSettlementCommandResult
	res.json(&proposed)
	require.NotNil(t, proposed.Override)
	assert.Nil(t, proposed.Settlement)
	assert.Equal(t, api.DerivativeOverrideProposed, proposed.Override.Status)
	assert.Equal(t, evidence, proposed.Override.EvidenceSha256)
	assert.Equal(t, w.operator.SubjectID, proposed.Override.ProposedBy.ActorId)
	overrideID := proposed.Override.Id.String()

	res = w.post(w.operator, settle, map[string]any{"method": "APPROVE_OVERRIDE", "override_id": overrideID, "reason": "agreed"})
	require.Equal(t, http.StatusForbidden, res.Code, "the proposer cannot approve it; body=%s", res.Body.String())

	second := w.operator
	second.SubjectID = uuid.NewString()
	res = w.post(second, settle, map[string]any{"method": "APPROVE_OVERRIDE", "override_id": overrideID,
		"reason": "checked against the bulletin", "price": "67000"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var approved api.DerivativeSettlementCommandResult
	res.json(&approved)
	require.NotNil(t, approved.Settlement)
	require.NotNil(t, approved.Override)
	assert.Equal(t, api.DerivativeOverrideApproved, approved.Override.Status)
	assert.Equal(t, "6700000", approved.Settlement.PriceMantissa)
	require.NotNil(t, approved.Settlement.PriceRule)
	assert.Equal(t, api.DerivativePriceRuleOperatorOverride, *approved.Settlement.PriceRule)
	require.NotNil(t, approved.Settlement.OverrideId)
	assert.Equal(t, overrideID, approved.Settlement.OverrideId.String())
	assert.Equal(t, 2, approved.Settlement.Positions)

	res = w.get(w.custA, derivBase+"/contracts/"+fut.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&c)
	assert.True(t, c.Settled)
	require.NotNil(t, c.Settlement)
	require.NotNil(t, c.Settlement.PriceRule)
	assert.Equal(t, api.DerivativePriceRuleOperatorOverride, *c.Settlement.PriceRule)
	require.Len(t, *c.SettlementOverrides, 1)
	got := (*c.SettlementOverrides)[0]
	assert.Equal(t, api.DerivativeOverrideApproved, got.Status)
	require.NotNil(t, got.Decision)
	assert.Equal(t, second.SubjectID, got.Decision.DecidedBy.ActorId)
	require.NotNil(t, got.SettlementId)

	// The rule's settlement is now a replay of the one that happened.
	res = w.post(w.operator, settle, map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var replay api.DerivativeSettlementCommandResult
	res.json(&replay)
	require.NotNil(t, replay.Settlement)
	assert.True(t, replay.Settlement.Replay)
	assert.Equal(t, "6700000", replay.Settlement.PriceMantissa)
}

// TestIntegration_AnOperatorDefinesAContractThatTradesThroughHTTP: an
// operator defines a perpetual over the API, through the derivatives service's
// definition, on the underlying of the world's own perpetual and settled by
// default in the sandbox dollar. It is answered, listed and read exactly as
// defined; once marked, a customer opens a position in it. Its key replays it,
// its symbol is taken, a customer may not define one, and a definition the
// service could not settle exactly is refused with nothing written.
func TestIntegration_AnOperatorDefinesAContractThatTradesThroughHTTP(t *testing.T) {
	w := newDerivativesWorld(t)
	res := w.get(w.operator, derivBase+"/contracts/"+w.perp.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var existing api.DerivativeContract
	res.json(&existing)
	require.NotNil(t, existing.Terms.UnderlyingInstrumentId)
	underlying := existing.Terms.UnderlyingInstrumentId.String()

	symbol := "DEF" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:8]) + "-PERP"
	definition := func(symbol, multiplier string) map[string]any {
		return map[string]any{
			"kind": "PERPETUAL", "symbol": symbol, "underlying_instrument_id": underlying, "contract_multiplier": multiplier,
			"price_scale": 2, "tick_size": "50", "lot_size": "1", "min_quantity": "1", "funding_interval_ms": 8 * 3600 * 1000,
			"oracle_max_age_ms": 60_000, "oracle_sources": []string{"itest-operator"}, "risk_class": "SPECULATIVE",
		}
	}

	refused := w.post(w.custA, derivBase+"/contracts", definition(symbol, "1000000"))
	require.Equal(t, http.StatusForbidden, refused.Code, "a customer defines no contract; body=%s", refused.Body.String())

	key := "itest-define-" + uuid.NewString()
	res = w.as(w.operator).do(http.MethodPost, derivBase+"/contracts", definition(symbol, "1000000"), "Idempotency-Key", key)
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var c api.DerivativeContract
	res.json(&c)
	assert.Equal(t, api.SIMULATED, c.ValueDomain)
	assert.Equal(t, api.DerivativeKindPerpetual, c.Kind)
	assert.Equal(t, symbol, c.CanonicalName, "named by its symbol")
	assert.Equal(t, "ACTIVE", c.Status)
	assert.Equal(t, 1, c.TermsVersion)
	assert.Equal(t, symbol, c.Terms.Symbol)
	assert.Equal(t, "24x7", c.Terms.TradingCalendar)
	assert.Equal(t, "NODAL-SANDBOX", c.Terms.VenueCode)
	assert.Equal(t, "LINEAR", string(c.Terms.MarginModel))
	assert.Equal(t, "CASH", string(c.Terms.SettlementStyle))
	assert.Equal(t, econ.SandboxDollarSymbol, c.SettlementAsset.Symbol, "the sandbox dollar, by default")
	assert.False(t, c.Marked)
	assert.False(t, c.Settled)

	again := w.as(w.operator).do(http.MethodPost, derivBase+"/contracts", definition(symbol, "1000000"), "Idempotency-Key", key)
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, res.Body.String(), again.Body.String(), "the same key is the same definition, not a second contract")
	taken := w.post(w.operator, derivBase+"/contracts", definition(symbol, "1000000"))
	require.Equal(t, http.StatusConflict, taken.Code, "body=%s", taken.Body.String())
	assert.Equal(t, errs.CodeConflict, taken.problem().Code)

	res = w.get(w.custA, derivBase+"/contracts?kind=PERPETUAL&limit=200")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var list api.DerivativeContractList
	res.json(&list)
	listed := 0
	for _, item := range list.Items {
		if item.Terms.Symbol == symbol {
			listed++
			assert.Equal(t, c.Id, item.Id)
		}
	}
	assert.Equal(t, 1, listed, "listed once")

	defined, err := instruments.ParseInstrumentID(c.Id.String())
	require.NoError(t, err)
	w.markOn(defined, "65000.00", "65000.00")
	open := map[string]any{"account_id": w.a.String(), "instrument_id": c.Id.String(), "side": "LONG", "quantity": "1", "mode": "PAPER"}
	resA := w.post(w.custA, derivBase+"/positions", open)
	require.Equal(t, http.StatusCreated, resA.Code, "a defined contract trades; body=%s", resA.Body.String())
	var trade api.DerivativeTradeResult
	resA.json(&trade)
	require.NotNil(t, trade.Position)
	assert.Equal(t, "1", trade.Position.Quantity)

	// One base unit of the underlying a contract moves 0.005 of a base unit of
	// the sandbox dollar a tick: the service refuses it, and nothing is written.
	inexact := "DEF" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:8]) + "-PERP"
	res = w.post(w.operator, derivBase+"/contracts", definition(inexact, "1"))
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
	var terms int
	require.NoError(t, w.d.Pool().QueryRow(t.Context(), `SELECT count(*) FROM instrument_terms WHERE symbol = $1`, inexact).Scan(&terms))
	assert.Zero(t, terms, "a refused definition writes nothing")
}
