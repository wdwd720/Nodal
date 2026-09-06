-- +goose Up
-- Identity, accounts, sessions, compliance profile skeleton.
-- PII is separated from financial state (PART 121); core financial tables reference users/accounts by id only.

CREATE TABLE users (
    id            uuid PRIMARY KEY,
    idp_issuer    text NOT NULL,
    idp_subject   text NOT NULL,
    email_hash    bytea,                                 -- sha256(lowercase email); lookup only
    status        text NOT NULL CHECK (status IN ('ACTIVE','SUSPENDED','CLOSED')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (idp_issuer, idp_subject)
);
CREATE INDEX users_email_hash_idx ON users (email_hash) WHERE email_hash IS NOT NULL;
CREATE TRIGGER users_updated_at BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE identity_pii (
    user_id               uuid PRIMARY KEY REFERENCES users(id),
    email_encrypted       bytea,
    legal_name_encrypted  bytea,
    dob_encrypted         bytea,
    country_code          text,
    region_code           text,
    key_version           integer NOT NULL DEFAULT 1,
    updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER identity_pii_updated_at BEFORE UPDATE ON identity_pii FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE accounts (
    id                 uuid PRIMARY KEY,
    owner_user_id      uuid NOT NULL REFERENCES users(id),
    kind               text NOT NULL CHECK (kind IN ('CUSTOMER','CANARY','PLATFORM')),
    status             text NOT NULL CHECK (status IN ('ACTIVE','RESTRICTED','FROZEN','CLOSED')),
    status_reason      text,
    frozen_at          timestamptz,
    cost_basis_method  text NOT NULL DEFAULT 'FIFO' CHECK (cost_basis_method IN ('FIFO')),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX accounts_owner_idx ON accounts (owner_user_id);
CREATE TRIGGER accounts_updated_at BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE account_status_transitions (
    id              uuid PRIMARY KEY,
    account_id      uuid NOT NULL REFERENCES accounts(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    reason          text NOT NULL,
    correlation_id  text,
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX account_status_transitions_account_idx ON account_status_transitions (account_id, occurred_at);
CREATE TRIGGER account_status_transitions_immutable BEFORE UPDATE OR DELETE ON account_status_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE compliance_profiles (
    user_id                uuid PRIMARY KEY REFERENCES users(id),
    identity_state         text NOT NULL CHECK (identity_state IN ('UNVERIFIED','PENDING','VERIFIED','REJECTED','EXPIRED')),
    age_verified           boolean NOT NULL DEFAULT false,
    jurisdiction_country   text,
    jurisdiction_region    text,
    residency_country      text,
    sanctions_state        text NOT NULL DEFAULT 'UNKNOWN' CHECK (sanctions_state IN ('UNKNOWN','CLEAR','HIT','REVIEW')),
    provider               text,
    provider_ref           text,
    policy_version         text,
    restrictions           jsonb NOT NULL DEFAULT '[]'::jsonb,
    verified_at            timestamptz,
    expires_at             timestamptz,
    updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER compliance_profiles_updated_at BEFORE UPDATE ON compliance_profiles FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE sessions (
    id             uuid PRIMARY KEY,
    token_hash     bytea NOT NULL UNIQUE,                -- sha256(opaque token); raw token never stored
    user_id        uuid NOT NULL REFERENCES users(id),
    actor_type     text NOT NULL CHECK (actor_type IN ('USER','OPERATOR')),
    roles          text[] NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_seen_at   timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL,
    auth_time      timestamptz NOT NULL,
    amr            text[] NOT NULL DEFAULT '{}',
    acr            text,
    ip             inet,
    user_agent     text,
    device_label   text,
    rotated_from   uuid,
    revoked_at     timestamptz,
    revoke_reason  text
);
CREATE INDEX sessions_user_active_idx ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

CREATE TABLE operator_roles (
    user_id     uuid NOT NULL REFERENCES users(id),
    role        text NOT NULL,
    granted_by  uuid REFERENCES users(id),
    granted_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz,
    revoked_at  timestamptz,
    reason      text NOT NULL,
    PRIMARY KEY (user_id, role)
);

CREATE TABLE security_events (
    id           uuid PRIMARY KEY,
    kind         text NOT NULL,
    severity     text NOT NULL CHECK (severity IN ('INFO','WARN','HIGH','CRITICAL')),
    user_id      uuid,
    session_id   uuid,
    account_id   uuid,
    detail       jsonb NOT NULL DEFAULT '{}'::jsonb,
    ip           inet,
    user_agent   text,
    request_id   text,
    occurred_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX security_events_occurred_idx ON security_events (occurred_at);
CREATE INDEX security_events_user_idx ON security_events (user_id, occurred_at) WHERE user_id IS NOT NULL;
CREATE TRIGGER security_events_immutable BEFORE UPDATE OR DELETE ON security_events
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT, UPDATE ON users, identity_pii, accounts, compliance_profiles, sessions, operator_roles TO cp_app;
GRANT SELECT, INSERT ON account_status_transitions, security_events TO cp_app;
GRANT SELECT ON users, accounts, compliance_profiles, account_status_transitions, security_events, operator_roles TO cp_readonly, cp_ops;

-- +goose Down
DROP TABLE IF EXISTS security_events;
DROP TABLE IF EXISTS operator_roles;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS compliance_profiles;
DROP TABLE IF EXISTS account_status_transitions;
DROP TABLE IF EXISTS accounts;
DROP TABLE IF EXISTS identity_pii;
DROP TABLE IF EXISTS users;
