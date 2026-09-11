//go:build integration

package nativemarket

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/prediction"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// STAGE 15: Reality, Prediction and Proof, over Domain A.
//
// The three subsystems already existed and worked over Domain B and C. What
// did not exist was any way for them to see the Nodal-native economy: no
// prices, no instrument to name a market by, and no audit events, so a native
// trade was invisible to the tamper-evident history and unscoreable by the
// prediction ledger. These tests are the evidence that it is no longer.

// TestIntegration_ATradePublishesAPriceWhoseKnowledgeTimeIsTheTrade is the
// Reality half (PART XLII).
//
// For an external feed, observed_at and received_at differ and the difference
// is the provider's delivery lag. For this venue there is no provider: Nodal
// observed the trade by executing it. The two instants are therefore equal by
// construction, and the test asserts that rather than accepting whatever the
// code happened to write, because an invented lag is exactly what would let a
// backtest believe a price was knowable before it existed.
func TestIntegration_ATradePublishesAPriceWhoseKnowledgeTimeIsTheTrade(t *testing.T) {
	f := newFixture(t)

	// The market published an opening price when it was created. Without one,
	// a prediction committed before the first trade could not be resolved at
	// all: the resolver needs a price at or before the prediction's own
	// commit instant and correctly refuses to invent one.
	opening := f.pricesFor(t)
	require.Len(t, opening, 1, "market creation publishes the opening price")
	require.Equal(t, "native_market:"+f.market.ID.String(), opening[0].rawRef)

	buy, err := f.buy(f.trader, 2_000_000_000, money.Quantity{})
	require.NoError(t, err)

	after := f.pricesFor(t)
	require.Len(t, after, 2)
	p := after[1]
	require.Equal(t, buy.Fill.SpotAfter.String(), p.mantissa,
		"the published price is the marginal price the NEXT trader faces, not this trade's effective price")
	require.EqualValues(t, PriceScale, p.scale)
	require.Equal(t, PriceSource, p.source)
	require.Equal(t, "native_market_fill:"+buy.FillID.String(), p.rawRef,
		"every price must be traceable to the trade that set it")
	require.True(t, p.observedAt.Equal(p.receivedAt),
		"Nodal is the venue: there is no provider clock to lag behind, so inventing a gap would be a lie")

	// A second trade at the same fake-clock instant must still produce a
	// distinct, strictly later observation. asset_prices is append-only and
	// its identity includes observed_at, so a collision would either fail the
	// trade or lose the price; instead the stamp is nudged forward.
	sell, err := f.sell(f.trader, buy.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err)
	third := f.pricesFor(t)
	require.Len(t, third, 3)
	require.True(t, third[2].observedAt.After(third[1].observedAt),
		"two prices from one market must not share an instant")
	require.Equal(t, sell.Fill.SpotAfter.String(), third[2].mantissa)
	require.True(t, third[2].observedAt.Equal(third[2].receivedAt))
}

// TestIntegration_ATradeIsInTheAccountsVerifiableAuditStream is the Proof half
// (PARTS 87-88).
//
// internal/proof builds Merkle checkpoints and proof bundles out of
// audit_events. A native trade that wrote no audit event was outside all of
// it — the tamper-evident history simply did not cover the economy the product
// is built on. The assertion is not "a row exists" but "the stream verifies",
// because a row whose hash chain does not check proves nothing.
func TestIntegration_ATradeIsInTheAccountsVerifiableAuditStream(t *testing.T) {
	f := newFixture(t)
	buy, err := f.buy(f.trader, 1_500_000_000, money.Quantity{})
	require.NoError(t, err)

	stream := audit.AccountStream(f.trader.String())
	var (
		action, resourceType, resourceID string
		contentHash                      []byte
	)
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT action, resource_type, resource_id, content_hash
		   FROM audit_events WHERE stream = $1 ORDER BY stream_seq DESC LIMIT 1`,
		stream).Scan(&action, &resourceType, &resourceID, &contentHash))
	require.Equal(t, "native_market.fill", action)
	require.Equal(t, "native_market_fill", resourceType)
	require.Equal(t, buy.FillID.String(), resourceID)
	require.NotEmpty(t, contentHash)

	rep, err := audit.NewVerifier().VerifyStream(f.ctx, testDB, stream)
	require.NoError(t, err)
	require.True(t, rep.OK, "the trader's audit chain must verify: %s", rep.Reason)
	require.Positive(t, rep.Events)

	// The audit row cannot outlive a rolled-back trade, because it is written
	// in the trade's transaction. A trade that fails leaves no event behind to
	// suggest it happened.
	before := f.countEvents(stream)
	err = testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.svc.Execute(ctx, tx, ExecuteRequest{
			MarketID: f.market.ID, AccountID: f.trader, Side: Buy,
			Amount: money.QuantityFromInt64(1_000_000), MinOutput: money.QuantityFromInt64(1),
			IdempotencyKey: "rollback-" + uuid.NewString(), EffectiveAt: f.clk.Now(),
		}); err != nil {
			return err
		}
		return errRollbackOnPurpose
	})
	require.ErrorIs(t, err, errRollbackOnPurpose)
	require.Equal(t, before, f.countEvents(stream),
		"an audit event that can commit without its trade is a record that can disagree with what happened")
}

var errRollbackOnPurpose = errTestRollback{}

type errTestRollback struct{}

func (errTestRollback) Error() string { return "rolled back on purpose" }

// TestIntegration_ANativeMarketIsRegisteredAsAnInstrument: predictions,
// strategy IR and every other part of the platform name tradable things by
// instrument id. Without this row a Domain A market could not be referred to
// at all.
func TestIntegration_ANativeMarketIsRegisteredAsAnInstrument(t *testing.T) {
	f := newFixture(t)
	ins := f.instrumentFor(t)
	require.Equal(t, "SPOT_PAIR", string(ins.Type))
	require.NotNil(t, ins.BaseAssetID)
	require.Equal(t, f.asset.AssetID, *ins.BaseAssetID)
	require.NotNil(t, ins.QuoteAssetID)
	require.Equal(t, f.creditAsset, *ins.QuoteAssetID)
	require.Equal(t, f.creditAsset, ins.SettlementAssetID)
	require.Equal(t, "native-market:"+f.market.ID.String(), ins.PolicyRef,
		"the instrument names the market it came from")
	require.Equal(t, "ACTIVE", string(ins.Status),
		"the fixture launches its market, and the registry follows it: the instrument "+
			"is created HALTED with the PENDING market and mirrored to ACTIVE when it opens")
}

// TestIntegration_APredictionOnANativeMarketResolvesFromNodalNativePrices is
// the Prediction half (PART XLIII), end to end and through the REAL ledger and
// resolver rather than a Domain A copy of them.
//
// An agent commits a prediction about a native market before the outcome is
// knowable, the market then moves, and the outcome is scored from the prices
// this venue published — the same PGPriceReader that scores an external
// instrument, with no Domain A special case.
func TestIntegration_APredictionOnANativeMarketResolvesFromNodalNativePrices(t *testing.T) {
	f := newFixture(t)
	ins := f.instrumentFor(t)
	sc := f.newStrategyScaffold(t)

	// Committed BEFORE anything moves. The horizon has not elapsed and the
	// outcome is unknowable, which is the only state in which a prediction is
	// worth recording.
	horizon := time.Hour
	led, err := prediction.NewLedger(f.clk)
	require.NoError(t, err)
	draft := prediction.Prediction{
		AgentID: sc.agentID, AgentVersion: 1, RunID: sc.runID, ActionName: "buy-the-dip",
		StrategyVersionID: sc.versionID, AccountID: f.trader.String(), Mode: prediction.ModeShadow,
		InstrumentID: ins.ID, Horizon: horizon,
		Direction: prediction.DirectionUp, ProbabilityDirection: mustProb(t, "0.7"),
		ExpectedReturnBPS: money.BPS(500), DownsideProbability: mustProb(t, "0.2"),
		MaxDownsideBPS: money.BPS(300), Confidence: mustProb(t, "0.6"),
		InformationSetHash: prediction.InformationSetHash([]prediction.InformationItem{
			{Dependency: "native_market_price", InvocationID: "inv-" + f.market.ID.String()},
		}),
		DecisionAvailableAt: f.clk.Now().Add(-time.Minute),
	}
	agentCtx := security.WithPrincipal(f.ctx, security.AgentPrincipal(sc.agentID, f.trader.String()))
	var committed prediction.Prediction
	require.NoError(t, testDB.InTx(agentCtx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
		func(ctx context.Context, tx pgx.Tx) error {
			p, err := led.Commit(ctx, tx, draft)
			committed = p
			return err
		}))

	// The market moves up inside the horizon. Buying is what moves a
	// constant-product price up, so this is a real move, not a written number.
	f.clk.Advance(10 * time.Minute)
	_, err = f.buy(f.trader, 5_000_000_000, money.Quantity{})
	require.NoError(t, err)

	// The horizon elapses. Nothing may be resolved before it does.
	resolver, err := prediction.NewResolver(f.clk, prediction.NewPriceReader(), time.Hour)
	require.NoError(t, err)
	_, err = resolver.Resolve(f.ctx, testDB, committed)
	require.Error(t, err, "a prediction cannot be scored while its window is still open")

	f.clk.Advance(horizon)
	outcome, err := resolver.Resolve(f.ctx, testDB, committed)
	require.NoError(t, err)
	require.Equal(t, prediction.DirectionUp, outcome.RealizedDirection,
		"the price rose, so the direction is UP and the prediction was right")
	require.Positive(t, int(outcome.RealizedReturnBPS))
	require.Equal(t, PriceSource, outcome.ValuationSource,
		"the outcome must name where its prices came from")
	require.NotEmpty(t, outcome.PriceRefStart)
	require.NotEmpty(t, outcome.PriceRefEnd)
	require.NotEqual(t, outcome.PriceRefStart, outcome.PriceRefEnd)

	// No lookahead (PART XLII). A trade AFTER the horizon ends moves the price
	// the other way; re-resolving must produce the same outcome, because the
	// resolver reads with a knowledge cut-off of the horizon end and cannot
	// see a price it had not received by then.
	sellAll, err := f.buy(f.trader, 1_000_000, money.Quantity{})
	require.NoError(t, err)
	f.clk.Advance(2 * time.Hour)
	_, err = f.sell(f.trader, sellAll.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err)

	// The control that makes the assertion below mean something: the market's
	// price really did change after the horizon. Without this, "the outcome is
	// unchanged" would also be true of a resolver that looked ahead at a price
	// that happened to be the same.
	all := f.pricesFor(t)
	require.Greater(t, len(all), 3)
	require.NotEqual(t, all[len(all)-1].mantissa, all[0].mantissa,
		"the post-horizon trades must have moved the price, or this proves nothing")

	again, err := resolver.Resolve(f.ctx, testDB, committed)
	require.NoError(t, err)
	require.Equal(t, outcome.RealizedDirection, again.RealizedDirection)
	require.Equal(t, outcome.RealizedReturnBPS, again.RealizedReturnBPS,
		"a price received after the horizon must not change what the window is judged on")
	require.Equal(t, outcome.PriceRefEnd, again.PriceRefEnd)
	require.NotEqual(t, all[len(all)-1].mantissa, again.PriceRefEnd,
		"the outcome must not be anchored to the newest price in the table")
}

// ------------------------------------------------------------- test helpers --

type publishedPrice struct {
	mantissa   string
	scale      int32
	source     string
	rawRef     string
	observedAt time.Time
	receivedAt time.Time
}

// pricesFor returns this market's published prices oldest first.
func (f *fixture) pricesFor(t *testing.T) []publishedPrice {
	t.Helper()
	rows, err := testDB.Query(f.ctx,
		`SELECT mantissa::text, scale, source, coalesce(raw_ref,''), observed_at, received_at
		   FROM asset_prices
		  WHERE asset_id = $1 AND quote_asset_id = $2
		  ORDER BY observed_at`,
		f.asset.AssetID, f.creditAsset)
	require.NoError(t, err)
	defer rows.Close()
	var out []publishedPrice
	for rows.Next() {
		var p publishedPrice
		require.NoError(t, rows.Scan(&p.mantissa, &p.scale, &p.source, &p.rawRef, &p.observedAt, &p.receivedAt))
		out = append(out, p)
	}
	require.NoError(t, rows.Err())
	return out
}

func (f *fixture) instrumentFor(t *testing.T) instruments.Instrument {
	t.Helper()
	var insID instruments.InstrumentID
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT id FROM instruments WHERE base_asset_id = $1 AND quote_asset_id = $2`,
		f.asset.AssetID, f.creditAsset).Scan(&insID))
	ins, err := instruments.NewRepository().Get(f.ctx, testDB, insID)
	require.NoError(t, err)
	return ins
}

func (f *fixture) countEvents(stream string) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM audit_events WHERE stream = $1`, stream).Scan(&n))
	return n
}

func mustProb(t *testing.T, s string) ir.Decimal {
	t.Helper()
	d, err := ir.ParseDecimalString(s)
	require.NoError(t, err)
	return d
}

// strategyScaffold is the agent, strategy and run a prediction hangs off.
// predictions carry foreign keys to all three: the ledger will not accept a
// prediction from an agent that does not exist, which is the point.
type strategyScaffold struct {
	strategyID string
	versionID  string
	agentID    string
	runID      string
}

func (f *fixture) newStrategyScaffold(t *testing.T) strategyScaffold {
	t.Helper()
	// v7, not uuid.NewString().
	//
	// These go into columns the application reads back through typed ids, and
	// `id.Parse` refuses anything that is not an RFC 9562 version 7 UUID. A v4
	// here produces a row this system can write and cannot read:
	//
	//   agent: scan agent: can't scan into dest[0] (col: id):
	//   id: scan: id: not an rfc 9562 version 7 uuid: version 4
	//
	// Nothing caught it because `inttest` gives every package its own database,
	// so this fixture's rows were only ever read by this package, which does not
	// scan them as agents. It surfaced the first time two packages shared one.
	sc := strategyScaffold{
		strategyID: id.New[id.Any]().String(), versionID: id.New[id.Any]().String(),
		agentID: id.New[id.Any]().String(), runID: id.New[id.Any]().String(),
	}
	var ownerUser string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT owner_user_id FROM accounts WHERE id = $1`, f.trader).Scan(&ownerUser))

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := testDB.Exec(f.ctx, sql, args...)
		require.NoError(t, err, sql)
	}
	hash32 := func(seed string) []byte {
		out := make([]byte, 32)
		copy(out, seed)
		return out
	}
	exec(`INSERT INTO strategies (id, owner_account_id, owner_user_id, name, source_kind, status,
	          created_by_actor_type, created_by_actor_id)
	      VALUES ($1,$2,$3,$4,'NATURAL_LANGUAGE','ACTIVE','USER',$5)`,
		sc.strategyID, f.trader, ownerUser, "domain-a-"+sc.strategyID[:8], ownerUser)
	exec(`INSERT INTO strategy_versions (id, strategy_id, version, schema_version, ir, ir_hash, effect_set,
	          status, source_kind, source_hash, compiler_version, risk_policy_version, risk_policy_hash,
	          model_budget, data_budget, envelope_requirements, human_readable, built_at,
	          accepted_by_user_id, accepted_at)
	      VALUES ($1,$2,1,1,'{}'::jsonb,$3,ARRAY['READ_MARKET_DATA','COMMIT_PREDICTION'],
	              'ACCEPTED','NATURAL_LANGUAGE',$4,'c/1','risk/v1',$5,
	              '{}'::jsonb,'{}'::jsonb,'{}'::jsonb,'rendered',now(),$6,now())`,
		sc.versionID, sc.strategyID, hash32("ir"), hash32("src"), hash32("risk"), ownerUser)
	exec(`INSERT INTO agents (id, account_id, strategy_id, strategy_version_id, name, stage, state, mode,
	          version, created_by_actor_type, created_by_actor_id)
	      VALUES ($1,$2,$3,$4,'domain a itest','SHADOW','SHADOW','SHADOW',1,'OPERATOR','op')`,
		sc.agentID, f.trader, sc.strategyID, sc.versionID)
	exec(`INSERT INTO agent_runs (id, agent_id, agent_version, strategy_version_id, account_id, mode,
	          trigger_name, trigger_kind, trigger_dedup_key, decision_time, status, correlation_id)
	      VALUES ($1,$2,1,$3,$4,'SHADOW','tick','ON_INTERVAL',$5,now(),'STARTED',$6)`,
		sc.runID, sc.agentID, sc.versionID, f.trader, hash32(sc.runID), "corr-"+sc.runID)
	return sc
}

// TestIntegration_HaltingAMarketHaltsItsInstrument closes a gap the readiness
// report named: the platform's instrument registry could say an asset was
// ACTIVE while its market was halted.
//
// Two sources for "what may be traded" eventually disagree, and the
// disagreement is discovered by something moving that should not have. This
// package already refuses that shape for capabilities; the registry gets the
// same treatment.
func TestIntegration_HaltingAMarketHaltsItsInstrument(t *testing.T) {
	f := newFixture(t)
	require.Equal(t, "ACTIVE", string(f.instrumentFor(t).Status),
		"the fixture launches its market, so the instrument follows it live")

	for _, step := range []struct {
		market nativeMarketStatus
		want   string
	}{
		{StatusCloseOnly, "CLOSE_ONLY"},
		{StatusHalted, "HALTED"},
		{StatusActive, "ACTIVE"},
		{StatusFrozen, "HALTED"}, // the registry has no word for "no exits either"
	} {
		require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := f.svc.SetStatus(ctx, tx, f.market.ID, step.market, "registry mirroring test")
				return err
			}))
		require.Equal(t, step.want, string(f.instrumentFor(t).Status),
			"market %s must leave the instrument %s", step.market, step.want)
	}
}

// nativeMarketStatus is a local alias so the table above reads as a list of
// market states rather than of strings.
type nativeMarketStatus = Status
