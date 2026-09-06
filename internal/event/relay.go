package event

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
)

// Relay defaults, applied by NewRelay when the option is zero.
const (
	DefaultBatchSize        = 100
	DefaultPollInterval     = 250 * time.Millisecond
	DefaultMaxInterval      = 10 * time.Second
	DefaultJitterPercent    = 20
	DefaultRetryBackoffBase = time.Second
	DefaultRetryBackoffMax  = 5 * time.Minute
	DefaultRunTimeout       = 30 * time.Second

	// maxLastError bounds what is stored in outbox_events.last_error.
	maxLastError = 1024
)

// RelayOptions tunes a Relay. Zero values take the defaults above.
type RelayOptions struct {
	// BatchSize is the maximum number of rows claimed per RunOnce.
	BatchSize int
	// PollInterval is the sleep between runs when the outbox is drained.
	PollInterval time.Duration
	// MaxInterval caps the sleep grown by consecutive failing runs.
	MaxInterval time.Duration
	// JitterPercent spreads every sleep by ±percent so several relays do
	// not poll in lockstep.
	JitterPercent int
	// RetryBackoffBase is the per-row delay after the first failed publish;
	// it doubles per attempt up to RetryBackoffMax. The delay is measured
	// from the failure and persisted in outbox_events.next_attempt_at
	// (migration 00642), so a permanently failing row never hot-loops; the
	// run-level backoff above additionally protects against a bus outage.
	RetryBackoffBase time.Duration
	// RetryBackoffMax caps the per-row delay.
	RetryBackoffMax time.Duration
	// RunTimeout bounds one RunOnce inside Run.
	RunTimeout time.Duration
	// Clock supplies now for eligibility and lag; nil means the system clock.
	Clock clock.Clock
	// Observer receives metrics; nil means NopObserver.
	Observer Observer
}

func (o RelayOptions) withDefaults() RelayOptions {
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultBatchSize
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.MaxInterval <= 0 {
		o.MaxInterval = DefaultMaxInterval
	}
	if o.MaxInterval < o.PollInterval {
		o.MaxInterval = o.PollInterval
	}
	if o.JitterPercent < 0 {
		o.JitterPercent = 0
	}
	if o.JitterPercent == 0 {
		o.JitterPercent = DefaultJitterPercent
	}
	if o.JitterPercent > 100 {
		o.JitterPercent = 100
	}
	if o.RetryBackoffBase <= 0 {
		o.RetryBackoffBase = DefaultRetryBackoffBase
	}
	if o.RetryBackoffMax <= 0 {
		o.RetryBackoffMax = DefaultRetryBackoffMax
	}
	if o.RetryBackoffMax < o.RetryBackoffBase {
		o.RetryBackoffMax = o.RetryBackoffBase
	}
	if o.RunTimeout <= 0 {
		o.RunTimeout = DefaultRunTimeout
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Observer == nil {
		o.Observer = NopObserver{}
	}
	return o
}

// Relay moves committed outbox rows to the bus (PART 31, PART 117). See the
// package documentation for the delivery semantics.
type Relay struct {
	db   *db.DB
	bus  Bus
	log  *slog.Logger
	opts RelayOptions
}

// NewRelay builds a Relay. log may be nil (logs are discarded).
func NewRelay(database *db.DB, bus Bus, log *slog.Logger, opts RelayOptions) *Relay {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Relay{db: database, bus: bus, log: log, opts: opts.withDefaults()}
}

// Options returns the effective options after defaults.
func (r *Relay) Options() RelayOptions { return r.opts }

// Run polls until ctx is done and then returns nil. Each run is bounded by
// RunTimeout. The sleep between runs is PollInterval (with jitter) when the
// outbox is drained, near zero after a full batch, and grows exponentially
// up to MaxInterval while runs fail or publish nothing but failures.
func (r *Relay) Run(ctx context.Context) error {
	consecutiveFailures := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		rctx, cancel := context.WithTimeout(ctx, r.opts.RunTimeout)
		res, err := r.runOnce(rctx)
		cancel()

		var delay time.Duration
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil
			}
			consecutiveFailures++
			r.log.ErrorContext(ctx, "outbox relay run failed", "error", err, "consecutive_failures", consecutiveFailures)
			delay = r.failureDelay(consecutiveFailures)
		case res.failed > 0 && res.published == 0:
			consecutiveFailures++
			delay = r.failureDelay(consecutiveFailures)
		case res.claimed >= r.opts.BatchSize:
			consecutiveFailures = 0
			delay = 0
		default:
			consecutiveFailures = 0
			delay = r.opts.PollInterval
		}
		if err := sleepWithContext(ctx, r.jitter(delay)); err != nil {
			return nil
		}
	}
}

// failureDelay is PollInterval doubled per consecutive failure, capped at
// MaxInterval.
func (r *Relay) failureDelay(failures int) time.Duration {
	d := r.opts.PollInterval
	for i := 1; i < failures && d < r.opts.MaxInterval; i++ {
		d *= 2
	}
	if d > r.opts.MaxInterval {
		d = r.opts.MaxInterval
	}
	return d
}

// jitter spreads d by ±JitterPercent. A zero d yields a small random pause
// (up to JitterPercent of PollInterval) so back-to-back full batches still
// yield the connection between runs.
func (r *Relay) jitter(d time.Duration) time.Duration {
	if d <= 0 {
		span := r.opts.PollInterval * time.Duration(r.opts.JitterPercent) / 100
		if span <= 0 {
			return 0
		}
		return time.Duration(rand.Int64N(int64(span) + 1)) //nolint:gosec // G404: poll jitter is not security-sensitive
	}
	span := d * time.Duration(r.opts.JitterPercent) / 100
	if span <= 0 {
		return d
	}
	return d - span + time.Duration(rand.Int64N(int64(2*span)+1)) //nolint:gosec // G404: poll jitter is not security-sensitive
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RunOnce claims one batch of eligible unpublished rows, publishes them in
// (recorded_at, id) order and marks them published, all in one transaction.
// It returns the number of rows marked published and committed. On error
// nothing is marked (the transaction rolled back), but events handed to the
// bus before the error are not unpublished: they will be published again,
// which is the documented at-least-once behavior.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	res, err := r.runOnce(ctx)
	if err != nil {
		return 0, err
	}
	return res.published, nil
}

type runResult struct {
	claimed   int
	published int
	failed    int
	deferred  int
}

type claimedRow struct {
	env          Envelope
	idBytes      [16]byte
	topic        string
	partitionKey string
	attempts     int
}

type partition struct{ topic, key string }

const claimSQL = `
SELECT id, topic, partition_key, event_type, schema_version, source,
       aggregate_type, aggregate_id, correlation_id, causation_id,
       occurred_at, recorded_at, dedup_key, headers, payload, publish_attempts
FROM outbox_events
WHERE published_at IS NULL
  AND next_attempt_at <= $1
ORDER BY recorded_at, id
LIMIT $2
FOR UPDATE SKIP LOCKED`

// blockedSQL reports which CLAIMED ROWS still have an older unpublished row
// that this batch does not hold — locked by another relay, backed off, or
// otherwise deferred. Publishing such a row would reorder its partition.
//
// Two details carry the correctness of the whole thing.
//
// It is asked of EVERY row in the batch, not just the oldest row of each
// partition. `claim` uses FOR UPDATE SKIP LOCKED, so a batch is not a
// contiguous run of the outbox: it is the oldest rows nobody else holds. When
// another relay holds a row in the MIDDLE of a partition, the batch head is
// genuinely unblocked, and an answer computed only from that head would clear
// every later row of the partition to publish straight past the held one. A
// consumer would see an aggregate's 4th event before its 2nd.
//
// And older rows belonging to this same batch are excluded ($5). Claimed rows
// are still `published_at IS NULL` while the transaction runs, so without the
// exclusion every row would be blocked by its own predecessors and no
// partition could ever advance past one event per pass.
const blockedSQL = `
SELECT k.id
FROM unnest($1::text[], $2::text[], $3::timestamptz[], $4::uuid[]) AS k(topic, partition_key, recorded_at, id)
WHERE EXISTS (
    SELECT 1 FROM outbox_events e
    WHERE e.published_at IS NULL
      AND e.topic = k.topic
      AND e.partition_key = k.partition_key
      AND (e.recorded_at, e.id) < (k.recorded_at, k.id)
      AND NOT (e.id = ANY($5::uuid[])))`

const markPublishedSQL = `
UPDATE outbox_events SET published_at = $1, last_error = NULL WHERE id = ANY($2::uuid[])`

const markFailedSQL = `
UPDATE outbox_events SET publish_attempts = publish_attempts + 1, last_error = $2, next_attempt_at = $3 WHERE id = $1`

// retryDelay is the per-row delay after a failed publish; attempts is the
// number of failures before this one. It doubles from RetryBackoffBase and is
// capped at RetryBackoffMax without ever overflowing.
func (r *Relay) retryDelay(attempts int) time.Duration {
	d := r.opts.RetryBackoffBase
	for i := 0; i < attempts && d < r.opts.RetryBackoffMax; i++ {
		d *= 2
	}
	if d > r.opts.RetryBackoffMax {
		d = r.opts.RetryBackoffMax
	}
	return d
}

func (r *Relay) runOnce(ctx context.Context) (runResult, error) {
	var res runResult
	err := r.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		res = runResult{}
		now := r.opts.Clock.Now().UTC()
		rows, err := r.claim(ctx, tx, now)
		if err != nil {
			return err
		}
		res.claimed = len(rows)
		if len(rows) == 0 {
			return nil
		}
		blockedRow, err := r.blockedRows(ctx, tx, rows)
		if err != nil {
			return err
		}
		blocked := map[partition]bool{}

		var published [][16]byte
		type failure struct {
			id       [16]byte
			err      error
			attempts int // failures before this one
		}
		var failed []failure
		for _, row := range rows {
			p := partition{row.topic, row.partitionKey}
			// Once a row of this partition is held back, every later row of it
			// must wait too, or the partition reorders. Rows are sorted, so
			// carrying the flag forward is sufficient.
			if blocked[p] || blockedRow[row.idBytes] {
				blocked[p] = true
				res.deferred++
				continue
			}
			if err := r.publish(ctx, row, now); err != nil {
				failed = append(failed, failure{row.idBytes, err, row.attempts})
				blocked[p] = true // hold later rows of this partition back
				res.failed++
				r.opts.Observer.OnPublishFailed(row.topic, row.attempts+1, err)
				r.log.WarnContext(ctx, "outbox publish failed",
					"event_id", row.env.ID, "topic", row.topic, "attempts", row.attempts+1, "error", err)
				if ctx.Err() != nil {
					break
				}
				continue
			}
			published = append(published, row.idBytes)
			r.opts.Observer.OnPublished(row.topic, row.attempts+1, now.Sub(row.env.RecordedAt))
		}

		if len(published) > 0 {
			if _, err := tx.Exec(ctx, markPublishedSQL, now, published); err != nil {
				return fmt.Errorf("event: mark published: %w", err)
			}
		}
		for _, f := range failed {
			if _, err := tx.Exec(ctx, markFailedSQL, f.id, truncateError(f.err), now.Add(r.retryDelay(f.attempts))); err != nil {
				return fmt.Errorf("event: record publish failure: %w", err)
			}
		}
		res.published = len(published)
		return nil
	})
	if err != nil {
		return runResult{}, err
	}
	return res, nil
}

func (r *Relay) claim(ctx context.Context, tx pgx.Tx, now time.Time) ([]claimedRow, error) {
	rows, err := tx.Query(ctx, claimSQL, now, r.opts.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("event: claim outbox rows: %w", err)
	}
	defer rows.Close()

	var out []claimedRow
	for rows.Next() {
		var (
			c                                    claimedRow
			eid                                  [16]byte
			aggregateType, aggregateID           *string
			correlationID, causationID, dedupKey *string
			occurredAt, recordedAt               time.Time
			headers, payload                     []byte
		)
		if err := rows.Scan(&eid, &c.topic, &c.partitionKey, &c.env.Type, &c.env.SchemaVersion, &c.env.Source,
			&aggregateType, &aggregateID, &correlationID, &causationID,
			&occurredAt, &recordedAt, &dedupKey, &headers, &payload, &c.attempts); err != nil {
			return nil, fmt.Errorf("event: scan outbox row: %w", err)
		}
		id, err := idFromBytes(eid)
		if err != nil {
			return nil, fmt.Errorf("event: outbox row id: %w", err)
		}
		c.env.ID = id
		c.idBytes = eid
		c.env.AggregateType = deref(aggregateType)
		c.env.AggregateID = deref(aggregateID)
		c.env.CorrelationID = deref(correlationID)
		c.env.CausationID = deref(causationID)
		c.env.DedupKey = deref(dedupKey)
		c.env.OccurredAt = occurredAt.UTC()
		c.env.RecordedAt = recordedAt.UTC()
		c.env.Payload = payload
		if len(headers) > 0 {
			var h map[string]string
			if err := json.Unmarshal(headers, &h); err != nil {
				r.log.WarnContext(ctx, "outbox row headers are not a string map; ignored", "event_id", id, "error", err)
			} else if len(h) > 0 {
				c.env.Headers = h
			}
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("event: read outbox rows: %w", err)
	}
	return out, nil
}

// blockedRows returns the ids of claimed rows that must not publish in this
// pass because an older row of the same partition is unpublished and not held
// by this batch. The caller must also defer everything after a blocked row in
// the same partition; rows are sorted, so that is a single flag carried forward.
func (r *Relay) blockedRows(ctx context.Context, tx pgx.Tx, rows []claimedRow) (map[[16]byte]bool, error) {
	topics := make([]string, len(rows))
	keys := make([]string, len(rows))
	recorded := make([]time.Time, len(rows))
	ids := make([][16]byte, len(rows))
	for i, row := range rows {
		topics[i], keys[i] = row.topic, row.partitionKey
		recorded[i], ids[i] = row.env.RecordedAt, row.idBytes
	}
	res, err := tx.Query(ctx, blockedSQL, topics, keys, recorded, ids, ids)
	if err != nil {
		return nil, fmt.Errorf("event: check partition order: %w", err)
	}
	defer res.Close()
	blocked := map[[16]byte]bool{}
	for res.Next() {
		var id [16]byte
		if err := res.Scan(&id); err != nil {
			return nil, fmt.Errorf("event: scan blocked row: %w", err)
		}
		blocked[id] = true
	}
	if err := res.Err(); err != nil {
		return nil, fmt.Errorf("event: read blocked rows: %w", err)
	}
	return blocked, nil
}

func (r *Relay) publish(ctx context.Context, row claimedRow, now time.Time) error {
	value, err := row.env.CanonicalBytes()
	if err != nil {
		return fmt.Errorf("event: canonicalise stored envelope: %w", err)
	}
	return r.bus.Publish(ctx, row.topic, row.partitionKey, value, PublishHeaders(row.env))
}

// PublishHeaders builds the transport headers for an envelope: the
// producer's headers first, then the standard keys, which always win.
func PublishHeaders(e Envelope) map[string]string {
	h := make(map[string]string, len(e.Headers)+11)
	for k, v := range e.Headers {
		h[k] = v
	}
	h[HeaderEventID] = e.ID
	h[HeaderEventType] = e.Type
	h[HeaderSchemaVersion] = fmt.Sprint(e.SchemaVersion)
	h[HeaderSource] = e.Source
	h[HeaderAggregateType] = e.AggregateType
	h[HeaderAggregateID] = e.AggregateID
	h[HeaderOccurredAt] = formatTime(e.OccurredAt)
	h[HeaderContentType] = ContentTypeJSON
	if e.CorrelationID != "" {
		h[HeaderCorrelationID] = e.CorrelationID
	}
	if e.CausationID != "" {
		h[HeaderCausationID] = e.CausationID
	}
	if !e.RecordedAt.IsZero() {
		h[HeaderRecordedAt] = formatTime(e.RecordedAt)
	}
	return h
}

func idFromBytes(b [16]byte) (string, error) {
	eid, err := id.Bytes16[eventKind](b)
	if err != nil {
		return "", err
	}
	return eid.String(), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// truncateError renders err for last_error, bounded and never nil.
func truncateError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > maxLastError {
		s = s[:maxLastError]
	}
	return s
}
