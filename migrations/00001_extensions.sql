-- +goose Up
-- Foundation: extensions and shared trigger functions.
-- pgcrypto: gen_random_uuid(), digest(). btree_gist: exclusion constraints mixing
-- equality and range operators (used later for reservations/envelopes).
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- set_updated_at: BEFORE UPDATE trigger function keeping updated_at current.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- forbid_mutation: BEFORE UPDATE OR DELETE trigger function for append-only
-- (posted ledger) rows. Any attempt raises; changes are made by compensating
-- transactions, never by mutation (PART 19).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'immutable row: % on %.% is forbidden', TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME
        USING ERRCODE = 'P0001';
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- Extensions are intentionally retained: other objects may depend on them and
-- dropping them is an operator decision, not a migration rollback.
DROP FUNCTION IF EXISTS forbid_mutation();
DROP FUNCTION IF EXISTS set_updated_at();
