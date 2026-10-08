//go:build integration

package derivatives

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

func (f *fixture) tryOpen(r OpenRequest) (Outcome, error) {
	var out Outcome
	err := f.do(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.OpenPosition(ctx, tx, r)
		return err
	})
	return out, err
}

func requireRefusal(t *testing.T, err error, code errs.Code, reason string) *Refusal {
	t.Helper()
	require.Error(t, err)
	r, ok := AsRefusal(err)
	require.True(t, ok, "expected a refusal, got %v", err)
	assert.Equal(t, code, r.Code)
	if reason != "" {
		assert.Contains(t, r.Reasons, reason)
	}
	return r
}

// TestIntegration_StaleOraclePreventsARiskIncreasingOrderButNotClosing: a
// mark older than the risk policy's 30 seconds refuses new risk with
// RISK_ORACLE_STALE (recorded: the intent is REJECTED and the decision
// persisted), one older than the instrument's own 60 seconds is unusable
// altogether -- and neither stops the customer closing.
func TestIntegration_StaleOraclePreventsARiskIncreasingOrderButNotClosing(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a := f.account()
	f.fund(a, usdUnits(100_000))
	f.mark(perp, 65_000, 65_000)
	f.open(a, perp, Long, 2)

	f.clk.Advance(45 * time.Second)
	out, err := f.tryOpen(OpenRequest{IntentRequest: f.req(a, perp), Side: Long, Quantity: q(1)})
	ref := requireRefusal(t, err, errs.CodeRiskRejected, risk.ReasonOracleStale)
	require.NotNil(t, ref.DecisionID)
	assert.Equal(t, econ.StatusRejected, out.Intent.Status)
	var verdict string
	var reasons []string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT verdict, reason_codes FROM advanced_risk_decisions WHERE id = $1`, *ref.DecisionID).
		Scan(&verdict, &reasons))
	assert.Equal(t, "REJECT", verdict)
	assert.Contains(t, reasons, risk.ReasonOracleStale)
	var status, code string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status, rejection_code FROM economic_intents WHERE id = $1`, ref.IntentID).Scan(&status, &code))
	assert.Equal(t, "REJECTED", status)
	assert.Equal(t, "RISK_REJECTED", code)

	f.clk.Advance(30 * time.Second) // 75 seconds: beyond the instrument's own limit
	_, err = f.tryOpen(OpenRequest{IntentRequest: f.req(a, perp), Side: Long, Quantity: q(1)})
	requireRefusal(t, err, errs.CodeRiskRejected, risk.ReasonOracleUnavailable)
	assert.Equal(t, "2", f.position(a, perp).Quantity.String(), "no refused order changed the position")

	closed := f.closeAll(a, perp)
	assert.Equal(t, econ.StatusCompleted, closed.Intent.Status, "closing on a stale mark is never refused")
	assert.Equal(t, PositionClosed, f.position(a, perp).Status)
}

// TestIntegration_IdentityIsReadFromTheComplianceRecord: whatever the request
// claims, an account whose owner has no profile, is pending, expired, or
// verified with an expiry in the past, cannot open new derivative risk.
func TestIntegration_IdentityIsReadFromTheComplianceRecord(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	f.mark(perp, 65_000, 65_000)
	lapsed := f.account()
	_, err := testDB.Pool().Exec(f.ctx, `UPDATE compliance_profiles SET expires_at = $2
		WHERE user_id = (SELECT owner_user_id FROM accounts WHERE id = $1)`, lapsed, start.Add(-24*time.Hour))
	require.NoError(t, err)
	for _, a := range []accounts.AccountID{f.accountWithIdentity(""), f.accountWithIdentity("PENDING"),
		f.accountWithIdentity("EXPIRED"), lapsed} {
		f.fund(a, usdUnits(100_000))
		req := f.req(a, perp)
		req.Verification = valuedomain.VerificationEnhanced // claimed, not believed
		_, err := f.tryOpen(OpenRequest{IntentRequest: req, Side: Long, Quantity: q(1)})
		requireRefusal(t, err, errs.CodeRiskRejected, risk.ReasonIdentityUnverified)
	}
}

// TestIntegration_AStaleSettlementAssetPriceRefusesNewRiskButNotClosing: a
// contract settling in a sandbox euro is valued in dollars through the
// EUR/USD price. Once that price is stale the book cannot be valued: new risk
// is refused (recorded, as an unavailable oracle) and closing still works.
func TestIntegration_AStaleSettlementAssetPriceRefusesNewRiskButNotClosing(t *testing.T) {
	f := newFixture(t)
	eur := f.simAsset("DVEUR", 6, assets.RiskSettlement)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.FundClearingHouse(ctx, tx, eur, usdUnits(1_000_000), "itest-eur-"+uuid.NewString())
		return err
	}))
	perp := f.create(f.termsIn(instruments.TypePerpetual, eur))
	_, err := f.prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{
		AssetID: eur, QuoteAssetID: f.usd, Mantissa: q(108), Scale: 2, Source: "itest-fx", ObservedAt: f.clk.Now(),
	})
	require.NoError(t, err)
	a := f.account()
	f.fundIn(a, eur, usdUnits(100_000))
	f.mark(perp, 60_000, 60_000)
	f.open(a, perp, Long, 1)

	f.clk.Advance(45 * time.Second) // the EUR/USD price is older than the policy's 30 seconds
	f.mark(perp, 60_000, 60_000)    // the contract's own mark is fresh
	_, err = f.tryOpen(OpenRequest{IntentRequest: f.req(a, perp), Side: Long, Quantity: q(1)})
	requireRefusal(t, err, errs.CodeRiskRejected, risk.ReasonOracleUnavailable)
	closed := f.closeAll(a, perp)
	assert.Equal(t, econ.StatusCompleted, closed.Intent.Status, "closing is not blocked by a stale valuation")
	assert.Equal(t, PositionClosed, f.position(a, perp).Status)
}

// TestIntegration_MarkIndexDivergenceRefusesNewRisk: a mark too far from its
// index refuses new risk (RISK_ORACLE_DIVERGENCE); for an option, a premium
// outside the bounds its index allows.
func TestIntegration_MarkIndexDivergenceRefusesNewRisk(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	call := f.option(instruments.Call, instruments.European, 60_000, start.Add(30*24*time.Hour))
	a := f.account()
	f.fund(a, usdUnits(100_000))
	f.mark(perp, 65_000, 61_000) // 655.7 bps over a 500 bps limit
	_, err := f.tryOpen(OpenRequest{IntentRequest: f.req(a, perp), Side: Short, Quantity: q(1)})
	requireRefusal(t, err, errs.CodeRiskRejected, risk.ReasonOracleDivergence)
	f.mark(call, 1_000, 65_000) // below the $5 000 intrinsic value by 615 bps of the index
	_, err = f.tryOpen(OpenRequest{IntentRequest: f.req(a, call), Side: Long, Quantity: q(1)})
	requireRefusal(t, err, errs.CodeRiskRejected, risk.ReasonOracleDivergence)
	f.clk.Advance(time.Second)
	f.mark(call, 5_200, 65_000)
	assert.Equal(t, econ.StatusCompleted, f.open(a, call, Long, 1).Intent.Status, "a consistent premium trades")
}

// TestIntegration_AReservationStopsAnOverspendTheRiskCheckAllowed: the
// account's equity covers the order's margin, so the risk kernel allows it,
// but most of the wallet is already reserved for another order. The capital
// reservation -- taken under the account's totals lock, the step that makes
// concurrent spending of one balance impossible -- refuses it with
// INSUFFICIENT_BUYING_POWER, and nothing is posted.
func TestIntegration_AReservationStopsAnOverspendTheRiskCheckAllowed(t *testing.T) {
	f := newFixtureWith(t, func(c *Config) { c.RiskPolicy.MaxLeverageBPS = 500_000 })
	a := f.account()
	perp := f.perp()
	im := q(796_250_000) // 10 contracts at $65 000: 10% plus the 2.25% funding buffer
	f.fund(a, im.Add(im.MulBPS(5_000, money.RoundDown)))
	f.mark(perp, 65_000, 65_000)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.capital.Reserve(ctx, tx, capitalReserve(a, f.usd, im))
		return err
	}))

	_, err := f.tryOpen(OpenRequest{IntentRequest: f.req(a, perp), Side: Long, Quantity: q(10)})
	ref := requireRefusal(t, err, errs.CodeInsufficientBuyingPower, "")
	require.NotNil(t, ref.DecisionID)
	var verdict string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT verdict FROM advanced_risk_decisions WHERE id = $1`, *ref.DecisionID).Scan(&verdict))
	assert.Equal(t, "ALLOW", verdict, "the risk check allowed it; the reservation stopped it")
	assert.True(t, f.balance(a, ledger.CodeDerivativesMargin).IsZero())
	assert.Zero(t, f.count(`SELECT count(*) FROM derivative_positions WHERE instrument_id = $1`, perp))
	assert.Zero(t, f.count(`SELECT count(*) FROM asset_reservations WHERE idempotency_key = $1`, "derivatives:reserve:"+ref.IntentID.String()))
}

// TestIntegration_ConcurrentOpensCannotOverspendMargin: six concurrent
// orders, margin for three. Exactly three succeed whatever the interleaving;
// the rest are refused; nothing goes negative and every reservation is
// consumed.
func TestIntegration_ConcurrentOpensCannotOverspendMargin(t *testing.T) {
	f := newFixtureWith(t, func(c *Config) { c.RiskPolicy.MaxLeverageBPS = 500_000 })
	a := f.account()
	im := q(796_250_000)
	f.fund(a, im.Mul(q(3)).Add(q(200_000_000)))
	const n = 6
	perps := make([]instruments.InstrumentID, n)
	for i := range perps {
		perps[i] = f.perp()
		f.mark(perps[i], 65_000, 65_000)
	}
	var wg sync.WaitGroup
	errsCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.tryOpen(OpenRequest{IntentRequest: f.req(a, perps[i]), Side: Long, Quantity: q(10)})
			errsCh <- err
		}()
	}
	wg.Wait()
	close(errsCh)
	ok := 0
	for err := range errsCh {
		if err == nil {
			ok++
			continue
		}
		r, isRefusal := AsRefusal(err)
		require.True(t, isRefusal, "%v", err)
		assert.Contains(t, []errs.Code{errs.CodeInsufficientBuyingPower, errs.CodeRiskRejected}, r.Code)
	}
	assert.Equal(t, 3, ok)
	assert.Equal(t, im.Mul(q(3)).String(), f.balance(a, ledger.CodeDerivativesMargin).String())
	assert.Equal(t, "200000000", f.balance(a, ledger.CodeWallet).String())
	assert.Equal(t, 3, f.count(`SELECT count(*) FROM asset_reservations WHERE account_id = $1 AND status = 'CONSUMED'`, a))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM asset_reservations WHERE account_id = $1 AND status = 'ACTIVE'`, a))
	assert.Equal(t, 3, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1 AND status = 'REJECTED'`, a))
}

// TestIntegration_OpenPositionIsExactlyOncePerIdempotencyKey.
func TestIntegration_OpenPositionIsExactlyOncePerIdempotencyKey(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a := f.account()
	f.fund(a, usdUnits(100_000))
	f.mark(perp, 65_000, 65_000)
	req := OpenRequest{IntentRequest: f.req(a, perp), Side: Long, Quantity: q(2)}
	first, err := f.tryOpen(req)
	require.NoError(t, err)
	margin := f.balance(a, ledger.CodeDerivativesMargin)
	journals := f.count(`SELECT count(*) FROM journal_transactions`)
	second, err := f.tryOpen(req)
	require.NoError(t, err)
	assert.True(t, second.Replay)
	assert.Equal(t, first.Intent.ID, second.Intent.ID)
	require.NotNil(t, second.Fill)
	assert.Equal(t, first.Fill.ID, second.Fill.ID)
	assert.Equal(t, margin.String(), f.balance(a, ledger.CodeDerivativesMargin).String())
	assert.Equal(t, journals, f.count(`SELECT count(*) FROM journal_transactions`), "a replay posts nothing")
	fills, err := FillsForIntent(f.ctx, testDB, first.Intent.ID)
	require.NoError(t, err)
	assert.Len(t, fills, 1)

	reuse := req
	reuse.Quantity = q(3)
	_, err = f.tryOpen(reuse)
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))

	// The same book fill applied twice is one fill.
	did := f.allowDecision(a, perp)
	m, _, err := LatestMark(f.ctx, testDB, perp)
	require.NoError(t, err)
	fill := Fill{Key: "book:" + uuid.NewString(), AccountID: a, InstrumentID: perp, Side: Buy, Quantity: q(1), Price: dollars(65_000),
		Source: SourceBook, MarkID: m.ID, Direction: FillEither, DecisionID: did, OccurredAt: f.clk.Now()}
	for i := 0; i < 2; i++ {
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			res, err := f.svc.ApplyFill(ctx, tx, fill)
			assert.Equal(t, i == 1, res.Replay)
			return err
		}))
	}
	assert.Equal(t, "3", f.position(a, perp).Quantity.String())
}

// TestIntegration_ModeAndAuthorityAreEnforced: DEVNET is not a mode
// derivatives implement (the authorizer refuses it, recorded), LIVE cannot
// even be requested, and an agent cannot open a position: the settlement
// compiler refuses an agent below the authority the action needs, and above
// it the risk kernel refuses an agent intent that carries no authority
// section.
func TestIntegration_ModeAndAuthorityAreEnforced(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a := f.account()
	f.fund(a, usdUnits(100_000))
	f.mark(perp, 65_000, 65_000)

	devnet := f.req(a, perp)
	devnet.Mode = gates.ModeDevnet
	_, err := f.tryOpen(OpenRequest{IntentRequest: devnet, Side: Long, Quantity: q(1)})
	ref := requireRefusal(t, err, errs.CodeCapabilityNotApproved, gates.ReasonModeNotSupported)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_intents WHERE id = $1 AND status = 'REJECTED'`, ref.IntentID))

	live := f.req(a, perp)
	live.Mode = gates.ModeLive
	_, err = f.tryOpen(OpenRequest{IntentRequest: live, Side: Long, Quantity: q(1)})
	require.Error(t, err)
	_, isRefusal := AsRefusal(err)
	assert.False(t, isRefusal, "LIVE is not refused, it is unrepresentable")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	for _, level := range []agentauthority.Level{agentauthority.LevelResearchOnly, agentauthority.LevelUserApprovedRule} {
		agent := f.req(a, perp)
		agent.ActorType, agent.ActorID, agent.AgentID, agent.AgentAuthority = security.ActorAgent, "agent-1", uuid.NewString(), level
		_, err = f.tryOpen(OpenRequest{IntentRequest: agent, Side: Long, Quantity: q(1)})
		require.Error(t, err, "level %s", level)
		_, isRefusal = AsRefusal(err)
		assert.True(t, isRefusal, "level %s: %v", level, err)
	}
	assert.Zero(t, f.count(`SELECT count(*) FROM derivative_positions WHERE account_id = $1`, a), "no refused intent opened anything")
}

// TestIntegration_MarginReaderFeedsBuyingPowerV2 computes Buying Power V2 for
// an account holding a perpetual and a written call, through the derivatives
// MarginReader, and checks the numbers it reports.
func TestIntegration_MarginReaderFeedsBuyingPowerV2(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	call := f.option(instruments.Call, instruments.European, 65_000, start.Add(48*time.Hour))
	a, idle := f.account(), f.account()
	f.fund(a, usdUnits(100_000))
	f.fund(idle, usdUnits(1_000))
	f.mark(perp, 65_000, 65_000)
	f.mark(call, 1_000, 65_000)
	f.open(a, perp, Long, 10)
	f.open(a, call, Short, 2)

	rc, err := f.svc.AccountRisk(f.ctx, testDB, a)
	require.NoError(t, err)
	assert.Equal(t, "7800.00", rc.GrossNotional.String(), "$6 500 of perpetual, $1 300 of underlying")
	assert.Equal(t, "1296.25", rc.InitialMargin.String(), "$796.25 + $500")
	assert.Equal(t, "725.00", rc.MaintenanceMargin.String(), "$325 + $400")
	assert.Equal(t, "-6.52", rc.UnrealizedPnL.String(), "the paper slippage: $6.50 and $0.02")
	assert.Equal(t, "-26.50", rc.EquityAdjustment.String(), "−$6.50 unsettled, −$20 the written calls' value")
	require.Len(t, rc.Positions, 2)

	loader, err := buyingpower.NewV2Loader(buyingpower.V2LoaderConfig{
		Clock: f.clk, Prices: f.prices, QuoteAssetID: f.usd, MaxPriceAge: time.Minute, Margin: f.svc.MarginReader(),
	})
	require.NoError(t, err)
	in, err := loader.Load(f.ctx, testDB, "CUSTOMER", a.String())
	require.NoError(t, err)
	assert.Equal(t, buyingpower.V2MarginState{
		Mode: "ISOLATED", UnrealizedPnL: money.USDFromMinor(-2_650), InitialMarginUsed: money.USDFromMinor(129_625),
		MaintenanceMargin: money.USDFromMinor(72_500), InitialMarginRateBPS: 1_000,
	}, in.Margin)
	v2, err := buyingpower.EvaluateV2(in)
	require.NoError(t, err)
	// $100 000 + $19.98 of premium received, less the book's $26.50.
	assert.Equal(t, "99993.48", v2.GrossEquity.String())
	assert.Equal(t, "1296.25", v2.InitialMarginUsed.String())
	assert.Equal(t, "98697.23", v2.MarginAvailable.String())

	idleIn, err := loader.Load(f.ctx, testDB, "CUSTOMER", idle.String())
	require.NoError(t, err)
	assert.Equal(t, "CASH", idleIn.Margin.Mode, "no derivative positions, no derivative margin state")

	f.clk.Advance(2 * time.Minute)
	_, err = loader.Load(f.ctx, testDB, "CUSTOMER", a.String())
	require.Error(t, err, "buying power is not computed on a book whose marks are stale")
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))
}

// TestIntegration_TheIndexComesFromValuationWhenAvailable: a mark recorded
// without an index takes the underlying's fresh spot from asset_prices; with
// none fresh, it is recorded without one and cannot back new risk.
func TestIntegration_TheIndexComesFromValuationWhenAvailable(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a := f.account()
	f.fund(a, usdUnits(100_000))
	record := func() Mark {
		var m Mark
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			m, err = f.svc.RecordMark(ctx, tx, RecordMarkRequest{InstrumentID: perp, Source: "itest", ObservedAt: f.clk.Now(),
				Mark: money.Price{Mantissa: dollars(65_000), Scale: 2, QuoteAsset: f.usd.String(), Source: "itest", At: f.clk.Now()}})
			return err
		}))
		return m
	}
	bare := record()
	assert.False(t, bare.HasIndex)
	_, err := f.tryOpen(OpenRequest{IntentRequest: f.req(a, perp), Side: Long, Quantity: q(1)})
	requireRefusal(t, err, errs.CodeRiskRejected, risk.ReasonOracleUnavailable)

	f.clk.Advance(time.Second)
	_, err = f.prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{
		AssetID: f.btc, QuoteAssetID: f.usd, Mantissa: q(6_500_012_345), Scale: 5, Source: "itest-spot", ObservedAt: f.clk.Now(),
	})
	require.NoError(t, err)
	f.clk.Advance(time.Second)
	indexed := record()
	require.True(t, indexed.HasIndex)
	assert.Equal(t, "6500012345", indexed.IndexMantissa.String())
	assert.Equal(t, int32(5), indexed.IndexScale)
	assert.Equal(t, "asset_prices:itest-spot", indexed.IndexSource)
	assert.Equal(t, econ.StatusCompleted, f.open(a, perp, Long, 1).Intent.Status)

	// A replay of an observation says what it said the first time: the same
	// explicit index returns the stored mark, a different one is a CONFLICT.
	f.clk.Advance(time.Second)
	first := f.mark(perp, 65_000, 65_000)
	assert.Equal(t, first.ID, f.mark(perp, 65_000, 65_000).ID)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		idx := money.Price{Mantissa: dollars(66_000), Scale: 2, QuoteAsset: f.usd.String(), Source: "itest-index", At: f.clk.Now()}
		_, err := f.svc.RecordMark(ctx, tx, RecordMarkRequest{InstrumentID: perp, Source: "itest", ObservedAt: f.clk.Now(), Index: &idx,
			Mark: money.Price{Mantissa: dollars(65_000), Scale: 2, QuoteAsset: f.usd.String(), Source: "itest", At: f.clk.Now()}})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	// A mark off the tick grid is refused, never rounded by the store.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.RecordMark(ctx, tx, RecordMarkRequest{InstrumentID: perp, Source: "itest", ObservedAt: f.clk.Now(),
			Mark: money.Price{Mantissa: q(6_500_010), Scale: 2, QuoteAsset: f.usd.String(), Source: "itest", At: f.clk.Now()}})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}
