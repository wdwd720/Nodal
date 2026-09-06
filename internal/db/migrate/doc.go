// Package migrate runs the embedded SQL migrations (root package migrations)
// with goose v3 as a library, under the dedicated migration database role.
//
// # Responsibilities
//
//   - Up / UpTo / DownTo / Status / Version / Verify over a migration-role
//     connection string; every call opens its own short-lived connection so
//     the application never holds migration privileges.
//   - Record a SHA-256 checksum of each applied migration file in
//     schema_migration_checksums, in the same transaction that goose records
//     the version, and let Verify detect drift between the embedded files and
//     what was applied (PART 140, PART 218).
//   - Refuse DownTo below ProtectedVersion once a protected (ledger) migration
//     has been applied: ledger history is never destroyed by a rollback
//     (PART 19, PART 140).
//   - Provide the numbering ranges and the skeleton used by cmd/migrate create.
//
// # What this package must never do
//
//   - Be imported by request-path code or hold a pool: it is a deployment tool.
//   - Print or log connection strings.
//   - Drop or rewrite ledger tables; protected migrations carry "SELECT 1"
//     down sections and the guard blocks rolling back past them.
package migrate
