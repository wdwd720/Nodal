package ledger

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

func pgErr(code, constraint, message string) error {
	return &pgconn.PgError{Code: code, ConstraintName: constraint, Message: message}
}

func TestMapError(t *testing.T) {
	t.Parallel()
	already := errs.New(errs.CodeLedgerUnbalanced, "app-level")
	cases := []struct {
		name string
		err  error
		code errs.Code
	}{
		{"LG001", pgErr(SQLStateNegativeBalance, "", "LEDGER_NEGATIVE_BALANCE: ..."), errs.CodeLedgerNegativeBalance},
		{"LG002", pgErr(SQLStateUnbalanced, "", "LEDGER_UNBALANCED: ..."), errs.CodeLedgerUnbalanced},
		{"LG003", pgErr(SQLStateImmutable, "", "LEDGER_IMMUTABLE: ..."), errs.CodeLedgerImmutable},
		{"LG004", pgErr(SQLStateAssetMismatch, "", "LEDGER_ASSET_MISMATCH: ..."), errs.CodeLedgerAssetMismatch},
		{"LG005", pgErr(SQLStateAccountClosed, "", "LEDGER_ACCOUNT_CLOSED: ..."), errs.CodeLedgerAccountClosed},
		{"idempotency unique", pgErr(db.SQLStateUniqueViolation, constraintIdempotencyKey, "duplicate key"), errs.CodeInvalidIdempotencyReuse},
		{"other unique", pgErr(db.SQLStateUniqueViolation, "ledger_accounts_owner_type_owner_id_code_asset_id_key", "duplicate key"), errs.CodeConflict},
		{"reversal fk", pgErr(db.SQLStateForeignKeyViolation, constraintReversalOf, "fk"), errs.CodeValidationFailed},
		{"asset fk", pgErr(db.SQLStateForeignKeyViolation, constraintEntryAsset, "fk"), errs.CodeValidationFailed},
		{"check", pgErr(db.SQLStateCheckViolation, "journal_entries_quantity_check", "check"), errs.CodeValidationFailed},
		{"privilege", pgErr(db.SQLStateInsufficientPrivilege, "", "permission denied"), errs.CodeForbidden},
		{"statement timeout", pgErr(db.SQLStateQueryCanceled, "", "canceling statement"), errs.CodeConflict},
		{"lock timeout", pgErr(db.SQLStateLockNotAvailable, "", "lock timeout"), errs.CodeConflict},
		{"generic forbid_mutation trigger", pgErr(db.SQLStateRaiseException, "", "immutable row: UPDATE on public.x is forbidden"), errs.CodeLedgerImmutable},
		{"unknown pg error", pgErr("XX000", "", "boom"), errs.CodeInternal},
		{"plain error", errors.New("boom"), errs.CodeInternal},
		{"wrapped by InTx commit", fmt.Errorf("db: commit: %w", pgErr(SQLStateUnbalanced, "", "LEDGER_UNBALANCED")), errs.CodeLedgerUnbalanced},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := MapError(c.err)
			requireCode(t, got, c.code)
			var pe *pgconn.PgError
			if errors.As(c.err, &pe) {
				assert.True(t, errors.As(got, &pe), "cause must be preserved for logs")
			}
			e, ok := errs.As(got)
			require.True(t, ok)
			assert.NotEmpty(t, e.Detail)
		})
	}

	assert.NoError(t, MapError(nil))
	assert.Same(t, already, MapError(already), "errs errors pass through untouched")
	wrapped := fmt.Errorf("outer: %w", already)
	assert.Equal(t, wrapped, MapError(wrapped))
	assert.Equal(t, context.Canceled, MapError(context.Canceled))
	assert.ErrorIs(t, MapError(fmt.Errorf("x: %w", context.DeadlineExceeded)), context.DeadlineExceeded)

	deadlock := pgErr(db.SQLStateDeadlockDetected, "", "deadlock detected")
	assert.Equal(t, deadlock, MapError(deadlock), "retryable errors pass through so db.InTx can retry")
	assert.True(t, db.IsDeadlock(MapError(fmt.Errorf("db: commit: %w", deadlock))))
	serialization := pgErr(db.SQLStateSerializationFailure, "", "could not serialize")
	assert.True(t, db.IsSerializationFailure(MapError(serialization)))
}

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 5, 12, 34, 56, 789, time.FixedZone("x", 3600))
	txID := NewTransactionID()
	c := encodeCursor(at, txID)
	gotAt, gotID, err := decodeCursor(c)
	require.NoError(t, err)
	assert.Equal(t, at.UTC(), gotAt)
	assert.Equal(t, time.UTC, gotAt.Location())
	assert.Equal(t, txID, gotID)

	for _, bad := range []string{"", "!!!", "djF8", "dmVyc2lvbg", encodeCursor(at, TransactionID{}), "eDF8MjAyNi0wOS0wNVQxMjozNDo1Nlp8YWJj"} {
		_, _, err := decodeCursor(bad)
		requireCode(t, err, errs.CodeValidationFailed)
	}
}

// TestPost_GuardsBeforeDatabase covers the checks Post performs before it
// touches the transaction: validation, the SEED gate, and agent refusal.
func TestPost_GuardsBeforeDatabase(t *testing.T) {
	t.Parallel()
	svc := NewService(clock.NewFake(testEffective), "test")
	ctx := context.Background()

	bad := fundingPosting(100)
	bad.Entries[1].Quantity = q(1)
	_, err := svc.Post(ctx, nil, bad)
	requireCode(t, err, errs.CodeLedgerUnbalanced)

	seed := fundingPosting(100)
	seed.Kind = KindSeed
	_, err = svc.Post(ctx, nil, seed)
	requireCode(t, err, errs.CodeForbidden)
	svc.AllowSeedPostings()
	_, err = svc.Post(ctx, nil, seed)
	requireCode(t, err, errs.CodeInternal) // past the gate; nil tx is the next failure

	agentCtx := security.WithPrincipal(ctx, security.AgentPrincipal("agent-1", testAccount.String()))
	_, err = svc.Post(agentCtx, nil, fundingPosting(100))
	requireCode(t, err, errs.CodeForbidden)

	invalid := security.WithPrincipal(ctx, security.Principal{ActorType: security.ActorOperator})
	_, err = svc.Post(invalid, nil, fundingPosting(100))
	requireCode(t, err, errs.CodeForbidden)

	_, err = svc.PostInTx(ctx, nil, fundingPosting(100))
	requireCode(t, err, errs.CodeInternal)

	require.Panics(t, func() { NewService(nil, "x") })
}
