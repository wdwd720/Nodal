-- +goose Up
-- A structured strategy is an authoring path (D-129, F-256).
--
-- Migration 00500 gave three tables the same three-member vocabulary for HOW a
-- strategy document came to exist -- NATURAL_LANGUAGE, TYPESCRIPT_SDK, CLONE --
-- and `internal/strategy/ir.LineageSource` declares the same three in Go. The
-- compiler seam ADR-0029 describes assumed a model produced the document, so
-- every path through it was NATURAL_LANGUAGE.
--
-- There is now a fourth, and it is not natural language by any reading: a
-- sandbox tier can compile a strategy the user specified FIELD BY FIELD -- a
-- universe, a comparator, a threshold, limits, an interval -- through a
-- compiler that calls no model and parses no prose. Recording that as
-- NATURAL_LANGUAGE would be the one thing the whole review step exists to
-- prevent: a document whose provenance says a model read somebody's words when
-- no model ran and no words were read.
--
-- So STRUCTURED_SANDBOX joins the vocabulary on all three tables that hold it.
-- The name carries the tier in it deliberately: this authoring path exists only
-- where nothing real can move, and migration 00812 is what makes that a
-- property of the row rather than a promise about the code.
--
-- WHAT DOES NOT CHANGE. The three existing members, every row already written,
-- and `strategies.source_kind`, which stays NATURAL_LANGUAGE for a strategy a
-- person described in words even when a structured compiler is what compiled
-- it: the column says how the STRATEGY was authored, and a person still types
-- the description. The new member is admitted there too so the three lists stay
-- one vocabulary rather than three that happen to agree.

ALTER TABLE strategies DROP CONSTRAINT strategies_source_kind_check;
ALTER TABLE strategies ADD CONSTRAINT strategies_source_kind_check
    CHECK (source_kind IN ('NATURAL_LANGUAGE','TYPESCRIPT_SDK','CLONE','STRUCTURED_SANDBOX'));

ALTER TABLE strategy_versions DROP CONSTRAINT strategy_versions_source_kind_check;
ALTER TABLE strategy_versions ADD CONSTRAINT strategy_versions_source_kind_check
    CHECK (source_kind IN ('NATURAL_LANGUAGE','TYPESCRIPT_SDK','CLONE','STRUCTURED_SANDBOX'));

ALTER TABLE compile_attempts DROP CONSTRAINT compile_attempts_source_kind_check;
ALTER TABLE compile_attempts ADD CONSTRAINT compile_attempts_source_kind_check
    CHECK (source_kind IN ('NATURAL_LANGUAGE','TYPESCRIPT_SDK','CLONE','STRUCTURED_SANDBOX'));

-- +goose Down
SELECT 1; -- protected: reverting would leave strategy_versions and compile_attempts rows whose source_kind the CHECK refuses, and the provenance of a compiled document is not recoverable from a Down
