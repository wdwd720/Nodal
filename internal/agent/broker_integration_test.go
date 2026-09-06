//go:build integration

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// countingAdapter counts every dial. A refusal that still dialed would defeat
// the whole point of enforcing before the call.
type countingAdapter struct {
	code    string
	version int
	dials   atomic.Int64
	payload json.RawMessage
	untrust []Untrusted
	err     error
	// hostToDial, when set, is fetched through the egress-guarded client so a
	// test can prove the allowlist is what actually stops a connection.
	hostToDial string
}

func (a *countingAdapter) Code() string { return a.code }
func (a *countingAdapter) Version() int { return a.version }

func (a *countingAdapter) Fetch(ctx context.Context, req AdapterRequest) (AdapterResponse, error) {
	a.dials.Add(1)
	if a.hostToDial != "" {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, a.hostToDial, nil)
		if err != nil {
			return AdapterResponse{}, err
		}
		resp, err := req.HTTPClient.Do(httpReq)
		if err != nil {
			return AdapterResponse{}, err
		}
		_ = resp.Body.Close()
	}
	if a.err != nil {
		return AdapterResponse{}, a.err
	}
	payload := a.payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{"mid":"142.35"}`)
	}
	return AdapterResponse{
		Payload: payload, Untrusted: a.untrust, Source: "test-oracle",
		SourceEventAt: time.Now().UTC().Add(-time.Second),
	}, nil
}

// brokerFixture wires a broker over the integration fixture.
type brokerFixture struct {
	*fixture
	agent   Agent
	auth    Authority
	run     Run
	adapter *countingAdapter
	broker  *Broker
}

func newBrokerFixture(t *testing.T, stage Stage) *brokerFixture {
	t.Helper()
	f := newFixture(t)
	a := f.walkTo(t, stage)
	doc := f.authorityIR()
	env := EnvelopeSnapshot{}
	if a.EnvelopeID != "" {
		var err error
		env, err = NewEnvelopeReader().Envelope(context.Background(), testDB, a.EnvelopeID)
		require.NoError(t, err)
	}
	auth, err := NewAuthority(AuthorityInput{
		AgentID: a.ID, AgentVersion: a.Version, AccountID: a.AccountID,
		StrategyVersionID: a.StrategyVersionID, Stage: a.Stage, Mode: a.Mode,
		EnvelopeID: a.EnvelopeID, RiskPolicyVersion: a.RiskPolicyVersion,
		IR: doc, Envelope: env,
	})
	require.NoError(t, err)

	run := Run{
		ID: NewRunID(), AgentID: a.ID, AgentVersion: a.Version,
		StrategyVersionID: a.StrategyVersionID, AccountID: a.AccountID, EnvelopeID: a.EnvelopeID,
		Mode: a.Mode, TriggerName: "tick", TriggerKind: TriggerOnInterval,
		TriggerDedupKey: bytes32(newUUID()), DecisionTime: f.clk.Now(),
		Status: RunStarted, StartedAt: f.clk.Now(), CorrelationID: "corr-" + newUUID(),
	}
	require.NoError(t, inTx(context.Background(), t, func(ctx context.Context, tx pgx.Tx) error {
		created, _, err := f.store.CreateRun(ctx, tx, run)
		run = created
		return err
	}))

	adapter := &countingAdapter{code: f.toolCode, version: 1}
	broker, err := NewBroker(BrokerDeps{
		DB: testDB, Clock: f.clk, Registry: NewToolRegistry(), Budgets: NewBudgetReader(),
		Pauses: NewPauseChecker(), Recorder: NewInvocationRecorder(), ModelCalls: NewModelCallRecorder(),
		Adapters: map[string]ToolAdapter{ToolKey(f.toolCode, 1): adapter},
	}, auth, run.ID)
	require.NoError(t, err)

	return &brokerFixture{fixture: f, agent: a, auth: auth, run: run, adapter: adapter, broker: broker}
}

// newRun opens another run of the same agent, for counters that are per agent
// rather than per run.
func (b *brokerFixture) newRun(t *testing.T) Run {
	t.Helper()
	run := Run{
		ID: NewRunID(), AgentID: b.agent.ID, AgentVersion: b.agent.Version,
		StrategyVersionID: b.agent.StrategyVersionID, AccountID: b.agent.AccountID,
		EnvelopeID: b.agent.EnvelopeID, Mode: b.agent.Mode, TriggerName: "tick",
		TriggerKind: TriggerOnInterval, TriggerDedupKey: bytes32(newUUID()),
		DecisionTime: b.clk.Now(), Status: RunStarted, StartedAt: b.clk.Now(),
		CorrelationID: "corr-" + newUUID(),
	}
	require.NoError(t, inTx(context.Background(), t, func(ctx context.Context, tx pgx.Tx) error {
		created, _, err := b.store.CreateRun(ctx, tx, run)
		run = created
		return err
	}))
	return run
}

func (b *brokerFixture) priceCall() ToolCall {
	return ToolCall{
		RunID: b.run.ID,
		Dependency: ir.Dependency{
			Name: "price", Kind: ir.DepPrice, ToolCode: b.toolCode, ToolVersion: 1,
			DependencyVersion: 1, Params: map[string]string{"instrument": b.instrumentID},
			MaxAgeMS: 60_000, Required: true,
		},
		Effect: ir.EffectReadMarketData,
	}
}

// invocations returns the tool_invocations rows recorded for the run.
func (b *brokerFixture) invocations(t *testing.T) []Invocation {
	t.Helper()
	rows, err := testDB.Query(context.Background(),
		`SELECT id, tool_code, tool_version, effect, mode, success, coalesce(error_code, ''),
		        rate_limited, budget_refused, cost_usd_minor, latency_ms, source
		   FROM tool_invocations WHERE agent_run_id = $1 ORDER BY created_at, id`, b.run.ID)
	require.NoError(t, err)
	defer rows.Close()
	var out []Invocation
	for rows.Next() {
		var (
			inv       Invocation
			effect    string
			mode      string
			costMinor int64
			latencyMS int
		)
		require.NoError(t, rows.Scan(&inv.ID, &inv.ToolCode, &inv.ToolVersion, &effect, &mode,
			&inv.Success, &inv.ErrorCode, &inv.RateLimited, &inv.BudgetRefused, &costMinor, &latencyMS, &inv.Source))
		inv.Effect = ir.Effect(effect)
		inv.Mode = Mode(mode)
		inv.CostUSD = money.USDFromMinor(costMinor)
		inv.Latency = time.Duration(latencyMS) * time.Millisecond
		out = append(out, inv)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestBrokerInvokesADeclaredActiveTool(t *testing.T) {
	b := newBrokerFixture(t, StageShadow)
	obs, err := b.broker.Invoke(context.Background(), b.priceCall())
	require.NoError(t, err)

	assert.Equal(t, "price", obs.Dependency)
	assert.Equal(t, b.toolCode, obs.ToolCode)
	assert.Equal(t, ir.EffectReadMarketData, obs.Effect)
	assert.Equal(t, ModeShadow, obs.Mode, "the run's mode is copied onto the observation")
	assert.NotEmpty(t, obs.OutputHash)
	assert.Equal(t, int64(5), obs.CostUSD.Minor(), "the registered per-call cost is recorded")
	assert.False(t, obs.DecisionAvailableAt.IsZero())
	assert.Equal(t, obs.NormalizedAt, obs.DecisionAvailableAt,
		"a synchronous tool with no declared pipeline latency is usable as soon as it is normalized")
	assert.EqualValues(t, 1, b.adapter.dials.Load())

	invs := b.invocations(t)
	require.Len(t, invs, 1)
	assert.True(t, invs[0].Success)
	assert.Equal(t, b.toolCode, invs[0].ToolCode)
	assert.Equal(t, int64(5), invs[0].CostUSD.Minor())
	assert.False(t, invs[0].BudgetRefused)
	assert.False(t, invs[0].RateLimited)
}

// TestBrokerAddsThePipelineLatencyToDecisionAvailability: a tool whose data
// reaches the decision path through a pipeline is not usable the instant it is
// received, and decision_available_at says so.
func TestBrokerAddsThePipelineLatencyToDecisionAvailability(t *testing.T) {
	b := newBrokerFixture(t, StageShadow)
	_, err := testDB.Exec(context.Background(),
		`UPDATE tools SET pipeline_latency_ms = 250 WHERE id = $1`, b.toolID)
	require.NoError(t, err)

	obs, err := b.broker.Invoke(context.Background(), b.priceCall())
	require.NoError(t, err)
	assert.Equal(t, obs.NormalizedAt.Add(250*time.Millisecond), obs.DecisionAvailableAt)
	assert.False(t, obs.UsableAt(obs.NormalizedAt),
		"a decision at receipt time could not have used it yet")
	assert.True(t, obs.UsableAt(obs.DecisionAvailableAt))
}

func TestBrokerRefusesAnUndeclaredTool(t *testing.T) {
	b := newBrokerFixture(t, StageShadow)
	call := b.priceCall()
	call.Dependency.ToolCode = "wallet-signer"
	call.Dependency.Name = "signer"

	_, err := b.broker.Invoke(context.Background(), call)
	require.Error(t, err)
	assert.Equal(t, errs.CodeEffectForbidden, errs.CodeOf(err))
	assert.EqualValues(t, 0, b.adapter.dials.Load(), "an undeclared tool must never be dialed")
	assert.Empty(t, b.invocations(t), "there is no tools row to attribute the refusal to")
}

func TestBrokerRefusesAToolVersionThatWasNotDeclared(t *testing.T) {
	b := newBrokerFixture(t, StageShadow)
	call := b.priceCall()
	call.Dependency.ToolVersion = 2

	_, err := b.broker.Invoke(context.Background(), call)
	require.Error(t, err)
	assert.Equal(t, errs.CodeEffectForbidden, errs.CodeOf(err),
		"a different version is a different tool")
	assert.EqualValues(t, 0, b.adapter.dials.Load())
}

func TestBrokerRefusesAnEffectOutsideTheDeclaredSet(t *testing.T) {
	b := newBrokerFixture(t, StageShadow)
	call := b.priceCall()
	call.Effect = ir.EffectReadWalletIntelligence

	_, err := b.broker.Invoke(context.Background(), call)
	require.Error(t, err)
	assert.Equal(t, errs.CodeEffectForbidden, errs.CodeOf(err))
	assert.EqualValues(t, 0, b.adapter.dials.Load())
}

func TestBrokerRefusesADegradedOrDisabledTool(t *testing.T) {
	for _, status := range []ToolStatus{ToolDegraded, ToolDisabled} {
		t.Run(status.String(), func(t *testing.T) {
			b := newBrokerFixture(t, StageShadow)
			_, err := testDB.Exec(context.Background(),
				`UPDATE tools SET status = $2 WHERE id = $1`, b.toolID, string(status))
			require.NoError(t, err)

			_, err = b.broker.Invoke(context.Background(), b.priceCall())
			require.Error(t, err)
			assert.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
			assert.Contains(t, err.Error(), string(status))
			assert.EqualValues(t, 0, b.adapter.dials.Load(), "a %s tool must never be dialed", status)

			invs := b.invocations(t)
			require.Len(t, invs, 1, "a refusal is recorded, never silently skipped")
			assert.False(t, invs[0].Success)
			assert.Equal(t, string(errs.CodeProviderUnavailable), invs[0].ErrorCode)
			assert.Zero(t, invs[0].CostUSD.Minor(), "a call that was never made costs nothing")
		})
	}
}

func TestBrokerRefusesAToolWhoseEffectChangedSinceCompile(t *testing.T) {
	b := newBrokerFixture(t, StageShadow)
	_, err := testDB.Exec(context.Background(),
		`UPDATE tools SET effect = 'READ_WALLET_INTELLIGENCE' WHERE id = $1`, b.toolID)
	require.NoError(t, err)

	_, err = b.broker.Invoke(context.Background(), b.priceCall())
	require.Error(t, err)
	assert.Equal(t, errs.CodeEffectForbidden, errs.CodeOf(err))
	assert.EqualValues(t, 0, b.adapter.dials.Load(),
		"a tool that quietly changed its effect must not be reachable under the old declaration")
}

func TestBrokerRefusesWhenTheDataSpendBudgetIsExhausted(t *testing.T) {
	b := newBrokerFixture(t, StageShadow)
	// The fixture IR allows 100 minor units of data spend per UTC day at 5 per
	// call, so 20 calls exhaust it. They are attributed to an earlier run of
	// the same agent: the daily spend counter is per agent, so this exercises
	// the spend cap without touching any per-run cap.
	earlier := b.newRun(t)
	require.NoError(t, inTx(context.Background(), t, func(ctx context.Context, tx pgx.Tx) error {
		for i := 0; i < 20; i++ {
			if err := NewInvocationRecorder().Record(ctx, tx, Invocation{
				ID: NewInvocationID(), ToolID: b.toolID, ToolCode: b.toolCode, ToolVersion: 1,
				Effect: ir.EffectReadMarketData, AgentID: b.agent.ID, RunID: earlier.ID,
				Mode: ModeShadow, RequestHash: bytes32("r"), OutputHash: bytes32("o"),
				Source: "test-oracle", ReceivedAt: b.clk.Now(), CostUSD: money.USDFromMinor(5),
				Success: true,
			}); err != nil {
				return err
			}
		}
		return nil
	}))

	_, err := b.broker.Invoke(context.Background(), b.priceCall())
	require.Error(t, err)
	assert.Equal(t, errs.CodeBudgetExhausted, errs.CodeOf(err))
	assert.EqualValues(t, 0, b.adapter.dials.Load(),
		"the budget is enforced before the call, so a provider is never dialed after exhaustion")

	invs := b.invocations(t)
	require.Len(t, invs, 1, "the refusal is recorded against the run that was refused")
	assert.False(t, invs[0].Success)
	assert.True(t, invs[0].BudgetRefused, "the refusal is recorded as a budget refusal")
	assert.Zero(t, invs[0].CostUSD.Minor(), "a call that was never dialed costs nothing")
}

func TestBrokerRefusesWhenThePerRunCallCapIsHit(t *testing.T) {
	b := newBrokerFixture(t, StageShadow)
	// tools.max_calls_per_run is 3 in the fixture.
	for i := 0; i < 3; i++ {
		_, err := b.broker.Invoke(context.Background(), b.priceCall())
		require.NoErrorf(t, err, "call %d", i+1)
	}
	assert.EqualValues(t, 3, b.adapter.dials.Load())

	_, err := b.broker.Invoke(context.Background(), b.priceCall())
	require.Error(t, err)
	assert.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	assert.EqualValues(t, 3, b.adapter.dials.Load(), "no dial after the cap is reached")

	invs := b.invocations(t)
	last := invs[len(invs)-1]
	assert.True(t, last.RateLimited)
	assert.False(t, last.Success)
}

// TestPauseIsEffectiveImmediatelyForARunInFlight is PART 71's hardest claim:
// a pause committed while a run is between two tool calls stops the next one.
func TestPauseIsEffectiveImmediatelyForARunInFlight(t *testing.T) {
	b := newBrokerFixture(t, StageShadow)

	// The run is in flight and the first call succeeds.
	_, err := b.broker.Invoke(context.Background(), b.priceCall())
	require.NoError(t, err)
	assert.EqualValues(t, 1, b.adapter.dials.Load())

	// An operator pauses the agent, in a separate transaction, mid-run.
	require.NoError(t, inTx(ctxAs(b.operator()), t, func(ctx context.Context, tx pgx.Tx) error {
		_, perr := b.lifecycle.Pause(ctx, tx, b.agent.ID, PauseRequest{
			ReasonCode: PauseOperator, Reason: "stop this agent now",
			OpenOrdersPolicy: LeaveOpenOrders,
		})
		return perr
	}))

	// The very next call of the same, already-open run is refused.
	_, err = b.broker.Invoke(context.Background(), b.priceCall())
	require.Error(t, err, "a pause must reach a run that is already in flight")
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err))
	assert.EqualValues(t, 1, b.adapter.dials.Load(), "no further provider dial after the pause")
}

// TestPausedAgentProducesNoRuns is the dispatcher half of PART 71.
func TestPausedAgentProducesNoRuns(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageShadow)
	doc := f.authorityIR()
	every := int64(60_000)
	doc.Triggers = []ir.Trigger{{Name: "tick", Kind: ir.TriggerOnInterval, EveryMS: &every, DedupWindowMS: 60_000}}

	dispatcher, err := NewDispatcher(DispatcherDeps{
		DB: testDB, Clock: f.clk, Store: f.store, Pauses: NewPauseChecker(),
		IRFor:        func(context.Context, db.Querier, string) (*ir.IR, error) { return doc, nil },
		BuildVersion: "itest",
	})
	require.NoError(t, err)

	opened, err := dispatcher.OnTick(context.Background(), f.clk.Now())
	require.NoError(t, err)
	mine := runsFor(opened, a.ID)
	require.Len(t, mine, 1, "a runnable agent opens exactly one run per interval bucket")

	// The same tick again is deduplicated by (agent_id, trigger_dedup_key).
	opened, err = dispatcher.OnTick(context.Background(), f.clk.Now())
	require.NoError(t, err)
	assert.Empty(t, runsFor(opened, a.ID), "a repeated tick in the same bucket opens no second run")

	require.NoError(t, inTx(ctxAs(f.operator()), t, func(ctx context.Context, tx pgx.Tx) error {
		_, perr := f.lifecycle.Pause(ctx, tx, a.ID, PauseRequest{
			ReasonCode: PauseOperator, Reason: "paused for the dispatcher test",
			OpenOrdersPolicy: LeaveOpenOrders,
		})
		return perr
	}))

	f.clk.Advance(2 * time.Minute)
	opened, err = dispatcher.OnTick(context.Background(), f.clk.Now())
	require.NoError(t, err)
	assert.Empty(t, runsFor(opened, a.ID), "a paused agent opens no runs")

	// The suppression itself is visible exactly once per interval.
	n, err := f.store.CountSkipsSince(context.Background(), testDB, a.ID, SkipAgentPaused, f.clk.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the first suppressed trigger is recorded for observability")

	f.clk.Advance(time.Minute)
	_, err = dispatcher.OnTick(context.Background(), f.clk.Now())
	require.NoError(t, err)
	n, err = f.store.CountSkipsSince(context.Background(), testDB, a.ID, SkipAgentPaused, f.clk.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n, "further suppressed triggers in the same hour are not re-recorded")
}

func runsFor(runs []Run, agentID AgentID) []Run {
	var out []Run
	for _, r := range runs {
		if r.AgentID == agentID {
			out = append(out, r)
		}
	}
	return out
}

// TestRunIdentityModeAndLinkageAreImmutable proves the AG004 guard.
func TestRunIdentityModeAndLinkageAreImmutable(t *testing.T) {
	b := newBrokerFixture(t, StageShadow)
	for name, sql := range map[string]string{
		"mode":              `UPDATE agent_runs SET mode = 'LIVE' WHERE id = $1`,
		"agent":             `UPDATE agent_runs SET agent_id = gen_random_uuid() WHERE id = $1`,
		"strategy version":  `UPDATE agent_runs SET strategy_version_id = gen_random_uuid() WHERE id = $1`,
		"decision time":     `UPDATE agent_runs SET decision_time = now() + interval '1 day' WHERE id = $1`,
		"trigger dedup key": `UPDATE agent_runs SET trigger_dedup_key = '\x00'::bytea WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			err := inTx(context.Background(), t, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, sql, b.run.ID)
				return err
			})
			require.Errorf(t, err, "changing the %s of a run must be refused", name)
			assert.True(t, IsAgentRunImmutable(err), "expected AG004, got %v", err)
		})
	}
	t.Run("delete", func(t *testing.T) {
		err := inTx(context.Background(), t, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM agent_runs WHERE id = $1`, b.run.ID)
			return err
		})
		require.Error(t, err, "a run is never deleted")
	})
}

func TestToolInvocationsAreImmutable(t *testing.T) {
	b := newBrokerFixture(t, StageShadow)
	_, err := b.broker.Invoke(context.Background(), b.priceCall())
	require.NoError(t, err)
	invs := b.invocations(t)
	require.Len(t, invs, 1)

	err = inTx(context.Background(), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tool_invocations SET success = false WHERE id = $1`, invs[0].ID)
		return err
	})
	require.Error(t, err, "provenance is never rewritten")

	err = inTx(context.Background(), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM tool_invocations WHERE id = $1`, invs[0].ID)
		return err
	})
	require.Error(t, err, "provenance is never deleted")
}

// TestUntrustedContentFromAToolChangesNothing runs the adversarial corpus
// through the real broker: the content is recorded, the authority is
// unchanged, and no additional tool becomes reachable.
func TestUntrustedContentFromAToolChangesNothing(t *testing.T) {
	b := newBrokerFixture(t, StageShadow)
	before := b.auth.Fingerprint()
	beforeTools := b.auth.Tools()

	for _, a := range attacks {
		b.adapter.untrust = []Untrusted{Quarantine(UntrustedSocial, "post", "", a.text)}
		obs, err := b.broker.Invoke(context.Background(), b.priceCall())
		if err != nil {
			// A call cap or a spend cap may be reached partway through; either
			// is a refusal, not a success, and the authority still cannot change.
			assert.Contains(t, []errs.Code{errs.CodeRateLimited, errs.CodeBudgetExhausted}, errs.CodeOf(err))
		} else {
			require.Len(t, obs.Untrusted, 1)
			assert.NotEmpty(t, obs.Untrusted[0].ProvenanceRef,
				"quarantined content is attributed to the invocation that produced it")
			if len(a.expect) > 0 {
				assert.NotEmptyf(t, obs.Signals(), "%s must be recorded as an attempt", a.name)
			}
		}
		assert.Equalf(t, before, b.auth.Fingerprint(), "%s changed the authority", a.name)
		assert.Equalf(t, beforeTools, b.auth.Tools(), "%s changed the tool set", a.name)
	}

	// And the agent still cannot reach anything it did not declare.
	call := b.priceCall()
	call.Dependency.ToolCode = "wallet-signer"
	_, err := b.broker.Invoke(context.Background(), call)
	require.Error(t, err)
	assert.Equal(t, errs.CodeEffectForbidden, errs.CodeOf(err))
}

// TestAgentPrincipalHoldsOnlyItsClosedPermissionSet.
func TestAgentPrincipalHoldsOnlyItsClosedPermissionSet(t *testing.T) {
	f := newFixture(t)
	a := f.walkTo(t, StageShadow)
	p := f.agentPrincipal(a.ID)
	require.NoError(t, p.Validate())
	now := f.clk.Now()

	for _, allowed := range []security.Permission{
		security.PermAgentRun, security.PermPredictionCommit, security.PermIntentCreateAgent,
		security.PermAccountRead, security.PermTradeRead, security.PermStrategyRead,
	} {
		assert.Truef(t, p.Has(allowed, now), "an agent holds %s", allowed)
	}
	for _, denied := range []security.Permission{
		security.PermTradeCreate, security.PermAgentPause, security.PermAgentPromote,
		security.PermAgentPromoteApprove, security.PermWithdrawalCreate, security.PermKillActivate,
		security.PermGateApprove, security.PermRiskPolicyWrite, security.PermEnvelopeAuthorityWrite,
		security.PermLedgerPostCorrection, security.PermReconciliationResolve, security.PermStrategyWrite,
	} {
		assert.Falsef(t, p.Has(denied, now), "an agent must never hold %s", denied)
	}
	// It is bound to exactly one account and cannot reach another.
	assert.NoError(t, security.RequireAccount(ctxAs(p), f.accountID))
	assert.Error(t, security.RequireAccount(ctxAs(p), newUUID()))
}
