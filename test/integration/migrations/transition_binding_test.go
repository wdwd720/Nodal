//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/id"
)

// TestIntegration_StateChangeRequiresTransitionRow proves migration 00603:
// an audited entity's state cannot change unless a matching transition row
// is written in the same transaction, regardless of statement order, and a
// transition row for a different target state does not satisfy it.
func TestIntegration_StateChangeRequiresTransitionRow(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	app := connect(t, appURL)

	userID := id.New[id.Any]()
	accountID := id.New[id.Any]()
	_, err := app.Exec(ctx, `INSERT INTO users (id, idp_issuer, idp_subject, status) VALUES ($1, 'test', $2, 'ACTIVE')`, userID, "sub-"+userID.String())
	require.NoError(t, err)
	_, err = app.Exec(ctx, `INSERT INTO accounts (id, owner_user_id, kind, status) VALUES ($1, $2, 'CUSTOMER', 'ACTIVE')`, accountID, userID)
	require.NoError(t, err)

	// See the note above: the bare update runs as the owner, the only role that
	// can still write accounts.status and therefore the only one on which the
	// audit binding is still what is being measured.
	owner := connect(t, migrateURL)
	inTx := func(fn func(tx pgx.Tx) error) error {
		tx, err := owner.Begin(ctx)
		require.NoError(t, err)
		if err := fn(tx); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		return tx.Commit(ctx)
	}
	status := func() string {
		var s string
		require.NoError(t, app.QueryRow(ctx, `SELECT status FROM accounts WHERE id = $1`, accountID).Scan(&s))
		return s
	}

	// 1. Bare state update → refused at commit with AU001.
	err = inTx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, accountID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err), "got %v", err)
	assert.Equal(t, "ACTIVE", status())

	// 2. Transition row for a different target state → still refused.
	err = inTx(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO account_status_transitions (id, account_id, from_status, to_status, actor_type, actor_id, reason) VALUES ($1, $2, 'ACTIVE', 'RESTRICTED', 'OPERATOR', 'op', 'wrong target')`, id.New[id.Any](), accountID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, accountID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err), "got %v", err)
	assert.Equal(t, "ACTIVE", status())

	// 3. Update first, transition second (reverse order) → accepted: the check is deferred to commit.
	require.NoError(t, inTx(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, accountID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO account_status_transitions (id, account_id, from_status, to_status, actor_type, actor_id, reason) VALUES ($1, $2, 'ACTIVE', 'FROZEN', 'OPERATOR', 'op', 'compliance hold')`, id.New[id.Any](), accountID)
		return err
	}))
	assert.Equal(t, "FROZEN", status())

	// 4. Non-state updates need no transition row.
	require.NoError(t, inTx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE accounts SET status_reason = 'note' WHERE id = $1`, accountID)
		return err
	}))

	// 5. The flag is transaction-local: a later transaction cannot reuse it.
	err = inTx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE accounts SET status = 'ACTIVE' WHERE id = $1`, accountID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err))
	assert.Equal(t, "FROZEN", status())
}
