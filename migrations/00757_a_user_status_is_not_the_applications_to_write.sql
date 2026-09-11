-- +goose Up
-- A user status is not the application's to write.
--
-- F-42's pattern, applied to the last identity-shaped table that still had a
-- state column the application could set with a bare UPDATE. `users.status`
-- (ACTIVE / SUSPENDED / CLOSED) has been in the schema since 00010 and nothing
-- has ever moved it: `accounts.CreateUser` writes ACTIVE and `SetEmailHash` is
-- the only UPDATE in the repository. It is being wired now because the product
-- surfaces are about to give it its first movers -- a self-service closure
-- request that an operator effects, and an operator suspension -- and the time
-- to bind a state column is before something writes it, not after.
--
-- The shape is 00744's, because `users` is `accounts`' twin:
--
--   * `user_status_transitions` is the only way the column moves. It is
--     append-only (forbid_mutation) and carries the actor, the reason and the
--     correlation id, so "who closed this account and why" is one row.
--   * `user_status_transitions_flag_edge` (AFTER INSERT, tagged per 00741) and
--     the DEFERRED constraint trigger `users_require_transition` bind the EDGE,
--     not just the destination: a row claiming ACTIVE -> CLOSED does not license
--     SUSPENDED -> CLOSED (F-94).
--   * `user_status_transitions_writes_the_status` writes the column from the
--     row. The trigger name sorts after `..._flag_edge`, which is load-bearing
--     under `SET CONSTRAINTS ALL IMMEDIATE`; 00744's header explains why.
--   * `cp_app` loses UPDATE on `users` and gets exactly one column back:
--     `email_hash`, which `accounts.SetEmailHash` writes when the identity
--     provider finally asserts a verified address. That grant is also what
--     permits `SELECT ... FOR UPDATE`, which 00744 discovered the hard way.
--
-- ## The legal edges, and the one that is missing on purpose
--
-- ACTIVE -> SUSPENDED, ACTIVE -> CLOSED, SUSPENDED -> ACTIVE, SUSPENDED ->
-- CLOSED. CLOSED is terminal: there is no edge out of it, here or in Go.
--
-- Reopening a closed account is not a state change, it is a decision about
-- whether a person who asked to leave may come back, and it would arrive with a
-- retention question attached (ADR-0020: retention on an append-only table is
-- partition detachment, never row deletion). Nothing in the product needs it
-- today, so the schema does not quietly permit it.

CREATE TABLE user_status_transitions (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    reason          text NOT NULL,
    correlation_id  text,
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX user_status_transitions_user_idx ON user_status_transitions (user_id, occurred_at);
CREATE TRIGGER user_status_transitions_immutable BEFORE UPDATE OR DELETE ON user_status_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TRIGGER user_status_transitions_flag_edge AFTER INSERT ON user_status_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('user_id', 'from_status', 'to_status', 'users');

-- +goose StatementBegin
CREATE FUNCTION cp_user_apply_status_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    UPDATE users SET status = NEW.to_status WHERE id = NEW.user_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'USER_TRANSITION_ORPHANED: user_status_transitions names user % which does not exist',
            NEW.user_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER user_status_transitions_writes_the_status
    AFTER INSERT ON user_status_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_user_apply_status_transition();

REVOKE EXECUTE ON FUNCTION cp_user_apply_status_transition() FROM PUBLIC;

CREATE CONSTRAINT TRIGGER users_require_transition AFTER UPDATE OF status ON users
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'users');

REVOKE UPDATE ON users FROM cp_app;
-- email_hash is the one column the application writes (00010's fill-once
-- assertion record), and the grant is also what permits the row lock the
-- transition takes. See 00744: SELECT ... FOR UPDATE requires UPDATE privilege.
GRANT UPDATE (email_hash) ON users TO cp_app;

COMMENT ON FUNCTION cp_user_apply_status_transition() IS
    'Writes users.status from the transition row. The application holds UPDATE on email_hash only, so inserting a user_status_transitions row is the only way a user status changes (00757, F-42).';
COMMENT ON TABLE user_status_transitions IS
    'Every movement of users.status, with the actor and the reason. Append-only; the edge is bound, so a row describes exactly the change it licenses.';

GRANT SELECT, INSERT ON user_status_transitions TO cp_app;
GRANT SELECT ON user_status_transitions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: reverting returns the status column to the application's reach
