package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// MarketplaceDeps holds the strategy marketplace and the sandbox runtime
// behind it. Both are required together: leaving either nil leaves every
// /advanced/strategies route UNSUPPORTED rather than half-answering.
type MarketplaceDeps struct {
	Service *marketplace.Service
	Sandbox *sandboxrt.Service
}

func wireMarketplace(p *Ports, d WireDeps) {
	m := d.Advanced.Marketplace
	if m.Service == nil || m.Sandbox == nil || d.DB == nil {
		return
	}
	p.Marketplace = marketplaceAdapter{svc: m.Service, sandbox: m.Sandbox, db: d.DB}
}

// marketplaceAdapter serves MarketplacePort over the marketplace and sandbox
// services. Every write is one service operation in one transaction. The reads
// that the services expose are the services' own; the lists they do not expose
// (the catalogue, an account's listings, installs, follows and runs) are
// read-only projections in the manner of readmodel.go: every statement below is
// a SELECT, and nothing here decides anything.
type marketplaceAdapter struct {
	svc     *marketplace.Service
	sandbox *sandboxrt.Service
	db      *db.DB
}

// Bounds of one listing's detail. A listing is read whole, so what it carries
// is bounded rather than paged.
const (
	marketplaceMaxVersions    = 50
	marketplaceMaxEvaluations = 50
	marketplaceMaxPerformance = 100
)

func (a marketplaceAdapter) read(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, fn)
}

// write runs one service operation. A request that ended before the operation
// was confirmed is a transport fact: its outcome is unconfirmed, never a
// rejection, and the same Idempotency-Key may be retried (the services are
// idempotent on their own records: a version cannot be submitted twice, a
// snapshot time is followed once).
func (a marketplaceAdapter) write(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
	if err != nil && ctx.Err() != nil {
		return errs.Wrap(err, errs.CodeProviderUnavailable,
			"the request ended before the operation completed; its outcome is not confirmed, retry with the same Idempotency-Key")
	}
	return err
}

// --- columns and scans ------------------------------------------------------

const (
	marketplaceListingCols = `l.id, l.creator_account_id, l.creator_actor_type, l.creator_actor_id, l.title, l.forked_from_version_id, l.created_at`
	marketplaceSummaryCols = `v.id, v.listing_id, v.version, v.status, v.moderation_state, v.risk_class, v.description,
		coalesce(v.rejection_code, ''), v.created_at, v.updated_at`
)

// marketplaceRowScanner is what pgx.Row and pgx.Rows have in common.
type marketplaceRowScanner interface {
	Scan(dest ...any) error
}

func marketplaceListingDest(l *marketplace.Listing) []any {
	return []any{&l.ID, &l.CreatorAccountID, &l.Creator.Type, &l.Creator.ID, &l.Title, &l.ForkedFromVersionID, &l.CreatedAt}
}

func marketplaceSummaryDest(v *MarketplaceVersionSummary) []any {
	return []any{&v.ID, &v.ListingID, &v.Number, &v.Status, &v.Moderation, &v.RiskClass, &v.Description, &v.RejectionCode,
		&v.CreatedAt, &v.UpdatedAt}
}

func marketplaceUTC(l *marketplace.Listing, v *MarketplaceVersionSummary) {
	if l != nil {
		l.CreatedAt = l.CreatedAt.UTC()
	}
	if v != nil {
		v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
	}
}

// marketplaceCursor decodes a keyset cursor and refuses one whose id is not a
// UUID, so a tampered cursor is a VALIDATION_FAILED rather than a database
// error.
func marketplaceCursor(cursor string) (keysetCursor, bool, error) {
	c, ok, err := decodeCursor(cursor)
	if err != nil || !ok {
		return c, ok, err
	}
	if _, perr := id.ParseAny(c.ID); perr != nil {
		return keysetCursor{}, false, validationError("cursor", "cursor is not a valid pagination cursor")
	}
	return c, true, nil
}

func marketplaceIDs[K any](ids []id.ID[K]) []string {
	out := make([]string, 0, len(ids))
	for _, i := range ids {
		out = append(out, i.String())
	}
	return out
}

func marketplaceQueryErr(op string, err error) error {
	if ee, ok := errs.As(err); ok {
		return ee
	}
	return errs.Wrap(err, errs.CodeInternal, "marketplace read: "+op)
}

// --- reads ------------------------------------------------------------------

func (a marketplaceAdapter) Catalog(ctx context.Context, cursor string, limit int) ([]MarketplaceListingSummary, string, error) {
	return a.listingPage(ctx, cursor, limit,
		` AND v.status = 'APPROVED' AND v.moderation_state = 'CLEARED'`, "", nil)
}

func (a marketplaceAdapter) AccountListings(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]MarketplaceListingSummary, string, error) {
	return a.listingPage(ctx, cursor, limit, "", ` AND l.creator_account_id = $1`, []any{account})
}

// listingPage pages listings newest first, each with the newest version that
// satisfies versionWhere.
func (a marketplaceAdapter) listingPage(ctx context.Context, cursor string, limit int, versionWhere, where string, args []any) ([]MarketplaceListingSummary, string, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := marketplaceCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	sql := `SELECT ` + marketplaceListingCols + `, ` + marketplaceSummaryCols + ` FROM marketplace_listings l
		JOIN LATERAL (SELECT * FROM marketplace_listing_versions v WHERE v.listing_id = l.id` + versionWhere + `
			ORDER BY v.version DESC LIMIT 1) v ON true
		WHERE true` + where
	if hasCursor {
		sql += fmt.Sprintf(` AND (l.created_at, l.id) < ($%d, $%d::uuid)`, len(args)+1, len(args)+2)
		args = append(args, c.At, c.ID)
	}
	sql += fmt.Sprintf(` ORDER BY l.created_at DESC, l.id DESC LIMIT %d`, limit+1)
	var out []MarketplaceListingSummary
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return marketplaceQueryErr("list listings", err)
		}
		defer rows.Close()
		for rows.Next() {
			var s MarketplaceListingSummary
			if err := rows.Scan(append(marketplaceListingDest(&s.Listing), marketplaceSummaryDest(&s.Latest)...)...); err != nil {
				return marketplaceQueryErr("scan listing", err)
			}
			marketplaceUTC(&s.Listing, &s.Latest)
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1].Listing
		next = encodeCursor(keysetCursor{At: last.CreatedAt, ID: last.ID.String()})
	}
	return out, next, nil
}

func (a marketplaceAdapter) Listing(ctx context.Context, lid marketplace.ListingID) (MarketplaceListingView, error) {
	var out MarketplaceListingView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.listingView(ctx, tx, lid)
		return err
	})
	return out, err
}

// listingView assembles a listing with its newest versions and everything
// recorded about each: the services' own reads for the version, its validation
// and its provenance, and projections for its reviews, evaluations and the
// paper performance of its installs.
func (a marketplaceAdapter) listingView(ctx context.Context, q db.Querier, lid marketplace.ListingID) (MarketplaceListingView, error) {
	l, err := a.svc.Listing(ctx, q, lid)
	if err != nil {
		return MarketplaceListingView{}, err
	}
	view := MarketplaceListingView{Listing: l}
	rows, err := q.Query(ctx, `SELECT id FROM marketplace_listing_versions WHERE listing_id = $1 ORDER BY version DESC LIMIT $2`,
		lid, marketplaceMaxVersions)
	if err != nil {
		return MarketplaceListingView{}, marketplaceQueryErr("list versions", err)
	}
	var ids []marketplace.VersionID
	for rows.Next() {
		var vid marketplace.VersionID
		if err := rows.Scan(&vid); err != nil {
			rows.Close()
			return MarketplaceListingView{}, marketplaceQueryErr("scan version id", err)
		}
		ids = append(ids, vid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MarketplaceListingView{}, marketplaceQueryErr("list versions", err)
	}
	index := make(map[marketplace.VersionID]int, len(ids))
	for _, vid := range ids {
		v, err := a.svc.Version(ctx, q, vid)
		if err != nil {
			return MarketplaceListingView{}, err
		}
		vv := MarketplaceVersionView{Version: v, Reviews: []MarketplaceReview{}, Evaluations: []MarketplaceEvaluation{},
			Performance: []marketplace.PerformanceSnapshot{}}
		if val, ok, err := a.svc.ValidationOf(ctx, q, vid); err != nil {
			return MarketplaceListingView{}, err
		} else if ok {
			vv.Validation = &val
		}
		if vv.Provenance, err = a.svc.Provenance(ctx, q, vid); err != nil {
			return MarketplaceListingView{}, err
		}
		index[vid] = len(view.Versions)
		view.Versions = append(view.Versions, vv)
	}
	if len(ids) == 0 {
		return view, nil
	}
	keys := marketplaceIDs(ids)
	if err := a.attachReviews(ctx, q, keys, index, &view); err != nil {
		return MarketplaceListingView{}, err
	}
	if err := a.attachEvaluations(ctx, q, keys, index, &view); err != nil {
		return MarketplaceListingView{}, err
	}
	perf, err := a.performanceOf(ctx, q, `version_id = ANY($1::uuid[])`, keys, marketplaceMaxPerformance)
	if err != nil {
		return MarketplaceListingView{}, err
	}
	for _, p := range perf {
		if i, ok := index[p.VersionID]; ok {
			view.Versions[i].Performance = append(view.Versions[i].Performance, p)
		}
	}
	return view, nil
}

func (a marketplaceAdapter) attachReviews(ctx context.Context, q db.Querier, keys []string, index map[marketplace.VersionID]int, view *MarketplaceListingView) error {
	rows, err := q.Query(ctx, `SELECT version_id, decision, reason_code, note, reviewer_actor_type, reviewer_actor_id, reviewed_at
		FROM marketplace_reviews WHERE version_id = ANY($1::uuid[]) ORDER BY reviewed_at, id`, keys)
	if err != nil {
		return marketplaceQueryErr("list reviews", err)
	}
	defer rows.Close()
	for rows.Next() {
		var vid marketplace.VersionID
		var r MarketplaceReview
		if err := rows.Scan(&vid, &r.Decision, &r.ReasonCode, &r.Note, &r.Reviewer.Type, &r.Reviewer.ID, &r.ReviewedAt); err != nil {
			return marketplaceQueryErr("scan review", err)
		}
		r.ReviewedAt = r.ReviewedAt.UTC()
		if i, ok := index[vid]; ok {
			view.Versions[i].Reviews = append(view.Versions[i].Reviews, r)
		}
	}
	return rows.Err()
}

func (a marketplaceAdapter) attachEvaluations(ctx context.Context, q db.Querier, keys []string, index map[marketplace.VersionID]int, view *MarketplaceListingView) error {
	rows, err := q.Query(ctx, `SELECT id, version_id, kind, dataset_version, snapshots_count, snapshots_digest, results_digest, runs_ok,
		proposals_count, accepted_count, violations_count, evidence_ref, evidence_sha256, created_at
		FROM marketplace_evaluations WHERE version_id = ANY($1::uuid[]) ORDER BY created_at DESC, id DESC LIMIT $2`, keys, marketplaceMaxEvaluations)
	if err != nil {
		return marketplaceQueryErr("list evaluations", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e MarketplaceEvaluation
		ev := &e.Evaluation
		if err := rows.Scan(&ev.ID, &ev.VersionID, &ev.Kind, &ev.DatasetVersion, &e.SnapshotsCount, &ev.SnapshotsDigest, &ev.ResultsDigest,
			&ev.RunsOK, &ev.ProposalsCount, &ev.AcceptedCount, &ev.ViolationsCount, &ev.EvidenceRef, &ev.EvidenceSHA256, &e.CreatedAt); err != nil {
			return marketplaceQueryErr("scan evaluation", err)
		}
		e.CreatedAt = e.CreatedAt.UTC()
		if i, ok := index[ev.VersionID]; ok {
			view.Versions[i].Evaluations = append(view.Versions[i].Evaluations, e)
		}
	}
	return rows.Err()
}

// performanceOf reads recorded paper performance snapshots, newest first.
func (a marketplaceAdapter) performanceOf(ctx context.Context, q db.Querier, where string, keys []string, limit int) ([]marketplace.PerformanceSnapshot, error) {
	rows, err := q.Query(ctx, `SELECT install_id, version_id, follow_runs_count, first_as_of, last_as_of, history_digest, return_bps,
		max_drawdown_bps, volatility_bps, turnover_bps, computed_at
		FROM marketplace_performance_snapshots WHERE `+where+` ORDER BY computed_at DESC, id DESC LIMIT $2`, keys, limit)
	if err != nil {
		return nil, marketplaceQueryErr("list performance", err)
	}
	defer rows.Close()
	var out []marketplace.PerformanceSnapshot
	for rows.Next() {
		var p marketplace.PerformanceSnapshot
		if err := rows.Scan(&p.InstallID, &p.VersionID, &p.Points, &p.FirstAsOf, &p.LastAsOf, &p.HistoryDigest, &p.ReturnBPS,
			&p.MaxDrawdownBPS, &p.VolatilityBPS, &p.TurnoverBPS, &p.ComputedAt); err != nil {
			return nil, marketplaceQueryErr("scan performance", err)
		}
		p.FirstAsOf, p.LastAsOf, p.ComputedAt = p.FirstAsOf.UTC(), p.LastAsOf.UTC(), p.ComputedAt.UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

func (a marketplaceAdapter) ListingRecord(ctx context.Context, lid marketplace.ListingID) (marketplace.Listing, error) {
	return a.svc.Listing(ctx, a.db, lid)
}

func (a marketplaceAdapter) VersionRecord(ctx context.Context, vid marketplace.VersionID) (marketplace.Listing, marketplace.Version, error) {
	v, err := a.svc.Version(ctx, a.db, vid)
	if err != nil {
		return marketplace.Listing{}, marketplace.Version{}, err
	}
	l, err := a.svc.Listing(ctx, a.db, v.ListingID)
	if err != nil {
		return marketplace.Listing{}, marketplace.Version{}, err
	}
	return l, v, nil
}

func (a marketplaceAdapter) ReviewQueue(ctx context.Context, statuses []marketplace.Status, cursor string, limit int) ([]MarketplaceQueueItem, string, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := marketplaceCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	wanted := make([]string, 0, len(statuses))
	for _, s := range statuses {
		wanted = append(wanted, string(s))
	}
	args := []any{wanted}
	sql := `SELECT ` + marketplaceListingCols + `, ` + marketplaceSummaryCols + ` FROM marketplace_listing_versions v
		JOIN marketplace_listings l ON l.id = v.listing_id WHERE v.status = ANY($1::text[])`
	if hasCursor {
		sql += ` AND (v.created_at, v.id) > ($2, $3::uuid)`
		args = append(args, c.At, c.ID)
	}
	sql += fmt.Sprintf(` ORDER BY v.created_at, v.id LIMIT %d`, limit+1)
	var out []MarketplaceQueueItem
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return marketplaceQueryErr("list review queue", err)
		}
		defer rows.Close()
		for rows.Next() {
			var it MarketplaceQueueItem
			if err := rows.Scan(append(marketplaceListingDest(&it.Listing), marketplaceSummaryDest(&it.Version)...)...); err != nil {
				return marketplaceQueryErr("scan review queue", err)
			}
			marketplaceUTC(&it.Listing, &it.Version)
			out = append(out, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1].Version
		next = encodeCursor(keysetCursor{At: last.CreatedAt, ID: last.ID.String()})
	}
	return out, next, nil
}

const marketplaceInstallCols = `i.id, i.account_id, i.version_id, i.mode, i.status, i.installed_by_actor_type, i.installed_by_actor_id, i.created_at`

func marketplaceInstallDest(in *marketplace.Install) []any {
	return []any{&in.ID, &in.AccountID, &in.VersionID, &in.Mode, &in.Status, &in.InstalledBy.Type, &in.InstalledBy.ID, &in.CreatedAt}
}

func (a marketplaceAdapter) Installs(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]MarketplaceInstallView, string, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := marketplaceCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	args := []any{account}
	where := `i.account_id = $1`
	if hasCursor {
		where += ` AND (i.created_at, i.id) < ($2, $3::uuid)`
		args = append(args, c.At, c.ID)
	}
	var out []MarketplaceInstallView
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.installViews(ctx, tx, where, args, fmt.Sprintf(` ORDER BY i.created_at DESC, i.id DESC LIMIT %d`, limit+1))
		return err
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1].Install
		next = encodeCursor(keysetCursor{At: last.CreatedAt, ID: last.ID.String()})
	}
	return out, next, nil
}

// installViews reads installs with their listing, version and latest paper
// performance snapshot.
func (a marketplaceAdapter) installViews(ctx context.Context, q db.Querier, where string, args []any, tail string) ([]MarketplaceInstallView, error) {
	rows, err := q.Query(ctx, `SELECT `+marketplaceInstallCols+`, `+marketplaceListingCols+`, `+marketplaceSummaryCols+`
		FROM marketplace_installs i JOIN marketplace_listing_versions v ON v.id = i.version_id
		JOIN marketplace_listings l ON l.id = v.listing_id WHERE `+where+tail, args...)
	if err != nil {
		return nil, marketplaceQueryErr("list installs", err)
	}
	var out []MarketplaceInstallView
	for rows.Next() {
		var iv MarketplaceInstallView
		dest := append(append(marketplaceInstallDest(&iv.Install), marketplaceListingDest(&iv.Listing)...), marketplaceSummaryDest(&iv.Version)...)
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, marketplaceQueryErr("scan install", err)
		}
		iv.Install.CreatedAt = iv.Install.CreatedAt.UTC()
		marketplaceUTC(&iv.Listing, &iv.Version)
		out = append(out, iv)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, marketplaceQueryErr("list installs", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	ids := make([]marketplace.InstallID, 0, len(out))
	for _, iv := range out {
		ids = append(ids, iv.Install.ID)
	}
	latest, err := q.Query(ctx, `SELECT DISTINCT ON (install_id) install_id, version_id, follow_runs_count, first_as_of, last_as_of,
		history_digest, return_bps, max_drawdown_bps, volatility_bps, turnover_bps, computed_at
		FROM marketplace_performance_snapshots WHERE install_id = ANY($1::uuid[]) ORDER BY install_id, computed_at DESC, id DESC`,
		marketplaceIDs(ids))
	if err != nil {
		return nil, marketplaceQueryErr("latest performance", err)
	}
	defer latest.Close()
	byInstall := map[marketplace.InstallID]marketplace.PerformanceSnapshot{}
	for latest.Next() {
		var p marketplace.PerformanceSnapshot
		if err := latest.Scan(&p.InstallID, &p.VersionID, &p.Points, &p.FirstAsOf, &p.LastAsOf, &p.HistoryDigest, &p.ReturnBPS,
			&p.MaxDrawdownBPS, &p.VolatilityBPS, &p.TurnoverBPS, &p.ComputedAt); err != nil {
			return nil, marketplaceQueryErr("scan performance", err)
		}
		p.FirstAsOf, p.LastAsOf, p.ComputedAt = p.FirstAsOf.UTC(), p.LastAsOf.UTC(), p.ComputedAt.UTC()
		byInstall[p.InstallID] = p
	}
	if err := latest.Err(); err != nil {
		return nil, marketplaceQueryErr("latest performance", err)
	}
	for i := range out {
		if p, ok := byInstall[out[i].Install.ID]; ok {
			out[i].LatestPerformance = &p
		}
	}
	return out, nil
}

func (a marketplaceAdapter) installView(ctx context.Context, q db.Querier, iid marketplace.InstallID) (MarketplaceInstallView, error) {
	views, err := a.installViews(ctx, q, `i.id = $1`, []any{iid}, "")
	if err != nil {
		return MarketplaceInstallView{}, err
	}
	if len(views) != 1 {
		return MarketplaceInstallView{}, errs.New(errs.CodeNotFound, "marketplace: install not found").WithField("install_id", iid.String())
	}
	return views[0], nil
}

func (a marketplaceAdapter) InstallRecord(ctx context.Context, iid marketplace.InstallID) (marketplace.Install, error) {
	return a.svc.Install(ctx, a.db, iid)
}

const marketplaceFollowCols = `f.id, f.install_id, f.version_id, f.account_id, f.sandbox_run_id, f.trigger, f.as_of, f.input_digest, f.run_digest,
	r.termination_reason, f.proposals_count, f.accepted_count, f.violations_count, f.intents_submitted, f.intents_dispatched,
	f.intents_risk_rejected, f.intents_failed, f.equity::text, f.turnover::text, f.evidence_ref, f.evidence_sha256, f.created_at`

func (a marketplaceAdapter) Follows(ctx context.Context, account accounts.AccountID, install *marketplace.InstallID, cursor string, limit int) ([]MarketplaceFollowView, string, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := marketplaceCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	args := []any{account}
	sql := `SELECT ` + marketplaceFollowCols + ` FROM marketplace_follow_runs f JOIN sandbox_runs r ON r.id = f.sandbox_run_id
		WHERE f.account_id = $1`
	if install != nil {
		args = append(args, *install)
		sql += fmt.Sprintf(` AND f.install_id = $%d`, len(args))
	}
	if hasCursor {
		sql += fmt.Sprintf(` AND (f.created_at, f.id) < ($%d, $%d::uuid)`, len(args)+1, len(args)+2)
		args = append(args, c.At, c.ID)
	}
	sql += fmt.Sprintf(` ORDER BY f.created_at DESC, f.id DESC LIMIT %d`, limit+1)
	var out []MarketplaceFollowView
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return marketplaceQueryErr("list follow runs", err)
		}
		var runs []marketplace.FollowRun
		for rows.Next() {
			var f marketplace.FollowRun
			if err := rows.Scan(&f.ID, &f.InstallID, &f.VersionID, &f.AccountID, &f.SandboxRunID, &f.Trigger, &f.AsOf, &f.InputDigest,
				&f.RunDigest, &f.Termination, &f.ProposalsCount, &f.AcceptedCount, &f.ViolationsCount, &f.IntentsSubmitted,
				&f.IntentsDispatched, &f.IntentsRiskRejected, &f.IntentsFailed, &f.Equity, &f.Turnover, &f.EvidenceRef,
				&f.EvidenceSHA256, &f.CreatedAt); err != nil {
				rows.Close()
				return marketplaceQueryErr("scan follow run", err)
			}
			f.AsOf, f.CreatedAt = f.AsOf.UTC(), f.CreatedAt.UTC()
			runs = append(runs, f)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return marketplaceQueryErr("list follow runs", err)
		}
		for _, f := range runs {
			if f.Intents, err = a.svc.FollowIntents(ctx, tx, f.ID); err != nil {
				return err
			}
			view, err := a.followView(ctx, tx, f)
			if err != nil {
				return err
			}
			out = append(out, view)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1].Run
		next = encodeCursor(keysetCursor{At: last.CreatedAt, ID: last.ID.String()})
	}
	return out, next, nil
}

// followView attaches to a follow run the Risk Kernel V2 decision of every
// intent (the one whose hash the run recorded) and the state of every dispatch
// effect, as the risk store and the effect register hold them.
func (a marketplaceAdapter) followView(ctx context.Context, q db.Querier, run marketplace.FollowRun) (MarketplaceFollowView, error) {
	view := MarketplaceFollowView{Run: run, Decisions: map[econ.IntentID]risk.AdvancedDecisionRecord{},
		Effects: map[econ.EffectID]MarketplaceEffectState{}}
	if len(run.Intents) == 0 {
		return view, nil
	}
	hashOf := make(map[string]string, len(run.Intents))
	intentIDs := make([]string, 0, len(run.Intents))
	var effectIDs []string
	for _, fi := range run.Intents {
		hashOf[fi.IntentID.String()] = fi.DecisionHash
		intentIDs = append(intentIDs, fi.IntentID.String())
		if fi.EffectID != nil {
			effectIDs = append(effectIDs, fi.EffectID.String())
		}
	}
	rows, err := q.Query(ctx, `SELECT id, coalesce(intent_id::text, ''), coalesce(account_id::text, ''), intent_kind, stage, verdict,
		reason_codes, input_hash, policy_version, policy_hash, evaluator_version, decision_hash, evaluated_at
		FROM advanced_risk_decisions WHERE intent_id = ANY($1::uuid[]) ORDER BY evaluated_at, id`, intentIDs)
	if err != nil {
		return MarketplaceFollowView{}, marketplaceQueryErr("list risk decisions", err)
	}
	for rows.Next() {
		var r risk.AdvancedDecisionRecord
		if err := rows.Scan(&r.ID, &r.IntentID, &r.AccountID, &r.IntentKind, &r.Stage, &r.Verdict, &r.ReasonCodes, &r.InputHash,
			&r.PolicyVersion, &r.PolicyHash, &r.EvaluatorVersion, &r.DecisionHash, &r.EvaluatedAt); err != nil {
			rows.Close()
			return MarketplaceFollowView{}, marketplaceQueryErr("scan risk decision", err)
		}
		r.EvaluatedAt = r.EvaluatedAt.UTC()
		if r.ReasonCodes == nil {
			r.ReasonCodes = []string{}
		}
		if hashOf[r.IntentID] != r.DecisionHash {
			continue
		}
		iid, perr := econ.ParseIntentID(r.IntentID)
		if perr != nil {
			rows.Close()
			return MarketplaceFollowView{}, errs.Wrap(perr, errs.CodeInternal, "marketplace read: a risk decision names a malformed intent")
		}
		view.Decisions[iid] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MarketplaceFollowView{}, marketplaceQueryErr("list risk decisions", err)
	}
	if len(effectIDs) == 0 {
		return view, nil
	}
	effects, err := q.Query(ctx, `SELECT id, status, coalesce(failure_code, ''), coalesce(external_ref, '')
		FROM economic_effects WHERE id = ANY($1::uuid[])`, effectIDs)
	if err != nil {
		return MarketplaceFollowView{}, marketplaceQueryErr("list effects", err)
	}
	defer effects.Close()
	for effects.Next() {
		var eid econ.EffectID
		var st MarketplaceEffectState
		if err := effects.Scan(&eid, &st.Status, &st.FailureCode, &st.ExternalRef); err != nil {
			return MarketplaceFollowView{}, marketplaceQueryErr("scan effect", err)
		}
		view.Effects[eid] = st
	}
	return view, effects.Err()
}

func (a marketplaceAdapter) Runs(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]MarketplaceRunView, string, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := marketplaceCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	args := []any{account}
	sql := `SELECT run_id, created_at, follow_run_id, evaluation_id FROM (
			SELECT f.sandbox_run_id AS run_id, r.created_at, f.id AS follow_run_id, NULL::uuid AS evaluation_id
			FROM marketplace_follow_runs f JOIN sandbox_runs r ON r.id = f.sandbox_run_id WHERE f.account_id = $1
			UNION ALL
			SELECT er.sandbox_run_id, r.created_at, NULL::uuid, er.evaluation_id
			FROM marketplace_evaluation_runs er JOIN marketplace_evaluations e ON e.id = er.evaluation_id
			JOIN marketplace_listing_versions v ON v.id = e.version_id JOIN marketplace_listings l ON l.id = v.listing_id
			JOIN sandbox_runs r ON r.id = er.sandbox_run_id WHERE l.creator_account_id = $1
		) runs`
	if hasCursor {
		sql += ` WHERE (created_at, run_id) < ($2, $3::uuid)`
		args = append(args, c.At, c.ID)
	}
	sql += fmt.Sprintf(` ORDER BY created_at DESC, run_id DESC LIMIT %d`, limit+1)
	type ref struct {
		run   sandboxrt.RunID
		at    time.Time
		follo *marketplace.FollowRunID
		eval  *marketplace.EvaluationID
	}
	var out []MarketplaceRunView
	var refs []ref
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return marketplaceQueryErr("list sandbox runs", err)
		}
		for rows.Next() {
			var r ref
			if err := rows.Scan(&r.run, &r.at, &r.follo, &r.eval); err != nil {
				rows.Close()
				return marketplaceQueryErr("scan sandbox run", err)
			}
			refs = append(refs, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return marketplaceQueryErr("list sandbox runs", err)
		}
		for _, r := range refs {
			rec, err := a.sandbox.Run(ctx, tx, r.run)
			if err != nil {
				return err
			}
			violations, err := a.sandbox.RunViolations(ctx, tx, r.run)
			if err != nil {
				return err
			}
			if violations == nil {
				violations = []sandboxrt.Violation{}
			}
			out = append(out, MarketplaceRunView{Run: rec, FollowRunID: r.follo, EvaluationID: r.eval, Violations: violations})
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := refs[limit-1]
		next = encodeCursor(keysetCursor{At: last.at, ID: last.run.String()})
	}
	return out, next, nil
}

// --- writes -----------------------------------------------------------------

func (a marketplaceAdapter) CreateListing(ctx context.Context, r MarketplaceCreateListing) (MarketplaceListingView, error) {
	var out MarketplaceListingView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		l, _, err := a.svc.CreateListing(ctx, tx, marketplace.ListingRequest{CreatorAccountID: r.Account, Creator: r.Actor, Title: r.Title,
			Version: r.Version})
		if err != nil {
			return err
		}
		out, err = a.listingView(ctx, tx, l.ID)
		return err
	})
	return out, err
}

func (a marketplaceAdapter) NewVersion(ctx context.Context, r MarketplaceNewVersion) (marketplace.Version, error) {
	var out marketplace.Version
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.svc.NewVersion(ctx, tx, r.ListingID, r.Actor, r.Version)
		return err
	})
	return out, err
}

func (a marketplaceAdapter) Fork(ctx context.Context, r MarketplaceFork) (MarketplaceListingView, error) {
	var out MarketplaceListingView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		l, _, err := a.svc.Fork(ctx, tx, r.ParentID, marketplace.ForkRequest{CreatorAccountID: r.Account, Creator: r.Actor, Title: r.Title,
			Version: r.Version})
		if err != nil {
			return err
		}
		out, err = a.listingView(ctx, tx, l.ID)
		return err
	})
	return out, err
}

func (a marketplaceAdapter) Submit(ctx context.Context, vid marketplace.VersionID, actor econ.Actor) (marketplace.Version, error) {
	return a.versionOp(ctx, func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error) {
		return a.svc.Submit(ctx, tx, vid, actor)
	})
}

func (a marketplaceAdapter) Simulate(ctx context.Context, r MarketplaceSimulate) (MarketplaceEvaluation, error) {
	var out MarketplaceEvaluation
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		ev, err := a.svc.Simulate(ctx, tx, marketplace.SimulationRequest{VersionID: r.VersionID, DatasetVersion: r.DatasetVersion,
			Snapshots: r.Snapshots, Actor: r.Actor})
		if err != nil {
			return err
		}
		out = MarketplaceEvaluation{Evaluation: ev, SnapshotsCount: len(r.Snapshots)}
		if err := tx.QueryRow(ctx, `SELECT created_at FROM marketplace_evaluations WHERE id = $1`, ev.ID).Scan(&out.CreatedAt); err != nil {
			return marketplaceQueryErr("read evaluation", err)
		}
		out.CreatedAt = out.CreatedAt.UTC()
		return nil
	})
	return out, err
}

func (a marketplaceAdapter) Validate(ctx context.Context, vid marketplace.VersionID, actor econ.Actor) (marketplace.Version, marketplace.Validation, error) {
	var v marketplace.Version
	var val marketplace.Validation
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v, val, err = a.svc.Validate(ctx, tx, vid, actor)
		return err
	})
	return v, val, err
}

func (a marketplaceAdapter) StartReview(ctx context.Context, vid marketplace.VersionID, actor econ.Actor) (marketplace.Version, error) {
	return a.versionOp(ctx, func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error) {
		return a.svc.StartReview(ctx, tx, vid, actor)
	})
}

func (a marketplaceAdapter) Approve(ctx context.Context, vid marketplace.VersionID, actor econ.Actor, reasonCode, note string) (marketplace.Version, error) {
	return a.versionOp(ctx, func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error) {
		return a.svc.Approve(ctx, tx, vid, actor, reasonCode, note)
	})
}

func (a marketplaceAdapter) Reject(ctx context.Context, vid marketplace.VersionID, actor econ.Actor, reasonCode, note string) (marketplace.Version, error) {
	return a.versionOp(ctx, func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error) {
		return a.svc.Reject(ctx, tx, vid, actor, reasonCode, note)
	})
}

func (a marketplaceAdapter) Moderate(ctx context.Context, vid marketplace.VersionID, actor econ.Actor, to marketplace.Moderation, reason string) (marketplace.Version, error) {
	return a.versionOp(ctx, func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error) {
		return a.svc.Moderate(ctx, tx, vid, actor, to, reason)
	})
}

func (a marketplaceAdapter) Deprecate(ctx context.Context, vid marketplace.VersionID, actor econ.Actor, reason string) (marketplace.Version, error) {
	return a.versionOp(ctx, func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error) {
		return a.svc.Deprecate(ctx, tx, vid, actor, reason)
	})
}

func (a marketplaceAdapter) versionOp(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error)) (marketplace.Version, error) {
	var out marketplace.Version
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = fn(ctx, tx)
		return err
	})
	return out, err
}

func (a marketplaceAdapter) InstallVersion(ctx context.Context, r MarketplaceInstallRequest) (MarketplaceInstallView, bool, error) {
	var out MarketplaceInstallView
	var existed bool
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		in, ex, err := a.svc.InstallVersion(ctx, tx, marketplace.InstallRequest{VersionID: r.VersionID, AccountID: r.Account, Mode: r.Mode,
			Actor: r.Actor})
		if err != nil {
			return err
		}
		existed = ex
		out, err = a.installView(ctx, tx, in.ID)
		return err
	})
	return out, existed, err
}

func (a marketplaceAdapter) RemoveInstall(ctx context.Context, iid marketplace.InstallID, actor econ.Actor, reason string) (MarketplaceInstallView, error) {
	var out MarketplaceInstallView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := a.svc.RemoveInstall(ctx, tx, iid, actor, reason); err != nil {
			return err
		}
		var err error
		out, err = a.installView(ctx, tx, iid)
		return err
	})
	return out, err
}

func (a marketplaceAdapter) Follow(ctx context.Context, r MarketplaceFollowRequest) (MarketplaceFollowView, error) {
	var out MarketplaceFollowView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		run, err := a.svc.PaperFollow(ctx, tx, marketplace.FollowRequest{InstallID: r.InstallID, Input: r.Input,
			Trigger: marketplace.TriggerExplicit, Actor: r.Actor, CorrelationID: r.CorrelationID})
		if err != nil {
			return err
		}
		out, err = a.followView(ctx, tx, run)
		return err
	})
	return out, err
}

func (a marketplaceAdapter) RecordPerformance(ctx context.Context, iid marketplace.InstallID, actor econ.Actor) (marketplace.PerformanceSnapshot, error) {
	var out marketplace.PerformanceSnapshot
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.svc.RecordPerformance(ctx, tx, iid, actor)
		return err
	})
	return out, err
}

// --- what cmd/api injects into the marketplace -------------------------------
//
// internal/marketplace takes four collaborators it does not implement. The
// ArtifactBuilder is *sandboxrt.Builder wherever a Go toolchain exists. The
// other three have no production implementation in this build, so the
// thinnest real ones live here: risk sections read from the database, an
// executor that refuses honestly, and a builder that says it cannot build.

// UnavailableStrategyBuilder is the ArtifactBuilder of a deployment with no Go
// toolchain. It builds nothing and says so with UNSUPPORTED, which the
// marketplace returns unchanged: the validation records nothing and the
// version stays SUBMITTED. It never fabricates a build result.
type UnavailableStrategyBuilder struct {
	// Reason says why there is no toolchain, for the operator who asked.
	Reason string
}

// Build refuses.
func (b UnavailableStrategyBuilder) Build(context.Context, sandboxrt.Source) (sandboxrt.Build, error) {
	reason := strings.TrimSpace(b.Reason)
	if reason == "" {
		reason = "no Go toolchain is configured"
	}
	return sandboxrt.Build{}, errs.New(errs.CodeUnsupported,
		"strategies cannot be built in this deployment: "+reason+"; the version is unchanged")
}

// UnwiredPaperExecutor is the marketplace's IntentExecutor in a build that has
// no paper execution venue for economic intents (internal/orders, which the
// marketplace names as the implementer, is not part of this build). It
// executes nothing: every risk-approved intent it is handed is refused with
// UNSUPPORTED, which the marketplace records as a DISPATCH_FAILED outcome on a
// FAILED effect. It never reports a fill, so no follow can claim one.
type UnwiredPaperExecutor struct{}

// ExecutePaper refuses.
func (UnwiredPaperExecutor) ExecutePaper(context.Context, pgx.Tx, marketplace.PaperExecution) (marketplace.ExecutionReceipt, error) {
	return marketplace.ExecutionReceipt{}, errs.New(errs.CodeUnsupported,
		"no paper execution venue for strategy intents is wired in this deployment; nothing was executed")
}

// MarketplaceIdentityReader answers whether an account's owner holds a
// verified, unexpired identity. compliance.AccountVerification is the
// production one.
type MarketplaceIdentityReader interface {
	IdentityVerified(ctx context.Context, q db.Querier, account accounts.AccountID) (bool, error)
}

// MarketplaceSectionsConfig wires the production RiskSections.
type MarketplaceSectionsConfig struct {
	Clock        clock.Clock
	Accounts     *accounts.Repository
	Instruments  *instruments.Repository
	Identity     MarketplaceIdentityReader
	Prices       valuation.PriceSource
	KillSwitches *killswitch.Checker
	// MaxPriceAge bounds how far back the reference price is looked for; the
	// risk policy then judges its freshness itself.
	MaxPriceAge time.Duration
	// Verification and Jurisdiction feed the legal router, as they do the
	// settlement compiler of the native economy: nil means NONE and UNKNOWN.
	Verification VerificationResolver
	Jurisdiction JurisdictionResolver
}

// NewMarketplaceRiskSections builds the RiskSections the marketplace consults
// for every intent a strategy proposes.
func NewMarketplaceRiskSections(cfg MarketplaceSectionsConfig) (marketplace.RiskSections, error) {
	if cfg.Clock == nil || cfg.Accounts == nil || cfg.Instruments == nil || cfg.Identity == nil || cfg.Prices == nil ||
		cfg.KillSwitches == nil || cfg.MaxPriceAge <= 0 {
		return nil, errs.New(errs.CodeInternal, "marketplace risk sections: every dependency is required")
	}
	return marketplaceSections{cfg: cfg}, nil
}

// marketplaceCalendar24x7 is the one trading calendar this build can evaluate
// (instruments.Terms.TradingCalendar).
const marketplaceCalendar24x7 = "24x7"

// marketplaceSections is the production RiskSections. Every section comes from
// the database in the follow's own transaction -- the account, its owner's
// identity, the canonical instrument, the price store and the kill switches --
// and none from the snapshot the strategy ran on or from the strategy: the
// marketplace itself supplies the strategy and sandbox sections, and a section
// that cannot be established is left out, which the kernel refuses.
type marketplaceSections struct {
	cfg MarketplaceSectionsConfig
}

func (s marketplaceSections) RiskContext(ctx context.Context, q db.Querier, in econ.Intent, p sandboxrt.AcceptedProposal) (marketplace.RiskContext, error) {
	now := s.cfg.Clock.Now().UTC()
	acct, err := s.cfg.Accounts.Get(ctx, q, in.AccountID)
	if err != nil {
		return marketplace.RiskContext{}, err
	}
	verified, err := s.cfg.Identity.IdentityVerified(ctx, q, in.AccountID)
	if err != nil {
		return marketplace.RiskContext{}, err
	}
	input := risk.AdvancedInput{
		Account:  &risk.AdvancedAccountSnapshot{Status: string(acct.Status)},
		Identity: &risk.IdentitySnapshot{Verified: verified},
	}
	if input.Instrument, input.Oracle, err = s.instrumentSections(ctx, q, in.InstrumentID, p.InstrumentKind, now); err != nil {
		return marketplace.RiskContext{}, err
	}
	snap, err := s.cfg.KillSwitches.Snapshot(ctx, q)
	if err != nil {
		return marketplace.RiskContext{}, err
	}
	input.KillSwitches = []risk.KillSwitch{}
	for _, sw := range snap.Switches {
		if sw.Active {
			input.KillSwitches = append(input.KillSwitches, risk.KillSwitch{Kind: string(sw.Kind), Scope: sw.ScopeID})
		}
	}
	rc := marketplace.RiskContext{Input: input, Jurisdiction: "UNKNOWN", Verification: valuedomain.VerificationNone}
	if s.cfg.Verification != nil {
		if rc.Verification, err = s.cfg.Verification.Level(ctx, in.AccountID); err != nil {
			return marketplace.RiskContext{}, err
		}
	}
	if s.cfg.Jurisdiction != nil {
		j, err := s.cfg.Jurisdiction.Jurisdiction(ctx, in.AccountID)
		if err != nil {
			return marketplace.RiskContext{}, err
		}
		if strings.TrimSpace(j) != "" {
			rc.Jurisdiction = j
		}
	}
	return rc, nil
}

// instrumentSections reads the instrument and its reference price. An
// instrument the canonical model does not know, or one the strategy's snapshot
// described as a different kind, gets no section at all: the strategy's input
// misdescribed it, and the kernel refuses a missing section rather than
// trusting the description. Sessions are open for a SPOT_PAIR (continuous, as
// internal/margin's spot path holds) and, for an instrument with canonical
// terms, only on the 24x7 calendar (as internal/derivatives holds); there is no
// exchange-calendar evaluator, so any other calendar reads as closed.
func (s marketplaceSections) instrumentSections(ctx context.Context, q db.Querier, instrumentID, snapshotKind string, now time.Time) (*risk.InstrumentSnapshot, *risk.OracleSnapshot, error) {
	iid, err := instruments.ParseInstrumentID(instrumentID)
	if err != nil || iid.IsZero() {
		return nil, nil, nil
	}
	ins, err := s.cfg.Instruments.Get(ctx, q, iid)
	if errs.HasCode(err, errs.CodeNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if string(ins.Type) != snapshotKind {
		return nil, nil, nil
	}
	expired := ins.ActiveUntil != nil && !now.Before(*ins.ActiveUntil)
	open := false
	if ins.Type == instruments.TypeSpotPair {
		open = true
	} else {
		terms, err := s.cfg.Instruments.CurrentTerms(ctx, q, iid)
		switch {
		case err == nil:
			open = terms.Terms.TradingCalendar == marketplaceCalendar24x7
			if terms.Terms.Expiry != nil && !now.Before(*terms.Terms.Expiry) {
				expired = true
			}
		case errs.HasCode(err, errs.CodeNotFound):
		default:
			return nil, nil, err
		}
	}
	if now.Before(ins.ActiveFrom) {
		open = false
	}
	section := &risk.InstrumentSnapshot{Status: string(ins.Status), SessionOpen: open, Expired: expired}
	oracle := &risk.OracleSnapshot{Available: false, ObservedAt: now}
	if ins.BaseAssetID != nil && ins.QuoteAssetID != nil {
		price, err := s.cfg.Prices.Latest(ctx, q, *ins.BaseAssetID, *ins.QuoteAssetID, s.cfg.MaxPriceAge, now)
		switch {
		case err == nil:
			oracle = &risk.OracleSnapshot{Available: true, ObservedAt: price.At.UTC()}
		case errs.HasCode(err, errs.CodeStaleMarketData):
		default:
			return nil, nil, err
		}
	}
	return section, oracle, nil
}
