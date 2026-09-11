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

// sessionSweepActor is what the trail says expired an attempt.
const sessionSweepActor = "verification:session-expiry-sweep"

// UnstartedSessionGrace is how long a session that was never handed to a
// provider is kept before it is expired.
//
// A session row is written BEFORE the provider is called, so a crash between
// the two leaves a row with no provider reference (that is what the row is for:
// something to reconcile). Nothing can ever move it -- `Resume` refuses a
// session the provider never saw, and only the provider's answer moves a
// session that it did -- and the partial unique index in 00762 permits one open
// session per person, so it is the person's own verification that is blocked,
// by a row about an attempt that never happened.
//
// Fifteen minutes is far longer than any provider call in PROVIDER_BOUNDARY §3
// and short enough that somebody who hit a crash can try again while they are
// still at their desk.
const UnstartedSessionGrace = 15 * time.Minute

// ExpireOverdueSessions moves verification attempts that can no longer produce
// a decision to EXPIRED, through the transition row that is the only thing that
// writes a session status. It returns how many it moved.
//
// # What it is for
//
// The partial unique index in 00762 permits exactly one OPEN session per
// person. A hosted link that ran out is not open in any sense the person can
// use -- the provider will not accept it and `Poll` gets nothing new -- but its
// STATUS still says open, so that person cannot start another verification at
// all. Nothing expired a session, so "your link expired, start again" was a
// sentence the product could not honour: §20's own words for what happens next.
//
// # What it deliberately does not do
//
// It does not touch the profile. A session that ran out did not decide
// anything, and `applyToProfile` already refuses to let an abandoned attempt
// undo a standing verification; moving the profile here would be a clock
// deciding something about a person, which is the thing D-061 was careful not
// to do. The person's next `Start` moves the profile, as it always has.
//
// # Why one transaction per session
//
// The same reason ExpireOverdue gives: each is an independent attempt by an
// independent person, and one transaction over a batch would hold every row
// lock until the last one committed -- including the lock a person's own
// webhook is waiting for.
func (s *Service) ExpireOverdueSessions(ctx context.Context, database *db.DB, now time.Time, limit int) (int, error) {
	if database == nil {
		return 0, errs.New(errs.CodeInternal, "verification: the session expiry sweep needs a database")
	}
	if limit <= 0 || limit > SweepBatch {
		limit = SweepBatch
	}
	now = now.UTC()

	// As the system: it moves somebody else's attempt, which no customer
	// principal may do.
	ctx = security.WithPrincipal(ctx, security.Principal{
		SubjectID: sessionSweepActor, ActorType: security.ActorSystem, AuthTime: now,
	})

	due, err := s.deps.Repo.OverdueSessions(ctx, database, now, now.Add(-UnstartedSessionGrace), limit)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, d := range due {
		err := database.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, terr := s.deps.Repo.TransitionSession(ctx, tx, d.ID, SessionExpired, SessionChange{
				ActorType:  security.ActorSystem,
				ActorID:    sessionSweepActor,
				Reason:     d.reason,
				OccurredAt: now,
			})
			return terr
		})
		switch {
		case err == nil:
			moved++
		case errs.CodeOf(err) == errs.CodeInvalidStateTransition, errs.CodeOf(err) == errs.CodeNotFound:
			// The provider answered, or the person cancelled, between the read
			// and the write. The session is no longer open, so there is nothing
			// to expire and the next pass will not see it.
			continue
		default:
			return moved, err
		}
	}
	return moved, nil
}

// DueSession is one attempt the sweep found, and why it is due.
type DueSession struct {
	ID     SessionID
	reason string
}

// OverdueSessions returns the open verification sessions that can no longer
// produce a decision, oldest first, up to limit.
//
// Two kinds, and they are due for different reasons:
//
//   - a hosted link past its `expires_at`: the provider issued it with a life
//     and that life is over;
//   - a session still in CREATED with no provider reference, older than the
//     grace: the provider was never told about it, so no answer can arrive.
//
// Rows another transaction holds are skipped rather than waited for: a session
// somebody's webhook is writing at this moment is being decided, which is a
// better outcome than expiry, and the next pass sees whatever is left.
func (r *Repository) OverdueSessions(ctx context.Context, q db.Querier, now, unstartedBefore time.Time, limit int) ([]DueSession, error) {
	rows, err := q.Query(ctx, `SELECT id, (expires_at IS NOT NULL AND expires_at <= $1) AS linked
		  FROM verification_sessions
		 WHERE status IN ('CREATED','PENDING_USER_ACTION','PROCESSING','REQUIRES_INPUT','MANUAL_REVIEW')
		   AND ((expires_at IS NOT NULL AND expires_at <= $1)
		        OR (status = 'CREATED' AND provider_ref IS NULL AND created_at <= $2))
		 ORDER BY created_at
		 LIMIT $3
		   FOR UPDATE SKIP LOCKED`, now.UTC(), unstartedBefore.UTC(), limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []DueSession
	for rows.Next() {
		var d DueSession
		var linked bool
		if err := rows.Scan(&d.ID, &linked); err != nil {
			return nil, mapError(err)
		}
		if linked {
			d.reason = "the hosted verification session passed the expiry the provider gave it; " +
				"starting verification again issues a new one"
		} else {
			d.reason = "this attempt was never handed to the provider, so no decision can arrive for it; " +
				"starting verification again creates a new one"
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return out, nil
}

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
