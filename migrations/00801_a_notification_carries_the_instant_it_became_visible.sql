-- +goose Up
-- A notification carries the instant it became visible, and the resume cursor reads that one (D-104).
--
-- Three clocks described one notification and the product treated them as one:
--
--   1. `created_at` is the OCCURRENCE instant. Producer.Emit binds it to
--      Notification.OccurredAt -- the source row's own time -- so a capture that
--      happened five minutes before the follower read it is written now and
--      stamped then. It is the right value for "when did this happen" and it is
--      the wrong value for "what have I not been told yet".
--   2. The id a LIVE stream event carries is assigned by Hub.Publish from the
--      hub's clock: the instant the follower pass published it, which is one
--      tick plus however long the row waited, later.
--   3. `notifications.Since` -- the durable half of Last-Event-ID resume --
--      answered with `created_at > since`, comparing (3) against (1).
--
-- So a client whose last live event was published at P recorded E(P) and asked
-- for everything after P, and every notification whose occurrence sat at or
-- before P but which was WRITTEN after it was silently skipped. That is every
-- row the two-minute lap picks up and every row from the tick the client was
-- disconnected for: precisely the rows a resume exists to deliver (F-186).
--
-- `inserted_at` is the missing clock: when this row became a fact. It is
-- clock_timestamp() and not now(), deliberately -- now() is the transaction's
-- start, so a follower pass writing two hundred notifications would stamp all
-- two hundred identically and a cursor could not order them; clock_timestamp()
-- is the statement's own instant and rises within the transaction. It is still
-- assigned before COMMIT, so it does not order two concurrent writers by
-- visibility; nothing here claims it does. The in-memory replay buffer and the
-- resync that follows an empty one are what cover that window, and the dedup
-- key is what makes a re-read free.
--
-- Existing rows are backfilled from created_at. For every row this schema has
-- ever held the two are the same instant -- the product's only writer is the
-- follower, which has run in no deployment yet -- so the backfill invents
-- nothing, and a row whose occurrence really did precede its insertion cannot
-- be told apart after the fact anyway.

ALTER TABLE notifications ADD COLUMN inserted_at timestamptz;
UPDATE notifications SET inserted_at = created_at WHERE inserted_at IS NULL;
ALTER TABLE notifications ALTER COLUMN inserted_at SET DEFAULT clock_timestamp();
ALTER TABLE notifications ALTER COLUMN inserted_at SET NOT NULL;

-- The resume query's whole shape: one person, everything written since an
-- instant, oldest first, with the id breaking the tie.
CREATE INDEX notifications_user_inserted_idx ON notifications (user_id, inserted_at, id);

COMMENT ON COLUMN notifications.inserted_at IS
    'When this row became a fact, from clock_timestamp(). created_at is when the thing it describes happened; a Last-Event-ID resume filters on THIS column, because a client is asking what it has not been told (D-104).';

-- The guard grows by one column. A notification's content cannot change, and
-- when it was written is content: a row whose inserted_at could be edited is a
-- row that could be moved out of, or into, somebody's resume window.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION notifications_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'NOTIFICATION_IMMUTABLE: notifications are never deleted by the application' USING ERRCODE = 'LG003';
    END IF;
    IF NEW.user_id IS DISTINCT FROM OLD.user_id OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.title IS DISTINCT FROM OLD.title
       OR NEW.body IS DISTINCT FROM OLD.body OR NEW.data IS DISTINCT FROM OLD.data OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.inserted_at IS DISTINCT FROM OLD.inserted_at
       OR NEW.account_id IS DISTINCT FROM OLD.account_id OR NEW.dedup_key IS DISTINCT FROM OLD.dedup_key
       OR NEW.severity IS DISTINCT FROM OLD.severity OR NEW.sandbox IS DISTINCT FROM OLD.sandbox
       OR NEW.resource_type IS DISTINCT FROM OLD.resource_type OR NEW.resource_id IS DISTINCT FROM OLD.resource_id THEN
        RAISE EXCEPTION 'NOTIFICATION_IMMUTABLE: notification content cannot change' USING ERRCODE = 'LG003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
SELECT 1; -- protected: dropping the column would return every reconnecting client to a resume filtered on the occurrence clock, which silently skips whatever was written behind it
