package verification

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// The expiry sweep (D-084, and the half of D-061 that was left open).
//
// A verification decision stands for ValidityWindow and the transition that
// reaches VERIFIED writes `expires_at` onto the profile. Two things then have to
// happen, and only one of them existed.
//
// The resolver reads the timestamp itself and reports the BASE level for a
// profile whose window has elapsed, even while the state still says VERIFIED.
// That is what stops an expired verification paying somebody out, and it is
// deliberately not the sweep's job -- D-061's own words: "a sweep that moves an
// expired profile to EXPIRED will exist and will sometimes be late".
//
// This is that sweep, and it only tidies the STATE. Nothing depends on it for
// safety. What it buys is that the row and the level stop disagreeing: a
// profile that says VERIFIED while every surface treats it as unverified is a
// support conversation nobody can win, and §20 lists EXPIRED as a state the
// journey actually has -- with REVERIFY as its next step, which the profile
// view can only offer once the state is EXPIRED.

// SweepBatch bounds one pass. A backlog drains over several passes rather than
// in one long transaction holding a pool connection, exactly as the
// notification follower's batch does.
const SweepBatch = 200

// ExpireOverdue moves VERIFIED profiles whose validity window has elapsed to
// EXPIRED, through the transition row that is the only thing the schema lets
// write the state (00761). It returns how many it moved.
//
// # Why one transaction per profile
//
// The transition trigger is a SECURITY DEFINER function and each move takes a
// row lock on the profile it is about. Batching every move into one transaction
// would hold every one of those locks until the last one committed, and a
// person verifying at that moment would block behind a maintenance pass. A
// failure part way through leaves the profiles already moved, moved -- which is
// correct, because each is an independent fact about an independent person.
//
// # Why it is not `UPDATE compliance_profiles SET identity_state = 'EXPIRED'`
//
// `cp_app` holds no UPDATE on that column, deliberately. The statement would
// fail, and if it did not, it would be a state change with no edge, no actor
// and no reason -- which is the thing 00761 exists to make impossible.
//
// The actor is SYSTEM `verification:expiry-sweep`, so the trail says a clock
// did this rather than a person or a provider. No provider reference is
// recorded, because no provider decided anything: the decision that stands
// here is Nodal's own, made once when ValidityWindow was chosen.
func (s *Service) ExpireOverdue(ctx context.Context, database *db.DB, now time.Time, limit int) (int, error) {
	if database == nil {
		return 0, errs.New(errs.CodeInternal, "verification: the expiry sweep needs a database")
	}
	if limit <= 0 || limit > SweepBatch {
		limit = SweepBatch
	}
	now = now.UTC()

	// The sweep acts as the system: it writes a compliance transition for
	// somebody it is not, which no customer principal may do and no operator
	// principal should be asked to do on a timer.
	ctx = security.WithPrincipal(ctx, security.Principal{
		SubjectID: expirySweepActor, ActorType: security.ActorSystem, AuthTime: now,
	})

	overdue, err := s.deps.Repo.OverdueVerifications(ctx, database, now, limit)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, userID := range overdue {
		err := database.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, terr := s.deps.Repo.TransitionProfile(ctx, tx, ProfileTransition{
				UserID:    userID,
				To:        StateExpired,
				ActorType: security.ActorSystem,
				ActorID:   expirySweepActor,
				Reason: "the verification decision reached the end of its one-year validity window; " +
					"this is not a rejection and re-verifying starts a new session",
				OccurredAt: now,
			})
			return terr
		})
		switch {
		case err == nil:
			moved++
		case errs.CodeOf(err) == errs.CodeInvalidStateTransition, errs.CodeOf(err) == errs.CodeNotFound:
			// Somebody else moved this profile between the read and the write
			// -- a re-verification landing, an operator suspending it. The row
			// is no longer VERIFIED, so there is nothing to expire and the next
			// pass will not see it. Not an error.
			continue
		default:
			// One profile's failure does not abandon the rest: each is an
			// independent person, and a pass that stopped at the first problem
			// would leave the others stale for as long as the problem lasted.
			return moved, err
		}
	}
	return moved, nil
}

// expirySweepActor is what the trail says did this.
const expirySweepActor = "verification:expiry-sweep"

// OverdueVerifications returns the people whose VERIFIED profile has passed its
// validity window, oldest first, up to limit.
//
// It reads only VERIFIED rows: EXPIRED, REJECTED, RESTRICTED and SUSPENDED are
// not this pass's business, and RESTRICTED in particular is a decision a person
// made that a clock must not quietly undo.
func (r *Repository) OverdueVerifications(ctx context.Context, q db.Querier, now time.Time, limit int) ([]accounts.UserID, error) {
	rows, err := q.Query(ctx, `SELECT user_id FROM compliance_profiles
		WHERE identity_state = 'VERIFIED' AND expires_at IS NOT NULL AND expires_at <= $1
		ORDER BY expires_at
		LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []accounts.UserID
	for rows.Next() {
		var id accounts.UserID
		if err := rows.Scan(&id); err != nil {
			return nil, mapError(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return out, nil
}
