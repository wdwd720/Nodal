package migrate

import (
	"context"
	"fmt"

	"github.com/pressly/goose/v3/database"
)

// Table names. VersionTable is goose's default; ChecksumTable is ours.
const (
	VersionTable  = "goose_db_version"
	ChecksumTable = "schema_migration_checksums"
)

const createChecksumTableSQL = `CREATE TABLE IF NOT EXISTS ` + ChecksumTable + ` (
    version    bigint      PRIMARY KEY,
    sha256     text        NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    applied_at timestamptz NOT NULL DEFAULT now()
)`

// hardenBookkeepingSQL removes the application role's default-privilege DML on
// the runner's bookkeeping tables (only the migration role may rewrite
// history) while keeping read access for the read-only and ops roles. Guarded
// on role existence so it is portable across environments; idempotent.
const hardenBookkeepingSQL = `DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_app') THEN
        REVOKE ALL ON ` + VersionTable + `, ` + ChecksumTable + ` FROM cp_app;
        REVOKE ALL ON SEQUENCE ` + VersionTable + `_id_seq FROM cp_app;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_readonly') THEN
        GRANT SELECT ON ` + VersionTable + `, ` + ChecksumTable + ` TO cp_readonly;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_ops') THEN
        GRANT SELECT ON ` + VersionTable + `, ` + ChecksumTable + ` TO cp_ops;
    END IF;
END
$$`

// checksumStore decorates goose's PostgreSQL store so that the checksum of a
// migration is written (or removed) in the same statement batch/transaction in
// which goose records (or deletes) the version. A crash between the two is
// therefore impossible, and Verify can treat a missing checksum as drift.
type checksumStore struct {
	database.Store
	sums map[int64]source
}

var (
	_ database.Store         = (*checksumStore)(nil)
	_ database.StoreExtender = (*checksumStore)(nil)
)

func newChecksumStore(srcs []source) (*checksumStore, error) {
	base, err := database.NewStore(database.DialectPostgres, VersionTable)
	if err != nil {
		return nil, fmt.Errorf("migrate: goose store: %w", err)
	}
	return &checksumStore{Store: base, sums: indexByVersion(srcs)}, nil
}

func (s *checksumStore) CreateVersionTable(ctx context.Context, db database.DBTxConn) error {
	if err := s.Store.CreateVersionTable(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, createChecksumTableSQL); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, hardenBookkeepingSQL)
	return err
}

func (s *checksumStore) Insert(ctx context.Context, db database.DBTxConn, req database.InsertRequest) error {
	if err := s.Store.Insert(ctx, db, req); err != nil {
		return err
	}
	if req.Version == 0 {
		return nil // goose's sentinel row
	}
	src, ok := s.sums[req.Version]
	if !ok {
		return fmt.Errorf("migrate: no embedded source for version %d", req.Version)
	}
	// Under goose's session lock and inside the migration transaction, so the
	// IF NOT EXISTS cannot race with another runner.
	if _, err := db.ExecContext(ctx, createChecksumTableSQL); err != nil {
		return fmt.Errorf("migrate: ensure checksum table: %w", err)
	}
	if _, err := db.ExecContext(ctx, hardenBookkeepingSQL); err != nil {
		return fmt.Errorf("migrate: harden bookkeeping tables: %w", err)
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO `+ChecksumTable+` (version, sha256, applied_at) VALUES ($1, $2, now())
		 ON CONFLICT (version) DO UPDATE SET sha256 = EXCLUDED.sha256, applied_at = now()`,
		req.Version, src.SHA256)
	if err != nil {
		return fmt.Errorf("migrate: record checksum for %d: %w", req.Version, err)
	}
	return nil
}

func (s *checksumStore) Delete(ctx context.Context, db database.DBTxConn, version int64) error {
	if err := s.Store.Delete(ctx, db, version); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM `+ChecksumTable+` WHERE version = $1`, version); err != nil {
		return fmt.Errorf("migrate: remove checksum for %d: %w", version, err)
	}
	return nil
}

// TableExists forwards to goose's store so the provider keeps its fast path.
func (s *checksumStore) TableExists(ctx context.Context, db database.DBTxConn) (bool, error) {
	if ext, ok := s.Store.(database.StoreExtender); ok {
		return ext.TableExists(ctx, db)
	}
	return false, database.ErrNotImplemented
}
