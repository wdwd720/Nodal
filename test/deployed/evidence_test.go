//go:build deployed

package deployed

import (
	"context"
	"crypto/sha256"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The evidence archive is what a provider dispute is argued from. On this tier
// it lives in the same database as everything else, so "write-once" is a claim
// about privileges and a trigger rather than about an object store's retention
// lock -- and a claim like that is worth checking against the database that is
// actually deployed, not against a container that was built from the same
// migrations.

// TestDeployed_TheEvidenceOfADeliveryIsExactlyWhatWasSent.
//
// Delivered bytes, stored bytes and stored digest all have to agree. If the
// pipeline stored what it parsed rather than what it received, every dispute
// would be argued from our own interpretation of the provider's message.
func TestDeployed_TheEvidenceOfADeliveryIsExactlyWhatWasSent(t *testing.T) {
	requireSecret(t)
	d := requireDeployedDB(t)
	ctx := context.Background()

	id := uniqueEventID("evidence")
	body := stripeEvent(id, "payment_intent.succeeded", foreignIntent())

	res := deliver(t, body, time.Now())
	require.Equal(t, http.StatusOK, res.Status, "%s", res.Raw)

	got := record(t, d, id)
	require.NotEmpty(t, got.RawRef)

	stored := evidenceBody(t, d, got.RawRef)
	assert.Equal(t, body, stored, "the archived bytes are not the delivered bytes")

	// The digest stored alongside is the one a later read is checked against,
	// so it has to be the digest OF the stored bytes and not a value written
	// next to them.
	var digest []byte
	var length int
	require.NoError(t, d.QueryRow(ctx, `
		SELECT sha256, byte_len FROM provider_evidence WHERE key = $1`,
		evidenceKey(t, got.RawRef)).Scan(&digest, &length))

	want := sha256.Sum256(stored)
	assert.Equal(t, want[:], digest, "the stored digest does not describe the stored bytes")
	assert.Equal(t, len(stored), length)
}

// TestDeployed_TheApplicationRoleCannotRewriteEvidence.
//
// The role the API runs as is the role an attacker who reaches the API has.
// It must be able to insert evidence and unable to touch it afterwards -- by
// privilege, so the refusal does not depend on the application asking nicely.
//
// This runs as whichever role the test was given, which is the operations role
// rather than the application one, so it checks the GRANTS rather than trying
// the writes: what cp_app is permitted is a fact in the catalogue, and reading
// it is the honest way to assert it without connecting as cp_app.
func TestDeployed_TheApplicationRoleCannotRewriteEvidence(t *testing.T) {
	d := requireDeployedDB(t)
	ctx := context.Background()

	for _, priv := range []string{"UPDATE", "DELETE", "TRUNCATE"} {
		t.Run(priv, func(t *testing.T) {
			var granted bool
			require.NoError(t, d.QueryRow(ctx,
				`SELECT has_table_privilege('cp_app', 'provider_evidence', $1)`, priv).Scan(&granted))
			assert.False(t, granted,
				"cp_app may %s provider_evidence; evidence the disputing party can rewrite is not evidence", priv)
		})
	}

	// And it must still be able to add to the archive, or the pipeline cannot
	// preserve a delivery at all.
	var canInsert bool
	require.NoError(t, d.QueryRow(ctx,
		`SELECT has_table_privilege('cp_app', 'provider_evidence', 'INSERT')`).Scan(&canInsert))
	assert.True(t, canInsert, "cp_app cannot archive a delivery")
}

// TestDeployed_StoredEvidenceCannotBeRewrittenByAnyRoleAvailableToThisTier.
//
// The rule is stated twice, and the two statements fail differently, so both
// are attempted here against the deployed database with the real credentials
// this tier hands out.
//
// The operations role is refused by PRIVILEGE and never reaches the trigger.
// The migration role OWNS the table, so privileges do not stop it -- and that
// is the account a mistake is most likely to be made from, which is why there
// is a trigger at all. It answers everybody, including the owner, with LG003.
//
// A test that only drove one of them would pass while the other guarantee was
// missing entirely.
func TestDeployed_StoredEvidenceCannotBeRewrittenByAnyRoleAvailableToThisTier(t *testing.T) {
	requireSecret(t)
	d := requireDeployedDB(t)
	ctx := context.Background()

	// A row this test just created, so even a failure to refuse would damage
	// only the evidence of a foreign payment that was ignored.
	id := uniqueEventID("guard")
	body := stripeEvent(id, "payment_intent.succeeded", foreignIntent())
	require.Equal(t, http.StatusOK, deliver(t, body, time.Now()).Status)
	key := evidenceKey(t, record(t, d, id).RawRef)

	t.Run("the operations role is stopped by privilege", func(t *testing.T) {
		_, err := d.Exec(ctx, `UPDATE provider_evidence SET body = $2 WHERE key = $1`, key, []byte("rewritten"))
		require.Error(t, err, "the archive accepted a rewrite of stored evidence")
		assert.Contains(t, err.Error(), "42501", "refused, but not by the privilege that is supposed to refuse it")
	})

	t.Run("the owning role is stopped by the trigger", func(t *testing.T) {
		owner := requireOwnerDB(t)

		var isOwner bool
		require.NoError(t, owner.QueryRow(ctx,
			`SELECT has_table_privilege(current_user, 'provider_evidence', 'UPDATE')`).Scan(&isOwner))
		require.True(t, isOwner,
			"this role cannot UPDATE the table at all, so it cannot show whether the trigger would have stopped it")

		_, err := owner.Exec(ctx, `UPDATE provider_evidence SET body = $2 WHERE key = $1`, key, []byte("rewritten"))
		require.Error(t, err, "the table owner rewrote stored evidence")
		assert.Contains(t, err.Error(), "LG003", "refused, but not by the guard trigger")

		_, err = owner.Exec(ctx, `DELETE FROM provider_evidence WHERE key = $1`, key)
		require.Error(t, err, "the table owner deleted stored evidence")
		assert.Contains(t, err.Error(), "LG003")
	})

	// And through every attempt, the object is still exactly what was
	// delivered.
	assert.Equal(t, body, evidenceBody(t, d, "pg://provider_evidence/"+key))
}

// TestDeployed_ProviderEventsCannotBeRewrittenEither.
//
// provider_events is the other half of the record: what was received, when,
// whether its signature verified, and what was done about it. Rewriting a
// processing_status would make a reconciliation report agree with a story
// rather than with what happened.
func TestDeployed_ProviderEventsCannotBeRewrittenEither(t *testing.T) {
	d := requireDeployedDB(t)
	ctx := context.Background()

	// cp_app writes the four lifecycle columns and nothing else, so the grant
	// is column-scoped rather than absent. What must not be grantable is the
	// evidence itself: the payload hash, the reference and the verification
	// flag.
	for _, col := range []string{"payload_hash", "raw_ref", "signature_verified", "provider_event_id"} {
		t.Run(col, func(t *testing.T) {
			var granted bool
			require.NoError(t, d.QueryRow(ctx,
				`SELECT has_column_privilege('cp_app', 'provider_events', $1, 'UPDATE')`, col).Scan(&granted))
			assert.False(t, granted,
				"cp_app may rewrite provider_events.%s, which is part of what the record is FOR", col)
		})
	}
}
