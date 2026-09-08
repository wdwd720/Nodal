package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
)

// DefaultBlockedLimit bounds how many blocked partitions `status` lists.
const DefaultBlockedLimit = 20

// snapshotSQL is the single round trip behind both the `status` command and
// the gauge sampler. Every subquery reads the same `unpublished` CTE, which
// the partial index outbox_events_unpublished_idx (migration 00642) covers,
// so the cost is proportional to the backlog and not to the table.
//
// `heads` is the oldest unpublished row of each (topic, partition_key). It
// matters because the relay defers a partition whose oldest row is not
// publishable: a head that has failed or is waiting out its persisted
// next_attempt_at holds back everything behind it. That is the honest
// definition of "blocked", and it is the rule internal/event applies when it
// claims, read here from the other side.
const snapshotSQL = `
WITH unpublished AS (
    SELECT topic, partition_key, id, recorded_at, next_attempt_at, publish_attempts
    FROM outbox_events
    WHERE published_at IS NULL
), heads AS (
    SELECT DISTINCT ON (topic, partition_key) topic, partition_key, next_attempt_at, publish_attempts
    FROM unpublished
    ORDER BY topic, partition_key, recorded_at, id
)
SELECT
    (SELECT count(*) FROM unpublished),
    (SELECT count(*) FROM unpublished WHERE next_attempt_at <= $1),
    (SELECT count(*) FROM unpublished WHERE publish_attempts > 0),
    (SELECT coalesce(max(publish_attempts), 0) FROM unpublished),
    (SELECT min(recorded_at) FROM unpublished),
    (SELECT min(next_attempt_at) FROM unpublished),
    (SELECT count(*) FROM heads),
    (SELECT count(*) FROM heads WHERE publish_attempts > 0 OR next_attempt_at > $1)`

const topicStatusSQL = `
SELECT topic,
       count(*),
       count(*) FILTER (WHERE next_attempt_at <= $1),
       coalesce(max(publish_attempts), 0),
       min(recorded_at),
       (array_agg(last_error ORDER BY publish_attempts DESC, recorded_at) FILTER (WHERE last_error IS NOT NULL))[1]
FROM outbox_events
WHERE published_at IS NULL
GROUP BY topic
ORDER BY count(*) DESC, topic`

const blockedPartitionSQL = `
WITH unpublished AS (
    SELECT topic, partition_key, id, recorded_at, next_attempt_at, publish_attempts, last_error
    FROM outbox_events
    WHERE published_at IS NULL
), heads AS (
    SELECT DISTINCT ON (topic, partition_key)
           topic, partition_key, recorded_at, next_attempt_at, publish_attempts, last_error
    FROM unpublished
    ORDER BY topic, partition_key, recorded_at, id
)
SELECT h.topic, h.partition_key, h.recorded_at, h.next_attempt_at, h.publish_attempts, h.last_error,
       (SELECT count(*) FROM unpublished u WHERE u.topic = h.topic AND u.partition_key = h.partition_key)
FROM heads h
WHERE h.publish_attempts > 0 OR h.next_attempt_at > $1
ORDER BY h.recorded_at, h.topic, h.partition_key
LIMIT $2`

// Snapshot is the aggregate answer to "is the relay behind, and by how
// much". It is deliberately cheap enough to sample on a timer.
type Snapshot struct {
	// At is the instant the snapshot was taken (UTC).
	At time.Time `json:"at"`
	// Unpublished is the outbox depth: rows committed and not yet published.
	Unpublished int64 `json:"unpublished"`
	// Eligible is how many of those the relay may claim right now; the rest
	// are waiting out their persisted next_attempt_at.
	Eligible int64 `json:"eligible"`
	// Failing counts unpublished rows that have failed at least once.
	Failing int64 `json:"failing"`
	// MaxAttempts is the highest publish_attempts among unpublished rows.
	MaxAttempts int64 `json:"max_attempts"`
	// OldestRecordedAt is when the oldest unpublished row was written. Nil
	// when the outbox is drained.
	OldestRecordedAt *time.Time `json:"oldest_recorded_at,omitempty"`
	// NextAttemptAt is the earliest deadline among unpublished rows: when
	// nothing is eligible, this is when the relay next has real work.
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	// Partitions is the number of (topic, partition_key) pairs holding
	// unpublished rows.
	Partitions int64 `json:"partitions"`
	// BlockedPartitions counts partitions whose oldest unpublished row has
	// failed or is backed off; nothing behind such a head can publish
	// without breaking per-partition order.
	BlockedPartitions int64 `json:"blocked_partitions"`
}

// OldestAge is how far behind the relay is. It is zero when the outbox is
// drained, and never negative (a row recorded by a clock slightly ahead of
// this process reads as fresh, not as negative lag).
func (s Snapshot) OldestAge() time.Duration {
	if s.OldestRecordedAt == nil {
		return 0
	}
	return nonNegative(s.At.Sub(*s.OldestRecordedAt))
}

// TopicStatus is the per-topic breakdown.
type TopicStatus struct {
	Topic            string     `json:"topic"`
	Registered       bool       `json:"registered"`
	Unpublished      int64      `json:"unpublished"`
	Eligible         int64      `json:"eligible"`
	MaxAttempts      int64      `json:"max_attempts"`
	OldestRecordedAt *time.Time `json:"oldest_recorded_at,omitempty"`
	OldestAgeSeconds int64      `json:"oldest_age_seconds"`
	LastError        string     `json:"last_error,omitempty"`
}

// BlockedPartition is one stalled (topic, partition_key) and the head row
// holding it back.
type BlockedPartition struct {
	Topic            string    `json:"topic"`
	PartitionKey     string    `json:"partition_key"`
	Depth            int64     `json:"depth"`
	HeadRecordedAt   time.Time `json:"head_recorded_at"`
	HeadAgeSeconds   int64     `json:"head_age_seconds"`
	Attempts         int64     `json:"attempts"`
	NextAttemptAt    time.Time `json:"next_attempt_at"`
	RetryInSeconds   int64     `json:"retry_in_seconds"`
	LastError        string    `json:"last_error,omitempty"`
	UnregisteredHead bool      `json:"unregistered_topic,omitempty"`
}

// Status is the full report of the `status` command.
type Status struct {
	Snapshot          Snapshot           `json:"snapshot"`
	OldestAgeSeconds  int64              `json:"oldest_age_seconds"`
	Topics            []TopicStatus      `json:"topics"`
	Blocked           []BlockedPartition `json:"blocked"`
	UnregisteredTopic []string           `json:"unregistered_topics,omitempty"`
}

// TakeSnapshot reads the aggregate outbox state. It takes a Querier so it
// runs on the pool without a transaction, and it never writes.
func TakeSnapshot(ctx context.Context, q db.Querier, now time.Time) (Snapshot, error) {
	s := Snapshot{At: now.UTC()}
	err := q.QueryRow(ctx, snapshotSQL, s.At).Scan(
		&s.Unpublished, &s.Eligible, &s.Failing, &s.MaxAttempts,
		&s.OldestRecordedAt, &s.NextAttemptAt, &s.Partitions, &s.BlockedPartitions,
	)
	if err != nil {
		return Snapshot{}, fmt.Errorf("relay-worker: read outbox snapshot: %w", err)
	}
	s.OldestRecordedAt = utcPtr(s.OldestRecordedAt)
	s.NextAttemptAt = utcPtr(s.NextAttemptAt)
	return s, nil
}

// TakeStatus is TakeSnapshot plus the per-topic and per-partition detail an
// operator needs in order to act. blockedLimit bounds the partition list.
func TakeStatus(ctx context.Context, q db.Querier, now time.Time, blockedLimit int) (Status, error) {
	if blockedLimit <= 0 {
		blockedLimit = DefaultBlockedLimit
	}
	snap, err := TakeSnapshot(ctx, q, now)
	if err != nil {
		return Status{}, err
	}
	st := Status{Snapshot: snap, OldestAgeSeconds: int64(snap.OldestAge() / time.Second)}
	if st.Topics, err = topicStatus(ctx, q, snap.At); err != nil {
		return Status{}, err
	}
	for _, t := range st.Topics {
		if !t.Registered {
			st.UnregisteredTopic = append(st.UnregisteredTopic, t.Topic)
		}
	}
	sort.Strings(st.UnregisteredTopic)
	if st.Blocked, err = blockedPartitions(ctx, q, snap.At, blockedLimit); err != nil {
		return Status{}, err
	}
	return st, nil
}

func topicStatus(ctx context.Context, q db.Querier, now time.Time) ([]TopicStatus, error) {
	rows, err := q.Query(ctx, topicStatusSQL, now)
	if err != nil {
		return nil, fmt.Errorf("relay-worker: read per-topic outbox depth: %w", err)
	}
	defer rows.Close()
	var out []TopicStatus
	for rows.Next() {
		var (
			t         TopicStatus
			oldest    *time.Time
			lastError *string
		)
		if err := rows.Scan(&t.Topic, &t.Unpublished, &t.Eligible, &t.MaxAttempts, &oldest, &lastError); err != nil {
			return nil, fmt.Errorf("relay-worker: scan per-topic outbox depth: %w", err)
		}
		_, t.Registered = event.Lookup(event.Topic(t.Topic))
		t.OldestRecordedAt = utcPtr(oldest)
		if t.OldestRecordedAt != nil {
			t.OldestAgeSeconds = int64(nonNegative(now.Sub(*t.OldestRecordedAt)) / time.Second)
		}
		if lastError != nil {
			t.LastError = *lastError
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("relay-worker: read per-topic outbox depth: %w", err)
	}
	return out, nil
}

func blockedPartitions(ctx context.Context, q db.Querier, now time.Time, limit int) ([]BlockedPartition, error) {
	rows, err := q.Query(ctx, blockedPartitionSQL, now, limit)
	if err != nil {
		return nil, fmt.Errorf("relay-worker: read blocked partitions: %w", err)
	}
	defer rows.Close()
	var out []BlockedPartition
	for rows.Next() {
		var (
			b         BlockedPartition
			lastError *string
		)
		if err := rows.Scan(&b.Topic, &b.PartitionKey, &b.HeadRecordedAt, &b.NextAttemptAt,
			&b.Attempts, &lastError, &b.Depth); err != nil {
			return nil, fmt.Errorf("relay-worker: scan blocked partition: %w", err)
		}
		b.HeadRecordedAt = b.HeadRecordedAt.UTC()
		b.NextAttemptAt = b.NextAttemptAt.UTC()
		b.HeadAgeSeconds = int64(nonNegative(now.Sub(b.HeadRecordedAt)) / time.Second)
		b.RetryInSeconds = int64(nonNegative(b.NextAttemptAt.Sub(now)) / time.Second)
		if lastError != nil {
			b.LastError = *lastError
		}
		_, registered := event.Lookup(event.Topic(b.Topic))
		b.UnregisteredHead = !registered
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("relay-worker: read blocked partitions: %w", err)
	}
	return out, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func nonNegative(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

// WriteJSON prints the report as indented JSON.
func (s Status) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// WriteText prints the report for a human at 3 a.m. The first four lines
// answer "is the relay behind, and by how much" without any SQL.
func (s Status) WriteText(w io.Writer) error {
	snap := s.Snapshot
	fmt.Fprintf(w, "outbox depth          %d unpublished (%d eligible now, %d waiting on a retry deadline)\n",
		snap.Unpublished, snap.Eligible, snap.Unpublished-snap.Eligible)
	if snap.OldestRecordedAt == nil {
		fmt.Fprintln(w, "relay lag             0s (the outbox is drained)")
	} else {
		fmt.Fprintf(w, "relay lag             %s (oldest unpublished recorded_at %s)\n",
			snap.OldestAge().Round(time.Second), snap.OldestRecordedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(w, "partitions            %d with unpublished rows, %d blocked\n", snap.Partitions, snap.BlockedPartitions)
	fmt.Fprintf(w, "failing rows          %d (highest publish_attempts %d)\n", snap.Failing, snap.MaxAttempts)
	if snap.NextAttemptAt != nil && snap.Eligible == 0 && snap.Unpublished > 0 {
		fmt.Fprintf(w, "next retry due        %s (in %s)\n", snap.NextAttemptAt.Format(time.RFC3339),
			nonNegative(snap.NextAttemptAt.Sub(snap.At)).Round(time.Second))
	}
	fmt.Fprintf(w, "taken at              %s\n", snap.At.Format(time.RFC3339))

	if len(s.Topics) > 0 {
		fmt.Fprintln(w, "\nper topic")
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  TOPIC\tDEPTH\tELIGIBLE\tOLDEST\tATTEMPTS\tLAST ERROR")
		for _, t := range s.Topics {
			name := t.Topic
			if !t.Registered {
				name += " (UNREGISTERED)"
			}
			fmt.Fprintf(tw, "  %s\t%d\t%d\t%s\t%d\t%s\n", name, t.Unpublished, t.Eligible,
				time.Duration(t.OldestAgeSeconds)*time.Second, t.MaxAttempts, oneLine(t.LastError, 80))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(s.Blocked) > 0 {
		fmt.Fprintln(w, "\nblocked partitions (the oldest row of each has failed or is backed off; nothing behind it can publish)")
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  TOPIC\tPARTITION KEY\tDEPTH\tHEAD AGE\tATTEMPTS\tRETRY IN\tLAST ERROR")
		for _, b := range s.Blocked {
			fmt.Fprintf(tw, "  %s\t%s\t%d\t%s\t%d\t%s\t%s\n", b.Topic, b.PartitionKey, b.Depth,
				time.Duration(b.HeadAgeSeconds)*time.Second, b.Attempts,
				time.Duration(b.RetryInSeconds)*time.Second, oneLine(b.LastError, 80))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		if int64(len(s.Blocked)) < snap.BlockedPartitions {
			fmt.Fprintf(w, "  ... %d more blocked partitions not listed\n", snap.BlockedPartitions-int64(len(s.Blocked)))
		}
	}
	if len(s.UnregisteredTopic) > 0 {
		fmt.Fprintf(w, "\nWARNING unpublished rows name %d topic(s) that internal/event does not register: %s\n",
			len(s.UnregisteredTopic), strings.Join(s.UnregisteredTopic, ", "))
		fmt.Fprintln(w, "        the relay keeps trying to publish them and never drops them; an unregistered topic is a defect"+
			" in the producer or a topic renamed without updating the registry")
	}
	return nil
}

// oneLine flattens an error into a table cell of at most maxLen bytes.
func oneLine(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	if maxLen > 3 && len(s) > maxLen {
		s = s[:maxLen-3] + "..."
	}
	return s
}
