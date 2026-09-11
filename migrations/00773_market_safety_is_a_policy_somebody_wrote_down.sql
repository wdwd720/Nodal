-- +goose Up
-- Internal market safety limits, as a versioned policy (product goal §47 INTERNAL MARKET SAFETY).
--
-- WHAT WAS THERE. internal/risk already refuses a native BUY that would leave one account holding
-- too large a share of an asset's supply, or too much of its Credits with one creator
-- (max_native_market_concentration_bps, max_creator_concentration_bps; migration 00602's
-- risk_policies). internal/nativemarket/surveillance.go already RAISES alerts for wash trading,
-- rapid round-tripping, creator self-dealing, concentration and anomalous volume. The market status
-- machine already halts a market on an operator's recorded decision.
--
-- WHAT WAS NOT. §47 also names price impact, low liquidity and halted markets, and nothing bounded
-- any of them: a single order could move a thin market by any amount, a market could be opened with
-- a virtual reserve of one Credit, a creator could buy their own asset, and no automatic mechanism
-- ever stopped a market that was moving violently. Those four limits are what this file records.
--
-- WHY THEY ARE NOT risk_policies COLUMNS. A risk policy is composed GLOBAL ∧ ACCOUNT ∧ AGENT and
-- answers "how much risk may THIS ACCOUNT take". Three of these four limits are properties of a
-- MARKET, not of an account: a circuit breaker belongs to the venue, a minimum opening liquidity
-- belongs to the market being opened, and the creator restriction is about a relationship between
-- an account and an asset. Composing them per account would let an account-scoped row loosen a
-- venue rule, which is the wrong direction for every one of them. They get their own document, with
-- the same discipline: one row per version, immutable, hashed over its canonical rendering, chosen
-- by effective_at, and recorded with an actor and a reason that survive forever.
--
-- The two risk-kernel limits stay where they are and are reported alongside these in the asset
-- detail response, so "limits in force" means all of them.

CREATE TABLE native_market_safety_policies (
    id                    uuid PRIMARY KEY,
    version               text NOT NULL UNIQUE,
    rules                 jsonb NOT NULL,
    rules_hash            text NOT NULL CHECK (char_length(rules_hash) = 64),
    effective_at          timestamptz NOT NULL,
    created_by_actor_type text NOT NULL CHECK (created_by_actor_type <> 'AGENT'),
    created_by_actor_id   text NOT NULL CHECK (created_by_actor_id <> ''),
    reason                text NOT NULL CHECK (reason <> ''),
    created_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX native_market_safety_policies_effective_idx ON native_market_safety_policies (effective_at DESC);
CREATE TRIGGER native_market_safety_policies_immutable BEFORE UPDATE OR DELETE ON native_market_safety_policies
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- A circuit breaker trip, with everything needed to recompute it.
--
-- The trip itself is a market status change and goes through native_market_transitions like every
-- other one: there is one status machine for a market and a second "breaker state" column would be
-- a second answer to "may this market trade". This table is the EVIDENCE -- which policy, which
-- window, which two prices, how far apart -- so a halt can be explained without re-deriving it from
-- the price series.
CREATE TABLE native_market_breaker_events (
    id               uuid PRIMARY KEY,
    market_id        uuid NOT NULL REFERENCES native_markets(id),
    fill_id          uuid REFERENCES native_market_fills(id),
    policy_version   text NOT NULL,
    window_seconds   integer NOT NULL CHECK (window_seconds > 0),
    limit_bps        integer NOT NULL CHECK (limit_bps > 0),
    move_bps         integer NOT NULL,
    reference_price  numeric(78,0) NOT NULL CHECK (reference_price >= 0),
    observed_price   numeric(78,0) NOT NULL CHECK (observed_price >= 0),
    reference_at     timestamptz NOT NULL,
    tripped_at       timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX native_market_breaker_events_market_idx ON native_market_breaker_events (market_id, tripped_at DESC);
CREATE TRIGGER native_market_breaker_events_immutable BEFORE UPDATE OR DELETE ON native_market_breaker_events
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT ON native_market_safety_policies, native_market_breaker_events TO cp_app;
GRANT SELECT ON native_market_safety_policies, native_market_breaker_events TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: a limit that was in force and a halt that happened are audit record
