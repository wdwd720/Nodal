package marketplace

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// The reads below are the lists a reader of the marketplace sees: the
// catalogue, an account's own listings, installs and follow history, the
// review queue, and what is recorded about a version (its reviews,
// evaluations and paper performance). Each is a few SELECTs and decides
// nothing. They live beside the writes so the package's rules about what may
// be shown hold for every reader: the catalogue lists only APPROVED and
// CLEARED versions, and paper performance is only what a paper book proved --
// never a snapshot recorded before paper books, whose figures a caller
// supplied (review-2 F10).

// MaxReadLimit is the most rows one page of a list holds, and the most
// versions or installs one read looks up.
const MaxReadLimit = 200

// PageKey is a keyset position in a list: the creation time and id of the
// last row a page returned. Each list states its order; the next page
// continues strictly past the key.
type PageKey struct {
	At time.Time
	ID string
}

// VersionSummary is the part of a version a list shows.
type VersionSummary struct {
	ID            VersionID
	ListingID     ListingID
	Number        int
	Status        Status
	Moderation    Moderation
	RiskClass     RiskClass
	Description   string
	RejectionCode string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ListingSummary is a listing with the one version a list shows: the newest
// installable one in the catalogue, the newest of any status in an account's
// own list.
type ListingSummary struct {
	Listing Listing
	Latest  VersionSummary
}

// QueueItem is a version awaiting validation or review, with its listing.
type QueueItem struct {
	Listing Listing
	Version VersionSummary
}

// Review is one recorded review decision about a version.
type Review struct {
	Decision   ReviewDecision
	ReasonCode string
	Note       string
	Reviewer   econ.Actor
	ReviewedAt time.Time
}

// EvaluationRecord is a recorded simulation as read back.
type EvaluationRecord struct {
	Evaluation     Evaluation
	SnapshotsCount int
	CreatedAt      time.Time
}

// InstallSummary is an install with the listing and version it installed and
// the newest performance snapshot its paper book proved, if any.
type InstallSummary struct {
	Install           Install
	Listing           Listing
	Version           VersionSummary
	LatestPerformance *PerformanceSnapshot
}

func checkPage(after *PageKey, limit int) error {
	if limit < 1 || limit > MaxReadLimit {
		return errs.New(errs.CodeValidationFailed, fmt.Sprintf("marketplace: a page holds 1 to %d rows", MaxReadLimit)).
			WithField("limit", limit)
	}
	if after != nil {
		if _, err := id.ParseAny(after.ID); err != nil {
			return errs.New(errs.CodeValidationFailed, "marketplace: a page key names no row")
		}
	}
	return nil
}

func checkLookup(n int, what string) error {
	if n > MaxReadLimit {
		return errs.New(errs.CodeValidationFailed, fmt.Sprintf("marketplace: one read looks up at most %d %s", MaxReadLimit, what)).
			WithField("count", n)
	}
	return nil
}

// keyed cuts a list read with one row more than its page into the page and
// whether another page follows it.
func keyed[T any](rows []T, limit int) ([]T, bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

func idStrings[K id.Kind](ids []id.ID[K]) []string {
	out := make([]string, 0, len(ids))
	for _, i := range ids {
		out = append(out, i.String())
	}
	return out
}

const (
	summaryListingColumns = `l.id, l.creator_account_id, l.creator_actor_type, l.creator_actor_id, l.title, l.forked_from_version_id,
		l.created_at`
	summaryVersionColumns = `v.id, v.listing_id, v.version, v.status, v.moderation_state, v.risk_class, v.description,
		coalesce(v.rejection_code, ''), v.created_at, v.updated_at`
	summaryInstallColumns = `i.id, i.account_id, i.version_id, i.mode, i.status, i.installed_by_actor_type, i.installed_by_actor_id,
		i.paper_capital::text, i.created_at`
	performanceColumns = `install_id, version_id, follow_runs_count, first_as_of, last_as_of, history_digest, return_bps,
		max_drawdown_bps, volatility_bps, turnover_bps, computed_at`
)

func listingDest(l *Listing) []any {
	return []any{&l.ID, &l.CreatorAccountID, &l.Creator.Type, &l.Creator.ID, &l.Title, &l.ForkedFromVersionID, &l.CreatedAt}
}

func summaryDest(v *VersionSummary) []any {
	return []any{&v.ID, &v.ListingID, &v.Number, &v.Status, &v.Moderation, &v.RiskClass, &v.Description, &v.RejectionCode,
		&v.CreatedAt, &v.UpdatedAt}
}

func installDest(in *Install) []any {
	return []any{&in.ID, &in.AccountID, &in.VersionID, &in.Mode, &in.Status, &in.InstalledBy.Type, &in.InstalledBy.ID, &in.PaperCapital,
		&in.CreatedAt}
}

func summaryUTC(l *Listing, v *VersionSummary) {
	l.CreatedAt = l.CreatedAt.UTC()
	v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
}

func scanPerformance(row pgx.Row) (PerformanceSnapshot, error) {
	var p PerformanceSnapshot
	if err := row.Scan(&p.InstallID, &p.VersionID, &p.Points, &p.FirstAsOf, &p.LastAsOf, &p.HistoryDigest, &p.ReturnBPS,
		&p.MaxDrawdownBPS, &p.VolatilityBPS, &p.TurnoverBPS, &p.ComputedAt); err != nil {
		return PerformanceSnapshot{}, err
	}
	p.FirstAsOf, p.LastAsOf, p.ComputedAt = p.FirstAsOf.UTC(), p.LastAsOf.UTC(), p.ComputedAt.UTC()
	return p, nil
}

// Catalogue lists the listings anyone may install, newest listing first, each
// with its newest APPROVED and CLEARED version. A listing without one is not
// in the catalogue.
func Catalogue(ctx context.Context, q db.Querier, after *PageKey, limit int) ([]ListingSummary, bool, error) {
	return listingPage(ctx, q, after, limit, ` AND v.status = 'APPROVED' AND v.moderation_state = 'CLEARED'`, "", nil)
}

// AccountListings lists an account's own listings, newest first, each with
// its newest version of any status.
func AccountListings(ctx context.Context, q db.Querier, account accounts.AccountID, after *PageKey, limit int) ([]ListingSummary, bool, error) {
	return listingPage(ctx, q, after, limit, "", ` AND l.creator_account_id = $1`, []any{account})
}

func listingPage(ctx context.Context, q db.Querier, after *PageKey, limit int, versionWhere, where string, args []any) ([]ListingSummary, bool, error) {
	if err := checkPage(after, limit); err != nil {
		return nil, false, err
	}
	sql := `SELECT ` + summaryListingColumns + `, ` + summaryVersionColumns + ` FROM marketplace_listings l
		JOIN LATERAL (SELECT * FROM marketplace_listing_versions v WHERE v.listing_id = l.id` + versionWhere + `
			ORDER BY v.version DESC LIMIT 1) v ON true
		WHERE true` + where
	if after != nil {
		sql += fmt.Sprintf(` AND (l.created_at, l.id) < ($%d, $%d::uuid)`, len(args)+1, len(args)+2)
		args = append(args, after.At, after.ID)
	}
	sql += fmt.Sprintf(` ORDER BY l.created_at DESC, l.id DESC LIMIT %d`, limit+1)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, dbErr("list listings", err)
	}
	defer rows.Close()
	var out []ListingSummary
	for rows.Next() {
		var s ListingSummary
		if err := rows.Scan(append(listingDest(&s.Listing), summaryDest(&s.Latest)...)...); err != nil {
			return nil, false, dbErr("scan listing", err)
		}
		summaryUTC(&s.Listing, &s.Latest)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, false, dbErr("list listings", err)
	}
	page, more := keyed(out, limit)
	return page, more, nil
}

// Queue lists the versions whose status is one of statuses, oldest version
// first: the review queue.
func Queue(ctx context.Context, q db.Querier, statuses []Status, after *PageKey, limit int) ([]QueueItem, bool, error) {
	if err := checkPage(after, limit); err != nil {
		return nil, false, err
	}
	if len(statuses) == 0 {
		return nil, false, errs.New(errs.CodeValidationFailed, "marketplace: a queue names at least one status")
	}
	wanted := make([]string, 0, len(statuses))
	for _, st := range statuses {
		if !st.Valid() {
			return nil, false, errs.New(errs.CodeValidationFailed, "marketplace: unknown version status").WithField("status", string(st))
		}
		wanted = append(wanted, string(st))
	}
	args := []any{wanted}
	sql := `SELECT ` + summaryListingColumns + `, ` + summaryVersionColumns + ` FROM marketplace_listing_versions v
		JOIN marketplace_listings l ON l.id = v.listing_id WHERE v.status = ANY($1::text[])`
	if after != nil {
		sql += ` AND (v.created_at, v.id) > ($2, $3::uuid)`
		args = append(args, after.At, after.ID)
	}
	sql += fmt.Sprintf(` ORDER BY v.created_at, v.id LIMIT %d`, limit+1)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, dbErr("list review queue", err)
	}
	defer rows.Close()
	var out []QueueItem
	for rows.Next() {
		var it QueueItem
		if err := rows.Scan(append(listingDest(&it.Listing), summaryDest(&it.Version)...)...); err != nil {
			return nil, false, dbErr("scan review queue", err)
		}
		summaryUTC(&it.Listing, &it.Version)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, false, dbErr("list review queue", err)
	}
	page, more := keyed(out, limit)
	return page, more, nil
}

// ListingVersionIDs returns the ids of a listing's newest versions, newest
// first, at most limit.
func ListingVersionIDs(ctx context.Context, q db.Querier, lid ListingID, limit int) ([]VersionID, error) {
	if err := checkPage(nil, limit); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT id FROM marketplace_listing_versions WHERE listing_id = $1 ORDER BY version DESC LIMIT $2`, lid, limit)
	if err != nil {
		return nil, dbErr("list versions", err)
	}
	defer rows.Close()
	var out []VersionID
	for rows.Next() {
		var vid VersionID
		if err := rows.Scan(&vid); err != nil {
			return nil, dbErr("scan version id", err)
		}
		out = append(out, vid)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("list versions", err)
	}
	return out, nil
}

// Reviews returns the review decisions recorded about versions, each
// version's oldest first.
func Reviews(ctx context.Context, q db.Querier, versions []VersionID) (map[VersionID][]Review, error) {
	if err := checkLookup(len(versions), "versions"); err != nil {
		return nil, err
	}
	out := map[VersionID][]Review{}
	if len(versions) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT version_id, decision, reason_code, note, reviewer_actor_type, reviewer_actor_id, reviewed_at
		FROM marketplace_reviews WHERE version_id = ANY($1::uuid[]) ORDER BY reviewed_at, id`, idStrings(versions))
	if err != nil {
		return nil, dbErr("list reviews", err)
	}
	defer rows.Close()
	for rows.Next() {
		var vid VersionID
		var r Review
		if err := rows.Scan(&vid, &r.Decision, &r.ReasonCode, &r.Note, &r.Reviewer.Type, &r.Reviewer.ID, &r.ReviewedAt); err != nil {
			return nil, dbErr("scan review", err)
		}
		r.ReviewedAt = r.ReviewedAt.UTC()
		out[vid] = append(out[vid], r)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("list reviews", err)
	}
	return out, nil
}

// Evaluations returns the newest evaluations recorded about versions, newest
// first, at most limit in all.
func Evaluations(ctx context.Context, q db.Querier, versions []VersionID, limit int) ([]EvaluationRecord, error) {
	if err := checkPage(nil, limit); err != nil {
		return nil, err
	}
	if err := checkLookup(len(versions), "versions"); err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT id, version_id, account_id, kind, dataset_version, snapshots_count, snapshots_digest, results_digest,
		runs_ok, proposals_count, accepted_count, violations_count, evidence_ref, evidence_sha256, created_at
		FROM marketplace_evaluations WHERE version_id = ANY($1::uuid[]) ORDER BY created_at DESC, id DESC LIMIT $2`,
		idStrings(versions), limit)
	if err != nil {
		return nil, dbErr("list evaluations", err)
	}
	defer rows.Close()
	var out []EvaluationRecord
	for rows.Next() {
		var e EvaluationRecord
		var account *accounts.AccountID
		ev := &e.Evaluation
		if err := rows.Scan(&ev.ID, &ev.VersionID, &account, &ev.Kind, &ev.DatasetVersion, &e.SnapshotsCount, &ev.SnapshotsDigest,
			&ev.ResultsDigest, &ev.RunsOK, &ev.ProposalsCount, &ev.AcceptedCount, &ev.ViolationsCount, &ev.EvidenceRef,
			&ev.EvidenceSHA256, &e.CreatedAt); err != nil {
			return nil, dbErr("scan evaluation", err)
		}
		if account != nil {
			ev.AccountID = *account
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("list evaluations", err)
	}
	return out, nil
}

// PaperPerformance returns the performance snapshots of installs of versions
// that their paper books proved, newest first, at most limit. A snapshot
// recorded before paper books is never among them.
func PaperPerformance(ctx context.Context, q db.Querier, versions []VersionID, limit int) ([]PerformanceSnapshot, error) {
	if err := checkPage(nil, limit); err != nil {
		return nil, err
	}
	if err := checkLookup(len(versions), "versions"); err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT `+performanceColumns+` FROM marketplace_performance_snapshots
		WHERE from_paper_book AND version_id = ANY($1::uuid[]) ORDER BY computed_at DESC, id DESC LIMIT $2`, idStrings(versions), limit)
	if err != nil {
		return nil, dbErr("list performance", err)
	}
	defer rows.Close()
	var out []PerformanceSnapshot
	for rows.Next() {
		p, err := scanPerformance(rows)
		if err != nil {
			return nil, dbErr("scan performance", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("list performance", err)
	}
	return out, nil
}

// LatestPaperPerformance returns, for each install that has one, the newest
// performance snapshot its paper book proved.
func LatestPaperPerformance(ctx context.Context, q db.Querier, installs []InstallID) (map[InstallID]PerformanceSnapshot, error) {
	if err := checkLookup(len(installs), "installs"); err != nil {
		return nil, err
	}
	out := map[InstallID]PerformanceSnapshot{}
	if len(installs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (install_id) `+performanceColumns+` FROM marketplace_performance_snapshots
		WHERE from_paper_book AND install_id = ANY($1::uuid[]) ORDER BY install_id, computed_at DESC, id DESC`, idStrings(installs))
	if err != nil {
		return nil, dbErr("latest performance", err)
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanPerformance(rows)
		if err != nil {
			return nil, dbErr("scan performance", err)
		}
		out[p.InstallID] = p
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("latest performance", err)
	}
	return out, nil
}

// AccountInstalls lists an account's installs, newest first, each with its
// listing, version and the newest performance its paper book proved.
func AccountInstalls(ctx context.Context, q db.Querier, account accounts.AccountID, after *PageKey, limit int) ([]InstallSummary, bool, error) {
	if err := checkPage(after, limit); err != nil {
		return nil, false, err
	}
	args := []any{account}
	where := `i.account_id = $1`
	if after != nil {
		where += ` AND (i.created_at, i.id) < ($2, $3::uuid)`
		args = append(args, after.At, after.ID)
	}
	out, err := installSummaries(ctx, q, where, args, fmt.Sprintf(` ORDER BY i.created_at DESC, i.id DESC LIMIT %d`, limit+1))
	if err != nil {
		return nil, false, err
	}
	page, more := keyed(out, limit)
	return page, more, nil
}

// InstallSummaryOf returns one install with its listing, version and the
// newest performance its paper book proved.
func InstallSummaryOf(ctx context.Context, q db.Querier, iid InstallID) (InstallSummary, error) {
	out, err := installSummaries(ctx, q, `i.id = $1`, []any{iid}, "")
	if err != nil {
		return InstallSummary{}, err
	}
	if len(out) != 1 {
		return InstallSummary{}, errs.New(errs.CodeNotFound, "marketplace: install not found").WithField("install_id", iid.String())
	}
	return out[0], nil
}

func installSummaries(ctx context.Context, q db.Querier, where string, args []any, tail string) ([]InstallSummary, error) {
	rows, err := q.Query(ctx, `SELECT `+summaryInstallColumns+`, `+summaryListingColumns+`, `+summaryVersionColumns+`
		FROM marketplace_installs i JOIN marketplace_listing_versions v ON v.id = i.version_id
		JOIN marketplace_listings l ON l.id = v.listing_id WHERE `+where+tail, args...)
	if err != nil {
		return nil, dbErr("list installs", err)
	}
	var out []InstallSummary
	for rows.Next() {
		var s InstallSummary
		if err := rows.Scan(append(append(installDest(&s.Install), listingDest(&s.Listing)...), summaryDest(&s.Version)...)...); err != nil {
			rows.Close()
			return nil, dbErr("scan install", err)
		}
		s.Install.CreatedAt = s.Install.CreatedAt.UTC()
		summaryUTC(&s.Listing, &s.Version)
		out = append(out, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, dbErr("list installs", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	ids := make([]InstallID, 0, len(out))
	for _, s := range out {
		ids = append(ids, s.Install.ID)
	}
	latest, err := LatestPaperPerformance(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if p, ok := latest[out[i].Install.ID]; ok {
			out[i].LatestPerformance = &p
		}
	}
	return out, nil
}

// AccountFollowRuns lists an account's follow runs, newest first -- of one of
// its installs when install is set -- each with what became of its proposals
// and the reference prices it was valued at.
func AccountFollowRuns(ctx context.Context, q db.Querier, account accounts.AccountID, install *InstallID, after *PageKey, limit int) ([]FollowRun, bool, error) {
	if err := checkPage(after, limit); err != nil {
		return nil, false, err
	}
	args := []any{account}
	sql := `SELECT ` + followColumns + ` FROM marketplace_follow_runs f JOIN sandbox_runs r ON r.id = f.sandbox_run_id
		WHERE f.account_id = $1`
	if install != nil {
		args = append(args, *install)
		sql += fmt.Sprintf(` AND f.install_id = $%d`, len(args))
	}
	if after != nil {
		sql += fmt.Sprintf(` AND (f.created_at, f.id) < ($%d, $%d::uuid)`, len(args)+1, len(args)+2)
		args = append(args, after.At, after.ID)
	}
	sql += fmt.Sprintf(` ORDER BY f.created_at DESC, f.id DESC LIMIT %d`, limit+1)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, dbErr("list follow runs", err)
	}
	var out []FollowRun
	for rows.Next() {
		f, err := scanFollow(rows)
		if err != nil {
			rows.Close()
			return nil, false, dbErr("scan follow run", err)
		}
		out = append(out, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, dbErr("list follow runs", err)
	}
	page, more := keyed(out, limit)
	for i := range page {
		if page[i].Intents, err = followIntents(ctx, q, page[i].ID); err != nil {
			return nil, false, err
		}
		if page[i].Marks, err = followMarks(ctx, q, page[i].ID); err != nil {
			return nil, false, err
		}
	}
	return page, more, nil
}
