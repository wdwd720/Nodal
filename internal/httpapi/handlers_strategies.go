package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/nodal/controlplane/internal/agents"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/observability"
)

// StrategiesPort is the strategy half of the agent creation flow: describe,
// compile, review, approve (goal §18).
//
// Compile is a command rather than a read even when it produces nothing,
// because it always writes a compile_attempts row. "We did not try, and here is
// why" is as much a fact about a strategy as "we tried and it was rejected",
// and a surface that recorded only the second would leave a user looking at an
// empty history with no explanation.
//
// A nil port answers UNSUPPORTED.
type StrategiesPort interface {
	Create(ctx context.Context, req agents.CreateStrategyRequest) (agents.Strategy, error)
	Get(ctx context.Context, strategyID string) (agents.Strategy, error)
	List(ctx context.Context, accountID string, limit int) ([]agents.Strategy, error)
	Compile(ctx context.Context, strategyID, requestID, correlationID string) (agents.CompileOutcome, error)
	// CompilerConfigured reports whether this deployment can compile at all, so
	// the create flow can say so before a user writes a description rather than
	// after.
	CompilerConfigured() bool
}

// PostStrategies records a strategy description. Nothing is compiled here.
func (s *Server) PostStrategies(ctx context.Context, request api.PostStrategiesRequestObject) (api.PostStrategiesResponseObject, error) {
	if s.opts.Ports.Strategies == nil {
		return nil, errNotWired("strategies")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	var constraints json.RawMessage
	if request.Body.Constraints != nil {
		raw, merr := json.Marshal(*request.Body.Constraints)
		if merr != nil {
			return nil, validationError("constraints", "constraints must be a JSON object")
		}
		constraints = raw
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.Strategy, commandMeta, error) {
			st, cerr := s.opts.Ports.Strategies.Create(ctx, agents.CreateStrategyRequest{
				AccountID:     accountID.String(),
				Name:          request.Body.Name,
				Description:   request.Body.Description,
				Constraints:   constraints,
				CorrelationID: observability.CorrelationID(ctx),
			})
			if cerr != nil {
				return api.Strategy{}, commandMeta{}, cerr
			}
			return s.toAPIStrategy(st), commandMeta{
				Status: http.StatusCreated, ResourceType: "strategy", ResourceID: st.ID,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostStrategies200JSONResponse(res.Value), nil
	}
	return api.PostStrategies201JSONResponse(res.Value), nil
}

// GetStrategies lists the caller's strategies.
func (s *Server) GetStrategies(ctx context.Context, request api.GetStrategiesRequestObject) (api.GetStrategiesResponseObject, error) {
	if s.opts.Ports.Strategies == nil {
		return nil, errNotWired("strategies")
	}
	accountID, err := agentAccountScopeRead(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	list, err := s.opts.Ports.Strategies.List(ctx, accountID.String(), limitOrDefault(request.Params.Limit))
	if err != nil {
		return nil, err
	}
	items := make([]api.Strategy, 0, len(list))
	for _, st := range list {
		items = append(items, s.toAPIStrategy(st))
	}
	return api.GetStrategies200JSONResponse(api.StrategyPage{
		Items:              items,
		CompilerConfigured: s.opts.Ports.Strategies.CompilerConfigured(),
	}), nil
}

// GetStrategiesStrategyId returns one strategy and its current compiled
// version, which is what the review step in the creation flow shows.
func (s *Server) GetStrategiesStrategyId(ctx context.Context, request api.GetStrategiesStrategyIdRequestObject) (api.GetStrategiesStrategyIdResponseObject, error) {
	if s.opts.Ports.Strategies == nil {
		return nil, errNotWired("strategies")
	}
	st, err := s.opts.Ports.Strategies.Get(ctx, request.StrategyId.String())
	if err != nil {
		return nil, err
	}
	return api.GetStrategiesStrategyId200JSONResponse(s.toAPIStrategy(st)), nil
}

// PostStrategiesStrategyIdCompile compiles the description into a reviewable
// strategy, or records honestly why it did not.
//
// The idempotency key is reused as the compiler's request id, which is what
// bounds a compile request to at most eight attempts (compile_attempts'
// UNIQUE(request_id, attempt_no) and its 1..8 CHECK). Two different keys are
// two different requests, which is the right reading: asking again is a new
// decision by the user, not a retry of the last one.
func (s *Server) PostStrategiesStrategyIdCompile(ctx context.Context, request api.PostStrategiesStrategyIdCompileRequestObject) (api.PostStrategiesStrategyIdCompileResponseObject, error) {
	if s.opts.Ports.Strategies == nil {
		return nil, errNotWired("strategies")
	}
	strategyID := request.StrategyId.String()

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.CompileResult, commandMeta, error) {
			out, cerr := s.opts.Ports.Strategies.Compile(ctx, strategyID,
				request.Params.IdempotencyKey, observability.CorrelationID(ctx))
			if cerr != nil {
				return api.CompileResult{}, commandMeta{}, cerr
			}
			return toAPICompileResult(strategyID, out), commandMeta{
				Status: http.StatusOK, ResourceType: "compile_attempt", ResourceID: out.AttemptID,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostStrategiesStrategyIdCompile200JSONResponse(res.Value), nil
}

func (s *Server) toAPIStrategy(st agents.Strategy) api.Strategy {
	out := api.Strategy{
		Id:                 agentUUID(st.ID),
		AccountId:          agentUUID(st.AccountID),
		Name:               st.Name,
		Description:        st.Description,
		SourceKind:         api.StrategySourceKind(st.SourceKind),
		Status:             api.StrategyStatus(st.Status),
		CompilerConfigured: s.opts.Ports.Strategies != nil && s.opts.Ports.Strategies.CompilerConfigured(),
		CreatedAt:          st.CreatedAt,
	}
	if !st.UpdatedAt.IsZero() {
		u := st.UpdatedAt
		out.UpdatedAt = &u
	}
	if st.CurrentVersion != nil {
		v := toAPIStrategyVersion(*st.CurrentVersion)
		out.CurrentVersion = &v
	}
	return out
}

func toAPIStrategyVersion(v agents.StrategyVersion) api.StrategyVersion {
	out := api.StrategyVersion{
		Id:            agentUUID(v.ID),
		Version:       v.Version,
		Status:        api.StrategyVersionStatus(v.Status),
		IrHash:        v.IRHashHex,
		EffectSet:     v.EffectSet,
		HumanReadable: v.HumanReadable,
	}
	if out.EffectSet == nil {
		out.EffectSet = []string{}
	}
	if !v.BuiltAt.IsZero() {
		b := v.BuiltAt
		out.BuiltAt = &b
	}
	// The IR is handed over as the object it is. It is decoded rather than
	// passed through as bytes because the generated type is a map, and a
	// document that will not decode is omitted rather than rendered as a
	// string that looks like JSON to a person and is not JSON to a client.
	if len(v.IR) > 0 {
		var doc map[string]any
		if json.Unmarshal(v.IR, &doc) == nil && doc != nil {
			out.Ir = &doc
		}
	}
	return out
}

func toAPICompileResult(strategyID string, out agents.CompileOutcome) api.CompileResult {
	res := api.CompileResult{
		StrategyId: agentUUID(strategyID),
		AttemptId:  agentUUID(out.AttemptID),
		AttemptNo:  out.AttemptNo,
		Outcome:    api.CompileResultOutcome(out.Outcome),
		Detail:     out.Detail,
	}
	if len(out.FailureCodes) > 0 {
		codes := append([]string(nil), out.FailureCodes...)
		res.FailureCodes = &codes
	}
	if len(out.Clarifications) > 0 {
		cl := append([]string(nil), out.Clarifications...)
		res.Clarifications = &cl
	}
	if out.Version != nil {
		v := toAPIStrategyVersion(*out.Version)
		res.Version = &v
	}
	return res
}
