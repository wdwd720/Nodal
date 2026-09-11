-- +goose Up
-- An agent names a version OF the strategy it names, and the schema says so (D-105).
--
-- `agents` (00501) carries two independent foreign keys -- `strategy_id` to
-- strategies and `strategy_version_id` to strategy_versions -- and nothing tied
-- them together. Each was satisfied by any row in its own table, so an agent
-- could name one account's strategy and a completely different strategy's
-- compiled version, and every read that joined through either key would answer
-- confidently with a different story (F-187).
--
-- `agent_grants.strategy_version_id` -- the record of what the OWNER authorised
-- (00786) -- has the same shape, and the grant is the document an audit of
-- "what did this person agree to" reads.
--
-- The service now refuses the combination before it writes either row, and this
-- is the half of that refusal a later writer cannot skip:
--
--   * strategy_versions gains UNIQUE (strategy_id, id). It is redundant with
--     the primary key for uniqueness and that is not what it is for: it is the
--     target a composite foreign key needs, so the pair can be referenced as a
--     pair.
--   * agents gains FOREIGN KEY (strategy_id, strategy_version_id) REFERENCES
--     strategy_versions (strategy_id, id). MATCH SIMPLE, the default, is what
--     makes this compatible with a DRAFT agent: the constraint is satisfied
--     whenever any column of the pair is NULL, and 00501 already requires
--     strategy_version_id to be non-NULL for every stage past DRAFT.
--
-- What this does NOT say is who owns the strategy or whether anybody accepted
-- the version. Ownership is an account's, acceptance is a person's, and both
-- are checks against the REQUESTING principal, which no constraint can see.
-- internal/agents makes them, in the transaction that inserts the agent, under
-- a share lock on both rows.

ALTER TABLE strategy_versions ADD CONSTRAINT strategy_versions_strategy_id_id_key UNIQUE (strategy_id, id);

ALTER TABLE agents ADD CONSTRAINT agents_strategy_version_belongs_to_strategy_fk
    FOREIGN KEY (strategy_id, strategy_version_id) REFERENCES strategy_versions (strategy_id, id);

COMMENT ON CONSTRAINT agents_strategy_version_belongs_to_strategy_fk ON agents IS
    'The version an agent deploys is a version of the strategy it names. Two independent FKs admitted two different strategies (F-187); MATCH SIMPLE keeps a DRAFT agent with no version legal.';

-- +goose Down
SELECT 1; -- protected: dropping these would re-admit an agent whose strategy and strategy version describe two different strategies, and agent grants, runs and lifecycle rows already reference the agents they bind
