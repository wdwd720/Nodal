package event

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// Outbox writes domain events into outbox_events inside the caller's
// transaction (PART 31). It never publishes.
type Outbox struct {
	clk clock.Clock
}

// NewOutbox returns an Outbox stamping recorded_at from clk when the
// envelope leaves it zero. A nil clk means the system clock.
func NewOutbox(clk clock.Clock) *Outbox {
	if clk == nil {
		clk = clock.System()
	}
	return &Outbox{clk: clk}
}

const insertOutboxSQL = `
INSERT INTO outbox_events (
    id, topic, partition_key, event_type, schema_version, source,
    aggregate_type, aggregate_id, correlation_id, causation_id,
    occurred_at, recorded_at, dedup_key, headers, payload, next_attempt_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $12)`

// Constraint names from migrations/00002_outbox_inbox.sql.
const (
	constraintOutboxPK    = "outbox_events_pkey"
	constraintOutboxDedup = "outbox_events_topic_dedup_key_uidx"
)

// Enqueue validates every envelope against the topic registry and inserts
// one row per envelope in tx. Nothing is inserted when any envelope is
// invalid. A duplicate dedup_key within the topic (or a duplicate event id)
// is a CONFLICT *errs.Error, which the caller must let roll its transaction
// back: an event that cannot be recorded means the financial change it
// describes must not commit either.
//
// Database errors are wrapped with errs.Wrap so SQLSTATE classification
// (db.IsSerializationFailure and friends) keeps working through the chain.
func (o *Outbox) Enqueue(ctx context.Context, tx pgx.Tx, topic string, events ...Envelope) error {
	if len(events) == 0 {
		return nil
	}
	sp, ok := Lookup(Topic(topic))
	if !ok {
		return errs.Wrap(ErrUnknownTopic, errs.CodeValidationFailed, "event: unknown topic").WithField("topic", topic)
	}
	rows := make([]outboxRow, 0, len(events))
	for i, ev := range events {
		row, err := o.prepare(sp, ev)
		if err != nil {
			if e, ok := errs.As(err); ok {
				return e.WithField("index", i)
			}
			return err
		}
		rows = append(rows, row)
	}
	if tx == nil {
		return errs.New(errs.CodeInternal, "event: nil transaction")
	}
	for _, r := range rows {
		if _, err := tx.Exec(ctx, insertOutboxSQL,
			r.id, r.topic, r.partitionKey, r.eventType, r.schemaVersion, r.source,
			r.aggregateType, r.aggregateID, nullable(r.correlationID), nullable(r.causationID),
			r.occurredAt, r.recordedAt, nullable(r.dedupKey), r.headers, r.payload,
		); err != nil {
			return classifyInsertError(err, r)
		}
	}
	return nil
}

type outboxRow struct {
	id            string
	topic         string
	partitionKey  string
	eventType     string
	schemaVersion int
	source        string
	aggregateType string
	aggregateID   string
	correlationID string
	causationID   string
	occurredAt    time.Time
	recordedAt    time.Time
	dedupKey      string
	headers       []byte
	payload       []byte
}

func (o *Outbox) prepare(sp Spec, ev Envelope) (outboxRow, error) {
	if ev.RecordedAt.IsZero() {
		ev.RecordedAt = o.clk.Now().UTC()
	}
	if err := ev.Validate(); err != nil {
		return outboxRow{}, err
	}
	if !sp.Accepts(ev.Type) {
		return outboxRow{}, errs.New(errs.CodeValidationFailed, "event: type not accepted on topic").
			WithField("topic", string(sp.Topic)).WithField("type", ev.Type)
	}
	if ev.SchemaVersion != sp.SchemaVersion {
		return outboxRow{}, errs.New(errs.CodeValidationFailed, "event: schema_version does not match topic registry").
			WithField("topic", string(sp.Topic)).
			WithField("schema_version", ev.SchemaVersion).
			WithField("expected", sp.SchemaVersion)
	}
	key, err := sp.Key(ev)
	if err != nil {
		return outboxRow{}, errs.Wrap(err, errs.CodeValidationFailed, "event: cannot derive partition key").
			WithField("topic", string(sp.Topic))
	}
	payload, err := ev.CanonicalPayload()
	if err != nil {
		return outboxRow{}, errs.Wrap(err, errs.CodeValidationFailed, "event: payload is not canonicalisable")
	}
	headers, err := json.Marshal(headersOrEmpty(ev.Headers))
	if err != nil {
		return outboxRow{}, errs.Wrap(err, errs.CodeValidationFailed, "event: headers are not encodable")
	}
	return outboxRow{
		id:            ev.ID,
		topic:         string(sp.Topic),
		partitionKey:  key,
		eventType:     ev.Type,
		schemaVersion: ev.SchemaVersion,
		source:        ev.Source,
		aggregateType: ev.AggregateType,
		aggregateID:   ev.AggregateID,
		correlationID: ev.CorrelationID,
		causationID:   ev.CausationID,
		occurredAt:    ev.OccurredAt,
		recordedAt:    ev.RecordedAt,
		dedupKey:      ev.DedupKey,
		headers:       headers,
		payload:       payload,
	}, nil
}

// nullable maps "" to SQL NULL for optional text columns.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func classifyInsertError(err error, r outboxRow) error {
	if db.IsUniqueViolation(err) {
		switch db.ConstraintName(err) {
		case constraintOutboxDedup:
			return errs.Wrap(err, errs.CodeConflict, "event: duplicate dedup_key on topic").
				WithField("topic", r.topic).WithField("dedup_key", r.dedupKey)
		case constraintOutboxPK:
			return errs.Wrap(err, errs.CodeConflict, "event: duplicate event id").
				WithField("id", r.id)
		}
		return errs.Wrap(err, errs.CodeConflict, "event: outbox uniqueness violation")
	}
	return errs.Wrap(err, errs.CodeInternal, fmt.Sprintf("event: outbox insert on %s failed", r.topic))
}
