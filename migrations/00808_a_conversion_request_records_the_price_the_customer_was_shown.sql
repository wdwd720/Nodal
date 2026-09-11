-- +goose Up
-- A conversion request records the price the customer was shown.
--
-- `quote_id` was optional on `POST /v1/payouts` and optional in
-- `payout.CreateRequest`, and the whole minimum-and-fee branch of
-- `payout.Service.Create` sat inside `if r.QuoteID != nil`. So 50 Credits
-- against a provider publishing a $1.00 minimum and a 25c + 25bp fee -- which
-- `POST /v1/payouts/quote` refuses in as many words, `minimum_ok=false`,
-- `net_amount_minor=25` -- reached VERIFIED with the whole gross reserved and
-- then SETTLED, with the fee never taken (F-224/F-wv-1).
--
-- D-119 makes the quote REQUIRED and moves the minimum into the domain. This
-- migration is the storage half: the request row records the gross, the fee and
-- the net the CUSTOMER WAS SHOWN, in the currency they were shown them in.
--
-- Why store them when `quote_id` points at the quote: because the quote is a
-- separate row with its own lifetime and the request is the record of what was
-- agreed. A payout that settles is explained to a person, and to whoever audits
-- it, by one row saying "you gave up this many Credits, the provider took this
-- fee, this much money was sent". Re-deriving that from a fee schedule that has
-- since been repriced is exactly the failure `fee_model_version` exists to
-- prevent, one join further out.
--
-- The columns are nullable because rows written before this migration have no
-- quote and cannot be given one. Everything written after it has all four or
-- none, which is what the CHECK says, and `payout.Create` refuses a request
-- without a quote, so "none" is unreachable from the application from here on.
--
-- Nothing here is the application's to rewrite: 00807 revoked table-wide UPDATE
-- on `payout_requests` and granted back a named list, and these four are not on
-- it. They are written once, by the INSERT that creates the request.

ALTER TABLE payout_requests
    ADD COLUMN quote_gross_amount_minor bigint CHECK (quote_gross_amount_minor >= 0),
    ADD COLUMN quote_fee_amount_minor   bigint CHECK (quote_fee_amount_minor >= 0),
    ADD COLUMN quote_net_amount_minor   bigint CHECK (quote_net_amount_minor >= 0),
    ADD COLUMN quote_currency           text;

ALTER TABLE payout_requests
    -- All four or none. Three of them is a price nobody can read.
    ADD CONSTRAINT payout_requests_quoted_price_is_whole CHECK (
        (quote_gross_amount_minor IS NULL) = (quote_fee_amount_minor IS NULL)
        AND (quote_gross_amount_minor IS NULL) = (quote_net_amount_minor IS NULL)
        AND (quote_gross_amount_minor IS NULL) = (quote_currency IS NULL)),
    -- The fee comes out of the gross and the net is what is left, which is the
    -- way round a customer expects and the way round that cannot overdraw them.
    ADD CONSTRAINT payout_requests_quoted_price_adds_up CHECK (
        quote_gross_amount_minor IS NULL
        OR (quote_fee_amount_minor <= quote_gross_amount_minor
            AND quote_net_amount_minor = quote_gross_amount_minor - quote_fee_amount_minor)),
    -- A price with no quote behind it would be a number nobody was shown.
    ADD CONSTRAINT payout_requests_quoted_price_names_its_quote CHECK (
        quote_gross_amount_minor IS NULL OR quote_id IS NOT NULL);

COMMENT ON COLUMN payout_requests.quote_net_amount_minor IS
    'What the customer was told would reach them, in minor units of quote_currency, from the quote this request was created against. Recorded rather than re-derived: a fee schedule repriced in June must not change what a March payout says it sent (00808, F-224).';

-- +goose Down
SELECT 1; -- protected: the price a customer was shown is financial history and is never dropped by rollback
