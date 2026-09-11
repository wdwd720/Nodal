-- +goose Up
-- A revoked operator role stays revoked, and every movement of a grant is a row.
--
-- `operator_roles` is the only source of operator authority in this system:
-- ADR-0022 put Nodal's authority in Neon, ADR-0024 made this table the one thing
-- the login path reads, and `internal/identity` takes a role from nothing else.
-- It is also the one authority-bearing table that never got the treatment 00744
-- gave `accounts`, 00757 gave `users` and 00758 gave `account_closure_requests`:
-- 00010's blanket `GRANT SELECT, INSERT, UPDATE` was still in force, so the
-- application role held UPDATE on every column of the directory (F-175).
--
-- Nothing in Go has ever updated it, so the grant served nothing and cost this:
--
--   * A revocation did not stay revoked. `UPDATE operator_roles SET revoked_at =
--     NULL` put a withdrawn grant back, which is the opposite of what ADR-0024
--     §3 promises -- and the promise was made about the BOOTSTRAP path only
--     ("ON CONFLICT DO NOTHING"), while the column itself was writable.
--   * A role could be rewritten in place. `SUPPORT_READ_ONLY` became `ADMIN`
--     while `granted_by`, `granted_at` and `reason` went on describing the grant
--     somebody actually made.
--   * Nothing recorded that any of it happened. There was no transition table
--     for the directory, so a promotion left no trace at all.
--
-- ## What this does
--
-- The F-42 shape, adapted to a table whose "state" is two nullable timestamps
-- rather than one text column:
--
--   * `operator_role_transitions` is append-only (`forbid_mutation`), carries
--     the actor, the reason and the correlation id, and names the grant it is
--     about by the directory's own primary key. `cp_transition_stamp_is_honest`
--     (00798) bounds its `occurred_at` the same way it bounds a closure's.
--   * `cp_operator_role_apply_transition` writes `revoked_at` / `expires_at`
--     from the row. It is SECURITY DEFINER, so the application inserts the
--     record and the database makes the change -- the two cannot come apart.
--   * `operator_roles_provenance_is_immutable` refuses any change to `user_id`,
--     `role`, `granted_by`, `granted_at` or `reason`, whoever makes it. A grant
--     is a fact about a decision somebody made; changing what it says is not an
--     edit, it is a different grant, and a different grant is a different row.
--   * `revoked_at` is monotonic and one-way: once set it cannot be cleared and
--     cannot be moved earlier. This is the invariant ADR-0024 §3 states, and it
--     now holds against every role, not only against the bootstrap path.
--     Restoring a revoked grant means INSERTing a new one, which carries its own
--     `granted_by` and its own reason -- which is the point.
--   * `expires_at` cannot be moved on a revoked grant at all.
--
-- ## Grants
--
-- `cp_app` loses UPDATE and keeps `UPDATE (reason)` -- and only because
-- PostgreSQL requires UPDATE privilege on at least one column for
-- `SELECT ... FOR UPDATE`, which 00744 discovered the hard way. The grant is
-- inert by construction: `reason` is one of the five columns the immutability
-- trigger refuses to let anybody change, so it buys a row lock and nothing else.
-- INSERT stays, because the bootstrap declaration (ADR-0024 §2) writes the first
-- row, and the transitions table gets SELECT and INSERT so a revocation can be
-- recorded by the application whenever a surface for it exists.
--
-- ## What this does not do
--
-- It does not add a route. ADR-0024 §7 says there is none and that is still
-- true; this migration builds the mechanism a route would use, so that when one
-- arrives it cannot be built as a bare UPDATE. Until then a deployment revokes
-- with the migration credential by inserting a transition row, which is one
-- statement and leaves the record behind it.

CREATE TABLE operator_role_transitions (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL,
    role            text NOT NULL,
    -- What this row does to the grant. REVOKE ends it; SET_EXPIRY time-boxes it.
    action          text NOT NULL,
    -- The expiry SET_EXPIRY writes. Meaningless for a REVOKE, and the CHECK
    -- below says so rather than leaving it to be read as "no expiry".
    expires_at      timestamptz,
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    reason          text NOT NULL,
    correlation_id  text,
    occurred_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT operator_role_transitions_action_check CHECK (action IN ('REVOKE', 'SET_EXPIRY')),
    CONSTRAINT operator_role_transitions_expiry_belongs_to_the_action
        CHECK ((action = 'SET_EXPIRY') = (expires_at IS NOT NULL)),
    CONSTRAINT operator_role_transitions_names_a_grant
        FOREIGN KEY (user_id, role) REFERENCES operator_roles (user_id, role)
);
CREATE INDEX operator_role_transitions_grant_idx
    ON operator_role_transitions (user_id, role, occurred_at);

CREATE TRIGGER operator_role_transitions_immutable
    BEFORE UPDATE OR DELETE ON operator_role_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TRIGGER operator_role_transitions_stamp_is_honest
    BEFORE INSERT ON operator_role_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_transition_stamp_is_honest();

-- +goose StatementBegin
CREATE FUNCTION cp_operator_role_apply_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    revoked timestamptz;
BEGIN
    SELECT revoked_at INTO revoked FROM operator_roles
     WHERE user_id = NEW.user_id AND role = NEW.role FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'OPERATOR_ROLE_TRANSITION_ORPHANED: operator_role_transitions names the % grant to %, which does not exist',
            NEW.role, NEW.user_id USING ERRCODE = 'AD001';
    END IF;
    IF NEW.action = 'REVOKE' THEN
        -- coalesce, not assignment: a second revocation of the same grant is
        -- idempotent and keeps the moment the FIRST one happened.
        UPDATE operator_roles
           SET revoked_at = coalesce(revoked_at, NEW.occurred_at)
         WHERE user_id = NEW.user_id AND role = NEW.role;
    ELSE
        IF revoked IS NOT NULL THEN
            RAISE EXCEPTION 'OPERATOR_ROLE_REVOKED: the % grant to % is revoked; an expiry cannot be set on it, and restoring it is a new grant with its own granted_by and reason',
                NEW.role, NEW.user_id USING ERRCODE = 'AD001';
        END IF;
        UPDATE operator_roles
           SET expires_at = NEW.expires_at
         WHERE user_id = NEW.user_id AND role = NEW.role;
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER operator_role_transitions_writes_the_grant
    AFTER INSERT ON operator_role_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_operator_role_apply_transition();

REVOKE EXECUTE ON FUNCTION cp_operator_role_apply_transition() FROM PUBLIC;

-- +goose StatementBegin
CREATE FUNCTION cp_operator_role_provenance_is_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.user_id IS DISTINCT FROM OLD.user_id
       OR NEW.role IS DISTINCT FROM OLD.role
       OR NEW.granted_by IS DISTINCT FROM OLD.granted_by
       OR NEW.granted_at IS DISTINCT FROM OLD.granted_at
       OR NEW.reason IS DISTINCT FROM OLD.reason THEN
        RAISE EXCEPTION 'OPERATOR_ROLE_PROVENANCE: who was granted what, by whom, when and why is fixed at the INSERT; a different answer is a different grant'
            USING ERRCODE = 'AD001';
    END IF;
    IF OLD.revoked_at IS NOT NULL AND (NEW.revoked_at IS NULL OR NEW.revoked_at > OLD.revoked_at) THEN
        RAISE EXCEPTION 'OPERATOR_ROLE_REVOKED: a revoked grant stays revoked (ADR-0024); restoring one is a new INSERT carrying its own granted_by and reason'
            USING ERRCODE = 'AD001';
    END IF;
    IF OLD.revoked_at IS NOT NULL AND NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
        RAISE EXCEPTION 'OPERATOR_ROLE_REVOKED: the expiry of a revoked grant is history and does not move'
            USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER operator_roles_provenance_is_immutable
    BEFORE UPDATE ON operator_roles
    FOR EACH ROW EXECUTE FUNCTION cp_operator_role_provenance_is_immutable();

REVOKE EXECUTE ON FUNCTION cp_operator_role_provenance_is_immutable() FROM PUBLIC;

REVOKE UPDATE ON operator_roles FROM cp_app;
-- The row lock, and nothing else: PostgreSQL requires UPDATE on at least one
-- column for SELECT ... FOR UPDATE (00744), and `reason` is a column the
-- trigger above refuses to let anybody change.
GRANT UPDATE (reason) ON operator_roles TO cp_app;

GRANT SELECT, INSERT ON operator_role_transitions TO cp_app;
GRANT SELECT ON operator_role_transitions TO cp_readonly, cp_ops;

COMMENT ON TABLE operator_role_transitions IS
    'Every movement of an operator grant after it was made: its revocation, and any expiry set on it, with the actor and the reason. Append-only. cp_operator_role_apply_transition writes the directory from these rows, so the record and the change cannot come apart (00799, F-175).';
COMMENT ON FUNCTION cp_operator_role_apply_transition() IS
    'Writes operator_roles.revoked_at and expires_at from the transition row. The application holds no UPDATE on the directory, so inserting a row here is the only way a grant moves (00799, F-42).';
COMMENT ON FUNCTION cp_operator_role_provenance_is_immutable() IS
    'Refuses any change to who was granted what, by whom, when and why, and refuses un-revoking a grant or moving a revoked grant''s expiry. ADR-0024 §3 states the revocation rule; this is where it holds.';

-- +goose Down
SELECT 1; -- protected: reverting would put the only source of operator authority back inside the application role''s UPDATE reach, where a revocation does not stay revoked
