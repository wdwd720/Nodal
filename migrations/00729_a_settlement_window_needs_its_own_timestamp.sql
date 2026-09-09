-- +goose Up
-- credit_fundings.reversible_at: when the funding entered REVERSIBLE.
--
-- The reversibility window is the period during which a card payment can still be taken back, and
-- it is what decides when purchased Credits become capable of ever being paid out. It has to be
-- measured from something, and until now the only candidate was updated_at.
--
-- updated_at is the wrong thing, for two independent reasons.
--
-- It is maintained by a BEFORE UPDATE trigger, so it means "when was this row last touched" rather
-- than "when did this become reversible". Any later write for any reason silently restarts the
-- settlement clock. The direction of that error is safe -- value stays unpayable longer -- but a
-- control whose behaviour depends on unrelated writes is not a control anybody can reason about.
--
-- And it made the sweep untestable in the way that matters. The sweep compared updated_at against a
-- cutoff computed in the application, which is two clocks; a test with an injected clock could not
-- express "thirty-one days have passed" at all, and a production fleet with skew would answer
-- differently on different machines. Both halves are fixed here: this column is stamped by the
-- application when it makes the transition, and the sweep now computes its cutoff in SQL from the
-- database's own now().
--
-- This mirrors what deposits already does, where every status stamps its own column. A state that
-- matters is worth a timestamp of its own.

ALTER TABLE credit_fundings ADD COLUMN reversible_at timestamptz;

-- Existing rows: the mint moved them to REVERSIBLE and nothing else has touched them since, so
-- updated_at is the best evidence available for when that happened. Backfilling it is strictly
-- better than leaving NULL, which the sweep would skip forever.
UPDATE credit_fundings
   SET reversible_at = updated_at
 WHERE reversible_at IS NULL
   AND state IN ('REVERSIBLE','SETTLED','DISPUTED','REVERSED','REFUNDED');

-- The sweep reads exactly this: REVERSIBLE fundings ordered by how long they have been waiting.
CREATE INDEX credit_fundings_reversible_since_idx ON credit_fundings (reversible_at)
    WHERE state = 'REVERSIBLE';

COMMENT ON COLUMN credit_fundings.reversible_at IS
    'When the funding entered REVERSIBLE. The settlement window is measured from here, not from updated_at, which a trigger resets on any write.';

-- +goose Down
SELECT 1; -- protected: dropping this returns the settlement sweep to measuring a dispute window from a row-touch timestamp
