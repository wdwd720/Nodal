//go:build integration

package verification_test

// Reproduction for the withdrawal-verification audit (goal §54, F-wv-7), kept
// after the fix and inverted: every assertion below asserted the defect when it
// was written and asserts the closed hole now (F-227, D-121, migration 00806).
//
// docs/product/VERIFICATION_AND_WITHDRAWAL.md §4:
//
//	"The absence that matters: nothing reaches VERIFIED except from PENDING
//	 (a provider decided), from RESTRICTED (a limitation was lifted) or from
//	 SUSPENDED ... A person cannot become verified without a provider session
//	 having decided so, and that is a property of the transition table rather
//	 than of the code that reads it."
//
// It was a property of internal/verification.CanTransition and of nothing else.
// Migration 00761 binds a state change to a transition row that names the true
// origin (00731's edge binding), and 00796 does the same for the screen -- but
// no database object said which edges exist. A transition row whose from_state
// is the state the profile is actually in licensed ANY destination, including
// the six §20 forbids, and the SECURITY DEFINER trigger wrote identity_state,
// verified_at, expires_at and sanctions_state from it.
//
// The existing suite probes the two cases that were already closed (the Go path
// refuses the edge; a row claiming an origin the profile was never in licenses
// nothing). Neither of them is this one, and this is now the third.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/verification"
)

func TestAuditWV_TheProfileTransitionTableConstrainsWhichEdgesAreLegal(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	userID, accountID := newAccount(t)
	ensureProfile(t, userID)

	repo := verification.NewRepository()
	state, _, _, err := repo.ProfileState(ctx, testDB, userID)
	require.NoError(t, err)
	require.Equal(t, verification.StateUnverified, state, "fixture check: born UNVERIFIED")
	require.False(t, verification.CanTransition(verification.StateUnverified, verification.StateVerified),
		"fixture check: §20 has no UNVERIFIED -> VERIFIED edge")

	// 1. One INSERT, as cp_app, naming the origin the profile really is in.
	verifiedAt := time.Now().UTC()
	_, err = testDB.Exec(ctx, `INSERT INTO compliance_profile_transitions
		(id, user_id, from_state, to_state, from_sanctions_state, to_sanctions_state,
		 actor_type, actor_id, reason, verified_at, expires_at, occurred_at)
		VALUES ($1,$2,'UNVERIFIED','VERIFIED','UNKNOWN','CLEAR','SYSTEM','audit-probe',
		        'no provider session decided anything', $3, $4, $3)`,
		uuid.New(), userID, verifiedAt, verifiedAt.Add(365*24*time.Hour))

	require.Error(t, err,
		"F-227: the database accepted an edge §20 does not have, and the trigger wrote VERIFIED "+
			"and a CLEAR sanctions screen from it")
	assert.Equal(t, "AD001", db.SQLState(err), "got %v", err)
	assert.Contains(t, err.Error(), "COMPLIANCE_TRANSITION_ILLEGAL_EDGE")

	after, gotVerifiedAt, gotExpiry, serr := repo.ProfileState(ctx, testDB, userID)
	require.NoError(t, serr)
	assert.Equal(t, verification.StateUnverified, after,
		"the refused edge must leave the standing where it was")
	assert.Nil(t, gotVerifiedAt)
	assert.Nil(t, gotExpiry)

	// The positive control. An edge table that refused everything would pass
	// the assertion above while breaking every real verification, so a legal
	// edge must still commit.
	_, err = testDB.Exec(ctx, `INSERT INTO compliance_profile_transitions
		(id, user_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
		VALUES ($1,$2,'UNVERIFIED','REQUIRED','SYSTEM','audit-probe',
		        'a legal edge still commits', now())`,
		uuid.New(), userID)
	require.NoError(t, err, "the edge table refused an edge §20 does have")
	after, _, _, serr = repo.ProfileState(ctx, testDB, userID)
	require.NoError(t, serr)
	require.Equal(t, verification.StateRequired, after)

	// 2. The evidence a level rests on is INSERTable too: cp_app holds INSERT
	// on verification_sessions, verification_session_transitions and
	// verification_checks. A session may not go CREATED -> APPROVED in one row
	// either.
	sessionID := verification.NewSessionID()
	_, serr = testDB.Exec(ctx, `INSERT INTO verification_sessions
		(id, user_id, purpose, provider, provider_ref, status, jurisdiction_country,
		 jurisdiction_region, rules_version, environment, sandbox)
		VALUES ($1,$2,'PAYOUT_KYC','sandbox_verification',$3,'CREATED','US','CA',
		        'verification-rules-v1-us-only','TEST',false)`,
		sessionID, userID, "audit-forged-"+uuid.NewString())
	require.NoError(t, serr)

	_, serr = testDB.Exec(ctx, `INSERT INTO verification_session_transitions
		(id, session_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1,$2,'CREATED','APPROVED','SYSTEM','audit-probe','no provider was ever called')`,
		uuid.New(), sessionID)
	require.Error(t, serr,
		"F-227: a verification session went CREATED -> APPROVED in one INSERT; "+
			"verification.CanTransitionSession has no such edge")
	assert.Equal(t, "AD001", db.SQLState(serr), "got %v", serr)
	assert.Contains(t, serr.Error(), "VERIFICATION_TRANSITION_ILLEGAL_EDGE")

	// 3. And evidence cannot be hung off the session that never got there.
	for _, kind := range []string{"IDENTITY_DOCUMENT", "AGE", "JURISDICTION", "SANCTIONS"} {
		_, cerr := testDB.Exec(ctx, `INSERT INTO verification_checks
			(id, session_id, user_id, kind, outcome, provider, provider_ref, rules_version,
			 environment, sandbox, detail)
			VALUES ($1,$2,$3,$4,'PASS','sandbox_verification','audit-forged',
			        'verification-rules-v1-us-only','TEST',false,'FORGED')`,
			verification.NewCheckID(), sessionID, userID, kind)
		require.Error(t, cerr, "a %s check attached to a session no provider has answered", kind)
		assert.Equal(t, "AD001", db.SQLState(cerr), "got %v", cerr)
		assert.Contains(t, cerr.Error(), "VERIFICATION_CHECK_UNANSWERED_SESSION")
	}

	resolver, rerr := verification.NewResolver(
		verification.StaticBase("NODAL_IDENTITY"), repo, testDB, fixedClock(time.Now().UTC()),
	)
	require.NoError(t, rerr)
	level, lerr := resolver.Level(ctx, accountID)
	require.NoError(t, lerr)
	assert.NotEqual(t, "PAYOUT_KYC", string(level),
		"F-227: PAYOUT_KYC was reached with no provider, no hosted flow and no decision; "+
			"the evidence rule rested on rows cp_app may INSERT freely")
}
