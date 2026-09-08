-- +goose Up
-- MARKETPLACE is high risk in Go and was not in SQL (F-43).
--
-- F-16 reclassified MARKETPLACE from low to high risk, and the reason is on
-- internal/gates/capability.go: it gates internal commerce, which moves Credits
-- between users and MINTS the creator-earning provenance a payout policy may
-- one day permit to be withdrawn. Leaving it low risk would let one approver
-- switch on the only legitimate way withdrawable provenance comes into
-- existence.
--
-- That fix landed in `gates.IsHighRisk` and not in `cp_gate_is_high_risk`. The
-- two lists then disagreed -- 18 capabilities in Go, 17 in SQL -- and the one
-- they disagreed about was the one F-16 was about.
--
-- The consequence is precise. Migration 00701's header says the database check
-- exists to be "the line that holds when the Go check is bypassed": GT003
-- refuses to approve a high-risk gate without four evidence references and
-- three distinct principals. For MARKETPLACE that line was absent. The
-- application path was still protected, because gates.Admin asks Go first --
-- so nothing was exploitable through the API. What was missing is the thing
-- the database check exists for, which is what happens when the API is not the
-- caller.
--
-- docs/compliance-gates/PRODUCTION_GATES.md asserted the opposite of the code
-- ("MARKETPLACE is not high-risk in code... it can be activated with [evidence]
-- empty"), and asserted it as a CORRECTION to an earlier table, which is the
-- form a reader trusts most. That document is corrected in the same change.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_gate_is_high_risk(p_capability text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT p_capability IN ('LIVE_FUNDING','LIVE_MANUAL_TRADING','LIVE_AGENT_TRADING','WITHDRAWALS',
                            'SECURITIES','CEX_TRADING','CROSS_CHAIN','PREDICTION_MARKETS',
                            'CREDIT_PURCHASE','NATIVE_MARKET_TRADING',
                            'PAYOUT_RESERVE','PAYOUT_SETTLE',
                            'HOSTED_TRADING','HOSTED_FUNDING','MARKETPLACE',
                            'AGENT_BOUNDED_DISCRETION','AGENT_AUTONOMOUS_SELECTION','AGENT_AUTONOMOUS_PORTFOLIO');
$$;
-- +goose StatementEnd

-- Nothing is back-filled and nothing is re-evaluated. GT003 is checked when a
-- gate TRANSITIONS, so a MARKETPLACE gate approved before this migration keeps
-- whatever evidence it was approved with; this changes what the next approval
-- requires. Re-approving an existing one is an operator decision with its own
-- ceremony, not a migration's to make.
--
-- The parity this file restores is now asserted by
-- TestIntegration_GoAndSQLAgreeOnEveryCapabilitysRisk, which drives
-- cp_gate_is_high_risk for every capability gates.AllCapabilities() declares
-- and compares each answer with gates.IsHighRisk. A list duplicated in two
-- languages diverges when somebody edits one of them; the only defence is a
-- test that reads both.

-- +goose Down
SELECT 1; -- protected: reverting would restore a security control that is missing a capability
