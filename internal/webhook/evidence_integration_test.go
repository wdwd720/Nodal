//go:build integration

package webhook_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
)

// Provider evidence is immutable in the database, not only by convention.
//
// 00107 called provider_events "the evidence record" and had nothing enforcing
// it: no trigger, and a table-wide UPDATE grant to cp_app. What held the
// invariant up was that the one caller -- this package -- writes only the four
// lifecycle columns. That is F-49's shape, and the harm it leaves open is
// specific: this table is what you argue with a provider from when they
// dispute what they sent, and evidence the disputing party's own software can
// rewrite is not evidence.
//
// 00719 states it twice, with a column-scoped grant and a guard trigger. This
// test observes both, separately, because they fail differently: the grant
// answers with a permission error to cp_app, the trigger with LG003 to
// everybody including the owner. A test that only drove cp_app would pass on
// the grant alone and never learn whether the trigger existed.

func TestIntegration_ProviderEvidenceCannotBeRewritten(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A real delivery, so the row under test is one the production path made.
	eventID := "evt_" + uuid.NewString()
	raw, headers := f.provider.Webhook(eventID, "session.updated", f.session)
	status, _ := f.deliver(raw, headers)
	require.Equal(t, http.StatusOK, status)
	got, _, found := f.providerEvent(eventID)
	require.True(t, found, "the delivery must have written the evidence row this test mutates")
	require.Equal(t, "PROCESSED", *got)

	evidence := map[string]string{
		"payload_hash":       `payload_hash = '\x00'::bytea`,
		"signature_verified": `signature_verified = false`,
		"raw_ref":            `raw_ref = 'file:///somewhere-else.json'`,
		"provider_event_id":  `provider_event_id = 'a-different-event'`,
		"received_at":        `received_at = now() - interval '1 day'`,
		"verification_error": `verification_error = 'invented after the fact'`,
	}

	// The trigger. Driven as the owner, which every grant is irrelevant to --
	// so a refusal here is the trigger and nothing else.
	for col, set := range evidence {
		t.Run("trigger/"+col, func(t *testing.T) {
			err := testMigrate.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
				func(ctx context.Context, tx pgx.Tx) error {
					_, e := tx.Exec(ctx, `UPDATE provider_events SET `+set+
						` WHERE provider = $1 AND provider_event_id = $2`, f.provider.Name(), eventID)
					return e
				})
			require.Error(t, err, "the owner rewrote %s; the guard trigger is not there", col)
			assert.Contains(t, err.Error(), "PROVIDER_EVENT_IMMUTABLE")
			assert.True(t, db.IsImmutableRow(err), "the refusal must classify as an immutable row (LG003), not an unknown error")
		})
	}

	// The grant. Driven as the application role, which is the role a defect or
	// an intruder in this codebase would actually hold.
	for col, set := range evidence {
		t.Run("grant/"+col, func(t *testing.T) {
			err := testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
				func(ctx context.Context, tx pgx.Tx) error {
					_, e := tx.Exec(ctx, `UPDATE provider_events SET `+set+
						` WHERE provider = $1 AND provider_event_id = $2`, f.provider.Name(), eventID)
					return e
				})
			require.Error(t, err, "cp_app rewrote %s", col)
		})
	}

	// Deletion, which no role but the owner could even attempt.
	err := testMigrate.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `DELETE FROM provider_events WHERE provider = $1 AND provider_event_id = $2`,
				f.provider.Name(), eventID)
			return e
		})
	require.Error(t, err, "provider evidence was deleted")
	assert.Contains(t, err.Error(), "provider evidence is never deleted")

	// The evidence is still exactly what arrived.
	var (
		verified bool
		verr     *string
		rawRef   *string
	)
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT signature_verified, verification_error, raw_ref FROM provider_events
		  WHERE provider = $1 AND provider_event_id = $2`, f.provider.Name(), eventID).
		Scan(&verified, &verr, &rawRef))
	assert.True(t, verified)
	assert.Nil(t, verr)
	assert.NotNil(t, rawRef)
}

// TestIntegration_TheLifecycleColumnsStillMove is the positive control. A guard
// that refused everything would pass every assertion above and break the
// webhook pipeline entirely -- and the pipeline is what writes the evidence in
// the first place, so the failure would look like an empty table rather than an
// error. The four columns the handler needs must still be writable by cp_app,
// and the whole delivery path must still work.
func TestIntegration_TheLifecycleColumnsStillMove(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventID := "evt_" + uuid.NewString()
	raw, headers := f.provider.Webhook(eventID, "session.updated", f.session)
	status, _ := f.deliver(raw, headers)
	require.Equal(t, http.StatusOK, status)

	// The handler's own UPDATE reached PROCESSED, which is the pipeline proving
	// itself: the INSERT writes RECEIVED, and only the UPDATE writes PROCESSED.
	got, _, found := f.providerEvent(eventID)
	require.True(t, found)
	require.Equal(t, "PROCESSED", *got, "the lifecycle UPDATE did not run")

	// And the same columns by hand, as cp_app, which is the grant being wide
	// enough rather than the handler being lucky.
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `UPDATE provider_events
				SET processing_status = 'FAILED', processed_at = now(), error = 'a later failure',
				    request_id = 'req-2', canonical_event_id = gen_random_uuid()
				WHERE provider = $1 AND provider_event_id = $2`, f.provider.Name(), eventID)
			return e
		}), "cp_app must still be able to move the lifecycle columns")
}
