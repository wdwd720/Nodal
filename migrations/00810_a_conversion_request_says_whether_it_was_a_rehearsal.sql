-- +goose Up
-- A conversion request says whether it was a rehearsal, and a destination says
-- which subdivision it pays into.
--
-- ## The sandbox fact (F-232/F-wv-8)
--
-- `payout_requests` recorded nothing about the deployment it was created on.
-- The by-id read answered `"sandbox": true` by asking
-- `Ports.Payouts.SandboxProvider()` -- TODAY's provider mode -- and the list and
-- the create response did not answer it at all. So the same payout was a
-- rehearsal on one screen, unlabelled on two others, and would silently become
-- a real payout in the record the day a deployment swapped its provider.
--
-- This is D-096's treatment, applied where F-158 said it would be needed next:
-- the fact is written once, at creation, from the provider's availability and
-- the deployment's own tier, and every surface reads the column.
--
-- The columns are NULLABLE and that is deliberate, exactly as 00793's
-- `provider_mode` is. A row written before this migration has no recorded fact,
-- and `NOT NULL DEFAULT false` would assert about every one of them that it was
-- a real payout -- which is the one direction this label must never be wrong in.
-- NULL means "unrecorded", `toAPIPayout` renders it as a rehearsal, and
-- `payout.CreateRequest.Validate` refuses a request with no environment, so
-- nothing in this binary can write another NULL.
--
-- The CHECK is 00762's and 00764's: a rehearsal cannot exist where real value
-- moves, said in the place that cannot be redeployed around.
--
-- ## The subdivision (F-228/F-wv-9)
--
-- 00763 gave `payout_destinations` a `country`, because `Capabilities` carried
-- `SupportedCountries` and `ExcludedRegions` and nothing could feed them. It
-- fed half of them. `payout.RecipientProfile.Region` existed, `CanPayRecipient`
-- answered RECIPIENT_REGION_EXCLUDED and RECIPIENT_REGION_UNKNOWN, and nothing
-- ever set a region -- so a provider that pays the United States but not New
-- York had no way to say no to a New York recipient, and the destination was
-- accepted and marked VERIFIED.
--
-- `region` is the subdivision code without its country prefix, the same shape
-- `compliance_profiles.jurisdiction_region` and `verification_sessions`
-- already use.

ALTER TABLE payout_requests
    ADD COLUMN sandbox     boolean,
    ADD COLUMN environment text;

ALTER TABLE payout_requests
    ADD CONSTRAINT payout_requests_environment_check
        CHECK (environment IS NULL OR environment IN ('LOCAL','TEST','DEV','STAGING','PROD')),
    -- The rule. A rehearsal cannot exist where real value moves.
    ADD CONSTRAINT payout_requests_sandbox_never_in_prod
        CHECK (NOT coalesce(sandbox, false) OR environment IS DISTINCT FROM 'PROD');

COMMENT ON COLUMN payout_requests.sandbox IS
    'Whether this conversion request was a rehearsal, recorded at creation from the provider''s availability and the deployment''s tier. NULL is a row written before 00810, when the label was read from today''s provider mode rather than from the request; such a row is rendered as a rehearsal, because an unrecorded mode cannot be asserted to be real (00810, F-232, D-096''s treatment).';

ALTER TABLE payout_destinations ADD COLUMN region text;

COMMENT ON COLUMN payout_destinations.region IS
    'The subdivision within country, without its country prefix. Required whenever the provider publishes ExcludedRegions for that country: "we do not know which state" is not "any state" (00810, F-228, D-122).';

-- cp_app already holds INSERT on both tables, which covers the new columns.
-- 00807 revoked its table-wide UPDATE on payout_requests and 00763 on
-- payout_destinations, and neither column is on the grant that came back: what
-- a request WAS is written once.

-- +goose Down
SELECT 1; -- protected: the tier a payout was made on is financial history and is never dropped by rollback
