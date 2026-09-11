-- +goose Up
-- The constraints a person declared are not part of their description (D-129).
--
-- `POST /v1/strategies` has always accepted an optional `constraints` object --
-- "structured bounds the user stated up front" -- and there was nowhere to put
-- it, so `agents.StrategyService.Create` appended it to the description:
--
--     description + "\n\n[structured constraints]\n" + constraints
--
-- Two things were wrong with that, and both matter once a compiler exists.
--
-- The first is that the description is shown back to the person verbatim, on
-- the review screen and on every strategy card, as "what you wrote". It was not
-- what they wrote.
--
-- The second is the one this build turns on. The structured compiler reads the
-- CONSTRAINTS and never reads the description -- that separation is the whole
-- claim, and there is a test that a description saying "buy everything at any
-- price" changes nothing about the compiled document. A compiler cannot make
-- that claim about a column that holds both concatenated: it would have to find
-- the constraints by parsing prose, which is precisely the act being refused.
--
-- So the constraints get their own column. `{}` is the honest default and means
-- "none were stated"; the column is jsonb because the API's schema for it is a
-- declared object (`StructuredStrategy`, versioned by `schema_version`), and a
-- structured document stored as text would have to be re-parsed by everything
-- that reads it.
--
-- The description column keeps its own contents and nothing is migrated out of
-- it: a row written before today genuinely has that text in its description,
-- and rewriting a person's recorded words to tidy up an old shape would be a
-- worse defect than the one being fixed. Such a row has `constraints = '{}'`,
-- which is the truth -- the structured compiler was not there to read them, and
-- it will refuse to compile that strategy by name until the constraints are
-- stated again.

ALTER TABLE strategies ADD COLUMN constraints jsonb NOT NULL DEFAULT '{}'::jsonb;

-- +goose Down
SELECT 1; -- protected: strategies is referenced by compiled versions and the financial history under them; dropping the column would discard the declared bounds a compiled version was built from
