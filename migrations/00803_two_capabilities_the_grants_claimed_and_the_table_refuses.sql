-- +goose Up
-- Two capabilities the grants claimed and the schema refuses (D-106, F-188, F-191).
--
-- ## 1. Who may read a person's notification centre
--
-- `notifications` holds what a customer was TOLD: a new sign-in, an account
-- restriction, a payout that failed. ADR-0021 §4 states the rule this applies --
-- "a new table holding personal data joins that list and gets its own REVOKE" --
-- and the bootstrap's blanket `ALTER DEFAULT PRIVILEGES ... GRANT SELECT ON
-- TABLES TO cp_readonly, cp_ops` had granted both roles SELECT without anybody
-- deciding it.
--
-- Neither has a use for it. `cp_readonly` is analytics and support reads, and
-- PART 121 says personal data does not travel to analytics; `cp_ops` runs
-- retention and housekeeping, and there is no retention pass over this table --
-- there cannot be, see below. What a copy of somebody's inbox offers either
-- role is nothing they need and one more place a leak can come from.
--
-- The DELETE grant goes with it, and that one was never real. 00640's
-- `notifications_guard` raises NOTIFICATION_IMMUTABLE on DELETE for EVERY role,
-- including the table's owner, so `GRANT DELETE ... TO cp_ops` named a
-- capability the database refuses -- and `privileges_test.go` listed the table
-- under `opsHousekeeping`, "tables whose retention is an operational policy",
-- asserting a retention capability that has never existed. A grant nobody can
-- exercise is not harmless: it is the shape of a control that reports success
-- having done nothing, which is the defect class this register names most.
--
-- Retention here is therefore a question about CONTENT, not about privilege:
-- what may be written into a table nothing can purge. The follower now writes a
-- /24 (or /48) locality and a two-word device summary instead of a login's
-- address and User-Agent header, and points at /v1/me/audit for the exact
-- address -- which is served from `security_events`, partitioned by month so a
-- month can be dropped (00740), which is what the retention pass does.
--
-- ## 2. The in-flight marker nothing could ever observe
--
-- 00783 gave `notification_follower_cursors` a `pending_at` column and said it
-- "is written BEFORE the notifications of a pass are committed and cleared
-- after, so a crash between reading and emitting leaves the earlier position on
-- record". Both statements are true of the code and the mechanism they describe
-- does not exist: `markPending` and `saveCursor` run in the SAME transaction as
-- the emits, so no session other than that transaction can ever see a non-NULL
-- `pending_at`, and a crash rolls the write back along with everything else.
-- The recovery it claims to provide is provided -- by the transaction, and by
-- the dedup key that makes a re-read free -- and the column adds nothing but a
-- sentence a reader would believe (F-191).
--
-- It is dropped rather than documented, because a column whose only reader is a
-- test asserting it is NULL is a fixture, and 00783's own argument ("the cursor
-- is an optimisation; the dedup key is the correctness argument") is the reason
-- nothing needs it.

REVOKE SELECT, DELETE ON notifications FROM cp_readonly, cp_ops;

COMMENT ON TABLE notifications IS
    'What a customer was told. Personal data: neither cp_readonly nor cp_ops may read it (ADR-0021 §4, D-106). Nothing can DELETE from it -- notifications_guard refuses every role -- so what may be written here is bounded by what may be kept forever.';

ALTER TABLE notification_follower_cursors DROP COLUMN pending_at;

-- +goose Down
SELECT 1; -- protected: reverting would re-grant two roles a copy of every customer's notification centre, and re-add a column whose documented mechanism the code does not implement
