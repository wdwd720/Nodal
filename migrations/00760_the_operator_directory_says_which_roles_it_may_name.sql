-- +goose Up
-- The operator directory is the only source of operator authority, and it now
-- says which roles it may name.
--
-- `operator_roles` has been the answer to "who is an operator" since 00010:
-- `internal/identity.Complete` reads it on every login and nothing else decides
-- an actor type or a role (its package doc states that provider claims must
-- never do so). Two things about the table were left open.
--
-- ## 1. `role` was unconstrained text
--
-- A typo -- `ADMlN`, `Admin`, `OPERATOR` -- inserted cleanly and produced a
-- session carrying a role no permission matrix knows. `security.Principal.
-- Validate` refuses an unknown role, so the failure was fail-closed but
-- illegible: the operator simply could not log in, and the reason was in a
-- column nobody thought to look at. The CHECK moves the refusal to the INSERT,
-- where the mistake is.
--
-- `test/integration/enums` pairs this constraint with the Go list, so a role
-- added in one language and not the other fails a test rather than a login.
--
-- ## 2. BREAK_GLASS is not a standing role, and now cannot be written as one
--
-- `security.RoleBreakGlass` is time-boxed by construction: `Principal.Validate`
-- requires `BreakGlassUntil` alongside it, and the only production path that
-- grants it is an approved `BREAK_GLASS_GRANT` admin action (00153, two distinct
-- principals) executing `auth.Manager.Elevate`, which stamps an expiry on a live
-- session. A standing row here would name a role the login path cannot give an
-- expiry to, so it would lock the user out -- and if that were ever "fixed" by
-- defaulting an expiry, one INSERT would hand a single principal the entire
-- approve side of dual control.
--
-- The directory therefore names any role except that one. It is written as a
-- separate CHECK from the enum so the two facts stay legible: what the roles
-- are, and which of them is not granted this way.

ALTER TABLE operator_roles
    ADD CONSTRAINT operator_roles_role_check CHECK (role IN
        ('CUSTOMER','SUPPORT_READ_ONLY','OPERATIONS','RISK','COMPLIANCE','FINANCE','SECURITY','ADMIN'));

COMMENT ON TABLE operator_roles IS
    'The operator directory: the only source of operator authority in the system. internal/identity reads it at login and never takes a role from an identity-provider claim (ADR-0022). BREAK_GLASS is absent by CHECK because it is time-boxed and granted only through an approved admin action (00153, ADR-0024).';
COMMENT ON COLUMN operator_roles.granted_by IS
    'The user who granted this role, when a person did. NULL means the deployment declared it at boot through CP_AUTH_BOOTSTRAP_OPERATORS, and reason says so; there is no HTTP route that writes this table (D-052 is the same shape for gates).';

-- +goose Down
SELECT 1; -- protected: reverting would let the operator directory name a role no permission matrix knows, and would put BREAK_GLASS back within reach of a single INSERT
