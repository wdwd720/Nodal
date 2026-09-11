-- +goose Up
-- The Nodal profile: product-level state, and nothing a person did not choose to show.
--
-- ADR-0022 settled where the user lives: ZITADEL authenticates, `users` is the
-- Nodal user, `identity_pii` holds the verified address sealed under a key the
-- database never has. What had no home was everything the PRODUCT needs and
-- identity has no opinion about -- what to call somebody on a leaderboard, which
-- clock to render a fill in, whether they have finished onboarding.
--
-- Goal PART 4 asks for exactly this separation and names the failure it is
-- avoiding: "Do not combine these into one giant users table."
--
-- ## What is deliberately not here
--
-- * **No e-mail, no phone, no legal name, no date of birth.** Those are
--   `identity_pii`'s, sealed, and ADR-0021 decides who may read them. A display
--   name is different in kind: it is what the user typed into a box knowing it
--   would be shown. `TestProfile_HasNoPersonalDataColumn` asserts the absence
--   rather than trusting this paragraph.
-- * **No avatar upload.** `avatar_seed` is sixteen hex characters the client
--   renders an identicon from. Nodal stores no user-supplied image: an upload
--   path is a content-moderation surface, a malware surface and a storage cost,
--   and none of the three buys anything the seed does not.
-- * **No product status.** `users.status` (identity) and `accounts.status`
--   (financial) already answer "may this person be here" and "may this account
--   take risk". A third status on the profile would be a third answer to a
--   question that must have one (D-054).
--
-- ## Onboarding is timestamps, not a state machine (D-053)
--
-- Every state machine in this schema exists because a value moves through
-- states where the ORDER is the invariant and an illegal edge is a defect worth
-- refusing at the database. Onboarding is not that shape: its steps are
-- independent, a user may do them in any order, none can be undone, and there
-- is no illegal edge to refuse. Modelling it as a state column would invent a
-- linearisation the product does not have and then require a transition table
-- to protect it.
--
-- What IS worth refusing is a profile that claims to have finished something it
-- did not do, so the two properties that matter are enforced directly:
--
--   * nothing is born finished -- a profile INSERT carrying any completion
--     stamp is refused (`PROFILE_BORN_ONBOARDED`), the same control 00701 and
--     00713 apply to gates and payouts;
--   * a stamp is written once -- an UPDATE that moves a non-null stamp to a
--     different instant is refused (`PROFILE_STEP_RESTAMPED`), so "when did you
--     accept the terms" cannot be rewritten later.

CREATE TABLE user_profiles (
    user_id       uuid PRIMARY KEY REFERENCES users(id),

    -- Chosen, shown, and changeable. Length is bounded here as well as in Go
    -- because a column with no bound is a column somebody eventually writes a
    -- megabyte into.
    display_name  text CHECK (display_name IS NULL OR (length(display_name) BETWEEN 1 AND 64)),

    -- The handle is the one globally unique thing a user picks, so it is
    -- normalised to lower case ON THE WAY IN by internal/profile and stored that
    -- way: a UNIQUE index over a case-varying column would let `Nodal` and
    -- `nodal` both exist. The reserved-word list lives in Go, where the reason
    -- for each entry can be written down.
    handle        text UNIQUE CHECK (handle IS NULL OR handle ~ '^[a-z][a-z0-9_]{2,29}$'),

    -- Presentation only. Neither is ever used to infer a jurisdiction: that is a
    -- legal determination and a timezone is not evidence of one (the same rule
    -- cmd/api applies to IP addresses).
    locale        text NOT NULL DEFAULT 'en' CHECK (locale ~ '^[a-z]{2}(-[A-Z]{2})?$'),
    time_zone     text NOT NULL DEFAULT 'UTC' CHECK (time_zone ~ '^(UTC|[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+){1,2})$' AND length(time_zone) <= 64),

    -- Sixteen hex characters, derived from the user id at creation. Not an
    -- upload, and not derived from anything personal.
    avatar_seed   text NOT NULL CHECK (avatar_seed ~ '^[0-9a-f]{16}$'),

    -- Onboarding. Each column answers "when did this step complete", and NULL
    -- means it has not. There is no ordering constraint between them because
    -- the product does not have one.
    onboarding_started_at   timestamptz NOT NULL DEFAULT now(),
    display_name_set_at     timestamptz,
    terms_accepted_at       timestamptz,
    onboarding_completed_at timestamptz,

    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX user_profiles_handle_idx ON user_profiles (handle) WHERE handle IS NOT NULL;

CREATE TRIGGER user_profiles_updated_at BEFORE UPDATE ON user_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_profile_is_not_born_onboarded() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.display_name_set_at IS NOT NULL
       OR NEW.terms_accepted_at IS NOT NULL
       OR NEW.onboarding_completed_at IS NOT NULL THEN
        RAISE EXCEPTION 'PROFILE_BORN_ONBOARDED: a profile cannot be created with onboarding already complete; each step is stamped by the action that performs it'
            USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER user_profiles_not_born_onboarded BEFORE INSERT ON user_profiles
    FOR EACH ROW EXECUTE FUNCTION cp_profile_is_not_born_onboarded();

-- +goose StatementBegin
CREATE FUNCTION cp_profile_step_is_stamped_once() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    col  text;
    was  timestamptz;
    now_ timestamptz;
BEGIN
    FOREACH col IN ARRAY ARRAY['onboarding_started_at', 'display_name_set_at', 'terms_accepted_at', 'onboarding_completed_at'] LOOP
        EXECUTE format('SELECT ($1).%I, ($2).%I', col, col) INTO was, now_ USING OLD, NEW;
        IF was IS NOT NULL AND was IS DISTINCT FROM now_ THEN
            RAISE EXCEPTION 'PROFILE_STEP_RESTAMPED: user_profiles.% was completed at % and cannot be moved to %',
                col, was, coalesce(now_::text, 'NULL') USING ERRCODE = 'AD001';
        END IF;
    END LOOP;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER user_profiles_step_is_stamped_once BEFORE UPDATE ON user_profiles
    FOR EACH ROW EXECUTE FUNCTION cp_profile_step_is_stamped_once();

COMMENT ON TABLE user_profiles IS
    'Product-level user state: what the user chose to show, how to render things for them, and which onboarding steps they have completed. Personal data stays in identity_pii, sealed (ADR-0021); who the user IS stays in users (ADR-0022). Onboarding is timestamps rather than a state machine, and D-053 says why.';
COMMENT ON COLUMN user_profiles.avatar_seed IS
    'Sixteen hex characters an identicon is rendered from. Nodal accepts no avatar upload: the seed buys the same thing without a moderation, malware or storage surface.';

GRANT SELECT, INSERT, UPDATE ON user_profiles TO cp_app;
GRANT SELECT ON user_profiles TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: a profile carries the onboarding record and the handle a person claimed; reverting would take both away and free a handle somebody else could then take
