-- +goose Up
-- The operator directory does not name CUSTOMER.
--
-- 00760 derived the directory's role list as "every declared role except
-- BREAK_GLASS", and `operatorroles.Directory()` computes exactly that
-- subtraction. Both therefore admit `CUSTOMER`, which is not an operator role at
-- all: it is what `internal/identity` gives a principal who is in no directory
-- (`roles = []security.Role{security.RoleCustomer}` on the else branch).
--
-- Declaring it is worse than useless. `identity.Complete` decides the actor type
-- from whether the directory returned anything, not from what it returned, so a
-- `CUSTOMER` row produces a session whose `ActorType` is OPERATOR carrying only
-- customer permissions. That person's own terms acceptance is then written with
-- `actor_type = 'OPERATOR'`, which 00759 documents as meaning "an acceptance
-- recorded on somebody's behalf by an operator" -- so the record of their
-- consent says somebody else gave it. Their own audit events say the same thing
-- (F-179).
--
-- BREAK_GLASS is excluded because granting it standing would be dangerous.
-- CUSTOMER is excluded for a different reason and the CHECK is written as its
-- own clause in 00760's spirit, so the two facts stay legible: BREAK_GLASS is a
-- role this table may not GRANT, and CUSTOMER is not a role this table names at
-- all, because a customer is what a principal is when this table says nothing
-- about them.
--
-- No row can exist to migrate: nothing in the product writes this table but the
-- bootstrap declaration, which now refuses CUSTOMER before it reaches SQL, and
-- the CHECK below would fail to validate if one did -- which is the right
-- outcome, because a deployment holding such a row has an operator session it
-- did not mean to issue.

ALTER TABLE operator_roles
    DROP CONSTRAINT operator_roles_role_check;
ALTER TABLE operator_roles
    ADD CONSTRAINT operator_roles_role_check CHECK (role IN
        ('SUPPORT_READ_ONLY', 'OPERATIONS', 'RISK', 'COMPLIANCE', 'FINANCE', 'SECURITY', 'ADMIN'));

COMMENT ON TABLE operator_roles IS
    'The operator directory: the only source of operator authority in the system. internal/identity reads it at login and never takes a role from an identity-provider claim (ADR-0022). BREAK_GLASS is absent by CHECK because it is time-boxed and granted only through an approved admin action (00153, ADR-0024); CUSTOMER is absent because it is what a principal this table says nothing about already is, and naming it here records that person''s own actions as an operator''s (00800, F-179).';

-- +goose Down
SELECT 1; -- protected: reverting would let the operator directory name CUSTOMER, which issues an OPERATOR session to somebody with no operator permissions and records their own consent as given on their behalf
