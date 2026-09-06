package reality

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

var codeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,127}$`)

// dataSourceKind is the phantom kind of data source ids.
type dataSourceKind struct{}

// NewDataSourceID mints a data source id.
func NewDataSourceID() id.ID[dataSourceKind] { return id.New[dataSourceKind]() }

// Validate mirrors the data_sources CHECK constraints so a bad registration
// is refused before it reaches the database (PART 120).
func (d DataSource) Validate() error {
	fields := map[string]any{}
	if !codeRe.MatchString(d.Code) {
		fields["code"] = "must match ^[a-z0-9][a-z0-9_.-]{0,127}$"
	}
	if d.Provider == "" {
		fields["provider"] = "required"
	}
	if !oneOf(d.Kind, KindMarketData, KindOnchain, KindSocial, KindWalletIntelligence, KindModel, KindInternal) {
		fields["kind"] = "unknown"
	}
	if !RetentionClass(d.RetentionClass).Valid() {
		fields["retention_class"] = "unknown"
	}
	if d.RetentionDays <= 0 {
		fields["retention_days"] = "must be > 0 (never a hardcoded forever)"
	}
	if !oneOf(d.RedistributionPolicy, RedistributionNone, RedistributionInternalOnly, RedistributionCustomerDisplay, RedistributionRedistributable) {
		fields["redistribution_policy"] = "unknown"
	}
	if !oneOf(d.HistoricalUsePermitted, HistoricalUseYes, HistoricalUseNo, HistoricalUseUnknown) {
		fields["historical_use_permitted"] = "unknown"
	}
	if !oneOf(d.PersistenceCapability, PersistenceAllowed, PersistenceBlocked) {
		fields["persistence_capability"] = "unknown"
	}
	if d.PersistenceCapability == PersistenceAllowed && d.HistoricalUsePermitted != HistoricalUseYes {
		fields["persistence_capability"] = "must be BLOCKED unless historical use is permitted (YES)"
	}
	if !oneOf(d.DedupStrategy, DedupProviderID, DedupCompositeHash) {
		fields["dedup_strategy"] = "unknown"
	}
	if d.HeartbeatTimeout <= 0 {
		fields["heartbeat_timeout_ms"] = "must be > 0"
	}
	if !oneOf(d.Status, SourceActive, SourceDegraded, SourceDisabled) {
		fields["status"] = "unknown"
	}
	if d.CreatedByActorType == "" || d.CreatedByActorType == string(security.ActorAgent) {
		fields["created_by_actor_type"] = "required and never AGENT"
	}
	if d.CreatedByActorID == "" {
		fields["created_by_actor_id"] = "required"
	}
	if len(fields) > 0 {
		return errs.New(errs.CodeValidationFailed, "reality: invalid data source").WithFields(fields)
	}
	return nil
}

// PersistenceAllowed reports whether history beyond the operational window
// may be retained and used for backtests.
func (d DataSource) PersistenceAllowed() bool {
	return d.PersistenceCapability == PersistenceAllowed && d.HistoricalUsePermitted == HistoricalUseYes
}

// AllowedIn reports whether the source may be used in env (an empty list
// means every environment).
func (d DataSource) AllowedIn(env string) bool {
	if len(d.Environments) == 0 {
		return true
	}
	for _, e := range d.Environments {
		if e == env {
			return true
		}
	}
	return false
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// DefaultChainDataSource is the registration the ingest worker creates for
// a chain wallet-event stream when none exists: RAW_MARKET_DATA with the
// configured retention days, historical use UNKNOWN and therefore
// persistence BLOCKED until an operator records the license (PART 120).
func DefaultChainDataSource(code, providerName string, retentionDays int, heartbeat time.Duration, actorID string) DataSource {
	return DataSource{
		Code: code, Provider: providerName, Kind: KindOnchain,
		RetentionClass: string(RetentionRawMarketData), RetentionDays: retentionDays,
		RedistributionPolicy: RedistributionNone, HistoricalUsePermitted: HistoricalUseUnknown,
		PersistenceCapability: PersistenceBlocked, DedupStrategy: DedupProviderID,
		HeartbeatTimeout: heartbeat, SupportsReplay: true, SupportsSequence: true, Status: SourceActive,
		CreatedByActorType: string(security.ActorSystem), CreatedByActorID: actorID,
	}
}

// PgDataSourceStore is the Postgres registry.
type PgDataSourceStore struct{}

var _ DataSourceStore = PgDataSourceStore{}

const dataSourceColumns = `id, code, provider, kind, retention_class, retention_days, redistribution_policy, historical_use_permitted,
	persistence_capability, environments, license_ref, contract_ref, dedup_strategy, heartbeat_timeout_ms, supports_replay,
	supports_sequence, status, created_by_actor_type, created_by_actor_id, created_at, updated_at`

func scanDataSource(row pgx.Row) (DataSource, error) {
	var d DataSource
	var sid id.ID[dataSourceKind]
	var license, contract *string
	var heartbeatMS int
	if err := row.Scan(&sid, &d.Code, &d.Provider, &d.Kind, &d.RetentionClass, &d.RetentionDays, &d.RedistributionPolicy, &d.HistoricalUsePermitted,
		&d.PersistenceCapability, &d.Environments, &license, &contract, &d.DedupStrategy, &heartbeatMS, &d.SupportsReplay,
		&d.SupportsSequence, &d.Status, &d.CreatedByActorType, &d.CreatedByActorID, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return DataSource{}, err
	}
	d.ID = sid.String()
	d.LicenseRef, d.ContractRef = deref(license), deref(contract)
	d.HeartbeatTimeout = time.Duration(heartbeatMS) * time.Millisecond
	d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
	return d, nil
}

// Get implements DataSourceStore.
func (PgDataSourceStore) Get(ctx context.Context, q db.Querier, code string) (DataSource, error) {
	d, err := scanDataSource(q.QueryRow(ctx, `SELECT `+dataSourceColumns+` FROM data_sources WHERE code = $1`, code))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DataSource{}, errs.New(errs.CodeNotFound, "reality: data source not registered").WithField("code", code)
		}
		return DataSource{}, errs.Wrap(err, errs.CodeInternal, "reality: load data source")
	}
	return d, nil
}

// GetByID loads a data source by id.
func (PgDataSourceStore) GetByID(ctx context.Context, q db.Querier, dataSourceID string) (DataSource, error) {
	d, err := scanDataSource(q.QueryRow(ctx, `SELECT `+dataSourceColumns+` FROM data_sources WHERE id = $1`, dataSourceID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DataSource{}, errs.New(errs.CodeNotFound, "reality: data source not registered")
		}
		return DataSource{}, errs.Wrap(err, errs.CodeInternal, "reality: load data source")
	}
	return d, nil
}

// List implements DataSourceStore.
func (PgDataSourceStore) List(ctx context.Context, q db.Querier) ([]DataSource, error) {
	rows, err := q.Query(ctx, `SELECT `+dataSourceColumns+` FROM data_sources ORDER BY code`)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "reality: list data sources")
	}
	defer rows.Close()
	var out []DataSource
	for rows.Next() {
		d, err := scanDataSource(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "reality: scan data source")
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "reality: list data sources")
	}
	return out, nil
}

// Register inserts a data source. A duplicate code is CONFLICT; a
// persistence grant without historical-use permission is VALIDATION_FAILED
// (checked here and again by the database).
func (PgDataSourceStore) Register(ctx context.Context, tx pgx.Tx, d DataSource) (DataSource, error) {
	if err := d.Validate(); err != nil {
		return DataSource{}, err
	}
	sid := NewDataSourceID()
	envs := d.Environments
	if envs == nil {
		envs = []string{}
	}
	_, err := tx.Exec(ctx, `INSERT INTO data_sources (id, code, provider, kind, retention_class, retention_days, redistribution_policy,
		historical_use_permitted, persistence_capability, environments, license_ref, contract_ref, dedup_strategy, heartbeat_timeout_ms,
		supports_replay, supports_sequence, status, created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		sid, d.Code, d.Provider, d.Kind, d.RetentionClass, d.RetentionDays, d.RedistributionPolicy,
		d.HistoricalUsePermitted, d.PersistenceCapability, envs, nilIfEmpty(d.LicenseRef), nilIfEmpty(d.ContractRef), d.DedupStrategy,
		d.HeartbeatTimeout.Milliseconds(), d.SupportsReplay, d.SupportsSequence, d.Status, d.CreatedByActorType, d.CreatedByActorID)
	if err != nil {
		switch {
		case db.IsUniqueViolation(err):
			return DataSource{}, errs.Wrap(err, errs.CodeConflict, "reality: data source code already registered")
		case db.IsCheckViolation(err):
			return DataSource{}, errs.Wrap(err, errs.CodeValidationFailed, "reality: data source violates a registry constraint").WithField("constraint", db.ConstraintName(err))
		}
		return DataSource{}, errs.Wrap(err, errs.CodeInternal, "reality: register data source")
	}
	return PgDataSourceStore{}.Get(ctx, tx, d.Code)
}

// EnsureRegistered returns the existing registration of d.Code or registers
// d. It never changes an existing row: licensing metadata is an operator
// decision.
func (s PgDataSourceStore) EnsureRegistered(ctx context.Context, database *db.DB, d DataSource) (DataSource, error) {
	existing, err := s.Get(ctx, database, d.Code)
	if err == nil {
		return existing, nil
	}
	if errs.CodeOf(err) != errs.CodeNotFound {
		return DataSource{}, err
	}
	var out DataSource
	err = database.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var rerr error
		out, rerr = s.Register(ctx, tx, d)
		return rerr
	})
	if err != nil && errs.CodeOf(err) == errs.CodeConflict {
		return s.Get(ctx, database, d.Code)
	}
	return out, err
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
