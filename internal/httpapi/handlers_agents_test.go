package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/agents"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// The agent surface gets its own harness rather than joining the shared one.
// The shared fixtures are a single struct every domain adds a port to, and five
// domains landing in parallel on one file is a merge conflict per domain. These
// doubles are local to this file and the server they build is the real one.

type fakeAgents struct {
	view    agents.View
	list    []agents.View
	err     error
	created agents.CreateRequest
	acted   agents.ActRequest
	paused  string
	filter  agents.ListFilter
}

func (f *fakeAgents) Create(_ context.Context, req agents.CreateRequest) (agents.View, error) {
	f.created = req
	return f.view, f.err
}

func (f *fakeAgents) Get(context.Context, agent.AgentID) (agents.View, error) {
	return f.view, f.err
}

func (f *fakeAgents) List(context.Context, string, bool, int) ([]agents.View, error) {
	return f.list, f.err
}

func (f *fakeAgents) Act(_ context.Context, req agents.ActRequest) (agents.View, error) {
	f.acted = req
	return f.view, f.err
}

func (f *fakeAgents) AdminList(_ context.Context, filter agents.ListFilter) ([]agents.View, error) {
	f.filter = filter
	return f.list, f.err
}

func (f *fakeAgents) AdminPause(_ context.Context, _ agent.AgentID, reason, _ string) (agents.View, error) {
	f.paused = reason
	return f.view, f.err
}

type fakeStrategies struct {
	strategy   agents.Strategy
	list       []agents.Strategy
	outcome    agents.CompileOutcome
	err        error
	configured bool
	requestID  string
}

func (f *fakeStrategies) Create(context.Context, agents.CreateStrategyRequest) (agents.Strategy, error) {
	return f.strategy, f.err
}

func (f *fakeStrategies) Get(context.Context, string) (agents.Strategy, error) {
	return f.strategy, f.err
}

func (f *fakeStrategies) List(context.Context, string, int) ([]agents.Strategy, error) {
	return f.list, f.err
}

func (f *fakeStrategies) Compile(_ context.Context, _, requestID, _ string) (agents.CompileOutcome, error) {
	f.requestID = requestID
	return f.outcome, f.err
}

func (f *fakeStrategies) CompilerConfigured() bool { return f.configured }

type agentHarness struct {
	*harness
	agentsPort *fakeAgents
	strategies *fakeStrategies
}

func newAgentHarness(t *testing.T) *agentHarness {
	t.Helper()
	ap := &fakeAgents{view: sampleAgentView(t)}
	sp := &fakeStrategies{strategy: sampleStrategy()}
	p := customerPrincipal()
	h := &harness{t: t, princip: &p}
	srv, err := New(Options{
		Env:           config.EnvTest,
		BuildVersion:  "test-build",
		ConfigHash:    "hash-1",
		PublicBaseURL: "https://app.test",
		CookieName:    "cp_session",
		SessionTTL:    time.Hour,
		Clock:         clock.NewFake(testNow),
		Authenticator: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if h.princip == nil {
					next.ServeHTTP(w, r)
					return
				}
				next.ServeHTTP(w, r.WithContext(security.WithPrincipal(r.Context(), *h.princip)))
			})
		},
		Ports: Ports{Agents: ap, Strategies: sp, Idempotency: newFakeIdempotency()},
	})
	require.NoError(t, err)
	h.server = srv
	return &agentHarness{harness: h, agentsPort: ap, strategies: sp}
}

func agentQty(t *testing.T, s string) money.Quantity {
	t.Helper()
	q, err := money.ParseQuantity(s)
	require.NoError(t, err)
	return q
}

func sampleAgentView(t *testing.T) agents.View {
	t.Helper()
	limits := agents.Limits{
		BudgetCredits:        agentQty(t, "1000000"),
		PerTradeCapCredits:   agentQty(t, "50000"),
		DailyLossStopCredits: agentQty(t, "100000"),
		MaxPositionShareBPS:  2000,
		AllowedAssets:        []assets.AssetID{testAssetID},
		Schedule:             agents.Schedule{Kind: agents.ScheduleInterval, IntervalMinutes: 60},
	}
	a := agent.Agent{
		ID: agent.NewAgentID(), AccountID: testAccountID.String(), StrategyID: testIntentID.String(),
		StrategyVersionID: testOrderID.String(), Name: "dip buyer",
		Stage: agent.StageDraft, State: agent.StateDraft, Version: 1,
		CreatedByActorType: "USER", CreatedByActorID: testUserID.String(),
		CreatedAt: testNow, UpdatedAt: testNow,
	}
	return agents.View{
		Agent: a,
		Grant: agents.Grant{
			ID: agents.NewGrantID(), AgentID: a.ID, AccountID: a.AccountID,
			StrategyVersionID: a.StrategyVersionID, Level: agentauthority.LevelRecommendation,
			Limits: limits, GrantedByUserID: testUserID.String(), GrantedAt: testNow,
		},
		Runs:              agents.RuntimeEvidence{},
		Runtime:           agents.Runtime(agents.Deployment{}, agents.RuntimeEvidence{}),
		BudgetUsedCredits: agentQty(t, "0"),
		BudgetUsedSource:  agents.BudgetSourceNoRuns,
		StrategyID:        a.StrategyID,
	}
}

func sampleStrategy() agents.Strategy {
	return agents.Strategy{
		ID: testIntentID.String(), AccountID: testAccountID.String(), OwnerUserID: testUserID.String(),
		Name: "dip buyer", Description: "buy the dip", SourceKind: "NATURAL_LANGUAGE",
		Status: "ACTIVE", CreatedAt: testNow, UpdatedAt: testNow,
	}
}

func validCreateAgentBody() map[string]any {
	return map[string]any{
		"account_id":          testAccountID.String(),
		"strategy_id":         testIntentID.String(),
		"strategy_version_id": testOrderID.String(),
		"name":                "dip buyer",
		"authority_level":     1,
		"limits": map[string]any{
			"budget_credits":          "1000000",
			"per_trade_cap_credits":   "50000",
			"daily_loss_stop_credits": "100000",
			"max_position_share_bps":  2000,
			"allowed_asset_ids":       []string{testAssetID.String()},
			"schedule":                map[string]any{"kind": "INTERVAL", "interval_minutes": 60},
		},
	}
}

// TestAgents_AuthorityAboveThreeIsRefusedAtTheAPI is §17's rule at the
// boundary: 4, 5 and 6 never reach a domain service, and the refusal names the
// capability rather than reading as a typo.
func TestAgents_AuthorityAboveThreeIsRefusedAtTheAPI(t *testing.T) {
	t.Parallel()
	for _, level := range []int{4, 5, 6} {
		h := newAgentHarness(t)
		body := validCreateAgentBody()
		body["authority_level"] = level
		res := h.do(http.MethodPost, "/v1/agents", body, "Idempotency-Key", "agent-key-000001")

		require.Equal(t, http.StatusUnprocessableEntity, res.Code, res.Body.String())
		p := res.problem()
		assert.Equal(t, errs.CodeCapabilityNotApproved, p.Code)
		assert.Contains(t, p.Detail, "disabled by policy")
		assert.Empty(t, h.agentsPort.created.AccountID, "the request must not reach the domain service")
	}
}

func TestAgents_AnUndeclaredAuthorityLevelIsAValidationFailure(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t)
	body := validCreateAgentBody()
	body["authority_level"] = 9
	res := h.do(http.MethodPost, "/v1/agents", body, "Idempotency-Key", "agent-key-000002")
	require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
}

func TestAgents_CreateCarriesTheLimitsThroughAsExactQuantities(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t)
	res := h.do(http.MethodPost, "/v1/agents", validCreateAgentBody(), "Idempotency-Key", "agent-key-000003")
	require.Equal(t, http.StatusCreated, res.Code, res.Body.String())

	got := h.agentsPort.created
	assert.Equal(t, testAccountID.String(), got.AccountID)
	assert.Equal(t, agentauthority.LevelRecommendation, got.Level)
	assert.Equal(t, "1000000", got.Limits.BudgetCredits.String())
	assert.Equal(t, "50000", got.Limits.PerTradeCapCredits.String())
	assert.Equal(t, money.BPS(2000), got.Limits.MaxPositionShareBPS)
	assert.Equal(t, agents.ScheduleInterval, got.Limits.Schedule.Kind)
	assert.Equal(t, 60, got.Limits.Schedule.IntervalMinutes)

	var out api.Agent
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, "1000000", out.Limits.BudgetCredits, "money is a string on the wire, never a number")
	assert.Equal(t, "0", out.Budget.UsedCredits)
	assert.Equal(t, api.AgentBudgetSource(agents.BudgetSourceNoRuns), out.Budget.Source)
}

func TestAgents_AMoneyFieldThatIsNotExactIsRefused(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t)
	body := validCreateAgentBody()
	limits, _ := body["limits"].(map[string]any)
	limits["budget_credits"] = "1000.5"
	res := h.do(http.MethodPost, "/v1/agents", body, "Idempotency-Key", "agent-key-000004")
	require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
}

// TestAgents_TheReadModelSaysNothingIsRunning is the honesty requirement: an
// agent's status and its runtime are two different fields with two different
// sources, and on this tier the runtime says NOT_DEPLOYED with a reason.
func TestAgents_TheReadModelSaysNothingIsRunning(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t)
	view := sampleAgentView(t)
	view.Agent.Stage = agent.StageBacktestEligible
	view.Agent.State = agent.StateBacktestEligible
	view.Agent.Mode = agent.ModePaper
	h.agentsPort.view = view

	res := h.do(http.MethodGet, "/v1/agents/"+view.Agent.ID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	var out api.Agent
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, api.AgentStatus("ENABLED"), out.Status)
	assert.Equal(t, api.AgentRuntimeEvaluator("NOT_DEPLOYED"), out.Runtime.Evaluator)
	assert.Equal(t, api.AgentRuntimeExecutor("NOT_DEPLOYED"), out.Runtime.Executor)
	assert.Nil(t, out.Runtime.LastHeartbeat)
	assert.NotEmpty(t, out.Runtime.Detail)
	assert.Contains(t, out.Runtime.Detail, "not being evaluated",
		"a user reading ENABLED must be told nothing is evaluating it")
}

func TestAgents_ProductStatusNamesWhatAPersonAsked(t *testing.T) {
	t.Parallel()
	assert.Equal(t, api.AgentStatus("STOPPED"), productStatus(agent.StateDraft))
	assert.Equal(t, api.AgentStatus("STOPPED"), productStatus(agent.StateValidated))
	assert.Equal(t, api.AgentStatus("ENABLED"), productStatus(agent.StateBacktestEligible))
	assert.Equal(t, api.AgentStatus("PAUSED"), productStatus(agent.StatePaused))
	assert.Equal(t, api.AgentStatus("DISABLED"), productStatus(agent.StateRevoked))
	assert.Equal(t, api.AgentStatus("DISABLED"), productStatus(agent.StateSuperseded))
	assert.Equal(t, api.AgentStatus("FAILED"), productStatus(agent.StateFailed))
}

func TestAgents_EveryActionReachesTheServiceByName(t *testing.T) {
	t.Parallel()
	view := sampleAgentView(t)
	for i, action := range []string{"enable", "pause", "resume", "disable", "archive"} {
		h := newAgentHarness(t)
		h.agentsPort.view = view
		res := h.do(http.MethodPost, "/v1/agents/"+view.Agent.ID.String()+"/"+action,
			map[string]any{"reason": "because I said so"},
			"Idempotency-Key", "agent-act-00000"+string(rune('1'+i)))
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		assert.Equal(t, agents.Action(action), h.agentsPort.acted.Action)
		assert.Equal(t, "because I said so", h.agentsPort.acted.Reason)
	}
}

func TestAgents_AnUndeclaredActionIsRefused(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t)
	view := sampleAgentView(t)
	res := h.do(http.MethodPost, "/v1/agents/"+view.Agent.ID.String()+"/promote", nil,
		"Idempotency-Key", "agent-act-000099")
	require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
	assert.Empty(t, h.agentsPort.acted.Action, "an action the product does not have never reaches the service")
}

// TestAgents_ThePageCarriesTheWholeAuthorityLadder: a client must be able to
// show that levels 4-6 exist and are switched off, rather than discovering the
// day one is approved that it never knew about them.
func TestAgents_ThePageCarriesTheWholeAuthorityLadder(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t)
	h.agentsPort.list = []agents.View{sampleAgentView(t)}
	res := h.do(http.MethodGet, "/v1/agents?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	var page api.AgentPage
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	require.Len(t, page.AuthorityLevels, len(agentauthority.AllLevels()))
	for _, l := range page.AuthorityLevels {
		assert.NotEmpty(t, l.Summary)
		if l.Level > int(agentauthority.MaxSupportedLevel) {
			assert.False(t, l.Enabled)
			require.NotNil(t, l.RequiredCapability)
			assert.NotEmpty(t, *l.RequiredCapability)
		}
	}
}

func TestAgents_AnUnwiredPortAnswersUnsupportedRatherThanAnEmptyList(t *testing.T) {
	t.Parallel()
	h := newHarness(t) // the shared harness wires no agent ports
	res := h.do(http.MethodGet, "/v1/agents?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, res.Body.String())
	assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
}

func TestAgents_OperatorPauseNeedsAReasonAndReachesTheService(t *testing.T) {
	t.Parallel()
	view := sampleAgentView(t)

	h := newAgentHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	res := h.do(http.MethodPost, "/v1/admin/agents/"+view.Agent.ID.String()+"/pause",
		map[string]any{"reason": "short"}, "Idempotency-Key", "admin-pause-00001")
	require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
	assert.Empty(t, h.agentsPort.paused)

	h2 := newAgentHarness(t)
	op2 := operatorPrincipal()
	h2.as(&op2)
	res = h2.do(http.MethodPost, "/v1/admin/agents/"+view.Agent.ID.String()+"/pause",
		map[string]any{"reason": "INC-9: stopping this agent"}, "Idempotency-Key", "admin-pause-00002")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.Equal(t, "INC-9: stopping this agent", h2.agentsPort.paused)
}

// --------------------------------------------------------------- strategy --

func TestStrategies_CompileSaysCompilerUnavailableWithoutInventingAnything(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t)
	h.strategies.configured = false
	h.strategies.outcome = agents.CompileOutcome{
		AttemptID: testSessionID, AttemptNo: 1,
		Outcome:      "MODEL_UNAVAILABLE",
		FailureCodes: []string{agents.CompilerUnavailable},
		Detail:       "This deployment has no strategy compiler backend configured",
	}
	res := h.do(http.MethodPost, "/v1/strategies/"+testIntentID.String()+"/compile", nil,
		"Idempotency-Key", "compile-key-00001")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	var out api.CompileResult
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, api.CompileResultOutcome("MODEL_UNAVAILABLE"), out.Outcome)
	require.NotNil(t, out.FailureCodes)
	assert.Equal(t, []string{"COMPILER_UNAVAILABLE"}, *out.FailureCodes)
	assert.Nil(t, out.Version, "no IR is fabricated when there is no compiler")
	assert.Equal(t, "compile-key-00001", h.strategies.requestID,
		"the idempotency key is the compile request id, which is what bounds its attempts")
}

func TestStrategies_ASuccessfulCompileReturnsSomethingAPersonCanRead(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t)
	h.strategies.configured = true
	h.strategies.outcome = agents.CompileOutcome{
		AttemptID: testSessionID, AttemptNo: 1, Outcome: "SUCCESS",
		Detail: "Compiled.",
		Version: &agents.StrategyVersion{
			ID: testOrderID.String(), Version: 1, Status: "COMPILED", IRHashHex: "abcd",
			EffectSet: []string{"READ_MARKET_DATA"}, HumanReadable: "buy the dip, in words",
			IR: json.RawMessage(`{"schema_version":1}`), BuiltAt: testNow,
		},
	}
	res := h.do(http.MethodPost, "/v1/strategies/"+testIntentID.String()+"/compile", nil,
		"Idempotency-Key", "compile-key-00002")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	var out api.CompileResult
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	require.NotNil(t, out.Version)
	assert.Equal(t, "buy the dip, in words", out.Version.HumanReadable,
		"goal §18: a human-understandable compiled strategy before activation")
	require.NotNil(t, out.Version.Ir)
	assert.Equal(t, float64(1), (*out.Version.Ir)["schema_version"])
}

func TestStrategies_ThePageSaysWhetherThisDeploymentCanCompileAtAll(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t)
	h.strategies.configured = false
	h.strategies.list = []agents.Strategy{sampleStrategy()}
	res := h.do(http.MethodGet, "/v1/strategies?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	var page api.StrategyPage
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &page))
	assert.False(t, page.CompilerConfigured,
		"the create flow must be able to say so before a user writes a description")
	require.Len(t, page.Items, 1)
	assert.False(t, page.Items[0].CompilerConfigured)
}

func TestStrategies_AnUnwiredPortAnswersUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res := h.do(http.MethodPost, "/v1/strategies", map[string]any{
		"account_id": testAccountID.String(), "name": "x", "description": "y",
	}, "Idempotency-Key", "strategy-key-0001")
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, res.Body.String())
	assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
}
