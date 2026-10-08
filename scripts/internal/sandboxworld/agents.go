package sandboxworld

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/prediction"
	"github.com/nodal/controlplane/internal/strategy"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// AgentRuntime is the agent runtime over the world: the lifecycle that
// creates and promotes agents, and the dispatcher and runner that open and
// execute their runs, wired as cmd/agent-worker wires them -- the store, the
// pause checker, the envelope and budget readers, the prediction ledger, a
// broker per run over the tool registry with no provider adapter wired (an
// unwired tool is refused and the refusal recorded), no trade-intent emitter
// (cmd/agent-worker proposes none), the strategy version's IR read and
// parsed from the database, and the delegated path over the admission facade
// -- the only piece of delegated authority an agent tree may hold.
//
// The one collaborator a deployment does not have yet is the evaluator:
// cmd/agent-worker wires one that refuses every evaluation until
// internal/strategy exposes its deterministic evaluator. The caller supplies
// it through RunnerDeps.Evaluator, the runtime's own injection point.
type AgentRuntime struct {
	Lifecycle  *agent.Lifecycle
	Store      agent.Store
	Pauses     agent.PGPauseChecker
	Dispatcher *agent.PGDispatcher
	Runner     *agent.Runner
}

// OpenAgentRuntime wires the runtime with the given delegated authority and
// evaluator.
func (w *World) OpenAgentRuntime(delegated agent.DelegatedAuthority, evaluator agent.Evaluator) (*AgentRuntime, error) {
	lc, err := agent.NewLifecycle(agent.LifecycleDeps{
		Clock: w.Clock, Audit: agentAudit{w: w.Audit}, Approvals: agentApprovals{}, BuildVersion: w.component,
	})
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: agent lifecycle: %w", err)
	}
	store, pauses := agent.NewStore(), agent.NewPauseChecker()
	dispatcher, err := agent.NewDispatcher(agent.DispatcherDeps{
		DB: w.DB, Clock: w.Clock, Store: store, Pauses: pauses, IRFor: LoadStrategyIR, MaxAgents: 100, BuildVersion: w.component,
	})
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: agent dispatcher: %w", err)
	}
	ledger, err := prediction.NewLedger(w.Clock)
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: prediction ledger: %w", err)
	}
	runner, err := agent.NewRunner(agent.RunnerDeps{
		DB: w.DB, Clock: w.Clock, Store: store, Pauses: pauses, Envelope: agent.NewEnvelopeReader(), Evaluator: evaluator,
		Predictions: ledger,
		BrokerFor: func(a agent.Authority, runID agent.RunID) (agent.ToolBroker, error) {
			return agent.NewBroker(agent.BrokerDeps{
				DB: w.DB, Clock: w.Clock, Registry: agent.NewToolRegistry(), Budgets: agent.NewBudgetReader(), Pauses: pauses,
				Recorder: agent.NewInvocationRecorder(), ModelCalls: agent.NewModelCallRecorder(), Environment: string(w.Env),
				Adapters: map[string]agent.ToolAdapter{},
			}, a, runID)
		},
		EmitterFor: func(agent.Authority) (agent.IntentEmitter, error) {
			return nil, errs.New(errs.CodeUnsupported,
				"sandboxworld: as in cmd/agent-worker, no trade-intent emitter is wired; an agent here proposes no trade intent")
		},
		DelegatedFor: func(a agent.Authority) (agent.DelegatedProposer, error) {
			return agent.NewDelegatedEmitter(agent.DelegatedEmitterDeps{
				Clock: w.Clock, Authority: delegated, Pauses: pauses, Budgets: agent.NewBudgetReader(),
			}, a)
		},
		IRFor: LoadStrategyIR, BuildVersion: w.component,
	})
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: agent runner: %w", err)
	}
	return &AgentRuntime{Lifecycle: lc, Store: store, Pauses: pauses, Dispatcher: dispatcher, Runner: runner}, nil
}

// LoadStrategyIR reads the compiled IR of a strategy version and parses it
// through ir.ParseIR, exactly as cmd/agent-worker's loader does: a document
// that does not satisfy the schema is refused here rather than
// half-interpreted by the runtime.
func LoadStrategyIR(ctx context.Context, q db.Querier, strategyVersionID string) (*ir.IR, error) {
	var raw []byte
	if err := q.QueryRow(ctx, `SELECT ir FROM strategy_versions WHERE id = $1`, strategyVersionID).Scan(&raw); err != nil {
		return nil, errs.Wrap(err, errs.CodeNotFound, "sandboxworld: load strategy version ir")
	}
	doc, err := ir.ParseIR(raw)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "sandboxworld: parse strategy version ir")
	}
	return doc, nil
}

// StrategyVersion is a strategy and version the world recorded for an agent
// to run.
type StrategyVersion struct {
	StrategyID string
	VersionID  string
	IRHash     []byte
	Document   *ir.IR
}

// RecordStrategyVersion records a strategy owned by the customer and its
// first version, carrying doc.
//
// No service of this build persists a strategy version: the compiler
// (internal/strategy) produces one and nothing stores it. So the world
// records it the way the agent and delegation integration tests do, after
// the document stages the agent worker itself applies on every load
// (ir.ParseIR: forbidden effects, structure, declared effects equal to the
// derived ones) and with the compiler's own semantic hash and rendering. It
// is recorded COMPILED -- no compiler ran, and nobody accepted it -- with the
// compiler version naming this recorder, so it can never pass for the
// compiler's output.
func (w *World) RecordStrategyVersion(ctx context.Context, owner Customer, name string, doc *ir.IR) (StrategyVersion, error) {
	strategyID, versionID := id.New[id.Any]().String(), id.New[id.Any]().String()
	doc.SchemaVersion, doc.StrategyID, doc.Version = ir.SchemaVersion, strategyID, 1
	doc.Owner = ir.Owner{AccountID: owner.Account.String(), UserID: owner.User.String()}
	doc.BuiltAt = w.Clock.Now().UTC()
	doc.Hash = nil
	doc.Normalize()
	unhashed, err := json.Marshal(doc)
	if err != nil {
		return StrategyVersion{}, fmt.Errorf("sandboxworld: encode the strategy document: %w", err)
	}
	if _, err := ir.ParseIR(unhashed); err != nil {
		return StrategyVersion{}, fmt.Errorf("sandboxworld: the strategy document fails the document stages: %w", err)
	}
	hash, err := ir.SemanticHash(doc)
	if err != nil {
		return StrategyVersion{}, fmt.Errorf("sandboxworld: hash the strategy document: %w", err)
	}
	doc.Hash = hash
	human, err := strategy.Render(doc)
	if err != nil {
		return StrategyVersion{}, fmt.Errorf("sandboxworld: render the strategy document: %w", err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return StrategyVersion{}, fmt.Errorf("sandboxworld: encode the strategy document: %w", err)
	}
	if _, err := ir.ParseIR(raw); err != nil {
		return StrategyVersion{}, fmt.Errorf("sandboxworld: the hashed strategy document fails the document stages: %w", err)
	}
	source := sha256.Sum256(unhashed)
	budget := func(v any) ([]byte, error) { return json.Marshal(v) }
	model, err := budget(doc.ModelBudget)
	if err != nil {
		return StrategyVersion{}, err
	}
	data, err := budget(doc.DataBudget)
	if err != nil {
		return StrategyVersion{}, err
	}
	envelope, err := budget(doc.Envelope)
	if err != nil {
		return StrategyVersion{}, err
	}
	err = w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO strategies (id, owner_account_id, owner_user_id, name, description, source_kind, status,
			created_by_actor_type, created_by_actor_id) VALUES ($1, $2, $3, $4, $5, 'TYPESCRIPT_SDK', 'ACTIVE', 'USER', $6)`,
			strategyID, owner.Account, owner.User, name, "an agent strategy recorded by "+w.component, owner.User.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO strategy_versions (id, strategy_id, version, schema_version, ir, ir_hash, effect_set, status,
			source_kind, source_hash, compiler_version, risk_policy_version, risk_policy_hash, model_budget, data_budget,
			envelope_requirements, human_readable, built_at)
			VALUES ($1, $2, 1, $3, $4, $5, $6, 'COMPILED', 'TYPESCRIPT_SDK', $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			versionID, strategyID, ir.SchemaVersion, raw, hash, ir.EffectStrings(doc.Effects), source[:],
			w.component+": recorded after the document stages; no compiler ran", doc.RiskPolicy.Version, []byte(doc.RiskPolicy.Hash),
			model, data, envelope, human, doc.BuiltAt)
		return err
	})
	if err != nil {
		return StrategyVersion{}, fmt.Errorf("sandboxworld: record the strategy version: %w", err)
	}
	return StrategyVersion{StrategyID: strategyID, VersionID: versionID, IRHash: hash, Document: doc}, nil
}

// agentAudit is cmd/agent-worker's audit adapter: agent.AuditEvent copied
// onto the real audit writer, field for field.
type agentAudit struct{ w audit.Writer }

func (a agentAudit) Append(ctx context.Context, tx pgx.Tx, e agent.AuditEvent) error {
	_, err := a.w.Append(ctx, tx, audit.Event{
		Stream: e.Stream, ActorType: e.ActorType, ActorID: e.ActorID, Action: e.Action,
		ResourceType: e.ResourceType, ResourceID: e.ResourceID, Reason: e.Reason,
		EvidenceRef: e.EvidenceRef, PolicyVersion: e.PolicyVersion, CorrelationID: e.CorrelationID,
		Payload: e.Payload, OccurredAt: e.OccurredAt,
	})
	return err
}

// agentApprovals is cmd/agent-worker's approval adapter: it reads an
// approved admin action and nothing else -- it can neither create nor
// approve one.
type agentApprovals struct{}

func (agentApprovals) VerifyApproved(ctx context.Context, q db.Querier, approvalID, kind, targetID string) (agent.Approval, error) {
	var a agent.Approval
	err := q.QueryRow(ctx, `SELECT id::text, kind, target_id, proposed_by_user_id::text, coalesce(approved_by_user_id::text, ''), expires_at
		FROM admin_actions WHERE id = $1 AND status IN ('APPROVED','EXECUTED')`, approvalID).
		Scan(&a.ID, &a.Kind, &a.TargetID, &a.ProposedBy, &a.ApprovedBy, &a.ExpiresAt)
	if err != nil {
		return agent.Approval{}, errs.Wrap(err, errs.CodeForbidden, "sandboxworld: no approved admin action with that id")
	}
	if a.Kind != kind || a.TargetID != targetID {
		return agent.Approval{}, errs.New(errs.CodeForbidden, "sandboxworld: the approval does not match the requested promotion").
			WithField("approval_id", approvalID)
	}
	return a, nil
}
