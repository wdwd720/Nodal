package agent

import "github.com/nodal/controlplane/internal/id"

type (
	agentKind      struct{}
	runKind        struct{}
	transitionKind struct{}
	pauseKind      struct{}
	toolKind       struct{}
	invocationKind struct{}
	modelCallKind  struct{}
)

// Typed identifiers for the agent runtime aggregates.
type (
	// AgentID identifies one deployable strategy binding (agents.id).
	AgentID = id.ID[agentKind]
	// RunID identifies one bounded evaluation (agent_runs.id).
	RunID = id.ID[runKind]
	// TransitionID identifies one lifecycle transition row.
	TransitionID = id.ID[transitionKind]
	// PauseID identifies one pause record (agent_pauses.id).
	PauseID = id.ID[pauseKind]
	// ToolID identifies one registered tool (tools.id).
	ToolID = id.ID[toolKind]
	// InvocationID identifies one broker call (tool_invocations.id).
	InvocationID = id.ID[invocationKind]
	// ModelCallID identifies one model call record (model_calls.id).
	ModelCallID = id.ID[modelCallKind]
)

// NewAgentID returns a fresh agent id.
func NewAgentID() AgentID { return id.New[agentKind]() }

// NewRunID returns a fresh run id.
func NewRunID() RunID { return id.New[runKind]() }

// NewTransitionID returns a fresh lifecycle-transition id.
func NewTransitionID() TransitionID { return id.New[transitionKind]() }

// NewPauseID returns a fresh pause id.
func NewPauseID() PauseID { return id.New[pauseKind]() }

// NewToolID returns a fresh tool id.
func NewToolID() ToolID { return id.New[toolKind]() }

// NewInvocationID returns a fresh tool-invocation id.
func NewInvocationID() InvocationID { return id.New[invocationKind]() }

// NewModelCallID returns a fresh model-call id.
func NewModelCallID() ModelCallID { return id.New[modelCallKind]() }

// ParseAgentID parses the canonical 36-character form.
func ParseAgentID(s string) (AgentID, error) { return id.Parse[agentKind](s) }

// ParseRunID parses the canonical 36-character form.
func ParseRunID(s string) (RunID, error) { return id.Parse[runKind](s) }

// ParsePauseID parses the canonical 36-character form.
func ParsePauseID(s string) (PauseID, error) { return id.Parse[pauseKind](s) }

// ParseToolID parses the canonical 36-character form.
func ParseToolID(s string) (ToolID, error) { return id.Parse[toolKind](s) }

// ParseInvocationID parses the canonical 36-character form.
func ParseInvocationID(s string) (InvocationID, error) { return id.Parse[invocationKind](s) }
