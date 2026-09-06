-- +goose Up
-- Worker leases over execution plans (cmd/execution-worker).
--
-- The lease is operational scheduling state, never financial truth: it says
-- which worker process is currently running a plan and when the plan may be
-- attempted again. Correctness of execution does not depend on it — the
-- executor's per-step durable state does — but a lease keeps two workers from
-- driving the same plan at the same time and gives a crashed worker's plans
-- back to the fleet after the lease expires.
--
-- A plan is claimable when it has no lease row, or when its lease has expired
-- AND its retry deadline has passed. next_attempt_at mirrors the outbox relay's
-- retry-deadline behaviour of migration 00642: a plan that keeps failing backs
-- off instead of hot-looping.

CREATE TABLE execution_plan_leases (
    plan_id           uuid PRIMARY KEY REFERENCES execution_plans(id),
    owner             text NOT NULL,
    leased_at         timestamptz NOT NULL,
    lease_expires_at  timestamptz NOT NULL,
    next_attempt_at   timestamptz NOT NULL,
    attempts          integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_outcome      text,
    last_error        text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- The claim query orders by the plan, so the lease index only has to answer
-- "is this plan currently claimable".
CREATE INDEX execution_plan_leases_due_idx ON execution_plan_leases (next_attempt_at, lease_expires_at);

CREATE TRIGGER execution_plan_leases_updated_at BEFORE UPDATE ON execution_plan_leases
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

GRANT SELECT, INSERT, UPDATE ON execution_plan_leases TO cp_app;
GRANT SELECT ON execution_plan_leases TO cp_readonly, cp_ops;

-- +goose Down
-- Protected: no migration at or above ProtectedVersion=100 may drop, delete,
-- truncate or alter in its Down section.
--
-- Lease rows are operational, not financial truth, so dropping them would lose
-- nothing irreplaceable and the argument for an exception is genuinely
-- reasonable. It is refused anyway, for two reasons. A blanket rule with no
-- judgement calls is the only kind that can be audited cheaply and enforced by
-- a test; the moment one table is exempt because its author judged it
-- operational, the next author makes the same argument about a table that is
-- not. And a rollback that drops this table while workers are running would
-- strip every in-flight plan of its lease mid-execution, which is a live
-- incident rather than a clean revert.
--
-- Nothing is needed here in any case: leases expire on their own and are
-- rebuilt from the executor's durable per-step state.
SELECT 1; -- protected: operational lease state, dropped only by an explicit operator action
