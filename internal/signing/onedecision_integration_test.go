//go:build integration

package signing_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/wallet/wallettest"
)

// One attempt carries one signing decision, and the database says so (F-81).
//
// Sign is check-then-insert: findDecision looks for a decision on the attempt
// and replays it if there is one. Without a constraint, two concurrent Sign
// calls for one attempt both find nothing and both insert -- and the rows are
// immutable, so the loser is not corrected. findDecision concedes it in its own
// SQL, `ORDER BY created_at DESC, id DESC LIMIT 1`: "the latest decision" is a
// phrase that only makes sense if there can be more than one.
//
// It matters more here than a duplicate row usually does. A decision carries
// inspected_tx_hash, and the replay path binds the bytes offered against the
// bytes that decision approved (F-67). Two decisions for one attempt are two
// different sets of approved bytes, with the binding comparing against whichever
// row sorted last.
func TestIntegration_AnAttemptCannotCarryTwoSigningDecisions(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	fake, err := wallettest.New(config.EnvTest)
	require.NoError(t, err)
	s := seedChain(t, fake, defaultSeedOptions())
	svc := newService(t, fake, chainFor(s))

	first, _, err := svc.Sign(ctx, s.request())
	require.NoError(t, err)
	require.True(t, first.Approved, "reason codes: %v", first.ReasonCodes)

	// A second decision row for the same attempt, inserted directly -- which is
	// what the losing side of the race does, with the INSERT privilege the
	// application role holds.
	var (
		planID, intentID, riskID, walletID string
		expected, inspected                []byte
	)
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT plan_id, intent_id, risk_decision_id, wallet_id, expected_tx_hash, inspected_tx_hash
			FROM signing_decisions WHERE attempt_id = $1`, s.attemptID).
		Scan(&planID, &intentID, &riskID, &walletID, &expected, &inspected))

	_, err = testDB.Exec(ctx,
		`INSERT INTO signing_decisions
			(id, attempt_id, plan_id, intent_id, risk_decision_id, wallet_id,
			 expected_tx_hash, inspected_tx_hash, decision, checks, inspector_version,
			 requested_by_service, decided_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'APPROVED', '{}'::jsonb, 'v0', 'racing-writer', now())`,
		newID(), s.attemptID, planID, intentID, riskID, walletID,
		expected, []byte("different bytes entirely"))
	require.Error(t, err, "a second signing decision was written for one attempt")
	assert.Equal(t, "23505", db.SQLState(err), "expected unique_violation, got %v", err)

	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM signing_decisions WHERE attempt_id = $1`, s.attemptID))

	// The replay path still works and still returns the one decision there is:
	// a constraint that refused legitimate replays would be worse than the hole
	// it closes.
	replayed, _, err := svc.Sign(ctx, s.request())
	require.NoError(t, err)
	assert.True(t, replayed.Replayed)
	assert.Equal(t, first.ID, replayed.ID)
}

// TestIntegration_ADifferentAttemptStillGetsItsOwnDecision is the control. A
// constraint one column too wide would refuse the second attempt of a retrying
// plan and stop execution altogether.
func TestIntegration_ADifferentAttemptStillGetsItsOwnDecision(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	fake, err := wallettest.New(config.EnvTest)
	require.NoError(t, err)

	one := seedChain(t, fake, defaultSeedOptions())
	_, _, err = newService(t, fake, chainFor(one)).Sign(ctx, one.request())
	require.NoError(t, err)

	two := seedChain(t, fake, defaultSeedOptions())
	d, _, err := newService(t, fake, chainFor(two)).Sign(ctx, two.request())
	require.NoError(t, err)
	assert.True(t, d.Approved, "reason codes: %v", d.ReasonCodes)
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM signing_decisions WHERE attempt_id = $1`, two.attemptID))
}
