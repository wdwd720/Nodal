//go:build integration

package risk

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
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

// TestIntegration_AdvancedDecisionsListOnlyTheAccountsOwn: the listing is
// scoped to one account, newest first, and bounded.
func TestIntegration_AdvancedDecisionsListOnlyTheAccountsOwn(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	repo := accounts.NewRepository()
	newAcct := func() string {
		u, err := repo.CreateUser(ctx, testDB, "risk-itest", "sub-"+uuid.NewString(), nil)
		require.NoError(t, err)
		a, err := repo.CreateAccount(ctx, testDB, u.ID, accounts.KindCustomer)
		require.NoError(t, err)
		return a.ID.String()
	}
	mine, theirs := newAcct(), newAcct()
	s := NewStore()
	record := func(acct string) DecisionID {
		in := openPosition()
		in.Intent.ID = ""
		in.Intent.AccountID = acct
		d := EvaluateAdvanced(DefaultAdvancedPolicy(), in)
		var id DecisionID
		require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			id, err = s.RecordAdvancedDecision(ctx, tx, in, d, "corr-list")
			return err
		}))
		return id
	}
	var own []DecisionID
	for range 3 {
		own = append(own, record(mine))
	}
	record(theirs)

	got, err := s.ListAdvancedDecisions(ctx, testDB, mine, 10)
	require.NoError(t, err)
	require.Len(t, got, 3, "only this account's decisions")
	ids := map[DecisionID]bool{}
	for _, r := range got {
		assert.Equal(t, mine, r.AccountID)
		ids[r.ID] = true
	}
	for _, id := range own {
		assert.True(t, ids[id])
	}
	bounded, err := s.ListAdvancedDecisions(ctx, testDB, mine, 2)
	require.NoError(t, err)
	assert.Len(t, bounded, 2)

	_, err = s.ListAdvancedDecisions(ctx, testDB, "not-a-uuid", 10)
	require.Error(t, err)
}
