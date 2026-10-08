//go:build integration

package risk

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
)

// TestIntegration_AdvancedDecisionsArePersistedImmutablyAndUntampered: the
// decision a service stores is the one the kernel produced, and once stored
// it is history.
func TestIntegration_AdvancedDecisionsArePersistedImmutablyAndUntampered(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	in := openPosition()
	in.Intent.ID = ""        // no economic_intents row in this package's fixtures
	in.Intent.AccountID = "" // likewise; both columns are nullable
	d := EvaluateAdvanced(DefaultAdvancedPolicy(), in)

	s := NewStore()
	var id DecisionID
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = s.RecordAdvancedDecision(ctx, tx, in, d, "corr-1")
		return err
	}))
	var verdict, hash string
	var codes []string
	require.NoError(t, testDB.QueryRow(ctx, `SELECT verdict, decision_hash, reason_codes FROM advanced_risk_decisions WHERE id = $1`, id).
		Scan(&verdict, &hash, &codes))
	assert.Equal(t, string(d.Verdict), verdict)
	assert.Equal(t, d.Hash, hash)
	assert.Equal(t, d.ReasonCodes, codes)

	// Immutable.
	_, err := testDB.Exec(ctx, `UPDATE advanced_risk_decisions SET verdict = 'ALLOW' WHERE id = $1`, id)
	require.Error(t, err)

	// A decision edited after evaluation is refused before any SQL.
	tampered := d
	tampered.Verdict = Allow
	tampered.ReasonCodes = nil
	err = testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.RecordAdvancedDecision(ctx, tx, in, tampered, "")
		return err
	})
	require.Error(t, err)

	// The database refuses a verdict that disagrees with its reasons even when
	// Go is bypassed.
	_, err = testDB.Exec(ctx, `INSERT INTO advanced_risk_decisions (id, intent_kind, stage, verdict, reason_codes, input_hash,
		policy_version, policy_hash, evaluator_version, decision_hash, input, decision, evaluated_at)
		VALUES (gen_random_uuid(), 'OPEN_POSITION', 'PRE_TRADE', 'ALLOW', ARRAY['RISK_ORACLE_STALE'], 'x', 'v', 'h', 'e', 'd', '{}', '{}', now())`)
	require.Error(t, err)
}
