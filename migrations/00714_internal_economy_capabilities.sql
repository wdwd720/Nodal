-- +goose Up
-- Capability gates for the Nodal-native economy (gola.md PARTS XII-XXI, LXIV).
--
-- WHY THESE ARE GATES AND NOT FLAGS. PART LXXXIX is explicit that an ordinary feature flag must not
-- be the sole protection for a legal financial capability. The gate machinery already built here --
-- seven states, five activation conditions, dual control by distinct principals, evidence references
-- attested by each approver, and state authority held by the database rather than the application
-- (migration 00701) -- is the mechanism that PART LXXXIX is asking for. So the internal economy uses
-- it rather than growing a parallel switch.
--
-- The ten capabilities below are the ones internal/valuedomain and internal/agentauthority name.
-- Adding one here without adding it there leaves an unused gate; adding it there without adding it
-- here leaves the movement permanently refused, which is the safe direction to fail.
--
-- WHICH ARE HIGH RISK. cp_gate_is_high_risk decides whether the full evidence set -- legal review,
-- provider contract, risk approval, security approval -- is required before a gate can reach ACTIVE.
-- The rule applied here: a capability is high risk when exercising it moves value that a user could
-- reasonably believe is theirs, or when it changes who decides what happens to that value.
--
--   PAYOUT_RESERVE, PAYOUT_SETTLE            value leaves the system
--   NATIVE_MARKET_TRADING                    speculative markets in user-created assets
--   CREDIT_PURCHASE                          real money comes in
--   HOSTED_TRADING, HOSTED_FUNDING           partner-held customer funds
--   AGENT_BOUNDED_DISCRETION and above       an agent begins exercising judgement over capital
--
--   NATIVE_ASSET_CREATION is NOT high risk: creating a draft asset moves nothing. It is gated because
--   publication is a content and jurisdiction question, and it needs a legal review reference, but
--   requiring a provider contract to let somebody name a token would be theatre.

ALTER TABLE capability_gates DROP CONSTRAINT capability_gates_capability_check;
ALTER TABLE capability_gates ADD CONSTRAINT capability_gates_capability_check CHECK (capability IN (
    -- existing
    'LIVE_FUNDING','LIVE_MANUAL_TRADING','LIVE_AGENT_TRADING','WITHDRAWALS',
    'SOCIAL_DATA_PERSISTENCE','MARKETPLACE','CROSS_CHAIN','PREDICTION_MARKETS',
    'SECURITIES','CEX_TRADING',
    -- Nodal-native economy
    'CREDIT_PURCHASE','NATIVE_ASSET_CREATION','NATIVE_MARKET_TRADING',
    'PAYOUT_RESERVE','PAYOUT_SETTLE',
    'HOSTED_TRADING','HOSTED_FUNDING',
    -- agent authority above level 3
    'AGENT_BOUNDED_DISCRETION','AGENT_AUTONOMOUS_SELECTION','AGENT_AUTONOMOUS_PORTFOLIO'));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_gate_is_high_risk(p_capability text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT p_capability IN ('LIVE_FUNDING','LIVE_MANUAL_TRADING','LIVE_AGENT_TRADING','WITHDRAWALS',
                            'SECURITIES','CEX_TRADING','CROSS_CHAIN','PREDICTION_MARKETS',
                            'CREDIT_PURCHASE','NATIVE_MARKET_TRADING',
                            'PAYOUT_RESERVE','PAYOUT_SETTLE',
                            'HOSTED_TRADING','HOSTED_FUNDING',
                            'AGENT_BOUNDED_DISCRETION','AGENT_AUTONOMOUS_SELECTION','AGENT_AUTONOMOUS_PORTFOLIO');
$$;
-- +goose StatementEnd

-- No rows are inserted. A capability with no gate row is INACTIVE, and PART LXIII requires a fresh
-- deployment to start with every real-money capability disabled: an absent row IS that state, and
-- inserting DISABLED rows here would only make the absence look like an oversight rather than the
-- default. Migration 00701 already refuses to let a gate row be born in any state but DISABLED.

-- +goose Down
SELECT 1; -- protected: capability definitions are referenced by gate history and approval evidence
