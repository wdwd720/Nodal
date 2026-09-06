package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/observability"
)

// WakeSource is the inbox source of every message this worker consumes. The
// inbox key is (source, message id), so the same event consumed by another
// service is deduplicated independently of this one.
const WakeSource = "execution-worker"

// WakeTopics are the topics whose arrival means a plan may have progressed
// outside this process and should be looked at now rather than at the next
// poll. All three are keyed by the order, so a partition delivers them in
// order per order.
var WakeTopics = []event.Topic{
	event.TopicOrderTransitioned,
	event.TopicFillObserved,
	event.TopicExecutionAttemptTransitioned,
}

// Waker consumes execution events and clears the retry backoff of the plan
// they belong to, so the next claim picks it up immediately.
//
// # Why the nudge does no financial work
//
// The bus is at-least-once, so a consumer that recorded fills, posted
// journal entries or moved positions itself would have to re-implement the
// exactly-once machinery the settlement executor already owns — and would be
// a second, divergent path to the same money. Instead the only effect of a
// delivery is a scheduling hint. Two guards make a duplicate a no-op:
//
//  1. event.Inbox.Process deduplicates on (source, message id) inside the
//     same transaction as the effect, so the effect runs at most once per
//     message even under concurrent redelivery;
//  2. the effect itself is a monotonic UPDATE that only ever moves a
//     deadline earlier, so running it twice is indistinguishable from
//     running it once.
//
// Everything that touches money then happens exactly once because the
// executor's per-step durable state says so: a SUCCEEDED step is skipped,
// a fill is unique on (venue, external_fill_id), and journal_transaction_id
// and position_applied_at are each set once.
type Waker struct {
	db    *db.DB
	inbox *event.Inbox
	log   *slog.Logger
	// Source is the inbox source; defaults to WakeSource.
	Source string
}

// NewWaker builds a Waker.
func NewWaker(d *db.DB, inbox *event.Inbox, log *slog.Logger) (*Waker, error) {
	if d == nil || inbox == nil {
		return nil, errs.New(errs.CodeInternal, "execution-worker: waker needs a database and an inbox")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Waker{db: d, inbox: inbox, log: log, Source: WakeSource}, nil
}

// wakeSQL only ever brings a deadline forward, never pushes it out, and never
// touches a live lease: a plan another worker is running keeps its lease and
// is re-examined when that run releases it.
const wakeSQL = `
UPDATE execution_plan_leases
   SET next_attempt_at = now()
 WHERE plan_id = $1::uuid AND next_attempt_at > now()`

// planRef is the part of an execution event payload the waker reads.
type planRef struct {
	PlanID  string `json:"plan_id"`
	OrderID string `json:"order_id"`
}

// Handle processes one delivered message. Unknown topics and events that
// name no plan are acknowledged without effect: a nudge is an optimisation,
// and refusing to acknowledge one would stall the partition for no gain.
func (w *Waker) Handle(ctx context.Context, m event.Message) error {
	env, err := m.Envelope()
	if err != nil {
		return errs.Wrap(err, errs.CodeValidationFailed, "execution-worker: undecodable message").WithField("topic", m.Topic)
	}
	if !isWakeTopic(event.Topic(env.Type)) {
		return nil
	}
	var ref planRef
	if err := json.Unmarshal(env.Payload, &ref); err != nil {
		return errs.Wrap(err, errs.CodeValidationFailed, "execution-worker: undecodable payload").WithField("event_id", env.ID)
	}

	source := w.Source
	if source == "" {
		source = WakeSource
	}
	var outcome event.Outcome
	err = w.db.InTx(ctx, db.TxOptions{MaxRetries: 3}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		outcome, err = w.inbox.ProcessHashed(ctx, tx, source, env.ID, env.SchemaVersion, event.HashPayload(env.Payload),
			func(ctx context.Context, tx pgx.Tx) error {
				planID, err := resolvePlan(ctx, tx, ref)
				if err != nil || planID == "" {
					return err
				}
				if _, err := tx.Exec(ctx, wakeSQL, planID); err != nil {
					return errs.Wrap(err, errs.CodeInternal, "execution-worker: wake plan").WithField("plan_id", planID)
				}
				return nil
			})
		return err
	})
	switch {
	case err == nil:
		observability.LoggerFrom(ctx).DebugContext(ctx, "execution-worker: wake",
			"event_id", env.ID, "type", env.Type, "outcome", outcome.String(), "plan_id", ref.PlanID)
		return nil
	case errs.HasCode(err, errs.CodeIdempotencyInProgress):
		// Another transaction is processing the same message right now. Let
		// the bus redeliver rather than dropping the nudge.
		return err
	default:
		if merr := w.inbox.MarkFailed(ctx, w.db, source, env.ID, env.SchemaVersion, err); merr != nil {
			w.log.WarnContext(ctx, "execution-worker: inbox mark failed", "error", merr, "event_id", env.ID)
		}
		return err
	}
}

func isWakeTopic(t event.Topic) bool {
	for _, x := range WakeTopics {
		if x == t {
			return true
		}
	}
	return false
}

// resolvePlan finds the plan the event is about: from the payload when it
// carries one, otherwise from the order it names. An empty result means the
// event is not about a plan this worker drives.
func resolvePlan(ctx context.Context, tx pgx.Tx, ref planRef) (string, error) {
	if ref.PlanID != "" {
		return ref.PlanID, nil
	}
	if ref.OrderID == "" {
		return "", nil
	}
	var planID string
	err := tx.QueryRow(ctx, `SELECT plan_id::text FROM orders WHERE id = $1::uuid`, ref.OrderID).Scan(&planID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", errs.Wrap(err, errs.CodeInternal, "execution-worker: resolve plan from order")
	}
	return planID, nil
}

// Subscribe attaches the waker to every wake topic on bus under group.
func (w *Waker) Subscribe(ctx context.Context, bus event.Bus, group string) error {
	for _, t := range WakeTopics {
		if err := bus.Subscribe(ctx, string(t), group, w.Handle); err != nil {
			return errs.Wrap(err, errs.CodeInternal, "execution-worker: subscribe").WithField("topic", string(t))
		}
	}
	return nil
}
