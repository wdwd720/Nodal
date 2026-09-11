package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/agents"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// AgentsPort is the agent management surface (internal/agents).
//
// What is NOT on this interface is the point of it: nothing runs an agent,
// nothing evaluates a strategy, and nothing emits an intent. This is the
// surface a PERSON reaches — create, read, enable, pause, resume, disable,
// archive — and the runtime it manages has no caller in this build (F-65).
//
// A nil port answers UNSUPPORTED. A deployment that has not provisioned agent
// management says so rather than returning an empty list, which a client would
// read as "you have no agents".
type AgentsPort interface {
	Create(ctx context.Context, req agents.CreateRequest) (agents.View, error)
	Get(ctx context.Context, agentID agent.AgentID) (agents.View, error)
	List(ctx context.Context, accountID string, includeArchived bool, limit int) ([]agents.View, error)
	Act(ctx context.Context, req agents.ActRequest) (agents.View, error)
	AdminList(ctx context.Context, f agents.ListFilter) ([]agents.View, error)
	AdminPause(ctx context.Context, agentID agent.AgentID, reason, correlationID string) (agents.View, error)
}

// PostAgents creates an agent from a compiled strategy version, with the
// authority and the limits its owner granted it.
func (s *Server) PostAgents(ctx context.Context, request api.PostAgentsRequestObject) (api.PostAgentsResponseObject, error) {
	if s.opts.Ports.Agents == nil {
		return nil, errNotWired("agents")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	level, err := agents.ParseAuthorityLevel(request.Body.AuthorityLevel)
	if err != nil {
		return nil, err
	}
	limits, err := toDomainLimits(request.Body.Limits)
	if err != nil {
		return nil, err
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.Agent, commandMeta, error) {
			v, cerr := s.opts.Ports.Agents.Create(ctx, agents.CreateRequest{
				AccountID:         accountID.String(),
				StrategyID:        request.Body.StrategyId.String(),
				StrategyVersionID: request.Body.StrategyVersionId.String(),
				Name:              request.Body.Name,
				Level:             level,
				Limits:            limits,
				CorrelationID:     observability.CorrelationID(ctx),
			})
			if cerr != nil {
				return api.Agent{}, commandMeta{}, cerr
			}
			return toAPIAgent(v), commandMeta{
				Status: http.StatusCreated, ResourceType: "agent", ResourceID: v.Agent.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostAgents200JSONResponse(res.Value), nil
	}
	return api.PostAgents201JSONResponse(res.Value), nil
}

// GetAgents lists the caller's agents.
func (s *Server) GetAgents(ctx context.Context, request api.GetAgentsRequestObject) (api.GetAgentsResponseObject, error) {
	if s.opts.Ports.Agents == nil {
		return nil, errNotWired("agents")
	}
	accountID, err := agentAccountScopeRead(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	views, err := s.opts.Ports.Agents.List(ctx, accountID.String(),
		boolOr(request.Params.IncludeArchived, false), limitOrDefault(request.Params.Limit))
	if err != nil {
		return nil, err
	}
	return api.GetAgents200JSONResponse(toAPIAgentPage(views)), nil
}

// GetAgentsAgentId returns one agent: authority, limits, budget, last run and
// the honest runtime state.
func (s *Server) GetAgentsAgentId(ctx context.Context, request api.GetAgentsAgentIdRequestObject) (api.GetAgentsAgentIdResponseObject, error) {
	if s.opts.Ports.Agents == nil {
		return nil, errNotWired("agents")
	}
	agentID, err := parseAgentID(request.AgentId.String())
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Agents.Get(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return api.GetAgentsAgentId200JSONResponse(toAPIAgent(v)), nil
}

// PostAgentsAgentIdAction performs one lifecycle action on the caller's agent.
//
// The action is a path segment rather than a body field because each of the
// five is a different authority: an idempotency key is scoped to (actor,
// operation, key), and a body field would let one key replay across two
// different decisions.
func (s *Server) PostAgentsAgentIdAction(ctx context.Context, request api.PostAgentsAgentIdActionRequestObject) (api.PostAgentsAgentIdActionResponseObject, error) {
	if s.opts.Ports.Agents == nil {
		return nil, errNotWired("agents")
	}
	agentID, err := parseAgentID(request.AgentId.String())
	if err != nil {
		return nil, err
	}
	action := agents.Action(strings.ToLower(string(request.Action)))
	if !action.Valid() {
		return nil, validationError("action", "action must be enable, pause, resume, disable or archive")
	}
	reason := ""
	if request.Body != nil && request.Body.Reason != nil {
		reason = *request.Body.Reason
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.Agent, commandMeta, error) {
			v, cerr := s.opts.Ports.Agents.Act(ctx, agents.ActRequest{
				AgentID: agentID, Action: action, Reason: reason,
				CorrelationID: observability.CorrelationID(ctx),
			})
			if cerr != nil {
				return api.Agent{}, commandMeta{}, cerr
			}
			return toAPIAgent(v), commandMeta{
				Status: http.StatusOK, ResourceType: "agent", ResourceID: v.Agent.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAgentsAgentIdAction200JSONResponse(res.Value), nil
}

// GetAdminAgents is the operator's view of every account's agents.
func (s *Server) GetAdminAgents(ctx context.Context, request api.GetAdminAgentsRequestObject) (api.GetAdminAgentsResponseObject, error) {
	if s.opts.Ports.Agents == nil {
		return nil, errNotWired("agents")
	}
	f := agents.ListFilter{
		IncludeArchived: boolOr(request.Params.IncludeArchived, false),
		Limit:           limitOrDefault(request.Params.Limit),
	}
	if request.Params.AccountId != nil {
		f.AccountID = request.Params.AccountId.String()
	}
	views, err := s.opts.Ports.Agents.AdminList(ctx, f)
	if err != nil {
		return nil, err
	}
	return api.GetAdminAgents200JSONResponse(toAPIAgentPage(views)), nil
}

// PostAdminAgentsAgentIdPause is the operator pause: same table and same
// transition as an owner pause, under the OPERATOR reason code so the owner's
// history shows plainly that somebody else stopped it.
func (s *Server) PostAdminAgentsAgentIdPause(ctx context.Context, request api.PostAdminAgentsAgentIdPauseRequestObject) (api.PostAdminAgentsAgentIdPauseResponseObject, error) {
	if s.opts.Ports.Agents == nil {
		return nil, errNotWired("agents")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	agentID, err := parseAgentID(request.AgentId.String())
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(request.Body.Reason)
	if len(reason) < agent.MinReasonLength {
		return nil, validationError("reason", "an operator pause needs a reason of at least 8 characters")
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.Agent, commandMeta, error) {
			v, cerr := s.opts.Ports.Agents.AdminPause(ctx, agentID, reason, observability.CorrelationID(ctx))
			if cerr != nil {
				return api.Agent{}, commandMeta{}, cerr
			}
			return toAPIAgent(v), commandMeta{
				Status: http.StatusOK, ResourceType: "agent", ResourceID: v.Agent.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdminAgentsAgentIdPause200JSONResponse(res.Value), nil
}

// agentAccountScopeRead resolves an account id on a read, scoped to the caller.
// It is the read-side twin of accountScopeWrite: RequireAccount admits an
// operator holding account:read_any and the account's own owner, and refuses
// everybody else before the query runs.
func agentAccountScopeRead(ctx context.Context, u api.UUID) (accounts.AccountID, error) {
	return scopedAccount(ctx, u, security.RequireAccount)
}

// agentUUID renders a canonical UUID string as an api.UUID. Every caller
// passes a value the database already validated as a uuid column, so an
// unparsable one is the zero UUID rather than a fabricated identifier.
func agentUUID(s string) api.UUID {
	if u := parseUUIDText(s); u != nil {
		return *u
	}
	return api.UUID{}
}

func parseAgentID(s string) (agent.AgentID, error) {
	id, err := agent.ParseAgentID(s)
	if err != nil || id.IsZero() {
		return agent.AgentID{}, validationError("agentId", "agentId must be a canonical UUID")
	}
	return id, nil
}

func boolOr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func limitOrDefault(v *api.Limit) int {
	if v == nil {
		return 0
	}
	return int(*v)
}

// toDomainLimits parses the wire form of the limits into exact quantities.
// Every Credit figure arrives as a decimal string and becomes a big.Int-backed
// money.Quantity; none of them is ever parsed as a number.
func toDomainLimits(in api.AgentLimits) (agents.Limits, error) {
	var out agents.Limits
	fields := map[string]any{}
	parse := func(name, raw string) money.Quantity {
		q, err := money.ParseQuantity(raw)
		if err != nil {
			fields[name] = "must be an exact whole number of Credits"
			return money.Quantity{}
		}
		return q
	}
	out.BudgetCredits = parse("limits.budget_credits", in.BudgetCredits)
	out.PerTradeCapCredits = parse("limits.per_trade_cap_credits", in.PerTradeCapCredits)
	out.DailyLossStopCredits = parse("limits.daily_loss_stop_credits", in.DailyLossStopCredits)
	out.MaxPositionShareBPS = money.BPS(in.MaxPositionShareBps)
	for _, a := range in.AllowedAssetIds {
		id, err := assets.ParseAssetID(a.String())
		if err != nil || id.IsZero() {
			fields["limits.allowed_asset_ids"] = "every asset must be a canonical UUID"
			break
		}
		out.AllowedAssets = append(out.AllowedAssets, id)
	}
	out.Schedule = agents.Schedule{Kind: agents.ScheduleKind(in.Schedule.Kind)}
	if in.Schedule.IntervalMinutes != nil {
		out.Schedule.IntervalMinutes = *in.Schedule.IntervalMinutes
	}
	if len(fields) > 0 {
		return agents.Limits{}, errs.New(errs.CodeValidationFailed, "agents: the limits on this agent are not grantable").
			WithFields(fields)
	}
	return out, nil
}

// productStatus is the product's own word for a lifecycle state.
//
// It exists because "BACKTEST_ELIGIBLE" is a correct answer to a question
// nobody asked. What a person wants to know is whether the agent is off, on,
// paused or finished — and the mapping is stated once, here, rather than in
// every screen that renders one.
func productStatus(state agent.State) api.AgentStatus {
	switch {
	case state == agent.StatePaused:
		return "PAUSED"
	case state == agent.StateRevoked || state == agent.StateSuperseded:
		return "DISABLED"
	case state == agent.StateFailed:
		return "FAILED"
	case state.Runs():
		return "ENABLED"
	default:
		return "STOPPED"
	}
}

func toAPIAgent(v agents.View) api.Agent {
	out := api.Agent{
		Id:                toUUID(v.Agent.ID),
		AccountId:         agentUUID(v.Agent.AccountID),
		StrategyId:        agentUUID(v.Agent.StrategyID),
		StrategyVersionId: agentUUID(v.Agent.StrategyVersionID),
		Name:              v.Agent.Name,
		Stage:             api.AgentStage(v.Agent.Stage),
		State:             api.AgentState(v.Agent.State),
		Status:            productStatus(v.Agent.State),
		Authority:         toAPIAuthority(int(v.Grant.Level)),
		Limits:            toAPILimits(v.Grant.Limits),
		Budget: api.AgentBudget{
			GrantedCredits: v.Grant.Limits.BudgetCredits.String(),
			UsedCredits:    v.BudgetUsedCredits.String(),
			Source:         api.AgentBudgetSource(v.BudgetUsedSource),
		},
		Runtime:   toAPIRuntime(v.Runtime),
		Archived:  v.Grant.Archived(),
		CreatedAt: v.Agent.CreatedAt.UTC(),
	}
	if v.Agent.Mode != "" {
		m := api.AgentMode(v.Agent.Mode)
		out.Mode = &m
	}
	total := v.Runs.TotalRuns
	out.RunsTotal = &total
	out.LastRunAt = timePtr(v.Runs.LastRunAt)
	if v.Runs.LastRunStatus != "" {
		st := v.Runs.LastRunStatus
		out.LastRunStatus = &st
	}
	if v.Grant.GrantedByUserID != "" {
		u := agentUUID(v.Grant.GrantedByUserID)
		out.GrantedByUserId = &u
	}
	if !v.Grant.GrantedAt.IsZero() {
		g := v.Grant.GrantedAt.UTC()
		out.GrantedAt = &g
	}
	if !v.Agent.UpdatedAt.IsZero() {
		u := v.Agent.UpdatedAt.UTC()
		out.UpdatedAt = &u
	}
	if v.PauseOpen {
		p := api.AgentPause{
			ReasonCode:        v.Pause.ReasonCode.String(),
			Reason:            v.Pause.Reason,
			PausedByActorType: api.AgentPausePausedByActorType(v.Pause.PausedByActorType),
			PausedAt:          v.Pause.PausedAt.UTC(),
		}
		if v.Pause.OpenOrdersPolicy != "" {
			pol := api.AgentPauseOpenOrdersPolicy(v.Pause.OpenOrdersPolicy)
			p.OpenOrdersPolicy = &pol
		}
		out.Pause = &p
	}
	return out
}

func toAPILimits(l agents.Limits) api.AgentLimits {
	out := api.AgentLimits{
		BudgetCredits:        l.BudgetCredits.String(),
		PerTradeCapCredits:   l.PerTradeCapCredits.String(),
		DailyLossStopCredits: l.DailyLossStopCredits.String(),
		MaxPositionShareBps:  int(l.MaxPositionShareBPS),
		Schedule:             api.AgentSchedule{Kind: api.AgentScheduleKind(l.Schedule.Kind)},
	}
	out.AllowedAssetIds = make([]api.UUID, 0, len(l.AllowedAssets))
	for _, a := range l.AllowedAssets {
		out.AllowedAssetIds = append(out.AllowedAssetIds, toUUID(a))
	}
	if l.Schedule.Kind == agents.ScheduleInterval {
		m := l.Schedule.IntervalMinutes
		out.Schedule.IntervalMinutes = &m
	}
	return out
}

func toAPIRuntime(r agents.RuntimeStatus) api.AgentRuntime {
	return api.AgentRuntime{
		Evaluator:     api.AgentRuntimeEvaluator(r.Evaluator),
		Executor:      api.AgentRuntimeExecutor(r.Executor),
		LastHeartbeat: r.LastHeartbeat,
		Detail:        r.Detail,
	}
}

func toAPIAuthority(level int) api.AuthorityLevel {
	for _, d := range agents.AuthorityLevels() {
		if d.Level == level {
			return api.AuthorityLevel{
				Level: d.Level, Name: api.AuthorityLevelName(d.Name), Summary: d.Summary,
				Enabled: d.Enabled, RequiredCapability: optionalString(d.RequiredCapability),
			}
		}
	}
	// Unreachable for a stored grant: the column CHECK stops at the maximum
	// supported level. Returning a marked-disabled row rather than a zero value
	// means an impossible level would still read as "not enabled" and never as
	// "level 0, research only".
	return api.AuthorityLevel{Level: level, Name: "RESEARCH_ONLY", Summary: "unknown authority level", Enabled: false}
}

func toAPIAgentPage(views []agents.View) api.AgentPage {
	items := make([]api.Agent, 0, len(views))
	for _, v := range views {
		items = append(items, toAPIAgent(v))
	}
	levels := agents.AuthorityLevels()
	ladder := make([]api.AuthorityLevel, 0, len(levels))
	for _, d := range levels {
		ladder = append(ladder, api.AuthorityLevel{
			Level: d.Level, Name: api.AuthorityLevelName(d.Name), Summary: d.Summary,
			Enabled: d.Enabled, RequiredCapability: optionalString(d.RequiredCapability),
		})
	}
	return api.AgentPage{Items: items, AuthorityLevels: ladder}
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
