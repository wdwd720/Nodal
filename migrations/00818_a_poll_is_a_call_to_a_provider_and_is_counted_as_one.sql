-- +goose Up
-- A poll is a call to a provider, and the session records when the last one
-- happened.
--
-- `GET /v1/me/verification/sessions/{id}` calls
-- `verification.Provider.Get` on EVERY request whose session is not terminal.
-- It is a GET, so `internal/httpapi`'s rate-limit selection puts it in the
-- General class, which the deployment configures at 600 a minute per principal
-- -- and the browser suite raises to 6000. One signed-in person can therefore
-- make this deployment call an identity provider six hundred times a minute,
-- against a contract whose pricing and rate limits are the provider's and not
-- ours (the second observation of the second withdrawal audit, D-133).
--
-- ## Why an interval and not a rate-limit class
--
-- The other option was to move the route into the Quote class, which is the
-- provider-expensive one. It was not taken, for three reasons:
--
--   1. The Quote class is selected in `internal/httpapi` by URL PATH, before
--      routing, so the rule would be a second path pattern in a function whose
--      whole job is to classify by shape rather than by route -- and the next
--      provider-calling GET would need a third.
--   2. A budget is per PRINCIPAL. Two people polling the same session, or one
--      person with two tabs, get two budgets; the provider sees the sum. An
--      interval on the SESSION bounds the calls per session, which is the
--      quantity the provider's contract is written in.
--   3. A rate limit REFUSES. A poll inside the interval does not need to be
--      refused: the answer is already on the row, it is the answer the last
--      call got, and the route is the one a person watching a spinner hits.
--      Answering from the record is better product behaviour AND fewer
--      provider calls than a 429.
--
-- Ten seconds, and the number is chosen rather than inherited: a hosted
-- identity check takes tens of seconds to minutes to come back, the web client
-- polls this route while a person waits, and a provider that answers in under
-- ten seconds is answering faster than the person can read the screen. The
-- webhook path is unaffected -- `IngestWebhook` is how a provider volunteers a
-- decision and it never consults this column -- so a deployment with webhooks
-- wired sees the decision immediately whatever the interval says.
--
-- `provider_polled_at` is NULL until the first poll, which is "nobody has
-- asked", and the first poll therefore always calls. `cp_app` gets UPDATE on
-- this column and nothing else: it is a record of what the application DID, not
-- a fact about the person, and the three columns 00762 already granted are
-- unchanged.

ALTER TABLE verification_sessions ADD COLUMN provider_polled_at timestamptz;

COMMENT ON COLUMN verification_sessions.provider_polled_at IS
    'When verification.Service.Poll last called the provider about this session. A poll inside PollMinimumInterval answers from the recorded status and calls nobody, so one signed-in person cannot make this deployment call an identity provider six hundred times a minute (00818, D-133).';

GRANT UPDATE (provider_polled_at) ON verification_sessions TO cp_app;

-- +goose Down
SELECT 1; -- protected: reverting returns the poll route to calling a provider on every request
