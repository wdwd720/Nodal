package db

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL SQLSTATE codes this package classifies. Kept local (rather than
// importing github.com/jackc/pgerrcode) to stay within the pinned module set.
const (
	SQLStateUniqueViolation       = "23505"
	SQLStateForeignKeyViolation   = "23503"
	SQLStateCheckViolation        = "23514"
	SQLStateSerializationFailure  = "40001"
	SQLStateDeadlockDetected      = "40P01"
	SQLStateInsufficientPrivilege = "42501"
	SQLStateQueryCanceled         = "57014" // statement_timeout
	SQLStateLockNotAvailable      = "55P03" // lock_timeout
	SQLStateRaiseException        = "P0001" // RAISE EXCEPTION default (forbid_mutation trigger)
)

// SQLState returns the SQLSTATE of the first *pgconn.PgError in err's chain, or "".
func SQLState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// ConstraintName returns the violated constraint name, if the error carries one.
func ConstraintName(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.ConstraintName
	}
	return ""
}

// IsUniqueViolation reports SQLSTATE 23505.
func IsUniqueViolation(err error) bool { return SQLState(err) == SQLStateUniqueViolation }

// IsForeignKeyViolation reports SQLSTATE 23503.
func IsForeignKeyViolation(err error) bool { return SQLState(err) == SQLStateForeignKeyViolation }

// IsCheckViolation reports SQLSTATE 23514.
func IsCheckViolation(err error) bool { return SQLState(err) == SQLStateCheckViolation }

// IsSerializationFailure reports SQLSTATE 40001 (could not serialize access).
func IsSerializationFailure(err error) bool { return SQLState(err) == SQLStateSerializationFailure }

// IsDeadlock reports SQLSTATE 40P01.
func IsDeadlock(err error) bool { return SQLState(err) == SQLStateDeadlockDetected }

// IsRetryable reports whether a transaction that failed with err may be
// re-run from the top: only serialization failures and deadlocks qualify.
func IsRetryable(err error) bool { return IsSerializationFailure(err) || IsDeadlock(err) }

// IsInsufficientPrivilege reports SQLSTATE 42501 (role separation violations).
func IsInsufficientPrivilege(err error) bool { return SQLState(err) == SQLStateInsufficientPrivilege }

// IsStatementTimeout reports SQLSTATE 57014 raised by statement_timeout.
func IsStatementTimeout(err error) bool { return SQLState(err) == SQLStateQueryCanceled }

// IsLockTimeout reports SQLSTATE 55P03 raised by lock_timeout.
func IsLockTimeout(err error) bool { return SQLState(err) == SQLStateLockNotAvailable }

// IsMutationForbidden reports that a write was refused either by an
// immutability trigger or because the connecting role lacks the privilege.
// Tests of append-only tables accept both: the application role has no
// UPDATE/DELETE grant on history tables, and the trigger is the second line
// of defense for roles that do.
func IsMutationForbidden(err error) bool { return IsImmutableRow(err) || IsInsufficientPrivilege(err) }

// SQLStateImmutable is the custom SQLSTATE raised by every immutability
// trigger that guards financial or evidentiary rows (ledger LG003, plans,
// fills, notifications).
const SQLStateImmutable = "LG003"

// IsImmutableRow reports an error raised by an immutability trigger: the
// generic forbid_mutation trigger from migration 00001 (P0001 "immutable
// row ...") or any domain trigger raising SQLSTATE LG003.
func IsImmutableRow(err error) bool {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return false
	}
	if pe.Code == SQLStateImmutable {
		return true
	}
	return pe.Code == SQLStateRaiseException && strings.HasPrefix(pe.Message, "immutable row")
}
