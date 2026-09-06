// Package db owns PostgreSQL connectivity for the control plane: the pgx
// connection pool (with OpenTelemetry tracing and per-connection session
// settings), the Querier abstraction shared by repositories, transaction
// helpers with bounded retry on serialization failures and deadlocks, and
// SQLSTATE classification helpers.
//
// # Responsibilities
//
//   - Open a pool from a Config, refusing non-verified TLS when RequireTLS is
//     set (fail closed for PROD).
//   - Apply application_name, statement_timeout, lock_timeout and UTC time zone
//     to every connection.
//   - InTx / Serializable: run fn inside a transaction, commit on nil error,
//     roll back on error or panic (the panic is re-raised), and retry the whole
//     transaction ONLY on SQLSTATE 40001 (serialization_failure) or 40P01
//     (deadlock_detected), with exponential backoff and jitter, up to
//     TxOptions.MaxRetries. Any other error returned by fn is returned as-is
//     without retrying.
//
// # What this package must never do
//
//   - Hold package-level connections or configuration (no global state).
//   - Retry side effects that live outside the transaction: fn must be pure
//     with respect to everything but the tx it receives.
//   - Interpret domain errors or map them to API codes; that belongs to the
//     caller and internal/errs.
//   - Run migrations; see internal/db/migrate.
package db
