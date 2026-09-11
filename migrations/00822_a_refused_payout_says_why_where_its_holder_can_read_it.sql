-- +goose Up
-- A payout refused at submit says why, where its holder can read it (F-277).
--
-- `payout.Service.Submit` refuses a request whose destination has stopped being
-- usable, inside the claim transaction and before the transition to SUBMITTED,
-- so the transaction rolls back and the request stays VERIFIED with its value
-- reserved. That is the right SHAPE -- moving it to FAILED would return the
-- reservation on the strength of a fact the person can undo by registering a
-- new destination -- and it left the person with nothing to read:
--
--   * the refusal is an error returned to the sweep, and the sweep logs it for
--     an operator;
--   * the only customer-facing mention is a field on the DELETE response that
--     disabled the destination, which is gone as soon as the page is;
--   * the request itself says VERIFIED, which reads as "on its way";
--   * `failure_reason` is not it. It belongs to a request that FAILED, and the
--     notification follower renders it as "your withdrawal failed". This one
--     did not fail; it is waiting for a decision only its holder can make.
--
-- So: `blocked_reason`, with the instant it was recorded. It is a REASON, not a
-- state -- the request is still VERIFIED and still reserved, and cancelling it
-- is still what releases the Credits -- and the read carries it so
-- GET /v1/payouts and the Withdraw page can put the sentence beside the Cancel
-- control.
--
-- There is deliberately no route that re-points a payout at another
-- destination. A destination is the thing a quote, a fee and a provider
-- idempotency key were all computed against; changing it after the fact is a
-- new request in everything but name, and "cancel and ask again" is the honest
-- version of it.
--
-- Both columns are the APPLICATION's to write, like `failure_reason` beside
-- them: they record what this deployment decided, not what state the request is
-- in. The state and the money stay where 00807 put them, written only by the
-- transition trigger.

ALTER TABLE payout_requests
    ADD COLUMN blocked_reason text,
    ADD COLUMN blocked_at     timestamptz;

ALTER TABLE payout_requests
    ADD CONSTRAINT payout_requests_blocked_reason_has_an_instant CHECK (
        (blocked_reason IS NULL) = (blocked_at IS NULL));

COMMENT ON COLUMN payout_requests.blocked_reason IS
    'Why this reserved payout cannot be submitted, in words its holder can read, or NULL when nothing is blocking it. It is not a state and not a failure: the request is still VERIFIED and its Credits are still reserved, and cancelling it is what releases them (00822, F-277).';
COMMENT ON COLUMN payout_requests.blocked_at IS
    'When blocked_reason was recorded. Paired with it by a CHECK so a reason without an instant cannot exist (00822, F-277).';

GRANT UPDATE (blocked_reason, blocked_at) ON payout_requests TO cp_app;

-- +goose Down
SELECT 1; -- protected: why somebody's reserved value could not be sent is part of that payout's history
