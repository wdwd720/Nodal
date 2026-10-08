package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
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
// services. Every write is one service operation in one transaction. Every
// list is the marketplace package's own read (the catalogue, an account's
// listings, installs and follows, the review queue, and a version's reviews,
// evaluations and paper performance), in one read-only transaction. What is
// left here are read-only projections in the manner of readmodel.go -- the
// Risk Kernel V2 decision and the effect behind each follow intent, and an
// account's sandbox runs: every statement below is a SELECT, and nothing here
// decides anything.
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

// --- reads ------------------------------------------------------------------

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

func marketplaceQueryErr(op string, err error) error {
	if ee, ok := errs.As(err); ok {
		return ee
	}
	return errs.Wrap(err, errs.CodeInternal, "marketplace read: "+op)
}

// marketplaceList reads one page of one of the marketplace package's lists in
// a read-only transaction, and the cursor of the page after it: key names the
// keyset position of a row.
func marketplaceList[T any](ctx context.Context, a marketplaceAdapter, cursor string, limit int,
	read func(ctx context.Context, q db.Querier, after *marketplace.PageKey, limit int) ([]T, bool, error),
	key func(T) (time.Time, string)) ([]T, string, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := marketplaceCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var after *marketplace.PageKey
	if hasCursor {
		after = &marketplace.PageKey{At: c.At, ID: c.ID}
	}
	var out []T
	var more bool
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, more, err = read(ctx, tx, after, limit)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	if !more || len(out) == 0 {
		return out, "", nil
	}
	at, last := key(out[len(out)-1])
	return out, encodeCursor(keysetCursor{At: at, ID: last}), nil
}

func marketplaceListingKey(s MarketplaceListingSummary) (time.Time, string) {
	return s.Listing.CreatedAt, s.Listing.ID.String()
}

func (a marketplaceAdapter) Catalog(ctx context.Context, cursor string, limit int) ([]MarketplaceListingSummary, string, error) {
	return marketplaceList(ctx, a, cursor, limit, marketplace.Catalogue, marketplaceListingKey)
}

func (a marketplaceAdapter) AccountListings(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]MarketplaceListingSummary, string, error) {
	return marketplaceList(ctx, a, cursor, limit,
		func(ctx context.Context, q db.Querier, after *marketplace.PageKey, limit int) ([]MarketplaceListingSummary, bool, error) {
			return marketplace.AccountListings(ctx, q, account, after, limit)
		}, marketplaceListingKey)
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
// recorded about each, from the services' own reads: the version, its
// validation and provenance, its reviews and evaluations, and the paper
// performance of its installs.
func (a marketplaceAdapter) listingView(ctx context.Context, q db.Querier, lid marketplace.ListingID) (MarketplaceListingView, error) {
	l, err := a.svc.Listing(ctx, q, lid)
	if err != nil {
		return MarketplaceListingView{}, err
	}
	view := MarketplaceListingView{Listing: l}
	ids, err := marketplace.ListingVersionIDs(ctx, q, lid, marketplaceMaxVersions)
	if err != nil || len(ids) == 0 {
		return view, err
	}
	reviews, err := marketplace.Reviews(ctx, q, ids)
	if err != nil {
		return MarketplaceListingView{}, err
	}
	evaluations, err := marketplace.Evaluations(ctx, q, ids, marketplaceMaxEvaluations)
	if err != nil {
		return MarketplaceListingView{}, err
	}
	performance, err := marketplace.PaperPerformance(ctx, q, ids, marketplaceMaxPerformance)
	if err != nil {
		return MarketplaceListingView{}, err
	}
	index := make(map[marketplace.VersionID]int, len(ids))
	for _, vid := range ids {
		v, err := a.svc.Version(ctx, q, vid)
		if err != nil {
			return MarketplaceListingView{}, err
		}
		vv := MarketplaceVersionView{Version: v, Reviews: reviews[vid], Evaluations: []MarketplaceEvaluation{},
			Performance: []marketplace.PerformanceSnapshot{}}
		if vv.Reviews == nil {
			vv.Reviews = []MarketplaceReview{}
		}
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
	for _, e := range evaluations {
		if i, ok := index[e.Evaluation.VersionID]; ok {
			view.Versions[i].Evaluations = append(view.Versions[i].Evaluations, e)
		}
	}
	for _, p := range performance {
		if i, ok := index[p.VersionID]; ok {
			view.Versions[i].Performance = append(view.Versions[i].Performance, p)
		}
	}
	return view, nil
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
	return marketplaceList(ctx, a, cursor, limit,
		func(ctx context.Context, q db.Querier, after *marketplace.PageKey, limit int) ([]MarketplaceQueueItem, bool, error) {
			return marketplace.Queue(ctx, q, statuses, after, limit)
		}, func(it MarketplaceQueueItem) (time.Time, string) { return it.Version.CreatedAt, it.Version.ID.String() })
}

func (a marketplaceAdapter) Installs(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]MarketplaceInstallView, string, error) {
	return marketplaceList(ctx, a, cursor, limit,
		func(ctx context.Context, q db.Querier, after *marketplace.PageKey, limit int) ([]MarketplaceInstallView, bool, error) {
			return marketplace.AccountInstalls(ctx, q, account, after, limit)
		}, func(iv MarketplaceInstallView) (time.Time, string) {
			return iv.Install.CreatedAt, iv.Install.ID.String()
		})
}

func (a marketplaceAdapter) installView(ctx context.Context, q db.Querier, iid marketplace.InstallID) (MarketplaceInstallView, error) {
	return marketplace.InstallSummaryOf(ctx, q, iid)
}

func (a marketplaceAdapter) InstallRecord(ctx context.Context, iid marketplace.InstallID) (marketplace.Install, error) {
	return a.svc.Install(ctx, a.db, iid)
}

func (a marketplaceAdapter) Follows(ctx context.Context, account accounts.AccountID, install *marketplace.InstallID, cursor string, limit int) ([]MarketplaceFollowView, string, error) {
	return marketplaceList(ctx, a, cursor, limit,
		func(ctx context.Context, q db.Querier, after *marketplace.PageKey, limit int) ([]MarketplaceFollowView, bool, error) {
			runs, more, err := marketplace.AccountFollowRuns(ctx, q, account, install, after, limit)
			if err != nil {
				return nil, false, err
			}
			out := make([]MarketplaceFollowView, 0, len(runs))
			for _, run := range runs {
				view, err := a.followView(ctx, q, run)
				if err != nil {
					return nil, false, err
				}
				out = append(out, view)
			}
			return out, more, nil
		}, func(v MarketplaceFollowView) (time.Time, string) { return v.Run.CreatedAt, v.Run.ID.String() })
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
		ev, err := a.svc.Simulate(ctx, tx, marketplace.SimulationRequest{VersionID: r.VersionID, AccountID: r.Account,
			DatasetVersion: r.DatasetVersion, Snapshots: r.Snapshots, Actor: r.Actor})
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

// Validate claims, builds and records a version between transactions (the
// service opens them on the database itself: no transaction is held while the
// strategy builds).
func (a marketplaceAdapter) Validate(ctx context.Context, vid marketplace.VersionID, actor econ.Actor) (marketplace.Version, marketplace.Validation, error) {
	v, val, err := a.svc.RunValidation(ctx, a.db, vid, actor)
	if err != nil && ctx.Err() != nil {
		return marketplace.Version{}, marketplace.Validation{}, errs.Wrap(err, errs.CodeProviderUnavailable,
			"the request ended before the validation completed; its outcome is not confirmed, retry with the same Idempotency-Key")
	}
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
		run, err := a.svc.PaperFollow(ctx, tx, marketplace.FollowRequest{InstallID: r.InstallID, Trigger: marketplace.TriggerExplicit,
			Actor: r.Actor, CorrelationID: r.CorrelationID})
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
// ArtifactBuilder is *sandboxrt.Builder wherever a Go toolchain exists (and a
// builder that says it cannot build where none does). The IntentExecutor is
// the order book's paper venue (internal/orders), adapted below. The risk
// sections are read from the database, below.

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

// UnwiredPaperExecutor is the marketplace's IntentExecutor in a process that
// wires no paper venue. cmd/api wires MarketplacePaperExecutor, over the order
// book's paper venue; a process that builds no venue executes nothing: every
// risk-approved intent it is handed is refused with UNSUPPORTED, which the
// marketplace records as a DISPATCH_FAILED outcome on a FAILED effect. It
// never reports a fill, so no follow can claim one.
type UnwiredPaperExecutor struct{}

// ExecutePaper refuses.
func (UnwiredPaperExecutor) ExecutePaper(context.Context, pgx.Tx, marketplace.PaperExecution) (marketplace.ExecutionReceipt, error) {
	return marketplace.ExecutionReceipt{}, errs.New(errs.CodeUnsupported,
		"no paper execution venue for strategy intents is wired in this process; nothing was executed")
}

// MarketplacePaperSchedule is the one published charge schedule of strategy
// paper fills, for every process that wires MarketplacePaperExecutor: a 10
// bps spread, a 10 bps platform fee and a 5 bps strategy fee, each itemized
// on every fill's stored disclosure and posted to its own account.
func MarketplacePaperSchedule() fees.Schedule {
	return fees.Schedule{Version: "marketplace-paper-v1", SpreadBPS: 10, PercentageFeeBPS: 10, StrategyFeeBPS: 5,
		Rounding: fees.DefaultRounding()}
}

// MarketplacePaperExecutorConfig wires the marketplace's IntentExecutor over
// the order book's paper venue.
type MarketplacePaperExecutorConfig struct {
	// Venue is the paper venue's configuration (orders.PaperConfig); its
	// Schedules are not read: every instrument a strategy trades is priced
	// under Schedule.
	Venue orders.PaperConfig
	// Schedule is the published charge schedule of strategy fills -- its
	// spread and fees disclosed on every fill. It is quoted, fill by fill, in
	// the instrument's own quote asset; the asset and decimals it names are
	// replaced by the instrument's.
	Schedule fees.Schedule
	// Intents moves each executed intent through EXECUTING to COMPLETED: the
	// venue never moves an intent, its caller does.
	Intents *econ.IntentStore
	// Actor is the execution actor on those transitions (default SERVICE
	// "marketplace-paper-execution").
	Actor econ.Actor
}

// MarketplacePaperExecutor executes the marketplace's risk-approved PAPER
// intents on the order book's paper venue: an immediate fill of the intent's
// quantity at the reference mark, priced through the published schedule,
// whose spread and fees are itemized on the fill's stored disclosure and
// posted to their own accounts -- the account's real paper execution. The
// marketplace hands it only intents Risk V2 allowed, inside a savepoint, and
// exactly once.
type MarketplacePaperExecutor struct {
	cfg MarketplacePaperExecutorConfig
}

var _ marketplace.IntentExecutor = (*MarketplacePaperExecutor)(nil)

// NewMarketplacePaperExecutor validates the venue's dependencies and the
// schedule.
func NewMarketplacePaperExecutor(cfg MarketplacePaperExecutorConfig) (*MarketplacePaperExecutor, error) {
	if cfg.Intents == nil || cfg.Venue.Instruments == nil || cfg.Venue.Assets == nil {
		return nil, errs.New(errs.CodeInternal, "marketplace paper executor: the intent store, instruments and assets are required")
	}
	if strings.TrimSpace(cfg.Schedule.Version) == "" {
		return nil, errs.New(errs.CodeInternal, "marketplace paper executor: the published schedule needs a version")
	}
	probe := cfg.Schedule
	probe.QuoteAsset, probe.QuoteDecimals = econ.SandboxDollarID(), econ.SandboxDollarDecimals
	if err := probe.Validate(); err != nil {
		return nil, err
	}
	cfg.Venue.Schedules = nil
	if _, err := orders.NewPaperVenue(cfg.Venue); err != nil {
		return nil, err
	}
	if cfg.Actor == (econ.Actor{}) {
		cfg.Actor = econ.Actor{Type: security.ActorService, ID: "marketplace-paper-execution"}
	}
	return &MarketplacePaperExecutor{cfg: cfg}, nil
}

// ExecutePaper fills the intent on the paper venue and completes it. A refusal
// of the venue (no fresh mark, the account cannot cover the fill, a kind the
// venue does not fill) is returned as it is: the marketplace records the
// dispatch as failed, and nothing the attempt wrote survives its savepoint.
func (e *MarketplacePaperExecutor) ExecutePaper(ctx context.Context, tx pgx.Tx, ex marketplace.PaperExecution) (marketplace.ExecutionReceipt, error) {
	iid, err := instruments.ParseInstrumentID(ex.Intent.InstrumentID)
	if err != nil || iid.IsZero() {
		return marketplace.ExecutionReceipt{}, errs.New(errs.CodeValidationFailed, "marketplace paper executor: the intent names no instrument")
	}
	quoteID, err := e.quoteAsset(ctx, tx, iid)
	if err != nil {
		return marketplace.ExecutionReceipt{}, err
	}
	quote, err := e.cfg.Venue.Assets.Get(ctx, tx, quoteID)
	if err != nil {
		return marketplace.ExecutionReceipt{}, err
	}
	schedule := e.cfg.Schedule
	schedule.QuoteAsset, schedule.QuoteDecimals = quote.ID, quote.Decimals
	venueCfg := e.cfg.Venue
	venueCfg.Schedules = map[instruments.InstrumentID]fees.Schedule{iid: schedule}
	venue, err := orders.NewPaperVenue(venueCfg)
	if err != nil {
		return marketplace.ExecutionReceipt{}, err
	}
	if _, err := e.cfg.Intents.Transition(ctx, tx, ex.Intent.ID, econ.StatusExecuting, e.cfg.Actor, "paper venue fill", ""); err != nil {
		return marketplace.ExecutionReceipt{}, err
	}
	fill, err := venue.ExecuteIntent(ctx, tx, ex.Intent)
	if err != nil {
		return marketplace.ExecutionReceipt{}, err
	}
	ref := "paper-fill:" + fill.ID.String()
	if _, err := e.cfg.Intents.Transition(ctx, tx, ex.Intent.ID, econ.StatusCompleted, e.cfg.Actor, "filled on the paper venue ("+ref+")", ""); err != nil {
		return marketplace.ExecutionReceipt{}, err
	}
	return marketplace.ExecutionReceipt{ExternalRef: ref}, nil
}

// quoteAsset is the asset an instrument is quoted in, as the paper venue reads
// it: the canonical terms' quote (or settlement) asset, else a spot pair's
// quote asset.
func (e *MarketplacePaperExecutor) quoteAsset(ctx context.Context, q db.Querier, iid instruments.InstrumentID) (assets.AssetID, error) {
	terms, err := e.cfg.Venue.Instruments.CurrentTerms(ctx, q, iid)
	switch {
	case err == nil:
		if terms.Terms.QuoteAssetID != nil {
			return *terms.Terms.QuoteAssetID, nil
		}
		return terms.Terms.SettlementAssetID, nil
	case !errs.HasCode(err, errs.CodeNotFound):
		return assets.AssetID{}, err
	}
	ins, err := e.cfg.Venue.Instruments.Get(ctx, q, iid)
	if err != nil {
		return assets.AssetID{}, err
	}
	if ins.QuoteAssetID == nil {
		return assets.AssetID{}, errs.New(errs.CodeUnsupported, "marketplace paper executor: the instrument has no quote asset the paper venue can fill in")
	}
	return *ins.QuoteAssetID, nil
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
