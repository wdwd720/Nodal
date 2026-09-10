-- +goose Up
-- A security trail that can be bounded without being rewritten.
--
-- F-105's remaining half: `security_events` is bounded per minute and still
-- unprunable. The launch tier's database ceiling is 500 MB and the capacity
-- guard refuses every financial action at 67% of it, so on a table nothing can
-- remove a row from, any steady rate is a countdown rather than a steady state.
-- The failure arrives looking like a capacity refusal, not a retention failure.
--
-- ## The fact ADR-0020 got wrong, and it is the one the decision rests on
--
-- ADR-0020's classification table places `security_events` under OPERATIONAL_LOG
-- and records that it does NOT refuse DELETE -- putting it with `login_attempts`,
-- whose retention is a plain DELETE on a ticker (F-79).
--
-- It does refuse. Verified against the schema before writing this:
--
--     CREATE TRIGGER security_events_immutable BEFORE DELETE OR UPDATE
--       ON public.security_events FOR EACH ROW EXECUTE FUNCTION forbid_mutation()
--
-- and reproduced as the table OWNER, which is the strongest caller there is:
--
--     DELETE FROM security_events WHERE kind = 'TEST_PROBE';
--     ERROR:  immutable row: DELETE on public.security_events is forbidden
--
-- So decision 2 applies to this table and the `login_attempts` precedent does
-- not. An implementer following the ADR's table would have written a DELETE that
-- fails at runtime -- or, worse, reached for the trigger, which decision 1
-- forbids permanently. The ADR's table is corrected in the same commit as this
-- migration.
--
-- ## What this does
--
-- `security_events` becomes RANGE-partitioned by month on `occurred_at`.
-- Retention becomes partition detachment: DDL on a table, not DML on rows, so it
-- never passes through `forbid_mutation` and never needs it weakened. What was
-- dropped is a stated month, not a set somebody chose.
--
-- ADR-0020 decision 3 -- nothing is dropped that something else still proves --
-- is satisfied trivially here, and that was checked rather than assumed:
-- `security_events` has no inbound foreign keys, no outbound ones, and no
-- Merkle checkpoint or hash chain covers it (`internal/proof` and
-- `internal/audit` do not name it). It is written and never read back by
-- application code.
--
-- ## Two things this gives up, both stated rather than discovered later
--
-- 1. **`id` is no longer unique by itself.** PostgreSQL requires the partition
--    key in every unique index on a partitioned table, so the primary key
--    becomes `(id, occurred_at)`. Global uniqueness of `id` is now a property of
--    uuid generation rather than a database constraint. Nothing in this
--    repository reads a security event by id -- the table is write-only from the
--    application's side -- so this costs no query, but it is a weaker guarantee
--    than yesterday and it should not be discovered by someone relying on it.
--
-- 2. **"a durable security_events row that no role can ever delete" is no longer
--    exactly true.** `internal/httpapi/middleware.go` says so and it was right.
--    No role can delete a ROW; the owner can now drop a MONTH. That is the
--    trade the ceiling forces, and the comment is corrected in this commit.
--
-- ## Why the drop path is a function and not a grant
--
-- Detaching a partition needs ownership of the parent. The alternative to what
-- follows is granting `cp_ops` DDL on this table, which is a general power to
-- solve a specific problem, and ADR-0020 rejected exactly that shape for
-- DELETE.
--
-- Instead there are two SECURITY DEFINER functions, owned by the migration role,
-- each doing one thing, with EXECUTE granted to `cp_ops`:
--
--   cp_security_events_ensure_partitions(months_ahead) -- creates runway.
--       Not destructive. Safe to call on every pass.
--   cp_security_events_drop_expired(retain_days)       -- drops whole months
--       strictly older than the window, never the default partition, and
--       refuses a window below the floor.
--
-- The floor is 90 days and it is not configurable. A retention control that can
-- be pointed at yesterday is an erase-the-evidence control with a retention
-- control's name, and the caller of this function is a web service holding an
-- operations credential -- exactly the principal an attacker would want.
-- `search_path` is pinned on both, because a SECURITY DEFINER function that
-- resolves an unqualified name through the caller's path is a privilege
-- escalation waiting for a schema.
--
-- ## The default partition
--
-- There is one, and it is deliberate. Writers pass `occurred_at` explicitly from
-- a clock, so a skewed or test clock can produce a timestamp outside every
-- partition. Without a default that INSERT fails -- and the write that fails is
-- a security event, which is the worst possible thing to drop on the floor.
-- With one, it lands and is visible. `cp_security_events_drop_expired` never
-- touches it: rows there are of unknown age by definition.
--
-- Custom SQLSTATE: none. These raise plain exceptions; they are operator-facing
-- refusals, not application error paths.

-- +goose StatementBegin
DO $$
DECLARE
    lo   date;
    hi   date;
    cur  date;
    part text;
BEGIN
    -- The old table keeps its rows until the new one has them.
    ALTER TABLE security_events RENAME TO security_events_unpartitioned;
    ALTER INDEX security_events_pkey RENAME TO security_events_unpartitioned_pkey;
    ALTER INDEX security_events_occurred_idx RENAME TO security_events_unpartitioned_occurred_idx;
    ALTER INDEX security_events_user_idx RENAME TO security_events_unpartitioned_user_idx;
    ALTER TABLE security_events_unpartitioned
        RENAME CONSTRAINT security_events_severity_check TO security_events_unpartitioned_severity_check;

    CREATE TABLE security_events (
        id           uuid NOT NULL,
        kind         text NOT NULL,
        severity     text NOT NULL CHECK (severity IN ('INFO','WARN','HIGH','CRITICAL')),
        user_id      uuid,
        session_id   uuid,
        account_id   uuid,
        detail       jsonb NOT NULL DEFAULT '{}'::jsonb,
        ip           inet,
        user_agent   text,
        request_id   text,
        occurred_at  timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY (id, occurred_at)
    ) PARTITION BY RANGE (occurred_at);

    -- Cover every month that already has a row, and twelve ahead of today, so
    -- that a deployment which never calls the ensure function still writes into
    -- a real partition for a year rather than into the default.
    SELECT date_trunc('month', min(occurred_at))::date,
           date_trunc('month', max(occurred_at))::date
      INTO lo, hi
      FROM security_events_unpartitioned;

    lo := least(coalesce(lo, date_trunc('month', now())::date), date_trunc('month', now())::date);
    hi := greatest(coalesce(hi, date_trunc('month', now())::date),
                   (date_trunc('month', now()) + interval '12 months')::date);

    cur := lo;
    WHILE cur <= hi LOOP
        part := 'security_events_' || to_char(cur, 'YYYY_MM');
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF security_events FOR VALUES FROM (%L) TO (%L)',
            part, cur, (cur + interval '1 month')::date);
        cur := (cur + interval '1 month')::date;
    END LOOP;

    CREATE TABLE security_events_default PARTITION OF security_events DEFAULT;

    INSERT INTO security_events
        (id, kind, severity, user_id, session_id, account_id, detail, ip, user_agent, request_id, occurred_at)
    SELECT id, kind, severity, user_id, session_id, account_id, detail, ip, user_agent, request_id, occurred_at
      FROM security_events_unpartitioned;

    -- The old table refuses DELETE, which is the whole point; DROP is DDL and
    -- does not pass through the trigger.
    DROP TABLE security_events_unpartitioned;
END;
$$;
-- +goose StatementEnd

CREATE INDEX security_events_occurred_idx ON security_events (occurred_at);
CREATE INDEX security_events_user_idx ON security_events (user_id, occurred_at) WHERE user_id IS NOT NULL;

-- A row trigger on a partitioned parent is cloned to every partition, including
-- ones created after this statement runs. That is what keeps a partition made
-- next year as immutable as one made today.
CREATE TRIGGER security_events_immutable BEFORE UPDATE OR DELETE ON security_events
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Privileges are checked on the parent when the parent is what is named, which
-- is how every writer and reader in this system reaches it.
GRANT SELECT, INSERT ON security_events TO cp_app;
GRANT SELECT ON security_events TO cp_readonly, cp_ops;

COMMENT ON TABLE security_events IS
    'Security audit trail, RANGE-partitioned by month on occurred_at. Rows are immutable to every role including the owner; retention is partition detachment (ADR-0020 decision 2), never row deletion. Partitioned by 00740 (F-105).';

-- +goose StatementBegin
CREATE FUNCTION cp_security_events_ensure_partitions(months_ahead integer)
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    cur     date;
    stop    date;
    part    text;
    made    integer := 0;
BEGIN
    IF months_ahead IS NULL OR months_ahead < 1 OR months_ahead > 60 THEN
        RAISE EXCEPTION 'cp_security_events_ensure_partitions: months_ahead must be between 1 and 60, got %', months_ahead;
    END IF;

    cur  := date_trunc('month', now())::date;
    stop := (date_trunc('month', now()) + make_interval(months => months_ahead))::date;

    WHILE cur <= stop LOOP
        part := 'security_events_' || to_char(cur, 'YYYY_MM');
        IF to_regclass('public.' || quote_ident(part)) IS NULL THEN
            BEGIN
                EXECUTE format(
                    'CREATE TABLE %I PARTITION OF security_events FOR VALUES FROM (%L) TO (%L)',
                    part, cur, (cur + interval '1 month')::date);
                made := made + 1;
            EXCEPTION WHEN others THEN
                -- The one expected failure is the default partition already
                -- holding a row in this range, which blocks the attach. It is
                -- reported and skipped rather than aborting the remaining
                -- months: runway for eleven months is better than none.
                RAISE WARNING 'cp_security_events_ensure_partitions: could not create %: %', part, SQLERRM;
            END;
        END IF;
        cur := (cur + interval '1 month')::date;
    END LOOP;

    RETURN made;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_security_events_drop_expired(retain_days integer)
RETURNS TABLE (partition_name text, upper_bound date)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    floor_days constant integer := 90;
    cutoff     date;
    r          record;
BEGIN
    IF retain_days IS NULL OR retain_days < floor_days THEN
        RAISE EXCEPTION 'cp_security_events_drop_expired: retain_days must be at least % days, got %; a retention control that can be pointed at yesterday is not one',
            floor_days, retain_days;
    END IF;

    cutoff := (now() - make_interval(days => retain_days))::date;

    FOR r IN
        SELECT c.relname::text AS name,
               -- The upper bound is the second FROM/TO value in the bound
               -- expression. Reading it back from the catalogue rather than
               -- re-deriving it from the name means a partition whose name and
               -- range disagree is judged by its range.
               (regexp_match(pg_get_expr(c.relpartbound, c.oid),
                             'TO \(''([0-9-]+)'))[1]::date AS ub
          FROM pg_class c
          JOIN pg_inherits i ON i.inhrelid = c.oid
         WHERE i.inhparent = 'public.security_events'::regclass
           AND c.relname <> 'security_events_default'
           AND pg_get_expr(c.relpartbound, c.oid) NOT LIKE 'DEFAULT%'
    LOOP
        CONTINUE WHEN r.ub IS NULL OR r.ub > cutoff;
        EXECUTE format('ALTER TABLE security_events DETACH PARTITION %I', r.name);
        EXECUTE format('DROP TABLE %I', r.name);
        partition_name := r.name;
        upper_bound := r.ub;
        RETURN NEXT;
    END LOOP;
    RETURN;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION cp_security_events_ensure_partitions(integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION cp_security_events_drop_expired(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION cp_security_events_ensure_partitions(integer) TO cp_ops;
GRANT EXECUTE ON FUNCTION cp_security_events_drop_expired(integer) TO cp_ops;

COMMENT ON FUNCTION cp_security_events_ensure_partitions(integer) IS
    'Creates missing monthly partitions of security_events out to months_ahead. Not destructive; safe on every pass. SECURITY DEFINER so cp_ops needs no DDL grant (ADR-0020, F-105).';
COMMENT ON FUNCTION cp_security_events_drop_expired(integer) IS
    'Detaches and drops whole monthly partitions of security_events older than retain_days. Refuses below 90 days and never touches the default partition. SECURITY DEFINER so cp_ops needs no DDL grant (ADR-0020 decision 2, F-105).';

-- +goose Down
SELECT 1; -- protected: reverting returns security_events to a table nothing can prune, which is the finding
