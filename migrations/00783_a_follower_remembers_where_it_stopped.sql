-- +goose Up
-- Where the in-process notification follower stopped reading each source (D-069).
--
-- The follower reads rows that already exist -- credit funding transitions, payout request
-- transitions, native market fills and transitions, account status transitions, security events --
-- and turns the ones a person needs to know about into notifications. It is a reader, so it owns no
-- state except its own position, and this table is that position.
--
-- The position is (last_at, last_id) rather than a sequence, because none of the tables it follows
-- has a monotonic counter: each has a timestamp defaulting to now() and a UUIDv7 primary key. A
-- timestamp cursor alone can skip a row -- occurred_at is the transaction's start time, so a long
-- transaction commits a row BEHIND a cursor that has already passed it -- and no amount of ordering
-- fixes that. So the cursor is not the safety property. Every notification carries a dedup_key
-- derived from the source row's own id, unique per (user_id, dedup_key), and the follower re-reads a
-- bounded lap behind its cursor on every pass. Late rows are picked up by the lap; rows the lap sees
-- twice are refused by the unique index. The cursor is an optimisation; the dedup key is the
-- correctness argument.
--
-- `pending_at` is written BEFORE the notifications of a pass are committed and cleared after, so a
-- crash between reading and emitting leaves the earlier position on record and the next pass re-reads
-- the window rather than assuming it was handled.

CREATE TABLE notification_follower_cursors (
    source       text PRIMARY KEY,
    last_at      timestamptz NOT NULL,
    last_id      uuid NOT NULL,
    pending_at   timestamptz,
    emitted      bigint NOT NULL DEFAULT 0 CHECK (emitted >= 0),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER notification_follower_cursors_updated_at BEFORE UPDATE ON notification_follower_cursors
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

GRANT SELECT, INSERT, UPDATE ON notification_follower_cursors TO cp_app;
GRANT SELECT ON notification_follower_cursors TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: losing the follower cursors would make the next pass re-read from the instant it restarted, silently skipping every row written before it
