package agents

import (
	"time"

	"github.com/nodal/controlplane/internal/agent"
)

// ComponentState is what one half of the agent runtime is actually doing.
type ComponentState string

// The three honest answers, and only three.
const (
	// ComponentNotDeployed: no process that could do this work runs in this
	// deployment. It is not "idle": idle means something is there and has
	// nothing to do, and a product that says idle when nothing is deployed is
	// telling the user their agent is waiting its turn.
	ComponentNotDeployed ComponentState = "NOT_DEPLOYED"
	// ComponentIdle: the component is deployed and has no work in flight.
	ComponentIdle ComponentState = "IDLE"
	// ComponentRunning: work is in flight right now.
	ComponentRunning ComponentState = "RUNNING"
)

// RuntimeStatus is what is actually evaluating and executing for an agent.
//
// It is derived from evidence, never from the agent's lifecycle state. An agent
// can be ENABLED, correct, fully funded and evaluated by nothing at all, which
// is exactly the case on a deployment that runs no agent worker — so
// "enabled" and "running" are two different fields with two different sources.
type RuntimeStatus struct {
	// Evaluator opens runs and evaluates the strategy IR (cmd/agent-worker).
	Evaluator ComponentState `json:"evaluator"`
	// Executor turns an accepted intent into an order (cmd/execution-worker).
	Executor ComponentState `json:"executor"`
	// LastHeartbeat is the most recent evidence that either component was
	// alive. Nil when there has never been any, which on this tier is always.
	LastHeartbeat *time.Time `json:"last_heartbeat,omitempty"`
	// Detail says, in one sentence, why the components are in the states they
	// are in. It is always populated: a status field with no explanation is
	// how "NOT_DEPLOYED" gets read as a transient glitch.
	Detail string `json:"detail"`
}

// RuntimeEvidence is what the read model found in the database about an agent's
// runs. It is the only input to a runtime status besides the deployment's own
// declaration of which workers it runs.
type RuntimeEvidence struct {
	// OpenRuns counts agent_runs in a non-terminal status for this agent.
	OpenRuns int
	// TotalRuns counts every agent_runs row for this agent.
	TotalRuns int
	// LastRunAt is the most recent run's started_at, if any.
	LastRunAt *time.Time
	// LastRunStatus is that run's status.
	LastRunStatus string
}

// Deployment is what this binary knows about the worker processes. Both are
// false in every deployment of this build: neither worker is deployed on the
// free tier, cmd/agent-worker wires no evaluator, and F-65's premise is that
// nothing calls the runtime at all.
type Deployment struct {
	EvaluatorDeployed bool
	ExecutorDeployed  bool
}

// notDeployedDetail is the sentence the API returns when nothing runs. It names
// the component and the consequence, because the user's question is not "what
// is the state" but "will my agent do anything".
const notDeployedDetail = "No agent evaluator is deployed in this environment, so this agent is not being " +
	"evaluated and will not open runs or propose orders. Its authority, limits and history are recorded and " +
	"enforced; nothing is scheduled."

// Runtime derives the honest runtime status from evidence.
//
// The rule is one-directional: evidence can only ever make the answer WEAKER
// than the deployment claims. A deployment that says it runs an evaluator but
// has produced no run for this agent is IDLE, not RUNNING; a deployment that
// runs nothing is NOT_DEPLOYED regardless of what rows exist.
func Runtime(d Deployment, ev RuntimeEvidence) RuntimeStatus {
	s := RuntimeStatus{Evaluator: ComponentNotDeployed, Executor: ComponentNotDeployed}
	switch {
	case !d.EvaluatorDeployed:
		s.Evaluator = ComponentNotDeployed
	case ev.OpenRuns > 0:
		s.Evaluator = ComponentRunning
	default:
		s.Evaluator = ComponentIdle
	}
	switch {
	case !d.ExecutorDeployed:
		s.Executor = ComponentNotDeployed
	default:
		s.Executor = ComponentIdle
	}
	if d.EvaluatorDeployed && ev.LastRunAt != nil {
		s.LastHeartbeat = ev.LastRunAt
	}

	switch {
	case !d.EvaluatorDeployed:
		s.Detail = notDeployedDetail
	case ev.TotalRuns == 0:
		s.Detail = "An evaluator is deployed and this agent has not been evaluated yet."
	case s.Evaluator == ComponentRunning:
		s.Detail = "An evaluation is in flight."
	default:
		s.Detail = "An evaluator is deployed; the last evaluation finished as " + ev.LastRunStatus + "."
	}
	return s
}

// Runnable reports whether the agent's own lifecycle state permits runs at all.
// It is agent.State.Runs, restated here so a reader of this package sees that
// "enabled" is a question about the ladder and "running" is a question about
// processes, and that the two are answered separately.
func Runnable(s agent.State) bool { return s.Runs() }
