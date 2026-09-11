-- +goose Up
-- A quote is what the provider said before anybody committed.
--
-- Goal §19 and §22 want a withdrawal the customer can SEE before they ask for
-- it: how many Credits leave, what the provider charges, what actually arrives,
-- and by when the answer stops being true. PROVIDER_BOUNDARY §3 makes the same
-- point from the provider's side -- `QuotePayout` is a separate pre-commitment
-- call, and `minimum_ok` is judged net of fees, because sub-minimum dust is
-- destroyed rather than returned.
--
-- Without a stored quote the fee is computed twice: once for the screen and
-- once for the request, by two pieces of code that will eventually disagree,
-- and the one the customer saw is the one nobody kept.
--
-- ## Not a state machine, deliberately
--
-- A quote has no status column and needs none. It is created, it expires at a
-- time written when it was created, and it is consumed at most once. Those are
-- three facts about time, not three states, and giving them a state column
-- would mean a transition table, an edge binding and a trigger to express
-- "later than a timestamp". `consumed_at` is the only mutable column, it moves
-- once, and a trigger refuses to un-consume it or to change anything else.
--
-- ## What it stores, and what it refuses to store
--
-- Credits on one side, minor units of the payout currency on the other, both as
-- exact integers. There is no rate column holding a decimal: the Credit price
-- is a pricing-policy VERSION, recorded here, and the arithmetic is redone from
-- that version if anybody needs to check it. A float in this table would be the
-- first float in the system's money.
--
-- `fee_model_version` names where the fee came from. Every provider adapter
-- reports its own fee model in `Capabilities`, and an adapter that has not been
-- read against a real contract reports that it does not know -- in which case
-- there is no quote, rather than a quote of zero. A zero fee that means
-- "unknown" is how a customer is promised a net amount nobody agreed to.
--
-- `sandbox` and `environment` carry the same rule as 00762: a rehearsal quote
-- cannot exist where real value moves, and a CHECK says so rather than a
-- comment.
--
-- ## payout_requests.quote_id
--
-- The request names the quote it was created under, so "what was this customer
-- shown" is answerable from the row rather than from a log. It is written at
-- INSERT and never updated, which is why no UPDATE privilege is granted for it.
--
-- Custom SQLSTATEs: PQ001 quote immutability.

CREATE TABLE payout_quotes (
    id                 uuid PRIMARY KEY,
    account_id         uuid NOT NULL REFERENCES accounts(id),
    destination_id     uuid NOT NULL REFERENCES payout_destinations(id),
    provider           text NOT NULL,

    -- The Credit side, in base units.
    gross_quantity     numeric(38,0) NOT NULL CHECK (gross_quantity > 0),
    fee_quantity       numeric(38,0) NOT NULL CHECK (fee_quantity >= 0),
    net_quantity       numeric(38,0) NOT NULL CHECK (net_quantity >= 0),

    -- The money side, in minor units of `currency`.
    currency           text NOT NULL,
    gross_amount_minor bigint NOT NULL CHECK (gross_amount_minor >= 0),
    fee_amount_minor   bigint NOT NULL CHECK (fee_amount_minor >= 0),
    net_amount_minor   bigint NOT NULL CHECK (net_amount_minor >= 0),

    -- Which rules produced this answer. Three versions rather than one, because
    -- three independent things can change underneath a quote.
    pricing_version    text NOT NULL,
    fee_model_version  text NOT NULL,
    policy_version     text NOT NULL,

    -- Judged net of fees (PROVIDER_BOUNDARY §3). False means the provider will
    -- not send this much, and the quote records the refusal rather than
    -- rounding it away.
    minimum_ok         boolean NOT NULL,
    minimum_amount_minor bigint NOT NULL DEFAULT 0 CHECK (minimum_amount_minor >= 0),

    environment        text NOT NULL CHECK (environment IN ('LOCAL','TEST','DEV','STAGING','PROD')),
    sandbox            boolean NOT NULL DEFAULT false,

    -- Scoped by account rather than globally unique, which is the strongest
    -- form of the control test/integration/migrations asks for: a caller
    -- reusing another account's key cannot collide with their row at all,
    -- rather than colliding and being refused by a comparison in Go.
    -- trade_intents does the same.
    idempotency_key    text NOT NULL,
    expires_at         timestamptz NOT NULL,
    consumed_at        timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),

    UNIQUE (account_id, idempotency_key),
    CHECK (net_quantity + fee_quantity = gross_quantity),
    CHECK (net_amount_minor + fee_amount_minor = gross_amount_minor),
    CHECK (expires_at > created_at),
    CONSTRAINT payout_quotes_sandbox_never_in_prod CHECK (NOT sandbox OR environment <> 'PROD')
);
CREATE INDEX payout_quotes_account_idx ON payout_quotes (account_id, created_at DESC);

-- +goose StatementBegin
CREATE FUNCTION cp_payout_quote_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'PAYOUT_QUOTE_IMMUTABLE: a quote is what the customer was shown and is never deleted'
            USING ERRCODE = 'PQ001';
    END IF;
    IF NEW.account_id         IS DISTINCT FROM OLD.account_id
    OR NEW.destination_id     IS DISTINCT FROM OLD.destination_id
    OR NEW.provider           IS DISTINCT FROM OLD.provider
    OR NEW.gross_quantity     IS DISTINCT FROM OLD.gross_quantity
    OR NEW.fee_quantity       IS DISTINCT FROM OLD.fee_quantity
    OR NEW.net_quantity       IS DISTINCT FROM OLD.net_quantity
    OR NEW.currency           IS DISTINCT FROM OLD.currency
    OR NEW.gross_amount_minor IS DISTINCT FROM OLD.gross_amount_minor
    OR NEW.fee_amount_minor   IS DISTINCT FROM OLD.fee_amount_minor
    OR NEW.net_amount_minor   IS DISTINCT FROM OLD.net_amount_minor
    OR NEW.pricing_version    IS DISTINCT FROM OLD.pricing_version
    OR NEW.fee_model_version  IS DISTINCT FROM OLD.fee_model_version
    OR NEW.policy_version     IS DISTINCT FROM OLD.policy_version
    OR NEW.minimum_ok         IS DISTINCT FROM OLD.minimum_ok
    OR NEW.environment        IS DISTINCT FROM OLD.environment
    OR NEW.sandbox            IS DISTINCT FROM OLD.sandbox
    OR NEW.expires_at         IS DISTINCT FROM OLD.expires_at THEN
        RAISE EXCEPTION 'PAYOUT_QUOTE_IMMUTABLE: only consumed_at may change; a quote that can be edited is not a quote'
            USING ERRCODE = 'PQ001';
    END IF;
    IF OLD.consumed_at IS NOT NULL AND NEW.consumed_at IS DISTINCT FROM OLD.consumed_at THEN
        RAISE EXCEPTION 'PAYOUT_QUOTE_IMMUTABLE: a consumed quote cannot be consumed again or released'
            USING ERRCODE = 'PQ001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER payout_quotes_immutable BEFORE UPDATE OR DELETE ON payout_quotes
    FOR EACH ROW EXECUTE FUNCTION cp_payout_quote_immutable();

ALTER TABLE payout_requests ADD COLUMN quote_id uuid REFERENCES payout_quotes(id);

GRANT SELECT, INSERT ON payout_quotes TO cp_app;
-- Consumption only. Everything else about a quote is what the customer saw.
GRANT UPDATE (consumed_at) ON payout_quotes TO cp_app;
GRANT SELECT ON payout_quotes TO cp_readonly, cp_ops;

COMMENT ON TABLE payout_quotes IS
    'What a provider said a payout would cost, before the customer committed: gross, fee and net on both the Credit side and the money side, the three rule versions that produced it, whether it clears the provider minimum NET of fees, and when it stops being true. Consumed at most once (00764, goal §19, §22).';

-- +goose Down
SELECT 1; -- protected: a quote is evidence of what a customer was shown and is never dropped by rollback
