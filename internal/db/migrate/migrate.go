package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/nodal/controlplane/migrations"
)

// Verify failure classes. Verify returns an errors.Join of one entry per
// finding, each wrapping exactly one of these sentinels.
var (
	ErrChecksumMismatch = errors.New("migrate: applied migration checksum does not match embedded file")
	ErrMissingChecksum  = errors.New("migrate: applied migration has no recorded checksum")
	ErrUnknownMigration = errors.New("migrate: database has an applied version with no embedded source")
)

// MigrationStatus is one row of Status: every embedded migration plus any
// applied version the binary no longer ships (Name empty, Orphaned true).
type MigrationStatus struct {
	Version   int64
	Name      string
	SHA256    string // checksum of the embedded file ("" when orphaned)
	Applied   bool
	AppliedAt time.Time // zero unless Applied
	Orphaned  bool
}

// Up applies every pending migration.
func Up(ctx context.Context, migrateURL string) error {
	r, err := open(ctx, migrateURL, migrations.FS)
	if err != nil {
		return err
	}
	defer r.close()
	_, err = r.provider.Up(ctx)
	return wrapGoose("up", err)
}

// UpTo applies pending migrations up to and including version.
func UpTo(ctx context.Context, migrateURL string, version int64) error {
	if version < 0 {
		return fmt.Errorf("migrate: invalid target version %d", version)
	}
	r, err := open(ctx, migrateURL, migrations.FS)
	if err != nil {
		return err
	}
	defer r.close()
	_, err = r.provider.UpTo(ctx, version)
	return wrapGoose("up-to", err)
}

// DownTo rolls back applied migrations above version. It refuses (with
// ErrProtectedVersion) whenever the current version is at or above
// ProtectedVersion and version is below it.
func DownTo(ctx context.Context, migrateURL string, version int64) error {
	r, err := open(ctx, migrateURL, migrations.FS)
	if err != nil {
		return err
	}
	defer r.close()
	current, err := r.provider.GetDBVersion(ctx)
	if err != nil {
		return wrapGoose("read version", err)
	}
	if err := guardDownTo(current, version, ProtectedVersion); err != nil {
		return err
	}
	_, err = r.provider.DownTo(ctx, version)
	return wrapGoose("down-to", err)
}

// Version returns the highest applied migration version (0 when none).
func Version(ctx context.Context, migrateURL string) (int64, error) {
	r, err := open(ctx, migrateURL, migrations.FS)
	if err != nil {
		return 0, err
	}
	defer r.close()
	v, err := r.provider.GetDBVersion(ctx)
	return v, wrapGoose("read version", err)
}

// Status lists embedded migrations with their applied state, plus orphaned
// applied versions, ascending by version.
func Status(ctx context.Context, migrateURL string) ([]MigrationStatus, error) {
	r, err := open(ctx, migrateURL, migrations.FS)
	if err != nil {
		return nil, err
	}
	defer r.close()
	return r.status(ctx)
}

// Verify checks that every applied migration still matches the embedded file
// it was applied from: same SHA-256, a checksum row present, and a source
// present. Pending migrations are not a failure (use Status for those).
func Verify(ctx context.Context, migrateURL string) error {
	r, err := open(ctx, migrateURL, migrations.FS)
	if err != nil {
		return err
	}
	defer r.close()
	return r.verify(ctx)
}

// runner bundles one migration session: a small database/sql pool over pgx,
// the goose provider, and the embedded sources.
type runner struct {
	db       *sql.DB
	provider *goose.Provider
	sources  []source
}

func open(ctx context.Context, migrateURL string, fsys fs.FS) (*runner, error) {
	if strings.TrimSpace(migrateURL) == "" {
		return nil, errors.New("migrate: migration database URL is empty")
	}
	srcs, err := loadSources(fsys)
	if err != nil {
		return nil, err
	}
	if len(srcs) == 0 {
		return nil, errors.New("migrate: no embedded migrations found")
	}

	cfg, err := pgx.ParseConfig(migrateURL)
	if err != nil {
		return nil, fmt.Errorf("migrate: parse config: %w", err)
	}
	if cfg.RuntimeParams["application_name"] == "" {
		cfg.RuntimeParams["application_name"] = "cp-migrate"
	}
	cfg.RuntimeParams["TimeZone"] = "UTC"
	sqlDB := stdlib.OpenDB(*cfg)
	sqlDB.SetMaxOpenConns(2)
	sqlDB.SetMaxIdleConns(2)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate: connect: %w", err)
	}

	store, err := newChecksumStore(srcs)
	if err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate: session locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectCustom, sqlDB, fsys,
		goose.WithStore(store),
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate: goose provider: %w", err)
	}
	return &runner{db: sqlDB, provider: provider, sources: srcs}, nil
}

func (r *runner) close() {
	_ = r.provider.Close()
	_ = r.db.Close()
}

func wrapGoose(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("migrate: %s: %w", op, err)
}

// appliedVersions reads goose's table directly: (version -> applied_at).
// A missing table means nothing is applied. Like goose's provider, the
// presence of a row is what counts (the provider deletes rows on down and
// never consults is_applied), so Verify covers exactly what goose will skip.
func (r *runner) appliedVersions(ctx context.Context) (map[int64]time.Time, error) {
	exists, err := r.tableExists(ctx, VersionTable)
	if err != nil || !exists {
		return map[int64]time.Time{}, err
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT version_id, MAX(tstamp) FROM `+VersionTable+` WHERE version_id > 0 GROUP BY version_id`)
	if err != nil {
		return nil, fmt.Errorf("migrate: list applied versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]time.Time{}
	for rows.Next() {
		var v int64
		var at time.Time
		if err := rows.Scan(&v, &at); err != nil {
			return nil, fmt.Errorf("migrate: scan applied version: %w", err)
		}
		out[v] = at.UTC()
	}
	return out, rows.Err()
}

// recordedChecksums reads our side table. A missing table means no checksums.
func (r *runner) recordedChecksums(ctx context.Context) (map[int64]string, error) {
	exists, err := r.tableExists(ctx, ChecksumTable)
	if err != nil || !exists {
		return map[int64]string{}, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT version, sha256 FROM `+ChecksumTable)
	if err != nil {
		return nil, fmt.Errorf("migrate: list checksums: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var v int64
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, fmt.Errorf("migrate: scan checksum: %w", err)
		}
		out[v] = sum
	}
	return out, rows.Err()
}

func (r *runner) tableExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("migrate: check table %s: %w", name, err)
	}
	return exists, nil
}

func (r *runner) status(ctx context.Context) ([]MigrationStatus, error) {
	applied, err := r.appliedVersions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]MigrationStatus, 0, len(r.sources)+len(applied))
	known := map[int64]bool{}
	for _, s := range r.sources {
		known[s.Version] = true
		st := MigrationStatus{Version: s.Version, Name: s.Name, SHA256: s.SHA256}
		if at, ok := applied[s.Version]; ok {
			st.Applied, st.AppliedAt = true, at
		}
		out = append(out, st)
	}
	for v, at := range applied {
		if !known[v] {
			out = append(out, MigrationStatus{Version: v, Applied: true, AppliedAt: at, Orphaned: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func (r *runner) verify(ctx context.Context) error {
	applied, err := r.appliedVersions(ctx)
	if err != nil {
		return err
	}
	recorded, err := r.recordedChecksums(ctx)
	if err != nil {
		return err
	}
	byVersion := indexByVersion(r.sources)

	versions := make([]int64, 0, len(applied))
	for v := range applied {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })

	var problems []error
	for _, v := range versions {
		src, ok := byVersion[v]
		if !ok {
			problems = append(problems, fmt.Errorf("%w: version %d", ErrUnknownMigration, v))
			continue
		}
		sum, ok := recorded[v]
		if !ok {
			problems = append(problems, fmt.Errorf("%w: version %d (%s)", ErrMissingChecksum, v, src.Name))
			continue
		}
		if sum != src.SHA256 {
			problems = append(problems, fmt.Errorf("%w: version %d (%s) recorded=%s embedded=%s",
				ErrChecksumMismatch, v, src.Name, sum, src.SHA256))
		}
	}
	return errors.Join(problems...)
}
