-- +goose Up
-- Persist the relay's per-row retry deadline (PART 31). Before this, backoff was derived from
-- recorded_at, so a row that kept failing became eligible on every poll once the cap had elapsed.
-- next_attempt_at is set by the relay on each failure to failure_time + backoff(attempts).
ALTER TABLE outbox_events ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now();
DROP INDEX IF EXISTS outbox_events_unpublished_idx;
CREATE INDEX outbox_events_unpublished_idx ON outbox_events (next_attempt_at, recorded_at, id) WHERE published_at IS NULL;
-- Per-partition ordering guard used by the relay's blocked-partition check.
CREATE INDEX outbox_events_unpublished_partition_idx ON outbox_events (topic, partition_key, recorded_at, id) WHERE published_at IS NULL;

-- +goose Down
SELECT 1; -- protected: outbox rows are financial event evidence; the column stays
