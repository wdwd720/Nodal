// Package migrations embeds the SQL schema migrations for the control plane.
//
// Files are named NNNNN_name.sql and use goose annotations (-- +goose Up /
// -- +goose Down). Numbering ranges are defined in
// docs/architecture/CONVENTIONS.md and mirrored by internal/db/migrate.Ranges.
//
// This package must never contain executable logic: it exists only so the
// migration runner (internal/db/migrate) and cmd/migrate can ship the SQL
// inside the binary and verify applied checksums against it.
package migrations

import "embed"

// FS holds every *.sql migration file in this directory.
//
//go:embed *.sql
var FS embed.FS
