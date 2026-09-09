-- 00728: two more credit funding states, and the reason each one has to exist.
--
-- CANCELED. A purchase abandoned before any money moved is not a failure. Folding it into FAILED
-- costs nothing technically and costs a support queue real time: every abandoned checkout looks
-- like a declined card, and somebody chases a customer whose only act was closing a tab.
--
-- MANUAL_REVIEW. This is the important one. The provider status vocabulary is the provider's, and
-- it changes without asking us. Before this state existed, an unmapped status left the ingestion
-- path two options: crash, or pick the nearest state and act on it. Picking is how a payment nobody
-- understood becomes Credits somebody spent. A funding parked here has had no economic effect and
-- is waiting for a person -- which is the same shape internal/funding already uses for its
-- REVIEW_REQUIRED, and the same shape payout_requests uses for MANUAL_REVIEW.
--
-- What is deliberately NOT added: a resolution from MANUAL_REVIEW to SETTLED. Settlement means the
-- dispute window closed, which is a fact about a clock and a policy. An operator who could assert
-- it by hand could make value payout-eligible by closing a ticket. Resolving to REVERSIBLE puts the
-- funding back on the path that reaches SETTLED honestly. The Go transition table enforces that and
-- this migration does not weaken it.

BEGIN;

ALTER TABLE credit_fundings DROP CONSTRAINT credit_fundings_state_check;

ALTER TABLE credit_fundings ADD CONSTRAINT credit_fundings_state_check CHECK (state IN (
    'CREATED','AUTHORIZATION_PENDING','AUTHORIZED','CAPTURE_PENDING','CAPTURED',
    'REVERSIBLE','SETTLED','REVERSED','REFUNDED','DISPUTED','FAILED',
    'CANCELED','MANUAL_REVIEW'));

-- The partial index exists so that the sweeper can find fundings that still need attention. CANCELED
-- is terminal and belongs with the other terminal states; MANUAL_REVIEW emphatically does not --
-- something is stuck and a person has to look at it, which is exactly what this index is for.
DROP INDEX credit_fundings_state_idx;
CREATE INDEX credit_fundings_state_idx ON credit_fundings (state)
    WHERE state NOT IN ('SETTLED','REVERSED','REFUNDED','FAILED','CANCELED');

-- A funding waiting for a person should be findable by how long it has been waiting, because "the
-- oldest thing nobody has looked at" is the only useful ordering for a review queue.
CREATE INDEX credit_fundings_manual_review_idx ON credit_fundings (updated_at)
    WHERE state = 'MANUAL_REVIEW';

COMMIT;
