//go:build integration

package compliance_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

func openDB(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(context.Background(), db.Config{URL: url, MaxConns: 4, MinConns: 1, AppName: "compliance-test"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

func TestIntegration_Compliance_UpsertAuditsAndRejectsSelfAttestation(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	u, err := accounts.NewRepository().CreateUser(ctx, d.Pool(), "https://idp.test", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	writer := audit.NewWriter()
	repo := compliance.NewRepository(writer)

	_, err = repo.Get(ctx, d.Pool(), u.ID)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err), "missing profile is NOT_FOUND (eligibility fails closed)")

	now := time.Now().UTC().Truncate(time.Microsecond)
	var p compliance.Profile
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = repo.Upsert(ctx, tx, compliance.Profile{
			UserID: u.ID, IdentityState: compliance.IdentityVerified, AgeVerified: true, JurisdictionCountry: "US", JurisdictionRegion: "CA",
			ResidencyCountry: "US", SanctionsState: compliance.SanctionsClear, Provider: "kyc-provider", ProviderRef: "ver_123",
			PolicyVersion: "kyc-v1", Restrictions: []string{"no_leverage"}, VerifiedAt: &now,
		}, compliance.Change{ActorType: security.ActorSystem, ActorID: "kyc-webhook", Reason: "provider verification completed", CorrelationID: "corr-1"})
		return err
	}))
	// Migration 00761 took the state half of the profile out of this package's
	// reach: a profile is born UNVERIFIED and reaches every other state through
	// a transition row, written by internal/verification. Upsert therefore
	// IGNORES the IdentityState and VerifiedAt asked for above and returns what
	// the row actually says, which is the stronger property -- an application
	// statement can no longer make somebody verified.
	assert.Equal(t, compliance.IdentityUnverified, p.IdentityState,
		"Upsert writes the attribute half of the profile and never its state")
	assert.Nil(t, p.VerifiedAt)
	assert.Equal(t, []string{"no_leverage"}, p.Restrictions)
	assert.Equal(t, "US", p.JurisdictionCountry, "the attributes it does own are written")
	assert.True(t, p.AgeVerified)
	assert.Equal(t, compliance.SanctionsClear, p.SanctionsState)

	// Update by an operator; audit trail grows with before/after hashes.
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		p.SanctionsState = compliance.SanctionsReview
		_, err := repo.Upsert(ctx, tx, p, compliance.Change{ActorType: security.ActorOperator, ActorID: "op-1", Reason: "manual review opened"})
		return err
	}))
	got, err := repo.Get(ctx, d.Pool(), u.ID)
	require.NoError(t, err)
	assert.Equal(t, compliance.SanctionsReview, got.SanctionsState)
	var n int
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_type = 'compliance_profile' AND resource_id = $1`, u.ID.String()).Scan(&n))
	assert.Equal(t, 2, n)
	var withBefore int
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_type = 'compliance_profile' AND resource_id = $1 AND before_hash IS NOT NULL`, u.ID.String()).Scan(&withBefore))
	assert.Equal(t, 1, withBefore, "second write carries the before hash")

	// Customers and agents cannot self-attest.
	for _, actor := range []security.ActorType{security.ActorUser, security.ActorAgent, security.ActorService} {
		err := d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := repo.Upsert(ctx, tx, p, compliance.Change{ActorType: actor, ActorID: "x", Reason: "self"})
			return err
		})
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), string(actor))
	}

	// Validation fails closed on unknown enums and bad country codes.
	bad := p
	bad.IdentityState = "MAYBE"
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(bad.Validate()))
	bad = p
	bad.JurisdictionCountry = "usa"
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(bad.Validate()))
	bad = p
	bad.IdentityState = compliance.IdentityVerified
	bad.VerifiedAt = nil
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(bad.Validate()),
		"a VERIFIED profile read back without a verified_at is internally inconsistent")

	// And the privilege the migration revoked is genuinely gone: the
	// application role cannot write the state column by any statement.
	_, err = d.Pool().Exec(ctx,
		`UPDATE compliance_profiles SET identity_state = 'VERIFIED' WHERE user_id = $1`, u.ID)
	require.Error(t, err, "cp_app must not be able to write a verification state")
}

// TestIntegration_Compliance_ASanctionsScreenIsRecordedAsADecision.
//
// 00761 took the verification state out of this package's reach and left the
// sanctions screen in the attribute grant beside it. The screen is not an
// attribute: internal/eligibility reads it as one of the allowlists that decides
// whether a payout may proceed, and verification derives it from a provider's
// sanctions and PEP checks. One UPDATE changed it, and nothing recorded who,
// when, on what evidence, or from which value (F-168).
//
// Upsert still takes a Profile with a sanctions state in it -- a caller says
// what the screen now is and the change is recorded for it -- and what it can no
// longer do is change the screen without saying who decided and why.
func TestIntegration_Compliance_ASanctionsScreenIsRecordedAsADecision(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	repo := compliance.NewRepository(audit.NewWriter())
	u, err := accounts.NewRepository().CreateUser(ctx, d.Pool(), "https://idp.test", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)

	upsert := func(state compliance.SanctionsState, reason string) compliance.Profile {
		t.Helper()
		var out compliance.Profile
		require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var uerr error
			out, uerr = repo.Upsert(ctx, tx, compliance.Profile{
				UserID: u.ID, JurisdictionCountry: "US", ResidencyCountry: "US", SanctionsState: state,
			}, compliance.Change{
				ActorType: security.ActorSystem, ActorID: "verification:test-provider",
				Reason: reason, CorrelationID: "corr-screen",
			})
			return uerr
		}))
		return out
	}

	// Born UNKNOWN: nothing was decided, so there is nothing to record.
	born := upsert(compliance.SanctionsUnknown, "profile created")
	assert.Equal(t, compliance.SanctionsUnknown, born.SanctionsState)
	assert.Zero(t, screenRows(t, d, u.ID), "a profile nobody screened recorded a screening decision")

	// A screen that arrives is a decision, and the row that carries it says who
	// decided, why, and what it moved from.
	cleared := upsert(compliance.SanctionsClear, "provider sanctions and PEP checks passed")
	assert.Equal(t, compliance.SanctionsClear, cleared.SanctionsState,
		"Upsert returns the row as it stands, and the trigger wrote the screen")
	require.Equal(t, 1, screenRows(t, d, u.ID))

	var from, to, actorType, actorID, reason, fromState, toState string
	require.NoError(t, d.Pool().QueryRow(ctx, `
		SELECT from_sanctions_state, to_sanctions_state, actor_type, actor_id, reason, from_state, to_state
		  FROM compliance_profile_transitions
		 WHERE user_id = $1 AND to_sanctions_state IS NOT NULL`, u.ID).
		Scan(&from, &to, &actorType, &actorID, &reason, &fromState, &toState))
	assert.Equal(t, "UNKNOWN", from)
	assert.Equal(t, "CLEAR", to)
	assert.Equal(t, "SYSTEM", actorType)
	assert.Equal(t, "verification:test-provider", actorID)
	assert.Equal(t, "provider sanctions and PEP checks passed", reason)
	assert.Equal(t, fromState, toState,
		"a row about the screen moves the verification state nowhere")

	// Writing the same screen again decides nothing and records nothing.
	upsert(compliance.SanctionsClear, "the same provider answer, redelivered")
	assert.Equal(t, 1, screenRows(t, d, u.ID), "a redelivery recorded a second decision")

	// And the privilege is gone: no statement this role can write moves it.
	_, err = d.Pool().Exec(ctx,
		`UPDATE compliance_profiles SET sanctions_state = 'HIT' WHERE user_id = $1`, u.ID)
	require.Error(t, err, "cp_app must not be able to write a sanctions screen")
	assert.Contains(t, err.Error(), "permission denied")
}

// TestIntegration_Compliance_AProfileBornScreenedRecordsIt: a birth is not a
// change, so no binding about changes applies to it -- which is the hole F-122
// found in the verification state and the same hole the screen had. The
// database records the birth itself, because the row can be INSERTed by
// anything holding INSERT on the table.
func TestIntegration_Compliance_AProfileBornScreenedRecordsIt(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	u, err := accounts.NewRepository().CreateUser(ctx, d.Pool(), "https://idp.test", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)

	_, err = d.Pool().Exec(ctx,
		`INSERT INTO compliance_profiles (user_id, identity_state, sanctions_state, age_verified)
		 VALUES ($1,'UNVERIFIED','HIT',false)`, u.ID)
	require.NoError(t, err, "a profile may be born already screened; it may not be born unrecorded")

	var from, to, actorID string
	require.NoError(t, d.Pool().QueryRow(ctx, `
		SELECT from_sanctions_state, to_sanctions_state, actor_id
		  FROM compliance_profile_transitions
		 WHERE user_id = $1 AND to_sanctions_state IS NOT NULL`, u.ID).Scan(&from, &to, &actorID))
	assert.Equal(t, "UNKNOWN", from)
	assert.Equal(t, "HIT", to)
	assert.Equal(t, "compliance:profile-created", actorID)

	// The id is one this system can read back. A v4 there would be a row the
	// database can write and internal/id refuses to parse (F-131).
	var raw string
	require.NoError(t, d.Pool().QueryRow(ctx,
		`SELECT id::text FROM compliance_profile_transitions WHERE user_id = $1`, u.ID).Scan(&raw))
	_, perr := id.ParseAny(raw)
	assert.NoError(t, perr, "the birth row's id is not an RFC 9562 version 7 UUID")
}

func screenRows(t *testing.T, d *db.DB, userID accounts.UserID) int {
	t.Helper()
	var n int
	require.NoError(t, d.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM compliance_profile_transitions
		  WHERE user_id = $1 AND to_sanctions_state IS NOT NULL`, userID).Scan(&n))
	return n
}
