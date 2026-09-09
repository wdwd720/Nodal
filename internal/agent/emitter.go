package agent

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// IntentEmitter is the only money-adjacent write agent code can reach.
// Everything after it — eligibility, risk, capital reservation, planning,
// quoting, inspection, signing, submission, reconciliation, ledger posting —
// is deterministic platform code that treats an agent intent exactly like a
// manual one and that the agent cannot influence.
type IntentEmitter interface {
	Emit(ctx context.Context, tx pgx.Tx, req EmitRequest) (intent.TradeIntent, error)
}

// IntentWriter is the subset of intent.Repository the emitter uses. Nothing
// here can approve, reserve, plan, sign or submit.
type IntentWriter interface {
	Create(ctx context.Context, tx pgx.Tx, t intent.TradeIntent) (intent.TradeIntent, error)
}

// EmitRequest is one proposed trade. The runtime fills the actor, linkage,
// mode and idempotency key itself: an evaluator cannot choose to file an
// intent as a human, for another agent, in another mode, or without a
// prediction.
type EmitRequest struct {
	RunID             RunID
	ActionName        string
	PredictionID      string
	Action            intent.Action
	InstrumentID      instruments.InstrumentID
	NotionalUSD       *money.USD
	TargetExposureUSD *money.USD
	Quantity          *money.Quantity
	Constraints       intent.Constraints
	Deadline          time.Time
	CorrelationID     string
}

// EmitterDeps are the emitter's collaborators.
type EmitterDeps struct {
	Clock    clock.Clock
	Intents  IntentWriter
	Pauses   PauseChecker
	Budgets  BudgetReader
	Envelope EnvelopeReader
}

// Emitter turns an evaluator decision into a typed trade intent.
type Emitter struct {
	deps      EmitterDeps
	authority Authority
}

var _ IntentEmitter = (*Emitter)(nil)

// NewEmitter binds an emitter to one run's frozen authority.
func NewEmitter(deps EmitterDeps, authority Authority) (*Emitter, error) {
	switch {
	case deps.Clock == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: emitter requires a clock")
	case deps.Intents == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: emitter requires an intent writer")
	case deps.Pauses == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: emitter requires a pause checker")
	case deps.Budgets == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: emitter requires a budget reader")
	case deps.Envelope == nil:
		// NewBroker refuses six nil dependencies; this one was optional, and
		// Emit guarded the whole envelope block on `deps.Envelope != nil`. A
		// caller that forgot it lost the instrument allow-list and the
		// single-trade cap silently, at any stage, with the agent's own
		// authority still saying it had an envelope (F-75).
		return nil, errs.New(errs.CodeValidationFailed, "agent: emitter requires an envelope reader")
	case authority.AgentID().IsZero():
		return nil, errs.New(errs.CodeValidationFailed, "agent: emitter requires a frozen authority")
	}
	return &Emitter{deps: deps, authority: authority}, nil
}

// IdempotencyKeyFor is the deterministic key of an agent intent. Replaying
// the same run and action can therefore never create a second intent, which
// is what makes the runner resumable after a crash at any step.
func IdempotencyKeyFor(runID RunID, action string) string {
	return "run:" + runID.String() + ":" + action
}

// Emit proposes one trade intent. It refuses, in order: a non-AGENT
// principal, a principal bound to another account, an open pause, a missing
// prediction, an instrument the envelope does not allow, an order-rate budget
// that is spent, and a trade above the envelope's single-trade cap.
//
// It never reserves capital, never approves anything and never touches an
// order. The prediction-predates-execution rule is enforced here, again by
// intent.Validate, and finally by the trade_intents trigger (AG001/AG002/
// AG003) which no application code can bypass.
func (e *Emitter) Emit(ctx context.Context, tx pgx.Tx, req EmitRequest) (intent.TradeIntent, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return intent.TradeIntent{}, errs.New(errs.CodeUnauthenticated, "agent: emitting an intent requires a principal")
	}
	if p.ActorType != security.ActorAgent {
		return intent.TradeIntent{}, errs.Newf(errs.CodeForbidden,
			"agent: the agent intent path is only for AGENT principals, not %s", p.ActorType).
			WithField("actor_type", string(p.ActorType))
	}
	if p.SubjectID != e.authority.AgentID().String() {
		return intent.TradeIntent{}, errs.New(errs.CodeForbidden, "agent: principal is a different agent").
			WithField("subject_id", p.SubjectID)
	}
	if err := security.RequireAt(ctx, security.PermIntentCreateAgent, e.deps.Clock.Now); err != nil {
		return intent.TradeIntent{}, authError(err)
	}
	if err := security.RequireAccount(ctx, e.authority.AccountID()); err != nil {
		return intent.TradeIntent{}, authError(err)
	}
	if req.RunID.IsZero() || req.ActionName == "" {
		return intent.TradeIntent{}, errs.New(errs.CodeValidationFailed, "agent: an intent must name its run and action")
	}
	if req.PredictionID == "" {
		return intent.TradeIntent{}, errs.New(errs.CodeValidationFailed,
			"agent: an agent intent must carry the prediction that preceded it").
			WithField("prediction_id", "required")
	}

	// A pause is effective immediately, including for a run already in
	// flight: this read happens inside the caller's transaction.
	if _, paused, err := e.deps.Pauses.OpenPause(ctx, tx, e.authority.AgentID()); err != nil {
		return intent.TradeIntent{}, err
	} else if paused {
		return intent.TradeIntent{}, errs.New(errs.CodeKillSwitchActive, "agent: paused; no intent is created").
			WithField("agent_id", e.authority.AgentID().String())
	}

	now := e.deps.Clock.Now()
	snap, err := e.deps.Budgets.Snapshot(ctx, tx, e.authority, req.RunID, now)
	if err != nil {
		return intent.TradeIntent{}, err
	}
	if err := snap.CheckIntent(); err != nil {
		return intent.TradeIntent{}, err
	}

	// An empty envelope id is the legitimate case -- the stages below CANARY
	// carry no envelope, and agents_check2 is what says so. A nil reader is not:
	// NewEmitter refuses one, so reaching here means the check runs.
	if e.authority.EnvelopeID() != "" {
		env, err := e.deps.Envelope.Envelope(ctx, tx, e.authority.EnvelopeID())
		if err != nil {
			return intent.TradeIntent{}, err
		}
		if err := e.checkEnvelope(env, req, now); err != nil {
			return intent.TradeIntent{}, err
		}
	}

	agentID := e.authority.AgentID().String()
	strategyVersionID := e.authority.StrategyVersionID()
	predictionID := req.PredictionID
	t := intent.TradeIntent{
		ID:                intent.NewIntentID(),
		AccountID:         e.authority.AccountID(),
		ActorType:         security.ActorAgent,
		ActorID:           agentID,
		AgentID:           &agentID,
		StrategyVersionID: &strategyVersionID,
		PredictionID:      &predictionID,
		Action:            req.Action,
		InstrumentID:      req.InstrumentID,
		NotionalUSD:       req.NotionalUSD,
		TargetExposureUSD: req.TargetExposureUSD,
		Quantity:          req.Quantity,
		Constraints:       req.Constraints,
		Deadline:          req.Deadline,
		RequestedAt:       now,
		IdempotencyKey:    IdempotencyKeyFor(req.RunID, req.ActionName),
		CorrelationID:     req.CorrelationID,
		Mode:              intent.Mode(e.authority.Mode()),
	}
	if err := t.Validate(); err != nil {
		return intent.TradeIntent{}, err
	}
	created, err := e.deps.Intents.Create(ctx, tx, t)
	if err != nil {
		switch {
		case IsPredictionAfterIntent(err):
			return intent.TradeIntent{}, errs.Wrap(err, errs.CodeValidationFailed,
				"agent: the prediction must be committed before the intent is requested")
		case IsPredictionIntentMismatch(err):
			return intent.TradeIntent{}, errs.Wrap(err, errs.CodeValidationFailed,
				"agent: prediction and intent disagree on agent, strategy version, mode or account")
		case IsIntentRequiresPrediction(err):
			return intent.TradeIntent{}, errs.Wrap(err, errs.CodeValidationFailed,
				"agent: an AGENT intent must carry prediction, strategy version and agent linkage")
		}
		return intent.TradeIntent{}, err
	}
	return created, nil
}

// checkEnvelope applies the envelope's own limits before the intent is even
// filed. The risk kernel and the reservation path apply them again; this is
// the cheap early refusal, never the only one.
func (e *Emitter) checkEnvelope(env EnvelopeSnapshot, req EmitRequest, now time.Time) error {
	if !env.Usable(now) {
		return errs.Newf(errs.CodeInsufficientBuyingPower, "agent: capital envelope %s is %s", env.ID, env.Status).
			WithField("envelope_id", env.ID).WithField("status", env.Status)
	}
	if !env.AllowsInstrument(req.InstrumentID.String()) {
		return errs.Newf(errs.CodeAssetRestricted, "agent: instrument %s is not on the envelope allowlist", req.InstrumentID).
			WithField("instrument_id", req.InstrumentID.String())
	}
	notional := req.NotionalUSD
	if notional == nil {
		notional = req.TargetExposureUSD
	}
	if notional != nil && !env.MaxSingleTradeUSD.IsZero() && notional.Cmp(env.MaxSingleTradeUSD) > 0 {
		return errs.Newf(errs.CodeRiskMaxPosition, "agent: %s exceeds the envelope single-trade cap %s",
			notional.String(), env.MaxSingleTradeUSD.String()).
			WithField("notional_usd", notional.String())
	}
	return nil
}

// AgentPrincipal builds the only principal an agent run may execute under:
// ActorType AGENT, the agent's own id as subject, its single bound account,
// and no roles. security.AgentPrincipal refuses to produce anything else, and
// security.Principal.Validate refuses a variant that carries roles.
func AgentPrincipal(a Authority) security.Principal {
	return security.AgentPrincipal(a.AgentID().String(), a.AccountID())
}

// WithAgentPrincipal attaches the agent principal to ctx.
func WithAgentPrincipal(ctx context.Context, a Authority) context.Context {
	return security.WithPrincipal(ctx, AgentPrincipal(a))
}

// CountIntentsSince counts an agent's intents in a window, for reporting.
func CountIntentsSince(ctx context.Context, q db.Querier, agentID AgentID, since time.Time) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM trade_intents WHERE agent_id = $1 AND requested_at >= $2`,
		agentID, since).Scan(&n)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "agent: count intents")
	}
	return n, nil
}
