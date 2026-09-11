//go:build integration

package verification_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/verification"
)

// The expiry sweep (D-084), against the real schema.
//
// The claim is narrow and the narrowness is the point. The sweep does not decide
// anything about anybody's money: the resolver already reports a stale profile
// at its base level (D-061), and does so before any sweep has run. What the
// sweep buys is that the state column stops disagreeing with the level -- which
// is what makes §20's EXPIRED reachable and REVERIFY offerable.

// verifiedProfile puts a person in VERIFIED with a window ending at expiresAt,
// through the transition row that is the only thing 00761 lets write the state.
func verifiedProfile(t *testing.T, d *db.DB, expiresAt time.Time) accounts.UserID {
	t.Helper()
	ctx := context.Background()
	user, err := accounts.NewRepository().CreateUser(ctx, d,
		"expiry-itest", "sub-"+uuid.NewString(), nil)
	require.NoError(t, err)
	_, err = d.Exec(ctx, `INSERT INTO compliance_profiles (user_id, identity_state) VALUES ($1, 'UNVERIFIED')`, user.ID)
	require.NoError(t, err)
	_, err = d.Exec(ctx, `INSERT INTO compliance_profile_transitions
		(id, user_id, from_state, to_state, actor_type, actor_id, reason, verified_at, expires_at)
		VALUES ($1::uuid, $2, 'UNVERIFIED', 'VERIFIED', 'SYSTEM', 'itest', 'the provider decided', $3, $4)`,
		uuid.NewString(), user.ID, expiresAt.Add(-verification.ValidityWindow), expiresAt)
	require.NoError(t, err)
	return user.ID
}

func stateOf(t *testing.T, d *db.DB, user accounts.UserID) string {
	t.Helper()
	var state string
	require.NoError(t, d.QueryRow(context.Background(),
		`SELECT identity_state FROM compliance_profiles WHERE user_id = $1`, user).Scan(&state))
	return state
}

// TestIntegration_TheSweepExpiresOnlyWhatIsOverdue.
func TestIntegration_TheSweepExpiresOnlyWhatIsOverdue(t *testing.T) {
	requireEnv(t)
	d := testDB
	svc, _ := newService(t, true, time.Now)
	now := time.Now().UTC()

	overdue := verifiedProfile(t, d, now.Add(-time.Hour))
	current := verifiedProfile(t, d, now.Add(24*time.Hour))

	moved, err := svc.ExpireOverdue(context.Background(), d, now, verification.SweepBatch)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, moved, 1)

	assert.Equal(t, "EXPIRED", stateOf(t, d, overdue))
	assert.Equal(t, "VERIFIED", stateOf(t, d, current),
		"a decision inside its window is not the sweep's business")

	// The trail says a clock did this, and says it is not a rejection.
	var actorType, actorID, reason, fromState string
	require.NoError(t, d.QueryRow(context.Background(),
		`SELECT actor_type, actor_id, reason, from_state FROM compliance_profile_transitions
		  WHERE user_id = $1 AND to_state = 'EXPIRED'`, overdue).Scan(&actorType, &actorID, &reason, &fromState))
	assert.Equal(t, "SYSTEM", actorType)
	assert.Equal(t, "verification:expiry-sweep", actorID)
	assert.Equal(t, "VERIFIED", fromState)
	assert.Contains(t, reason, "not a rejection")

	// And the verified_at the profile carried is cleared by 00761's trigger,
	// so nothing reads a stale decision as a current fact.
	var verifiedAt *time.Time
	require.NoError(t, d.QueryRow(context.Background(),
		`SELECT verified_at FROM compliance_profiles WHERE user_id = $1`, overdue).Scan(&verifiedAt))
	assert.Nil(t, verifiedAt)
}

// TestIntegration_TheSweepIsIdempotentAndFindsNothingTwice.
func TestIntegration_TheSweepIsIdempotentAndFindsNothingTwice(t *testing.T) {
	requireEnv(t)
	d := testDB
	svc, _ := newService(t, true, time.Now)
	now := time.Now().UTC()
	user := verifiedProfile(t, d, now.Add(-time.Hour))

	first, err := svc.ExpireOverdue(context.Background(), d, now, verification.SweepBatch)
	require.NoError(t, err)
	require.GreaterOrEqual(t, first, 1)

	second, err := svc.ExpireOverdue(context.Background(), d, now, verification.SweepBatch)
	require.NoError(t, err)
	assert.Zero(t, second, "a second pass over the same database moves nothing")
	assert.Equal(t, "EXPIRED", stateOf(t, d, user))

	var rows int
	require.NoError(t, d.QueryRow(context.Background(),
		`SELECT count(*) FROM compliance_profile_transitions WHERE user_id = $1 AND to_state = 'EXPIRED'`,
		user).Scan(&rows))
	assert.Equal(t, 1, rows, "one expiry, one transition row")
}

// TestIntegration_TheResolverDidNotNeedTheSweep.
//
// D-061's load-bearing claim, asserted rather than trusted: the level is already
// the base one BEFORE any sweep runs, and running the sweep does not change it.
// If this ever stopped being true, an expired verification would pay somebody
// out for as long as the sweep was late.
func TestIntegration_TheResolverDidNotNeedTheSweep(t *testing.T) {
	requireEnv(t)
	d := testDB
	svc, _ := newService(t, true, time.Now)
	now := time.Now().UTC()
	user := verifiedProfile(t, d, now.Add(-time.Hour))

	resolver, err := verification.NewResolver(
		verification.StaticBase(valuedomain.VerificationNodalIdentity),
		verification.NewRepository(), d, fixedClock(now),
	)
	require.NoError(t, err)

	before, err := resolver.LevelForUser(context.Background(), user, valuedomain.VerificationNodalIdentity)
	require.NoError(t, err)
	assert.Equal(t, valuedomain.VerificationNodalIdentity, before,
		"an elapsed window reports the base level while the state still says VERIFIED")
	require.Equal(t, "VERIFIED", stateOf(t, d, user), "and the state has not been swept yet")

	_, err = svc.ExpireOverdue(context.Background(), d, now, verification.SweepBatch)
	require.NoError(t, err)

	after, err := resolver.LevelForUser(context.Background(), user, valuedomain.VerificationNodalIdentity)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the sweep tidies the state and decides nothing")
}

// TestIntegration_TheSweepLeavesEveryOtherStateAlone.
//
// RESTRICTED, SUSPENDED and REJECTED are decisions a person made. A clock must
// not quietly undo one, and a profile that never reached VERIFIED has no window
// to elapse.
func TestIntegration_TheSweepLeavesEveryOtherStateAlone(t *testing.T) {
	requireEnv(t)
	d := testDB
	svc, _ := newService(t, true, time.Now)
	now := time.Now().UTC()

	user := verifiedProfile(t, d, now.Add(-time.Hour))
	_, err := d.Exec(context.Background(), `INSERT INTO compliance_profile_transitions
		(id, user_id, from_state, to_state, actor_type, actor_id, reason)
		VALUES ($1::uuid, $2, 'VERIFIED', 'RESTRICTED', 'OPERATOR', 'op-1', 'a sanctions review')`,
		uuid.NewString(), user)
	require.NoError(t, err)
	require.Equal(t, "RESTRICTED", stateOf(t, d, user))

	moved, err := svc.ExpireOverdue(context.Background(), d, now, verification.SweepBatch)
	require.NoError(t, err)
	assert.Zero(t, moved)
	assert.Equal(t, "RESTRICTED", stateOf(t, d, user),
		"an operator's restriction outranks a clock")
}

// The session expiry sweep (F-170), against the real schema.
//
// Migration 00762 permits exactly ONE open verification session per person. A
// hosted link past its expiry is not open in any sense the person can use --
// the provider will not accept it and Poll gets nothing new -- but its STATUS
// still said open, and nothing in any binary ever moved it. So the index that
// exists to stop two attempts racing was, for that person, an index that
// stopped them verifying at all. §20's "your link expired, start again" was a
// sentence the product could not honour.
//
// The other half is the session that was never handed to a provider: the row is
// written BEFORE the call so a crash leaves something to reconcile, Resume
// refuses it, and no provider answer can ever arrive for it. It blocks its owner
// in exactly the same way and nothing could close it either.

// openSession writes an attempt with the given status, provider reference and
// hosted-link expiry, aged by `age`.
func openSession(t *testing.T, d *db.DB, user accounts.UserID, status, providerRef string, expiresAt *time.Time, age time.Duration) verification.SessionID {
	t.Helper()
	sid := verification.NewSessionID()
	_, err := d.Exec(context.Background(), `INSERT INTO verification_sessions
		(id, user_id, purpose, provider, status, rules_version, environment, sandbox, provider_ref, expires_at, created_at)
		VALUES ($1,$2,'PAYOUT_KYC','expiry-itest','CREATED','v1','LOCAL',false,NULLIF($3,''),$4, now() - $5::interval)`,
		sid, user, providerRef, expiresAt, age.String())
	require.NoError(t, err)
	if status != "CREATED" {
		_, err = d.Exec(context.Background(), `INSERT INTO verification_session_transitions
			(id, session_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
			VALUES ($1,$2,'CREATED',$3,'SYSTEM','expiry-itest','fixture',now())`,
			verification.NewTransitionID(), sid, status)
		require.NoError(t, err)
	}
	return sid
}

func sessionStatusOf(t *testing.T, d *db.DB, sid verification.SessionID) string {
	t.Helper()
	var status string
	require.NoError(t, d.QueryRow(context.Background(),
		`SELECT status FROM verification_sessions WHERE id = $1`, sid).Scan(&status))
	return status
}

func aUser(t *testing.T, d *db.DB) accounts.UserID {
	t.Helper()
	u, err := accounts.NewRepository().CreateUser(context.Background(), d, "expiry-itest", "sub-"+uuid.NewString(), nil)
	require.NoError(t, err)
	return u.ID
}

// TestIntegration_TheSessionSweepClosesWhatCannotBeDecided.
func TestIntegration_TheSessionSweepClosesWhatCannotBeDecided(t *testing.T) {
	requireEnv(t)
	d := testDB
	svc, _ := newService(t, true, time.Now)
	now := time.Now().UTC()

	ranOut := aUser(t, d)
	link := openSession(t, d, ranOut, "PENDING_USER_ACTION", "prov_ran_out", ptr(now.Add(-time.Minute)), time.Hour)

	live := aUser(t, d)
	liveLink := openSession(t, d, live, "PENDING_USER_ACTION", "prov_live", ptr(now.Add(time.Hour)), time.Minute)

	stranded := aUser(t, d)
	strandedSession := openSession(t, d, stranded, "CREATED", "", nil, verification.UnstartedSessionGrace+time.Minute)

	fresh := aUser(t, d)
	freshSession := openSession(t, d, fresh, "CREATED", "", nil, time.Minute)

	moved, err := svc.ExpireOverdueSessions(context.Background(), d, now, verification.SweepBatch)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, moved, 2)

	assert.Equal(t, "EXPIRED", sessionStatusOf(t, d, link), "a hosted link past its expiry stayed open")
	assert.Equal(t, "EXPIRED", sessionStatusOf(t, d, strandedSession),
		"an attempt the provider was never told about stayed open, and its owner stayed blocked")
	assert.Equal(t, "PENDING_USER_ACTION", sessionStatusOf(t, d, liveLink), "a live link was closed under somebody")
	assert.Equal(t, "CREATED", sessionStatusOf(t, d, freshSession),
		"an attempt seconds old was expired before the provider call could land")

	// The status moved through a transition row, which is the only thing that
	// can write it, and the row says a clock did this and why.
	var actorType, actorID, from, reason string
	require.NoError(t, d.QueryRow(context.Background(),
		`SELECT actor_type, actor_id, from_status, reason FROM verification_session_transitions
		  WHERE session_id = $1 AND to_status = 'EXPIRED'`, link).Scan(&actorType, &actorID, &from, &reason))
	assert.Equal(t, "SYSTEM", actorType)
	assert.Equal(t, "verification:session-expiry-sweep", actorID)
	assert.Equal(t, "PENDING_USER_ACTION", from)
	assert.Contains(t, reason, "expiry")

	// And the point of all of it: the person can start again.
	repo := verification.NewRepository()
	_, open, err := repo.OpenSession(context.Background(), d, ranOut)
	require.NoError(t, err)
	assert.False(t, open, "the one-open-session index still holds this person's verification shut")

	// A second pass finds nothing and moves nothing: the sweep converges.
	again, err := svc.ExpireOverdueSessions(context.Background(), d, now, verification.SweepBatch)
	require.NoError(t, err)
	assert.Zero(t, again, "the sweep expired something twice")
}

// TestIntegration_TheSessionSweepLeavesADecidedSessionAlone: a session the
// provider answered is terminal, and a clock must not describe it as having
// run out.
func TestIntegration_TheSessionSweepLeavesADecidedSessionAlone(t *testing.T) {
	requireEnv(t)
	d := testDB
	svc, _ := newService(t, true, time.Now)
	now := time.Now().UTC()

	user := aUser(t, d)
	decided := openSession(t, d, user, "PROCESSING", "prov_decided", ptr(now.Add(-time.Hour)), time.Hour)
	_, err := d.Exec(context.Background(), `INSERT INTO verification_session_transitions
		(id, session_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		VALUES ($1,$2,'PROCESSING','APPROVED','SYSTEM','expiry-itest','the provider decided',now())`,
		verification.NewTransitionID(), decided)
	require.NoError(t, err)

	_, err = svc.ExpireOverdueSessions(context.Background(), d, now, verification.SweepBatch)
	require.NoError(t, err)
	assert.Equal(t, "APPROVED", sessionStatusOf(t, d, decided),
		"a decided session was overwritten by a clock")
}

func ptr(t time.Time) *time.Time { return &t }
