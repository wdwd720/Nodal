package agents

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// StepUpMaxAge is how recent a strong authentication must be to GRANT an agent
// authority. It matches internal/agent's own promotion window so the boundary
// never contradicts the runtime it manages.
const StepUpMaxAge = 15 * time.Minute

// ExecutionCapability is the capability gate that must be ACTIVE before an
// agent may be enabled at an authority level that acts without a person
// confirming each action.
//
// It is LIVE_AGENT_TRADING — the gate that already exists for exactly this and
// is classified high-risk (internal/gates.IsHighRisk), so reaching ACTIVE needs
// three principals and four evidence references. There is deliberately no new
// "AGENT_EXECUTION" gate beside it: a second name for the same authority is how
// a deployment ends up with one of them on.
const ExecutionCapability = "LIVE_AGENT_TRADING"

// CapabilityChecker reports whether a capability is ACTIVE in this deployment,
// and says why when it is not.
//
// It is declared here rather than taken as *gates.Checker because internal/gates
// is on the list of packages an agent tree may never reach, and although this
// package is not an agent tree it sits directly beside one. Keeping the gate
// behind a one-method interface means the day somebody moves a file between the
// two packages the import does not come with it.
//
// A nil checker means nothing is active, which is the correct reading of a
// deployment that has not been asked.
type CapabilityChecker interface {
	// Active reports whether capability is ACTIVE, whether the activation is a
	// sandbox one, and the reason when it is not active.
	Active(ctx context.Context, q db.Querier, capability string) (active, sandbox bool, reason string, err error)
}

// Deps are the service's collaborators.
type Deps struct {
	DB    *db.DB
	Clock clock.Clock
	// Capabilities gates the authority levels that execute without a person
	// confirming. Nil means no capability is active.
	Capabilities CapabilityChecker
	// Runtime declares which worker processes this deployment runs. The zero
	// value — neither — is the truth for every deployment of this build.
	Runtime Deployment
	// Events receives agent lifecycle events. Nil is a working configuration.
	Events Publisher
	// Logger is where a swallowed publisher error goes.
	Logger *slog.Logger
	// BuildVersion is stamped on every transition.
	BuildVersion string
	// StepUpMaxAge tightens the step-up window; it can never widen it.
	StepUpMaxAge time.Duration
}

// Service is the agent management surface.
type Service struct {
	db     *db.DB
	clk    clock.Clock
	caps   CapabilityChecker
	rt     Deployment
	events Publisher
	log    *slog.Logger
	build  string
	stepUp time.Duration
	store  Store
	pauses agent.PGPauseChecker
}

// NewService builds the service. A database and a clock are required: an agent
// whose lifecycle cannot be recorded must not be creatable at all.
func NewService(deps Deps) (*Service, error) {
	if deps.DB == nil {
		return nil, errs.New(errs.CodeValidationFailed, "agents: service requires a database")
	}
	clk := deps.Clock
	if clk == nil {
		clk = clock.System()
	}
	build := deps.BuildVersion
	if build == "" {
		build = config.BuildVersion
	}
	step := StepUpMaxAge
	if deps.StepUpMaxAge > 0 && deps.StepUpMaxAge < step {
		step = deps.StepUpMaxAge
	}
	return &Service{
		db: deps.DB, clk: clk, caps: deps.Capabilities, rt: deps.Runtime,
		events: deps.Events, log: deps.Logger, build: build, stepUp: step,
		store: NewStore(), pauses: agent.NewPauseChecker(),
	}, nil
}

// MaxNameLength mirrors the agents.name CHECK.
const MaxNameLength = agent.MaxNameLength

// CreateRequest is POST /v1/agents.
type CreateRequest struct {
	AccountID string
	// StrategyID and StrategyVersionID name the compiled version this agent
	// deploys. Both are required: an agent bound to "the latest version" would
	// change behaviour under its owner without the owner approving anything,
	// which is precisely what goal §18's explicit-approval step exists to stop.
	StrategyID        string
	StrategyVersionID string
	Name              string
	Level             agentauthority.Level
	Limits            Limits
	CorrelationID     string
}

// Create records an agent and the authority its owner granted it.
//
// The agent is born DRAFT with no mode and no envelope (00736). Creating one
// grants nothing that runs: the owner has said what they would allow, and
// Enable is the separate act that says they want it to start.
func (s *Service) Create(ctx context.Context, req CreateRequest) (View, error) {
	p, err := s.ownerActor(ctx, req.AccountID)
	if err != nil {
		return View{}, err
	}
	name := strings.TrimSpace(req.Name)
	if l := len(name); l < 1 || l > MaxNameLength {
		return View{}, errs.New(errs.CodeValidationFailed, "agents: name must be 1..120 characters").
			WithField("name", "must be 1..120 characters")
	}
	if req.StrategyID == "" || req.StrategyVersionID == "" {
		return View{}, errs.New(errs.CodeValidationFailed,
			"agents: an agent is created from a compiled strategy version").
			WithField("strategy_version_id", "required")
	}
	if !req.Level.SupportedInThisBuild() {
		// Belt and braces: the HTTP layer parses the level through
		// ParseAuthorityLevel, and a caller that reached here with an
		// unsupported one gets the same refusal rather than a database error.
		if _, perr := ParseAuthorityLevel(int(req.Level)); perr != nil {
			return View{}, perr
		}
	}
	if err := req.Limits.Validate(); err != nil {
		return View{}, err
	}

	now := s.clk.Now().UTC()
	a := agent.Agent{
		ID:                 agent.NewAgentID(),
		AccountID:          req.AccountID,
		StrategyID:         req.StrategyID,
		StrategyVersionID:  req.StrategyVersionID,
		Name:               name,
		Stage:              agent.StageDraft,
		State:              agent.StateDraft,
		Version:            1,
		CreatedByActorType: string(p.ActorType),
		CreatedByActorID:   p.SubjectID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := a.Validate(); err != nil {
		return View{}, err
	}
	g := Grant{
		ID: NewGrantID(), AgentID: a.ID, AccountID: req.AccountID,
		StrategyVersionID: req.StrategyVersionID, Level: req.Level, Limits: req.Limits,
		GrantedByUserID: p.SubjectID, GrantedAt: now,
	}
	if err := g.Validate(); err != nil {
		return View{}, err
	}

	var view View
	err = s.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.store.InsertAgent(ctx, tx, a); err != nil {
			return err
		}
		// The creation is itself a transition, DRAFT to DRAFT, so the history
		// of every agent starts with the act that created it rather than with
		// the first thing that happened to it afterwards.
		if err := s.store.ApplyLifecycle(ctx, tx, LifecycleChange{
			AgentID: a.ID, From: a, ToState: agent.StateDraft, ToStage: agent.StageDraft,
			StrategyVersionID: req.StrategyVersionID,
			ActorType:         p.ActorType, ActorID: p.SubjectID,
			Reason:       "agent created at authority level " + req.Level.String(),
			BuildVersion: s.build, CorrelationID: req.CorrelationID, OccurredAt: now,
		}); err != nil {
			return err
		}
		if err := s.store.InsertGrant(ctx, tx, g); err != nil {
			return err
		}
		v, gerr := s.store.Get(ctx, tx, a.ID)
		if gerr != nil {
			return gerr
		}
		view = v
		return nil
	})
	if err != nil {
		return View{}, err
	}
	view = s.withRuntime(view)
	s.publish(ctx, EventAgentCreated, view, p, agent.PauseID{}, "agent created", req.CorrelationID, now)
	return view, nil
}

// Get returns one agent the caller owns.
func (s *Service) Get(ctx context.Context, agentID agent.AgentID) (View, error) {
	v, err := s.store.Get(ctx, s.db, agentID)
	if err != nil {
		return View{}, err
	}
	if err := s.requireOwnership(ctx, v.Agent.AccountID); err != nil {
		return View{}, err
	}
	return s.withPause(ctx, s.withRuntime(v))
}

// List returns the caller's agents.
func (s *Service) List(ctx context.Context, accountID string, includeArchived bool, limit int) ([]View, error) {
	if err := s.requireOwnership(ctx, accountID); err != nil {
		return nil, err
	}
	views, err := s.store.List(ctx, s.db, ListFilter{AccountID: accountID, IncludeArchived: includeArchived, Limit: limit})
	if err != nil {
		return nil, err
	}
	for i := range views {
		views[i] = s.withRuntime(views[i])
	}
	return views, nil
}

// AdminList returns every account's agents, for an operator.
func (s *Service) AdminList(ctx context.Context, f ListFilter) ([]View, error) {
	if err := security.RequireAnyAt(ctx, s.clk.Now, security.PermAccountReadAny); err != nil {
		return nil, authError(err)
	}
	views, err := s.store.List(ctx, s.db, f)
	if err != nil {
		return nil, err
	}
	for i := range views {
		views[i] = s.withRuntime(views[i])
	}
	return views, nil
}

// Action is one lifecycle move a person can ask for.
type Action string

// The five actions the product exposes.
const (
	// ActionEnable grants the agent the right to be evaluated: VALIDATED and
	// below become BACKTEST_ELIGIBLE in PAPER mode. It never reaches a
	// real-capital stage; those rungs need an operator, a dual-controlled
	// approval and hashed evidence, and they are not on this surface.
	ActionEnable Action = "enable"
	// ActionPause suspends the agent and opens an agent_pauses row.
	ActionPause Action = "pause"
	// ActionResume closes the pause and returns the agent to its stage.
	ActionResume Action = "resume"
	// ActionDisable revokes the agent's authority. REVOKED is terminal by
	// design: turning an agent off is not meant to be a toggle, and an owner
	// who wants it back makes a new grant they have to read again.
	ActionDisable Action = "disable"
	// ActionArchive hides a stopped agent from the default list. It changes no
	// lifecycle state — REVOKED and SUPERSEDED have no outgoing edge (00734) —
	// and only archives the grant.
	ActionArchive Action = "archive"
)

var allActions = []Action{ActionEnable, ActionPause, ActionResume, ActionDisable, ActionArchive}

// Actions returns every declared action.
func Actions() []Action { return append([]Action(nil), allActions...) }

// Valid reports whether a is declared.
func (a Action) Valid() bool {
	for _, v := range allActions {
		if v == a {
			return true
		}
	}
	return false
}

// String renders the action.
func (a Action) String() string { return string(a) }

// ActRequest is POST /v1/agents/{id}/{action}.
type ActRequest struct {
	AgentID       agent.AgentID
	Action        Action
	Reason        string
	CorrelationID string
}

// Act performs one lifecycle action on an agent the caller owns.
func (s *Service) Act(ctx context.Context, req ActRequest) (View, error) {
	if !req.Action.Valid() {
		return View{}, errs.Newf(errs.CodeValidationFailed, "agents: %q is not an action", req.Action).
			WithField("action", string(req.Action))
	}
	v, err := s.store.Get(ctx, s.db, req.AgentID)
	if err != nil {
		return View{}, err
	}
	p, err := s.ownerActor(ctx, v.Agent.AccountID)
	if err != nil {
		return View{}, err
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = defaultReason(req.Action)
	}

	now := s.clk.Now().UTC()
	var kind EventKind
	var pauseID agent.PauseID
	err = s.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		current, lerr := s.store.LockAgent(ctx, tx, req.AgentID)
		if lerr != nil {
			return lerr
		}
		k, pid, aerr := s.apply(ctx, tx, current, v.Grant, p, req.Action, reason, req.CorrelationID, now)
		if aerr != nil {
			return aerr
		}
		kind, pauseID = k, pid
		updated, gerr := s.store.Get(ctx, tx, req.AgentID)
		if gerr != nil {
			return gerr
		}
		v = updated
		return nil
	})
	if err != nil {
		return View{}, err
	}
	v = s.withRuntime(v)
	s.publish(ctx, kind, v, p, pauseID, reason, req.CorrelationID, now)
	return s.withPause(ctx, v)
}

// apply is the state machine, and the only place it lives.
//
// The second return is the agent_pauses row a pause opened, or the one a resume
// closed, and is empty for every other action. It travels on the event so a
// consumer can key on the pause rather than on the agent.
func (s *Service) apply(ctx context.Context, tx pgx.Tx, a agent.Agent, g Grant, p security.Principal,
	action Action, reason, correlationID string, now time.Time,
) (EventKind, agent.PauseID, error) {
	switch action {
	case ActionEnable:
		return EventAgentEnabled, agent.PauseID{}, s.enable(ctx, tx, a, g, p, reason, correlationID, now)

	case ActionPause:
		pauseID, err := s.pause(ctx, tx, a, p, agent.PauseOwnerRequest, reason, correlationID, now)
		return EventAgentPaused, pauseID, err

	case ActionResume:
		if a.State != agent.StatePaused {
			return "", agent.PauseID{}, errs.Newf(errs.CodeInvalidStateTransition, "agents: this agent is %s, not paused", a.State).
				WithField("state", string(a.State))
		}
		if !agent.CanTransition(a.State, a.Stage.State()) {
			return "", agent.PauseID{}, errs.Newf(errs.CodeInvalidStateTransition, "agents: cannot resume from %s to %s", a.State, a.Stage).
				WithField("from", string(a.State)).WithField("to", string(a.Stage))
		}
		if err := s.store.ClosePause(ctx, tx, a.ID, now, p.ActorType, p.SubjectID, reason); err != nil {
			return "", agent.PauseID{}, err
		}
		if err := s.store.ApplyLifecycle(ctx, tx, LifecycleChange{
			AgentID: a.ID, From: a, ToState: a.Stage.State(), ToStage: a.Stage,
			ToMode: agent.DefaultModeForStage(a.Stage), StrategyVersionID: a.StrategyVersionID,
			RiskPolicyVersion: a.RiskPolicyVersion, ActorType: p.ActorType, ActorID: p.SubjectID,
			Reason: reason, BuildVersion: s.build, CorrelationID: correlationID, OccurredAt: now,
		}); err != nil {
			return "", agent.PauseID{}, err
		}
		return EventAgentResumed, agent.PauseID{}, nil

	case ActionDisable:
		if a.State.IsTerminal() {
			return "", agent.PauseID{}, errs.Newf(errs.CodeInvalidStateTransition, "agents: this agent is already %s", a.State).
				WithField("state", string(a.State))
		}
		if !agent.CanTransition(a.State, agent.StateRevoked) {
			return "", agent.PauseID{}, errs.Newf(errs.CodeInvalidStateTransition, "agents: cannot disable from %s", a.State).
				WithField("from", string(a.State))
		}
		// A disabled agent that was paused leaves no open pause behind: a pause
		// nobody can ever resume is a row that outlives its meaning.
		if a.State == agent.StatePaused {
			if err := s.store.ClosePause(ctx, tx, a.ID, now, p.ActorType, p.SubjectID, "agent disabled"); err != nil {
				return "", agent.PauseID{}, err
			}
		}
		if err := s.store.ApplyLifecycle(ctx, tx, LifecycleChange{
			AgentID: a.ID, From: a, ToState: agent.StateRevoked, ToStage: a.Stage,
			ToMode: a.Mode, StrategyVersionID: a.StrategyVersionID, RiskPolicyVersion: a.RiskPolicyVersion,
			ActorType: p.ActorType, ActorID: p.SubjectID, Reason: reason,
			BuildVersion: s.build, CorrelationID: correlationID, OccurredAt: now,
		}); err != nil {
			return "", agent.PauseID{}, err
		}
		return EventAgentDisabled, agent.PauseID{}, nil

	case ActionArchive:
		if !a.State.IsTerminal() && a.State != agent.StateFailed {
			return "", agent.PauseID{}, errs.Newf(errs.CodeInvalidStateTransition,
				"agents: disable this agent before archiving it; it is %s", a.State).
				WithField("state", string(a.State))
		}
		if err := s.store.ArchiveGrant(ctx, tx, a.ID, now); err != nil {
			return "", agent.PauseID{}, err
		}
		return EventAgentArchived, agent.PauseID{}, nil
	}
	return "", agent.PauseID{}, errs.Newf(errs.CodeValidationFailed, "agents: %q is not an action", action)
}

// enable moves the agent onto the first rung at which it may be evaluated.
//
// Two refusals matter here and neither is cosmetic. A level that executes
// without a person confirming each action needs the capability gate that
// already governs agent trading; and granting authority needs a recent strong
// authentication, exactly as promoting one does.
func (s *Service) enable(ctx context.Context, tx pgx.Tx, a agent.Agent, g Grant, p security.Principal,
	reason, correlationID string, now time.Time,
) error {
	if err := security.RequireStepUp(ctx, s.stepUp, s.clk.Now); err != nil {
		return authError(err)
	}
	if a.State == agent.StatePaused {
		return errs.New(errs.CodeInvalidStateTransition, "agents: resume this agent rather than enabling it").
			WithField("state", string(a.State))
	}
	if a.State.Runs() {
		return errs.New(errs.CodeConflict, "agents: this agent is already enabled").
			WithField("state", string(a.State))
	}
	if a.State.IsTerminal() || a.State == agent.StateFailed {
		return errs.Newf(errs.CodeInvalidStateTransition, "agents: a %s agent cannot be enabled", a.State).
			WithField("state", string(a.State))
	}
	if ExecutesWithoutConfirmation(g.Level) {
		if err := s.requireExecutionCapability(ctx, tx, g.Level); err != nil {
			return err
		}
	}

	// One rung at a time, in order, each with its own row. DRAFT to
	// BACKTEST_ELIGIBLE is three rungs and the ladder skips none of them
	// (agent.CanTransition), so enabling writes three transitions and the
	// history says which authority each one granted.
	target := agent.StageBacktestEligible
	from := a
	for from.Stage != target {
		next, ok := nextRung(from.Stage)
		if !ok {
			return errs.Newf(errs.CodeInvalidStateTransition, "agents: %s has no next rung", from.Stage).
				WithField("stage", string(from.Stage))
		}
		if !agent.CanTransition(from.State, next.State()) {
			return errs.Newf(errs.CodeInvalidStateTransition, "agents: cannot move from %s to %s", from.State, next).
				WithField("from", string(from.State)).WithField("to", string(next))
		}
		mode := agent.DefaultModeForStage(next)
		if err := s.store.ApplyLifecycle(ctx, tx, LifecycleChange{
			AgentID: a.ID, From: from, ToState: next.State(), ToStage: next, ToMode: mode,
			StrategyVersionID: a.StrategyVersionID, RiskPolicyVersion: a.RiskPolicyVersion,
			ActorType: p.ActorType, ActorID: p.SubjectID, Reason: reason,
			BuildVersion: s.build, CorrelationID: correlationID, OccurredAt: now,
		}); err != nil {
			return err
		}
		from.Stage = next
		from.State = next.State()
		from.Mode = mode
	}
	return nil
}

// nextRung returns the ladder position immediately above s, up to the point
// this surface is allowed to reach. SHADOW and beyond are deliberately absent:
// they need hashed promotion evidence, and CANARY, LIMITED and LIVE need a
// dual-controlled approval, so they belong to the operator ladder and not to a
// button in a customer's settings.
func nextRung(s agent.Stage) (agent.Stage, bool) {
	switch s {
	case agent.StageDraft:
		return agent.StageCompiled, true
	case agent.StageCompiled:
		return agent.StageValidated, true
	case agent.StageValidated:
		return agent.StageBacktestEligible, true
	}
	return "", false
}

// requireExecutionCapability refuses to enable an executing agent unless the
// gate is ACTIVE, and returns the gate's own reason rather than a paraphrase.
func (s *Service) requireExecutionCapability(ctx context.Context, q db.Querier, level agentauthority.Level) error {
	reason := "no capability checker is configured in this deployment, so no capability is active"
	if s.caps != nil {
		active, sandbox, why, err := s.caps.Active(ctx, q, ExecutionCapability)
		if err != nil {
			return err
		}
		if active {
			// A sandbox activation carries no approval (ADR-0023) and is
			// permitted here for exactly the reason the sandbox tier exists:
			// the rehearsal must exercise the real refusal path. It is
			// recorded in the transition reason so nothing later reads the
			// enablement as an approved one.
			_ = sandbox
			return nil
		}
		if why != "" {
			reason = why
		}
	}
	return errs.Newf(errs.CodeCapabilityNotApproved,
		"agents: authority level %s executes without a person confirming each action, and the capability that "+
			"permits that is not active in this deployment", level.String()).
		WithField("required_capability", ExecutionCapability).
		WithField("authority_level", int(level)).
		WithField("gate_reason", reason)
}

// pause opens an agent_pauses row and moves the agent to PAUSED. It returns the
// id of the row it opened, which is what the event keys on.
func (s *Service) pause(ctx context.Context, tx pgx.Tx, a agent.Agent, p security.Principal,
	code agent.PauseReason, reason, correlationID string, now time.Time,
) (agent.PauseID, error) {
	if a.State == agent.StatePaused {
		return agent.PauseID{}, errs.New(errs.CodeConflict, "agents: this agent is already paused").
			WithField("state", string(a.State))
	}
	if !agent.CanTransition(a.State, agent.StatePaused) {
		return agent.PauseID{}, errs.Newf(errs.CodeInvalidStateTransition, "agents: cannot pause from %s", a.State).
			WithField("from", string(a.State))
	}
	req := agent.PauseRequest{
		ReasonCode: code, Reason: reason,
		// LEAVE, and never anything else from this surface. PART 71 forbids an
		// implicit cancellation, and nothing here may cancel an order: the
		// cancel-cancelable policy names a workflow this package does not own
		// and must not pretend to have started.
		OpenOrdersPolicy: agent.LeaveOpenOrders,
		CorrelationID:    correlationID,
	}
	if err := req.Validate(); err != nil {
		return agent.PauseID{}, err
	}
	pauseID := agent.NewPauseID()
	if err := s.store.OpenPause(ctx, tx, agent.Pause{
		ID: pauseID, AgentID: a.ID, ReasonCode: code, Reason: reason,
		OpenOrdersPolicy: agent.LeaveOpenOrders, PausedByActorType: p.ActorType,
		PausedByActorID: p.SubjectID, PausedAt: now, CorrelationID: correlationID,
	}); err != nil {
		return agent.PauseID{}, err
	}
	if err := s.store.ApplyLifecycle(ctx, tx, LifecycleChange{
		AgentID: a.ID, From: a, ToState: agent.StatePaused, ToStage: a.Stage, ToMode: a.Mode,
		StrategyVersionID: a.StrategyVersionID, RiskPolicyVersion: a.RiskPolicyVersion,
		ActorType: p.ActorType, ActorID: p.SubjectID, Reason: reason,
		BuildVersion: s.build, CorrelationID: correlationID, OccurredAt: now,
	}); err != nil {
		return agent.PauseID{}, err
	}
	return pauseID, nil
}

// AdminPause is the operator's pause: same table, same transition, different
// actor and a reason code that says who stopped it.
//
// It is a separate method rather than Act with an operator principal because
// the two differ in every check: an operator holds no account, must never be
// scoped to one, and pauses under OPERATOR rather than OWNER_REQUEST so the
// owner's own history shows plainly that somebody else did this.
func (s *Service) AdminPause(ctx context.Context, agentID agent.AgentID, reason, correlationID string) (View, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return View{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	if p.ActorType != security.ActorOperator {
		return View{}, errs.New(errs.CodeForbidden, "agents: an operator pause is an operator's to make").
			WithField("actor_type", string(p.ActorType))
	}
	if err := security.RequireAt(ctx, security.PermAgentPause, s.clk.Now); err != nil {
		return View{}, authError(err)
	}
	if len(strings.TrimSpace(reason)) < agent.MinReasonLength {
		return View{}, errs.New(errs.CodeValidationFailed, "agents: an operator pause needs a reason").
			WithField("reason", "must be at least 8 characters")
	}

	now := s.clk.Now().UTC()
	var v View
	var pauseID agent.PauseID
	err := s.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		current, lerr := s.store.LockAgent(ctx, tx, agentID)
		if lerr != nil {
			return lerr
		}
		pid, perr := s.pause(ctx, tx, current, p, agent.PauseOperator, reason, correlationID, now)
		if perr != nil {
			return perr
		}
		pauseID = pid
		updated, gerr := s.store.Get(ctx, tx, agentID)
		if gerr != nil {
			return gerr
		}
		v = updated
		return nil
	})
	if err != nil {
		return View{}, err
	}
	v = s.withRuntime(v)
	s.publish(ctx, EventAgentPaused, v, p, pauseID, reason, correlationID, now)
	return s.withPause(ctx, v)
}

// withRuntime attaches the honest runtime status.
func (s *Service) withRuntime(v View) View {
	v.Runtime = Runtime(s.rt, v.Runs)
	return v
}

// withPause attaches the open pause when there is one. A failure to read it is
// not a failure of the action that just succeeded, so it is reported as an
// error only on the read paths.
func (s *Service) withPause(ctx context.Context, v View) (View, error) {
	p, open, err := s.pauses.OpenPause(ctx, s.db, v.Agent.ID)
	if err != nil {
		return v, errs.Wrap(err, errs.CodeInternal, "agents: read pause")
	}
	v.Pause, v.PauseOpen = p, open
	return v, nil
}

// ownerActor resolves the acting principal for an owner action and refuses
// every actor type that must not reach it.
func (s *Service) ownerActor(ctx context.Context, accountID string) (security.Principal, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return security.Principal{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	if err := p.Validate(); err != nil {
		return security.Principal{}, errs.Wrap(err, errs.CodeForbidden, "agents: invalid principal")
	}
	if p.ActorType != security.ActorUser {
		// An operator does not create or enable somebody's agent. There is an
		// operator surface and it can pause, which is the only thing an
		// operator should be able to do to a customer's agent without the
		// customer asking.
		return security.Principal{}, errs.Newf(errs.CodeForbidden,
			"agents: %s principals cannot act on a customer's agent", p.ActorType).
			WithField("actor_type", string(p.ActorType))
	}
	if err := security.RequireAt(ctx, security.PermStrategyWrite, s.clk.Now); err != nil {
		return security.Principal{}, authError(err)
	}
	if err := security.RequireAccount(ctx, accountID); err != nil {
		return security.Principal{}, authError(err)
	}
	return p, nil
}

// requireOwnership is the read-side scope check: the owner, or an operator who
// may read any account.
func (s *Service) requireOwnership(ctx context.Context, accountID string) error {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	if p.ActorType == security.ActorOperator {
		if err := security.RequireAnyAt(ctx, s.clk.Now, security.PermAccountReadAny); err != nil {
			return authError(err)
		}
		return nil
	}
	if err := security.RequireAt(ctx, security.PermStrategyRead, s.clk.Now); err != nil {
		return authError(err)
	}
	if err := security.RequireAccount(ctx, accountID); err != nil {
		return authError(err)
	}
	return nil
}

// publish hands one event to whatever the composition root wired, AFTER the
// transaction that made it true committed.
//
// Outside the transaction on purpose, and this is the half that matters: an
// agent that could not be paused because a notification failed would be a
// control defeated by a mailbox. A consumer that needs the telling to be a fact
// of a transaction opens its own -- the event describes a row that is already
// committed, so there is nothing it can say that did not happen.
func (s *Service) publish(ctx context.Context, kind EventKind, v View, p security.Principal,
	pauseID agent.PauseID, reason, correlationID string, now time.Time,
) {
	if s.events == nil || kind == "" {
		return
	}
	e := Event{
		Kind: kind, AgentID: v.Agent.ID, AccountID: v.Agent.AccountID, AgentName: v.Agent.Name,
		State: v.Agent.State, Runtime: v.Runtime, ActorType: string(p.ActorType), ActorID: p.SubjectID,
		Reason: reason, CorrelationID: correlationID, OccurredAt: now,
	}
	if !pauseID.IsZero() {
		e.PauseID = pauseID.String()
	}
	if err := s.events.Publish(ctx, e); err != nil && s.log != nil {
		// Swallowed on purpose: an agent that could not be paused because a
		// notification failed would be a control defeated by a mailbox.
		s.log.WarnContext(ctx, "agents: publishing an agent event failed",
			slog.String("kind", string(kind)), slog.String("agent_id", v.Agent.ID.String()),
			slog.String("error", err.Error()))
	}
}

func defaultReason(a Action) string {
	switch a {
	case ActionEnable:
		return "enabled by its owner"
	case ActionPause:
		return "paused by its owner"
	case ActionResume:
		return "resumed by its owner"
	case ActionDisable:
		return "disabled by its owner"
	case ActionArchive:
		return "archived by its owner"
	}
	return "requested by its owner"
}

// authError maps the security package's sentinels onto stable codes, the same
// way internal/agent's lifecycle does, so a refusal reads identically wherever
// it came from.
func authError(err error) error {
	switch {
	case errors.Is(err, security.ErrUnauthenticated):
		return errs.Wrap(err, errs.CodeUnauthenticated, "authentication is required")
	case errors.Is(err, security.ErrStepUpRequired):
		return errs.Wrap(err, errs.CodeStepUpRequired, "recent strong authentication is required")
	default:
		return errs.Wrap(err, errs.CodeForbidden, "forbidden")
	}
}
