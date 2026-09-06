-- +goose Up
-- Proof bundles (PART 88) collect every audit event that recorded a fill, intent, plan, quote,
-- attempt, decision or journal transaction. Producers name resource types inconsistently
-- ("intent" vs "trade_intent"), and ids are unique across aggregates, so the bundle looks rows up
-- by resource_id alone; the existing (resource_type, resource_id, occurred_at) index cannot serve
-- that predicate. An index is not a mutation: audit_events rows stay append-only.
CREATE INDEX audit_events_resource_id_idx ON audit_events (resource_id);

-- +goose Down
SELECT 1; -- protected: audit history is never dropped
