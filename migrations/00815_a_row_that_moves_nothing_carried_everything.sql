-- +goose Up
-- A transition row that moves nothing carried everything.
--
-- 00806 left a same-state row legal, for one reason and one row: 00796's birth
-- screen writes `UNVERIFIED -> UNVERIFIED` carrying the sanctions result a
-- profile was created holding, and that is not a state change at all. D-121
-- recorded the residual as "a same-state row can still carry a screening
-- decision". 00807 copied the exemption to `payout_requests` without the reason
-- coming with it.
--
-- Both apply functions skip the edge check when
-- `NEW.from_state IS NOT DISTINCT FROM NEW.to_state`, and both then write every
-- other column the row carries. So the exemption is not "a row that changes
-- nothing"; it is "a row that changes everything EXCEPT the state", which is the
-- one shape neither the edge table nor 00731's deferred binding can see -- the
-- binding compares `old_val` to `new_val` and returns NULL when they are not
-- distinct.
--
-- ## 1. payout_requests: there is no legitimate same-state row (F-264)
--
-- `payout.Service.transitionWith` returns early when `req.State == to`, so
-- `internal/payout` has never written one. What the exemption bought was this,
-- as `cp_app`, against a request already REJECTED:
--
--     INSERT INTO payout_request_transitions
--         (id, request_id, from_state, to_state, ..., reserved_quantity,
--          settled_quantity, reserved_at, settled_at, provider_reference, provider_status)
--     SELECT ..., 'VERIFIED', state, ..., requested_quantity, requested_quantity,
--            now(), now(), 'forged-by-cp_app', 'settled'
--       FROM payout_requests WHERE id = ...;
--
-- The edge `VERIFIED -> REJECTED` is in the table, the request is ALREADY
-- REJECTED, so no state changes -- and the trigger wrote both quantities, both
-- instants and the provider's words onto a payout that had reserved nothing,
-- consumed nothing and been allocated nothing. `provider_reference` is the
-- column 00807 took out of `cp_app`'s UPDATE grant precisely because it is the
-- provider's word and not the application's.
--
-- The exemption goes. Every payout transition row must describe an edge
-- `payout_request_state_edges` has, and no state is its own successor there.
--
-- ## 2. The reservation invariant, said in the other direction
--
-- `cp_payout_reservation_balanced` (PO001, 00713) is a CONSTRAINT TRIGGER on
-- `payout_allocations`. It fires when an allocation row is written or its
-- `returned` flag moves, and compares the request's `reserved_quantity` to the
-- outstanding allocations. A `reserved_quantity` written with NO allocation row
-- behind it touches `payout_allocations` not at all, so the invariant was never
-- evaluated: the number was compared to nothing.
--
-- `payout_requests_reservation_backed` is the same comparison anchored at the
-- other end -- on `payout_requests`, when `reserved_quantity` is written. It is
-- DEFERRABLE INITIALLY DEFERRED for the same reason the first one is: the
-- reservation and its allocations are written in one transaction and neither
-- order is wrong.
--
-- `settled_quantity` needs no clause of its own. 00713's CHECK already says
-- `settled_quantity <= reserved_quantity`, so a settlement that was never
-- reserved has to forge the reservation first, and that is what this refuses.
--
-- Every legitimate writer already satisfies it: `reservedChange` writes the
-- quantity in the transaction that inserts the allocations, `releasedChange`
-- writes zero in the transaction that marks them returned, a REJECTED request
-- reserves nothing and is allocated nothing, and a SETTLED one keeps both.
--
-- ## 3. compliance_profiles: the exemption is narrowed to the row it was for
--    (F-265)
--
-- D-121's residual said the same-state path could still carry "a screening
-- decision". It also carried `expires_at`:
--
--     expires_at = CASE WHEN NEW.to_state = 'VERIFIED'
--                       THEN coalesce(NEW.expires_at, expires_at) ELSE expires_at END
--
-- so a `VERIFIED -> VERIFIED` row moved the validity window of a decision a
-- provider made once, for as long as the writer liked, with no session, no
-- provider and no state change. `verification.Service.ExpireOverdue` and
-- `verification.Resolver` both read that column, so the renewal is what decides
-- whether somebody is still PAYOUT_KYC.
--
-- A same-state row may now carry a sanctions screen change and nothing else:
-- `to_sanctions_state` must be present, and `verified_at`, `expires_at`,
-- `provider`, `provider_ref` and `session_id` must all be absent. `reason`,
-- `actor_*`, `correlation_id` and `occurred_at` are unrestricted -- they are the
-- trail, not the decision, and `compliance.Repository.screen` supplies a
-- correlation id.
--
-- The apply function is belt and braces about the same thing: on a same-state
-- row it leaves `verified_at` and `expires_at` exactly as the profile holds
-- them, rather than relying on the refusal above having found the columns NULL.
-- Without that, a same-state row could still put `occurred_at` into a NULL
-- `verified_at` through the existing coalesce.
--
-- The two writers that produce a same-state row both pass: 00796's birth-screen
-- trigger (which carries `from_sanctions_state`, `to_sanctions_state` and
-- nothing else) and `compliance.Repository.screen` (the same, plus a
-- correlation id). `verification.Repository.TransitionProfile` returns early
-- when the state has not moved, so it writes none.
--
-- D-121 and D-123 are amended, dated, with what the residual now is.
--
-- Custom SQLSTATEs: AD001 for the transition refusals, PO001 for the
-- reservation invariant -- the same code the trigger this one mirrors uses.

-- ---------------------------------------------------------------------------
-- 1. payout_requests: no same-state row at all
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_payout_apply_state_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    -- No exemption. payout.Service.transitionWith returns early when the state
    -- has not moved, so a same-state row is a row internal/payout did not
    -- write, and payout_request_state_edges has no self-edge for it to match.
    IF NOT EXISTS (SELECT 1 FROM payout_request_state_edges e
                    WHERE e.from_state = NEW.from_state AND e.to_state = NEW.to_state) THEN
        RAISE EXCEPTION 'PAYOUT_TRANSITION_ILLEGAL_EDGE: a payout cannot go % -> %; no such edge exists',
            NEW.from_state, NEW.to_state USING ERRCODE = 'AD001';
    END IF;
    UPDATE payout_requests
       SET state              = NEW.to_state,
           reserved_quantity  = coalesce(NEW.reserved_quantity, reserved_quantity),
           settled_quantity   = coalesce(NEW.settled_quantity, settled_quantity),
           reserved_at        = coalesce(NEW.reserved_at, reserved_at),
           settled_at         = coalesce(NEW.settled_at, settled_at),
           provider_reference = coalesce(NEW.provider_reference, provider_reference),
           provider_status    = coalesce(NEW.provider_status, provider_status)
     WHERE id = NEW.request_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'PAYOUT_TRANSITION_ORPHANED: payout_request_transitions names request % which does not exist',
            NEW.request_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 2. A reserved quantity has allocations behind it, checked where it is written
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_payout_request_reservation_backed() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    reserved numeric;
    alloc    numeric;
BEGIN
    -- Both sides are re-read at COMMIT rather than taken from NEW, which is
    -- cp_payout_reservation_balanced's shape and is not a detail. A deferred
    -- constraint trigger queues one event per row change and replays each with
    -- the values it had AT THE TIME, so an ordinary Create -- which inserts the
    -- request reserving nothing, walks it through ELIGIBILITY_CHECK, writes the
    -- allocations and only then reserves on the VERIFIED step -- would be
    -- refused at commit for the snapshot it passed through. What this asks is
    -- the question that matters: when this transaction ends, does the number
    -- the row states have allocations behind it.
    SELECT reserved_quantity INTO reserved FROM payout_requests WHERE id = NEW.id;
    IF NOT FOUND THEN
        -- Deleted in the same transaction. payout_requests has no deleter, so
        -- this is unreachable; it is here because a constraint trigger that
        -- raises on a missing row turns a legitimate future rollback path into
        -- an error about an invariant.
        RETURN NULL;
    END IF;
    SELECT coalesce(sum(quantity), 0) INTO alloc
      FROM payout_allocations WHERE request_id = NEW.id AND NOT returned;
    IF reserved <> alloc THEN
        RAISE EXCEPTION 'PAYOUT_RESERVATION_UNBALANCED: request % reserves % and its outstanding allocations total %',
            NEW.id, reserved, alloc USING ERRCODE = 'PO001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER payout_requests_reservation_backed
    AFTER INSERT OR UPDATE OF reserved_quantity ON payout_requests
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_payout_request_reservation_backed();

-- ---------------------------------------------------------------------------
-- 3. compliance_profiles: a same-state row is the screen, and only the screen
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_compliance_apply_state_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    same boolean := NEW.from_state IS NOT DISTINCT FROM NEW.to_state;
BEGIN
    IF NOT same
       AND NOT EXISTS (SELECT 1 FROM compliance_profile_state_edges e
                        WHERE e.from_state = NEW.from_state AND e.to_state = NEW.to_state) THEN
        RAISE EXCEPTION 'COMPLIANCE_TRANSITION_ILLEGAL_EDGE: a verification state cannot go % -> %; no such edge exists',
            NEW.from_state, NEW.to_state USING ERRCODE = 'AD001';
    END IF;
    IF same THEN
        -- The one row 00796 needs, and nothing more. A row that moves no state
        -- is about the SCREEN; it is not a provider decision, it does not name
        -- a session, and it does not say when somebody was verified or for how
        -- long.
        IF NEW.to_sanctions_state IS NULL THEN
            RAISE EXCEPTION 'COMPLIANCE_TRANSITION_SAME_STATE_EMPTY: a % -> % row changes no state and carries no sanctions screen, so it describes nothing',
                NEW.from_state, NEW.to_state USING ERRCODE = 'AD001';
        END IF;
        IF NEW.verified_at IS NOT NULL OR NEW.expires_at IS NOT NULL
           OR NEW.provider IS NOT NULL OR NEW.provider_ref IS NOT NULL
           OR NEW.session_id IS NOT NULL THEN
            RAISE EXCEPTION 'COMPLIANCE_TRANSITION_SAME_STATE_CARRIES_MORE: a % -> % row may carry a sanctions screen and nothing else; it may not carry verified_at, expires_at, a provider, a provider reference or a session',
                NEW.from_state, NEW.to_state USING ERRCODE = 'AD001';
        END IF;
    END IF;
    UPDATE compliance_profiles
       SET identity_state = NEW.to_state,
           -- Set on the way in; cleared when a decision stops being one. A
           -- profile that is no longer VERIFIED must not keep a verified_at
           -- that a reader would take for a current fact. A row that does not
           -- carry one leaves the standing value alone, so a transition about
           -- the SCREEN does not rewrite when the person was verified -- and a
           -- row that moves no state leaves both alone outright, which is the
           -- half the coalesce could not say (F-265).
           verified_at = CASE WHEN same THEN verified_at
                              WHEN NEW.to_state = 'VERIFIED'
                              THEN coalesce(NEW.verified_at, verified_at, NEW.occurred_at)
                              ELSE NULL END,
           expires_at  = CASE WHEN same THEN expires_at
                              WHEN NEW.to_state = 'VERIFIED'
                              THEN coalesce(NEW.expires_at, expires_at)
                              ELSE expires_at END,
           -- The screening decision, when the row carries one.
           sanctions_state = coalesce(NEW.to_sanctions_state, sanctions_state)
     WHERE user_id = NEW.user_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'COMPLIANCE_TRANSITION_ORPHANED: compliance_profile_transitions names user % which has no profile',
            NEW.user_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION cp_payout_apply_state_transition() IS
    'Writes payout_requests.state and the money beside it from the transition row, refusing any edge payout_request_state_edges does not have, including a same-state row: no legitimate writer produces one, and one carried both quantities, both instants and a forged provider reference (00815, 00807, F-264).';
COMMENT ON FUNCTION cp_payout_request_reservation_backed() IS
    'The PO001 reservation invariant anchored at payout_requests: a reserved_quantity is compared to the outstanding allocations where the quantity is written, not only where an allocation is. Deferred, because the two are written in one transaction (00815, 00713, F-264).';
COMMENT ON FUNCTION cp_compliance_apply_state_transition() IS
    'Writes compliance_profiles.identity_state, its timestamps and the sanctions screen from the transition row. A row whose endpoints are the same state may carry a sanctions screen and nothing else (00796 birth screen is the only kind there is), and never touches verified_at or expires_at (00815, 00806, F-265).';

-- +goose Down
SELECT 1; -- protected: reverting returns a payout's money columns and a verification's validity window to a row that licenses no change
