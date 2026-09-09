-- +goose Up
-- One signing decision per execution attempt, enforced rather than assumed
-- (F-81, recorded under F-67).
--
-- signing.Service treats a decision as the attempt's idempotency record: Sign
-- calls findDecision, and if a row exists it replays that decision instead of
-- inspecting and deciding again. The refusal path writes a row too, so a
-- rejected attempt replays its rejection.
--
-- 00300 gave attempt_id a plain index and no uniqueness. findDecision concedes
-- it in its own SQL:
--
--     WHERE attempt_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1
--
-- "the LATEST decision" is a phrase that only makes sense if there can be more
-- than one, and check-then-insert with no constraint is how there comes to be:
-- two concurrent Sign calls for one attempt both find nothing and both insert.
-- The rows are immutable, so the loser is not corrected -- it stays, and every
-- later read silently prefers whichever was written last.
--
-- That matters here more than the usual duplicate-row problem. A signing
-- decision carries inspected_tx_hash, and F-67 made the replay path bind the
-- bytes offered against the bytes that decision approved. Two decisions for one
-- attempt means two different sets of approved bytes, and the binding compares
-- against whichever row sorted last.
--
-- The unique index replaces the plain one rather than joining it: it serves
-- every lookup the plain index served.

DROP INDEX signing_decisions_attempt_idx;
CREATE UNIQUE INDEX signing_decisions_attempt_key ON signing_decisions (attempt_id);

COMMENT ON INDEX signing_decisions_attempt_key IS
    'One decision per attempt. signing.Service checks then inserts; without this, two concurrent Sign calls for one attempt both insert and findDecision silently prefers the later row (F-81).';

-- +goose Down
SELECT 1; -- protected: dropping this returns the signing path to a state where one attempt can carry two different sets of approved bytes
