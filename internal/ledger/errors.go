package ledger

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// SQLSTATEs raised by the triggers of migration 00101.
const (
	SQLStateNegativeBalance = "LG001"
	SQLStateUnbalanced      = "LG002"
	SQLStateImmutable       = "LG003"
	SQLStateAssetMismatch   = "LG004"
	SQLStateAccountClosed   = "LG005"
)

// Constraint names from migration 00101 that MapError recognizes.
const (
	constraintIdempotencyKey = "journal_transactions_idempotency_key_key"
	constraintReversalOf     = "journal_transactions_reversal_of_fkey"
	constraintEntryAsset     = "journal_entries_asset_id_fkey"
	constraintAccountAsset   = "ledger_accounts_asset_id_fkey"
)

// MapError translates a database error from a ledger write into a stable
// *errs.Error. It is applied by Post to every database error and must also
// be applied by callers to the error returned from db.InTx, because the
// balanced-per-asset and has-entries checks are deferred constraint triggers
// that fire at COMMIT:
//
//	err := d.InTx(ctx, opts, func(ctx context.Context, tx pgx.Tx) error {
//	    res, err := poster.Post(ctx, tx, p)
//	    ...
//	})
//	if err != nil {
//	    return ledger.MapError(err) // LEDGER_UNBALANCED, LEDGER_NEGATIVE_BALANCE, ...
//	}
//
// Errors that are already *errs.Error, context errors and retryable
// transaction failures (SQLSTATE 40001/40P01) are returned unchanged so that
// db.InTx can still retry them. Anything unrecognized becomes INTERNAL with
// the cause retained for logs.
func MapError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errs.As(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if db.IsRetryable(err) {
		return err
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return errs.Wrap(err, errs.CodeInternal, "ledger: database error")
	}
	// The value-domain triggers fire on the same inserts these codes come from,
	// and their five SQLSTATEs reached callers as unclassified INTERNAL until
	// F-58. Consulting valuedomain here means every ledger caller gets the
	// classification without each of them repeating the switch.
	if mapped := valuedomain.MapError(err); mapped != nil {
		return mapped
	}
	switch pe.Code {
	case SQLStateNegativeBalance:
		return errs.Wrap(err, errs.CodeLedgerNegativeBalance, "posting would drive a ledger account below zero")
	case SQLStateUnbalanced:
		return errs.Wrap(err, errs.CodeLedgerUnbalanced, "journal transaction rejected at commit: entries do not balance per asset")
	case SQLStateImmutable:
		return errs.Wrap(err, errs.CodeLedgerImmutable, "posted journal rows are append-only; post a compensating transaction")
	case SQLStateAssetMismatch:
		return errs.Wrap(err, errs.CodeLedgerAssetMismatch, "entry asset does not match the ledger account's asset")
	case SQLStateAccountClosed:
		return errs.Wrap(err, errs.CodeLedgerAccountClosed, "ledger account is closed")
	case db.SQLStateUniqueViolation:
		if pe.ConstraintName == constraintIdempotencyKey {
			return errs.Wrap(err, errs.CodeInvalidIdempotencyReuse, "idempotency key already used by another journal transaction")
		}
		return errs.Wrap(err, errs.CodeConflict, "ledger row already exists")
	case db.SQLStateForeignKeyViolation:
		switch pe.ConstraintName {
		case constraintReversalOf:
			return errs.Wrap(err, errs.CodeValidationFailed, "reversal_of refers to an unknown journal transaction")
		case constraintEntryAsset, constraintAccountAsset:
			return errs.Wrap(err, errs.CodeValidationFailed, "asset is not registered")
		}
		return errs.Wrap(err, errs.CodeValidationFailed, "posting references an unknown row")
	case db.SQLStateCheckViolation:
		return errs.Wrap(err, errs.CodeValidationFailed, "posting violates a ledger check constraint").
			WithField("constraint", pe.ConstraintName)
	case db.SQLStateInsufficientPrivilege:
		return errs.Wrap(err, errs.CodeForbidden, "database role lacks the privilege for this ledger operation")
	case db.SQLStateQueryCanceled, db.SQLStateLockNotAvailable:
		return errs.Wrap(err, errs.CodeConflict, "ledger operation timed out waiting for the database")
	}
	if pe.Code == db.SQLStateRaiseException && strings.HasPrefix(pe.Message, "immutable row") {
		return errs.Wrap(err, errs.CodeLedgerImmutable, "posted journal rows are append-only; post a compensating transaction")
	}
	return errs.Wrap(err, errs.CodeInternal, "ledger: database error")
}
