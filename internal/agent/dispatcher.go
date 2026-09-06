package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// TriggerEvent is the normalized event a dispatcher matches ON_EVENT triggers
// against. It is a value, not a reference into the stream: internal/agent does
// not import internal/reality, and the composition root adapts.
type TriggerEvent struct {
	EventID    string
	Type       string
	Source     string
	DedupID    string
	OccurredAt time.Time
	// InstrumentID scopes the event when it is instrument-specific.
	InstrumentID string
}

// PausedSkipInterval is how often a suppressed trigger is recorded while an
// agent is paused: once per interval per agent, for observability, rather
// than one row per suppressed event.
const PausedSkipInterval = time.Hour

// Dispatcher opens runs. It never evaluates inline: matching a trigger and
// performing an evaluation are separate so a slow provider cannot block event
// intake and so a run is durable before any money-adjacent work begins.
type Dispatcher interface {
	OnEvent(ctx context.Context, ev TriggerEvent) ([]Run, error)
	OnTick(ctx context.Context, now time.Time) ([]Run, error)
}

// DispatcherDeps are the dispatcher's collaborators.
type DispatcherDeps struct {
	DB     *db.DB
	Clock  clock.Clock
	Store  Store
	Pauses PauseChecker
	// IRFor loads the compiled IR of a strategy version.
	IRFor func(ctx context.Context, q db.Querier, strategyVersionID string) (*ir.IR, error)
	// MaxAgents bounds one dispatch pass.
	MaxAgents    int
	BuildVersion string
}

// PGDispatcher is the PostgreSQL dispatcher.
type PGDispatcher struct {
	deps DispatcherDeps
}

var _ Dispatcher = (*PGDispatcher)(nil)

// NewDispatcher builds the dispatcher.
func NewDispatcher(deps DispatcherDeps) (*PGDispatcher, error) {
	switch {
	case deps.DB == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: dispatcher requires a database")
	case deps.Clock == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: dispatcher requires a clock")
	case deps.Pauses == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: dispatcher requires a pause checker")
	case deps.IRFor == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: dispatcher requires an IR loader")
	}
	if deps.MaxAgents <= 0 {
		deps.MaxAgents = 200
	}
	if deps.BuildVersion == "" {
		deps.BuildVersion = config.BuildVersion
	}
	return &PGDispatcher{deps: deps}, nil
}

// OnEvent opens a run for every runnable agent whose ON_EVENT trigger matches.
func (d *PGDispatcher) OnEvent(ctx context.Context, ev TriggerEvent) ([]Run, error) {
	if ev.Type == "" {
		return nil, errs.New(errs.CodeValidationFailed, "agent: a trigger event needs a type")
	}
	return d.dispatch(ctx, TriggerOnEvent, ev, d.deps.Clock.Now())
}

// OnTick opens a run for every runnable agent whose ON_INTERVAL trigger is due.
func (d *PGDispatcher) OnTick(ctx context.Context, now time.Time) ([]Run, error) {
	if now.IsZero() {
		now = d.deps.Clock.Now()
	}
	return d.dispatch(ctx, TriggerOnInterval, TriggerEvent{}, now)
}

// MaxAgentsPerPass bounds one dispatch pass so a pathological deployment
// cannot make a single tick unbounded.
const MaxAgentsPerPass = 100_000

// dispatch walks every runnable agent, one page at a time. Paging rather than
// a single limited query matters: an agent past the page size would otherwise
// never be triggered, and would look identical to one whose trigger never
// matched.
func (d *PGDispatcher) dispatch(ctx context.Context, kind TriggerKind, ev TriggerEvent, now time.Time) ([]Run, error) {
	var (
		opened []Run
		cursor AgentCursor
		seen   int
	)
	for {
		agents, err := d.deps.Store.ListRunnablePage(ctx, d.deps.DB, cursor, d.deps.MaxAgents)
		if err != nil {
			return opened, err
		}
		if len(agents) == 0 {
			return opened, nil
		}
		for _, a := range agents {
			runs, derr := d.dispatchAgent(ctx, a, kind, ev, now)
			if derr != nil {
				return opened, derr
			}
			opened = append(opened, runs...)
		}
		last := agents[len(agents)-1]
		cursor = AgentCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		seen += len(agents)
		if len(agents) < d.deps.MaxAgents || seen >= MaxAgentsPerPass {
			return opened, nil
		}
	}
}

func (d *PGDispatcher) dispatchAgent(ctx context.Context, a Agent, kind TriggerKind, ev TriggerEvent, now time.Time) ([]Run, error) {
	// Stage eligibility comes first and is judged on the stage, not the state:
	// a paused agent keeps its stage, and the suppression below is what must
	// be recorded for it. Stages below BACKTEST_ELIGIBLE never run at all.
	if !a.Stage.State().Runs() || !ModeAllowed(a.Stage, a.Mode) {
		return nil, nil
	}
	// A paused agent produces no runs. The first suppressed trigger per
	// interval is recorded as SKIPPED{AGENT_PAUSED} so the suppression is
	// visible, and the rest are silent so a busy stream cannot flood the
	// table.
	if _, paused, err := d.deps.Pauses.OpenPause(ctx, d.deps.DB, a.ID); err != nil {
		return nil, err
	} else if paused {
		return d.recordPausedSkip(ctx, a, kind, ev, now)
	}
	// FAILED, REVOKED and SUPERSEDED agents run nothing and record nothing:
	// they are not suppressed, they are finished.
	if !a.Runnable() {
		return nil, nil
	}

	doc, err := d.deps.IRFor(ctx, d.deps.DB, a.StrategyVersionID)
	if err != nil {
		return nil, err
	}
	var opened []Run
	for _, trg := range doc.Triggers {
		if trg.Kind != ir.TriggerKind(kind) {
			continue
		}
		if kind == TriggerOnEvent && trg.EventType != "" && trg.EventType != ev.Type {
			continue
		}
		key := d.dedupKey(a, trg, ev, now)
		run := Run{
			ID: NewRunID(), AgentID: a.ID, AgentVersion: a.Version,
			StrategyVersionID: a.StrategyVersionID, AccountID: a.AccountID, EnvelopeID: a.EnvelopeID,
			Mode: a.Mode, TriggerName: trg.Name.String(), TriggerKind: kind, TriggerDedupKey: key,
			TriggerEventID: ev.EventID, DecisionTime: now, Status: RunStarted, StartedAt: now,
			CorrelationID: correlationFor(a.ID, key), BuildVersion: d.deps.BuildVersion,
		}
		if !ev.OccurredAt.IsZero() {
			at := ev.OccurredAt
			run.TriggerSourceEventAt = &at
		}
		var created Run
		var existed bool
		err := d.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 2},
			func(ctx context.Context, tx pgx.Tx) error {
				out, dup, cerr := d.deps.Store.CreateRun(ctx, tx, run)
				created, existed = out, dup
				return cerr
			})
		if err != nil {
			return opened, err
		}
		if !existed {
			opened = append(opened, created)
		}
	}
	return opened, nil
}

// recordPausedSkip writes at most one SKIPPED{AGENT_PAUSED} run per interval.
func (d *PGDispatcher) recordPausedSkip(ctx context.Context, a Agent, kind TriggerKind, ev TriggerEvent, now time.Time) ([]Run, error) {
	since := now.Add(-PausedSkipInterval)
	n, err := d.deps.Store.CountSkipsSince(ctx, d.deps.DB, a.ID, SkipAgentPaused, since)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, nil
	}
	bucket := now.Truncate(PausedSkipInterval)
	key := hashKey("paused", a.ID.String(), strconv.FormatInt(bucket.UnixNano(), 10))
	run := Run{
		ID: NewRunID(), AgentID: a.ID, AgentVersion: a.Version,
		StrategyVersionID: a.StrategyVersionID, AccountID: a.AccountID, EnvelopeID: a.EnvelopeID,
		Mode: a.Mode, TriggerName: "paused", TriggerKind: kind, TriggerDedupKey: key,
		TriggerEventID: ev.EventID, DecisionTime: now, Status: RunSkipped, SkipReason: SkipAgentPaused,
		StartedAt: now, FinishedAt: &now, CorrelationID: correlationFor(a.ID, key),
		BuildVersion: d.deps.BuildVersion,
	}
	err = d.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 2},
		func(ctx context.Context, tx pgx.Tx) error {
			_, _, cerr := d.deps.Store.CreateRun(ctx, tx, run)
			return cerr
		})
	if err != nil {
		return nil, err
	}
	return nil, nil
}

// dedupKey is the deterministic identity of one trigger firing. Re-delivering
// the same event, or ticking twice inside one interval window, produces the
// same key and therefore the same single run (the unique index on
// (agent_id, trigger_dedup_key) decides, not the application).
func (d *PGDispatcher) dedupKey(a Agent, trg ir.Trigger, ev TriggerEvent, now time.Time) []byte {
	if trg.Kind == ir.TriggerOnEvent {
		id := ev.DedupID
		if id == "" {
			id = ev.EventID
		}
		if window := trg.DedupWindowMS; window > 0 && id == "" {
			id = strconv.FormatInt(now.UnixMilli()/window, 10)
		}
		return hashKey("event", a.ID.String(), trg.Name.String(), ev.Type, id)
	}
	every := int64(0)
	if trg.EveryMS != nil {
		every = *trg.EveryMS
	}
	if every <= 0 {
		every = int64(time.Minute / time.Millisecond)
	}
	bucket := now.UnixMilli() / every
	return hashKey("interval", a.ID.String(), trg.Name.String(), strconv.FormatInt(bucket, 10))
}

// hashKey is a length-prefixed sha256 over its parts, so no two different
// tuples can collide by concatenation.
func hashKey(parts ...string) []byte {
	h := sha256.New()
	var n [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return h.Sum(nil)
}

// correlationFor derives a stable correlation id for a run, so every audit
// event, tool invocation, prediction and intent of that run shares one.
func correlationFor(agentID AgentID, key []byte) string {
	return "agent:" + agentID.String() + ":" + encodeHex(key[:8])
}
