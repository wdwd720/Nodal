package marketplace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/sandboxrt"
)

const listingColumns = `id, creator_account_id, creator_actor_type, creator_actor_id, title, forked_from_version_id, created_at`

func scanListing(row pgx.Row) (Listing, error) {
	var l Listing
	var forked *VersionID
	if err := row.Scan(&l.ID, &l.CreatorAccountID, &l.Creator.Type, &l.Creator.ID, &l.Title, &forked, &l.CreatedAt); err != nil {
		return Listing{}, err
	}
	l.ForkedFromVersionID = forked
	l.CreatedAt = l.CreatedAt.UTC()
	return l, nil
}

// Listing returns one listing.
func (s *Service) Listing(ctx context.Context, q db.Querier, lid ListingID) (Listing, error) {
	l, err := scanListing(q.QueryRow(ctx, `SELECT `+listingColumns+` FROM marketplace_listings WHERE id = $1`, lid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Listing{}, errs.New(errs.CodeNotFound, "marketplace: listing not found").WithField("listing_id", lid.String())
	}
	if err != nil {
		return Listing{}, dbErr("load listing", err)
	}
	return l, nil
}

const versionColumns = `id, listing_id, version, status, moderation_state, description, risk_class, supported_instrument_ids,
	manifest_canonical, manifest_hash, source_digest, source_commit, backtest_evidence_ref, data_dependencies,
	performance_fee_bps, management_fee_bps, fee_high_water_mark, config, created_by_actor_type, created_by_actor_id,
	coalesce(rejection_code, ''), created_at, updated_at`

func scanVersion(row pgx.Row) (Version, error) {
	var v Version
	var canonical, config []byte
	var perf, mgmt int64
	if err := row.Scan(&v.ID, &v.ListingID, &v.Number, &v.Status, &v.Moderation, &v.Description, &v.RiskClass,
		&v.SupportedInstrumentIDs, &canonical, &v.ManifestHash, &v.SourceDigest, &v.SourceCommit, &v.BacktestEvidenceRef,
		&v.DataDependencies, &perf, &mgmt, &v.Fees.HighWaterMark, &config, &v.CreatedBy.Type, &v.CreatedBy.ID,
		&v.RejectionCode, &v.CreatedAt, &v.UpdatedAt); err != nil {
		return Version{}, err
	}
	sum := sha256.Sum256(canonical)
	if !bytes.Equal(sum[:], v.ManifestHash) {
		return Version{}, errs.New(errs.CodeInternal, "marketplace: stored manifest does not match its hash")
	}
	m, err := sandboxrt.DecodeManifest(canonical)
	if err != nil {
		return Version{}, errs.Wrap(err, errs.CodeInternal, "marketplace: stored manifest is unreadable")
	}
	v.Manifest = m
	v.Fees.PerformanceFeeBPS, v.Fees.ManagementFeeBPS = money.BPS(perf), money.BPS(mgmt)
	v.Config = map[string]string{}
	if err := json.Unmarshal(config, &v.Config); err != nil {
		return Version{}, errs.Wrap(err, errs.CodeInternal, "marketplace: stored config is unreadable")
	}
	v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
	return v, nil
}

// Version returns one listing version.
func (s *Service) Version(ctx context.Context, q db.Querier, vid VersionID) (Version, error) {
	return s.loadVersion(ctx, q, vid, false)
}

func (s *Service) loadVersion(ctx context.Context, q db.Querier, vid VersionID, lock bool) (Version, error) {
	sql := `SELECT ` + versionColumns + ` FROM marketplace_listing_versions WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	v, err := scanVersion(q.QueryRow(ctx, sql, vid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, errs.New(errs.CodeNotFound, "marketplace: listing version not found").WithField("version_id", vid.String())
	}
	if err != nil {
		return Version{}, dbErr("load version", err)
	}
	return v, nil
}

// Source returns a version's stored source tree.
func (s *Service) Source(ctx context.Context, q db.Querier, vid VersionID) (sandboxrt.Source, error) {
	rows, err := q.Query(ctx, `SELECT path, content FROM marketplace_version_sources WHERE version_id = $1 ORDER BY path`, vid)
	if err != nil {
		return nil, dbErr("load source", err)
	}
	defer rows.Close()
	src := sandboxrt.Source{}
	for rows.Next() {
		var path string
		var content []byte
		if err := rows.Scan(&path, &content); err != nil {
			return nil, dbErr("scan source", err)
		}
		src[path] = content
	}
	return src, rows.Err()
}

// transition moves a version's lifecycle status, writing the bound
// transition row and the audit event in the same transaction. The version
// must have been loaded FOR UPDATE.
func (s *Service) transition(ctx context.Context, tx pgx.Tx, v Version, to Status, actor econ.Actor, reason, rejectionCode string) (Version, error) {
	if !CanTransition(v.Status, to) {
		return Version{}, errs.Newf(errs.CodeInvalidStateTransition, "marketplace: version %s cannot move %s -> %s", v.ID, v.Status, to).
			WithField("version_id", v.ID.String()).WithField("from_status", string(v.Status)).WithField("to_status", string(to))
	}
	if to == StatusRejected && strings.TrimSpace(rejectionCode) == "" {
		return Version{}, errs.New(errs.CodeValidationFailed, "marketplace: a rejection needs a rejection code")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO marketplace_version_transitions (id, version_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id.New[id.Any](), v.ID, v.Status, to, actor.Type, actor.ID, reasonOr(reason, string(to)), s.now()); err != nil {
		return Version{}, dbErr("record version transition", err)
	}
	out, err := scanVersion(tx.QueryRow(ctx, `UPDATE marketplace_listing_versions SET status = $2,
		rejection_code = coalesce(NULLIF($3,''), rejection_code) WHERE id = $1 RETURNING `+versionColumns, v.ID, to, rejectionCode))
	if err != nil {
		return Version{}, dbErr("update version status", err)
	}
	if err := s.auditEvent(ctx, tx, audit.SystemStream, actor, "marketplace.version."+strings.ToLower(string(to)), "marketplace_listing_version",
		v.ID.String(), reasonOr(reason, string(to)), map[string]string{"from": string(v.Status), "to": string(to), "strategy_ref": v.StrategyRef(),
			"rejection_code": rejectionCode}); err != nil {
		return Version{}, err
	}
	return out, nil
}

// moderate moves a version's moderation state the same way.
func (s *Service) moderate(ctx context.Context, tx pgx.Tx, v Version, to Moderation, actor econ.Actor, reason string) (Version, error) {
	if !CanModerate(v.Moderation, to) {
		return Version{}, errs.Newf(errs.CodeInvalidStateTransition, "marketplace: version %s moderation cannot move %s -> %s", v.ID, v.Moderation, to).
			WithField("version_id", v.ID.String())
	}
	if to == ModerationCleared && v.Status != StatusApproved && v.Status != StatusDeprecated {
		return Version{}, errs.Newf(errs.CodeInvalidStateTransition, "marketplace: a %s version cannot be cleared", v.Status)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO marketplace_moderation_transitions (id, version_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id.New[id.Any](), v.ID, v.Moderation, to, actor.Type, actor.ID, reasonOr(reason, string(to)), s.now()); err != nil {
		return Version{}, dbErr("record moderation transition", err)
	}
	out, err := scanVersion(tx.QueryRow(ctx, `UPDATE marketplace_listing_versions SET moderation_state = $2 WHERE id = $1 RETURNING `+versionColumns, v.ID, to))
	if err != nil {
		return Version{}, dbErr("update moderation", err)
	}
	if err := s.auditEvent(ctx, tx, audit.SystemStream, actor, "marketplace.moderation."+strings.ToLower(string(to)), "marketplace_listing_version",
		v.ID.String(), reasonOr(reason, string(to)), map[string]string{"from": string(v.Moderation), "to": string(to)}); err != nil {
		return Version{}, err
	}
	return out, nil
}

const installColumns = `id, account_id, version_id, mode, status, installed_by_actor_type, installed_by_actor_id, created_at`

func scanInstall(row pgx.Row) (Install, error) {
	var in Install
	if err := row.Scan(&in.ID, &in.AccountID, &in.VersionID, &in.Mode, &in.Status, &in.InstalledBy.Type, &in.InstalledBy.ID, &in.CreatedAt); err != nil {
		return Install{}, err
	}
	in.CreatedAt = in.CreatedAt.UTC()
	return in, nil
}

// Install returns one install.
func (s *Service) Install(ctx context.Context, q db.Querier, iid InstallID) (Install, error) {
	return s.loadInstall(ctx, q, iid, false)
}

func (s *Service) loadInstall(ctx context.Context, q db.Querier, iid InstallID, lock bool) (Install, error) {
	sql := `SELECT ` + installColumns + ` FROM marketplace_installs WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	in, err := scanInstall(q.QueryRow(ctx, sql, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Install{}, errs.New(errs.CodeNotFound, "marketplace: install not found").WithField("install_id", iid.String())
	}
	if err != nil {
		return Install{}, dbErr("load install", err)
	}
	return in, nil
}

func reasonOr(reason, fallback string) string {
	if strings.TrimSpace(reason) == "" {
		return fallback
	}
	return reason
}
