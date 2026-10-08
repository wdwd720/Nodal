//go:build integration

package marketplace

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/sandboxrt"
)

// everyPage reads a whole list, page by page, as a reader would.
func everyPage[T any](t *testing.T, read func(after *PageKey) ([]T, bool, error), key func(T) PageKey) []T {
	t.Helper()
	var all []T
	var after *PageKey
	for pages := 0; ; pages++ {
		require.Less(t, pages, 100, "a list that never ends")
		page, more, err := read(after)
		require.NoError(t, err)
		all = append(all, page...)
		if !more {
			return all
		}
		require.NotEmpty(t, page)
		k := key(page[len(page)-1])
		after = &k
	}
}

func listingKey(s ListingSummary) PageKey {
	return PageKey{At: s.Listing.CreatedAt, ID: s.Listing.ID.String()}
}

// TestIntegration_TheCatalogueListsOnlyWhatAnyoneMayInstall: the catalogue
// shows a listing with its newest APPROVED and CLEARED version -- never a
// newer draft, never a listing that has none, and not once moderation flags
// it -- while the creator's own list shows every listing with its newest
// version of any status, page by page.
func TestIntegration_TheCatalogueListsOnlyWhatAnyoneMayInstall(t *testing.T) {
	f := newFixture(t)
	approved, v1 := f.approved("momentum")
	var v2 Version
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v2, err = f.svc.NewVersion(ctx, tx, approved.ID, f.creator, f.versionRequest("momentum"))
		return err
	}))
	draftOnly, _ := f.draft("greedy")

	catalogue := func() map[ListingID]ListingSummary {
		all := everyPage(t, func(after *PageKey) ([]ListingSummary, bool, error) {
			return Catalogue(f.ctx, testDB, after, MaxReadLimit)
		}, listingKey)
		out := map[ListingID]ListingSummary{}
		for _, s := range all {
			out[s.Listing.ID] = s
		}
		return out
	}
	shown := catalogue()
	require.Contains(t, shown, approved.ID)
	assert.Equal(t, v1.ID, shown[approved.ID].Latest.ID, "the installable version, not the newer draft")
	assert.Equal(t, StatusApproved, shown[approved.ID].Latest.Status)
	assert.NotContains(t, shown, draftOnly.ID, "a listing with nothing approved is not in the catalogue")

	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.Moderate(ctx, tx, v1.ID, f.operator2, ModerationFlagged, "under investigation")
	})
	assert.NotContains(t, catalogue(), approved.ID, "a flagged version is not installable")

	// The creator's own list: newest listing first, one per page.
	first, more, err := AccountListings(f.ctx, testDB, f.creatorAccount, nil, 1)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.True(t, more)
	assert.Equal(t, draftOnly.ID, first[0].Listing.ID)
	key := listingKey(first[0])
	second, more, err := AccountListings(f.ctx, testDB, f.creatorAccount, &key, 1)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.False(t, more)
	assert.Equal(t, approved.ID, second[0].Listing.ID)
	assert.Equal(t, v2.ID, second[0].Latest.ID, "the newest version of any status")
	assert.Equal(t, StatusDraft, second[0].Latest.Status)
}

// TestIntegration_AVersionsRecordReadsBackWhole: the review queue holds a
// submitted version; a version's reviews and evaluations read back as they
// were recorded, the evaluation with the account it was charged to.
func TestIntegration_AVersionsRecordReadsBackWhole(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	reviews, err := Reviews(f.ctx, testDB, []VersionID{v.ID})
	require.NoError(t, err)
	require.Len(t, reviews[v.ID], 1)
	r := reviews[v.ID][0]
	assert.Equal(t, DecisionApprove, r.Decision)
	assert.Equal(t, "MEETS_POLICY", r.ReasonCode)
	assert.Equal(t, f.operator, r.Reviewer)

	ev, err := f.simulation(v, "closes-2026-09", []sandboxrt.Input{f.input(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))})
	require.NoError(t, err)
	evaluations, err := Evaluations(f.ctx, testDB, []VersionID{v.ID}, 10)
	require.NoError(t, err)
	require.Len(t, evaluations, 1)
	assert.Equal(t, ev.ID, evaluations[0].Evaluation.ID)
	assert.Equal(t, f.creatorAccount, evaluations[0].Evaluation.AccountID)
	assert.Equal(t, 1, evaluations[0].SnapshotsCount)
	assert.Equal(t, ev.ResultsDigest, evaluations[0].Evaluation.ResultsDigest)

	_, waiting := f.draft("greedy")
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.Submit(ctx, tx, waiting.ID, f.creator)
	})
	queue := everyPage(t, func(after *PageKey) ([]QueueItem, bool, error) {
		return Queue(f.ctx, testDB, []Status{StatusSubmitted}, after, MaxReadLimit)
	}, func(it QueueItem) PageKey { return PageKey{At: it.Version.CreatedAt, ID: it.Version.ID.String()} })
	var found bool
	for i, it := range queue {
		assert.Equal(t, StatusSubmitted, it.Version.Status)
		if i > 0 {
			assert.False(t, it.Version.CreatedAt.Before(queue[i-1].Version.CreatedAt), "oldest first")
		}
		found = found || it.Version.ID == waiting.ID
	}
	assert.True(t, found, "a submitted version waits in the queue")
}

// legacyPerformance writes a performance snapshot the way one was written
// before paper books: from figures a caller supplied (from_paper_book false).
// The database refuses such a row since 00757, so the migration's owner sets
// that binding aside for this one insert, inside one transaction.
func (f *fixture) legacyPerformance(in Install, returnBPS int64, at time.Time) {
	f.t.Helper()
	conn, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(f.t, err)
	defer func() { _ = conn.Close(f.ctx) }()
	tx, err := conn.Begin(f.ctx)
	require.NoError(f.t, err)
	defer func() { _ = tx.Rollback(f.ctx) }()
	_, err = tx.Exec(f.ctx, `ALTER TABLE marketplace_performance_snapshots DISABLE TRIGGER marketplace_performance_snapshots_from_book`)
	require.NoError(f.t, err)
	_, err = tx.Exec(f.ctx, `INSERT INTO marketplace_performance_snapshots (id, install_id, version_id, follow_runs_count, first_as_of,
		last_as_of, history_digest, return_bps, max_drawdown_bps, volatility_bps, turnover_bps, computed_at, from_paper_book)
		VALUES ($1,$2,$3,1,$4,$4,$5,$6,0,0,0,$4,false)`, uuid.New(), in.ID, in.VersionID, at, make([]byte, 32), returnBPS)
	require.NoError(f.t, err)
	_, err = tx.Exec(f.ctx, `ALTER TABLE marketplace_performance_snapshots ENABLE TRIGGER marketplace_performance_snapshots_from_book`)
	require.NoError(f.t, err)
	require.NoError(f.t, tx.Commit(f.ctx))
}

// TestIntegration_ReadsShowOnlyPerformanceAPaperBookProved is review-2 F10 at
// the reads: a snapshot written from a caller's figures before paper books --
// here a newer one claiming +9 900% -- is never shown, neither among a
// version's performance nor as an install's latest; an account's installs and
// follow runs read back with the book behind them.
func TestIntegration_ReadsShowOnlyPerformanceAPaperBookProved(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	in := f.install(v)
	run, err := f.follow(f.svc, in)
	require.NoError(t, err)
	require.Equal(t, 1, run.IntentsDispatched)
	var proved PerformanceSnapshot
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		proved, err = f.svc.RecordPerformance(ctx, tx, in.ID, f.followerActor)
		return err
	}))
	f.legacyPerformance(in, 990_000, f.clk.Now().Add(time.Hour))
	require.Equal(t, 2, f.count(`SELECT count(*) FROM marketplace_performance_snapshots WHERE install_id = $1`, in.ID))

	perf, err := PaperPerformance(f.ctx, testDB, []VersionID{v.ID}, 100)
	require.NoError(t, err)
	require.Len(t, perf, 1, "only what the book proved")
	assert.Equal(t, proved.ReturnBPS, perf[0].ReturnBPS)
	assert.Equal(t, proved.HistoryDigest, perf[0].HistoryDigest)

	latest, err := LatestPaperPerformance(f.ctx, testDB, []InstallID{in.ID})
	require.NoError(t, err)
	require.Contains(t, latest, in.ID)
	assert.Equal(t, proved.ReturnBPS, latest[in.ID].ReturnBPS, "the newer caller-figure snapshot is not the latest")

	installs, more, err := AccountInstalls(f.ctx, testDB, f.follower, nil, 10)
	require.NoError(t, err)
	assert.False(t, more)
	require.Len(t, installs, 1)
	assert.Equal(t, in.ID, installs[0].Install.ID)
	assert.Equal(t, "10000.00", installs[0].Install.PaperCapital)
	assert.Equal(t, v.ID, installs[0].Version.ID)
	require.NotNil(t, installs[0].LatestPerformance)
	assert.Equal(t, proved.ReturnBPS, installs[0].LatestPerformance.ReturnBPS)
	one, err := InstallSummaryOf(f.ctx, testDB, in.ID)
	require.NoError(t, err)
	assert.Equal(t, installs[0], one)

	runs, more, err := AccountFollowRuns(f.ctx, testDB, f.follower, &in.ID, nil, 10)
	require.NoError(t, err)
	assert.False(t, more)
	require.Len(t, runs, 1)
	assert.Equal(t, run.ID, runs[0].ID)
	assert.Equal(t, run.PaperCash, runs[0].PaperCash)
	assert.Len(t, runs[0].Intents, len(run.Intents))
	assert.Len(t, runs[0].Marks, len(run.Marks))
	other := id.New[installKind]()
	none, _, err := AccountFollowRuns(f.ctx, testDB, f.follower, &other, nil, 10)
	require.NoError(t, err)
	assert.Empty(t, none, "another install's history")
	stranger, _ := f.account()
	none, _, err = AccountFollowRuns(f.ctx, testDB, stranger, nil, nil, 10)
	require.NoError(t, err)
	assert.Empty(t, none, "another account's history")
}
