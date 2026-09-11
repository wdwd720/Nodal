-- +goose Up
-- credit_fundings.provider_mode: the provider mode that opened THIS payment.
--
-- ## What was wrong
--
-- `CreditPurchase.sandbox` was rendered from `s.opts.CreditPurchaseSandbox`, a
-- boolean the composition root computed once at startup from the deployment's
-- current provider configuration and handed to the HTTP layer. Every funding
-- the API returned carried that same value -- including fundings opened months
-- earlier, under a different mode.
--
-- So the flag answered "what mode is this deployment in now", and the question
-- it is asked is "was this payment real". Those are the same answer only until
-- somebody changes the configuration, and the direction that matters is the
-- cheap one: a deployment promoted from sandbox to live re-labels every sandbox
-- purchase it ever made as real value, in the API, in the browser's temperature
-- badge and in anything that reads either. A sandbox outcome has to be labelled
-- sandbox everywhere it is stored and shown, and a label recomputed from
-- today's configuration is not stored at all.
--
-- ## The shape
--
-- One column, written once at CreateFunding from the mode of the provider that
-- opened the payment, and never written again -- 00743 already revoked UPDATE
-- on this table from `cp_app` and granted back only `lot_id` and
-- `provider_reference`, so this column is write-once by privilege rather than by
-- convention. The CHECK holds the same three modes `internal/config` declares;
-- `test/integration/enums` keeps the two lists identical.
--
-- ## Why it is nullable
--
-- Because a funding opened before this column existed did not record the fact,
-- and a backfill would be the defect again in migration form: any value written
-- here now would be derived from today's configuration, which is exactly the
-- thing that must stop deciding what an old payment was. NULL means "this
-- funding predates the record", and the API renders it as SANDBOX rather than
-- as live -- an unrecorded mode cannot be asserted to be real money, and
-- over-labelling value as simulated is the safe direction of that mistake.
--
-- New rows are not nullable in practice: CreateFundingRequest.Validate refuses a
-- request with no mode, so nothing in this binary can write one.

ALTER TABLE credit_fundings ADD COLUMN provider_mode text;

ALTER TABLE credit_fundings
    ADD CONSTRAINT credit_fundings_provider_mode_check
    CHECK (provider_mode IS NULL OR provider_mode IN ('fake', 'sandbox', 'live'));

COMMENT ON COLUMN credit_fundings.provider_mode IS
    'The provider mode that opened this payment, recorded once at creation. NULL is a funding written before 00793, when the API flag was the deployment''s current mode rather than a recorded fact; such a funding is rendered as sandbox because an unrecorded mode cannot be asserted to be live (D-096, F-158).';

-- cp_app already holds INSERT on the table, which covers the new column; it
-- holds no UPDATE that could reach it.

-- +goose Down
SELECT 1; -- protected: dropping the recorded mode returns the sandbox label to being a property of today's configuration rather than of the payment
