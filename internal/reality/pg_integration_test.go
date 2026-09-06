//go:build integration

package reality_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/archive"
	"github.com/nodal/controlplane/internal/archive/archivetest"
	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/chain/chaintest"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/event/eventtest"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/reality"
	"github.com/nodal/controlplane/internal/security"
)

// The suite needs an isolated, migrated database:
//
//	eval "$(go run ./scripts/testdb -name reality -export)"
//
// It refuses the shared controlplane_test database and uses a unique data
// source code per test so runs never collide.
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	appURL := os.Getenv("CP_TEST_DATABASE_URL")
	if appURL == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	parsed, err := url.Parse(appURL)
	require.NoError(t, err)
	if strings.TrimPrefix(parsed.Path, "/") == "controlplane_test" {
		t.Fatal("refusing the shared controlplane_test database; use scripts/testdb -name reality")
	}
	pool, err := db.Open(context.Background(), db.Config{URL: appURL, MaxConns: 8, AppName: "reality-test"})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

type testKind struct{}

func uniqueCode(prefix string) string {
	return prefix + "." + uniqueToken()
}

// uniqueToken is a fresh, path-safe token for one test run. Raw archive
// dedup is global over (provider, event_type, dedup_key) by design — a
// redelivered provider payload must never be archived twice, whichever data
// source registration consumed it — so a fixture that hardcodes a signature
// collides with the row an earlier run against the same database left
// behind. Every fixture signature carries this token so the suite is
// re-runnable without dropping the database.
func uniqueToken() string {
	return strings.ToLower(strings.ReplaceAll(id.New[testKind]().String(), "-", ""))[:16]
}

func registerSource(t *testing.T, pool *db.DB, mutate func(*reality.DataSource)) reality.DataSource {
	t.Helper()
	d := reality.DefaultChainDataSource(uniqueCode("test.chain"), "helius", 90, 2*time.Minute, "reality-test")
	if mutate != nil {
		mutate(&d)
	}
	out, err := reality.PgDataSourceStore{}.EnsureRegistered(context.Background(), pool, d)
	require.NoError(t, err)
	return out
}

func testArchiveConfig() config.ArchiveConfig {
	return config.ArchiveConfig{RawBucket: "raw-events", EvidenceBucket: "provider-evidence", AuditBucket: "audit-evidence", Region: "us-east-1"}
}

func TestIntegration_DataSourceRegistry_PersistenceBlockedWithoutRights(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	store := reality.PgDataSourceStore{}

	bad := reality.DefaultChainDataSource(uniqueCode("test.bad"), "helius", 30, time.Minute, "reality-test")
	bad.PersistenceCapability = reality.PersistenceAllowed // rights still UNKNOWN
	err := pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.Register(ctx, tx, bad)
		return err
	})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "Go validation refuses ALLOWED without YES")

	// Bypass the Go layer: the database CHECK is the last line of defense.
	err = pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO data_sources (id, code, provider, kind, retention_class, retention_days, redistribution_policy, historical_use_permitted,
			persistence_capability, dedup_strategy, heartbeat_timeout_ms, status, created_by_actor_type, created_by_actor_id)
			VALUES ($1,$2,'helius','ONCHAIN','RAW_MARKET_DATA',30,'NONE','UNKNOWN','ALLOWED','PROVIDER_ID',60000,'ACTIVE','SYSTEM','reality-test')`,
			reality.NewDataSourceID(), uniqueCode("test.bypass"))
		return err
	})
	require.Error(t, err)
	require.True(t, db.IsCheckViolation(err), "database CHECK must refuse: %v", err)

	good := registerSource(t, pool, nil)
	require.Equal(t, reality.PersistenceBlocked, good.PersistenceCapability)
	require.NotEmpty(t, good.ID)
	again, err := store.EnsureRegistered(ctx, pool, good)
	require.NoError(t, err)
	require.Equal(t, good.ID, again.ID, "EnsureRegistered is idempotent and never rewrites licensing")
	loaded, err := store.Get(ctx, pool, good.Code)
	require.NoError(t, err)
	require.Equal(t, 2*time.Minute, loaded.HeartbeatTimeout)
	list, err := store.List(ctx, pool)
	require.NoError(t, err)
	require.NotEmpty(t, list)
	_, err = store.Get(ctx, pool, "does.not.exist")
	require.Equal(t, errs.CodeNotFound, errs.CodeOf(err))

	agent := reality.DefaultChainDataSource(uniqueCode("test.agent"), "helius", 30, time.Minute, "agent-1")
	agent.CreatedByActorType = string(security.ActorAgent)
	err = pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.Register(ctx, tx, agent)
		return err
	})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "agents never register data sources")
}

func newRawArchive(t *testing.T, pool *db.DB, clk clock.Clock) (*reality.RawArchive, *archivetest.Memory) {
	t.Helper()
	mem := archivetest.New(clk).EnableObjectLock("audit-evidence")
	ra, err := reality.NewRawArchive(mem, testArchiveConfig(), pool, clk, nil)
	require.NoError(t, err)
	return ra, mem
}

func TestIntegration_RawArchive_IndexesDedupsAndVerifies(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	clk := clock.NewFake(fixedNow)
	ds := registerSource(t, pool, nil)
	ra, mem := newRawArchive(t, pool, clk)

	tok := uniqueToken()
	sig1, sig9, auditKey := "sig1-"+tok, "sig9-"+tok, "audit-"+tok
	seq := uint64(42)
	raw := reality.RawObject{
		DataSource: ds.Code, Provider: "helius", EventType: "wallet_event", SourceEventID: sig1, DedupKey: sig1 + "/w1", SchemaVersion: 1,
		ContentType: "application/json", Body: []byte(`{"signature":"` + sig1 + `"}`),
		Timestamps: reality.Timestamps{SourceEventAt: fixedNow.Add(-time.Minute), ProviderPublishedAt: fixedNow.Add(-time.Second), PlatformReceivedAt: fixedNow.Add(-time.Second / 2)},
		Stream:     "wallet_events", Partition: "w1", Offset: "sig1", Sequence: &seq, CorrelationID: "corr-1",
	}
	var ref reality.ArchiveRef
	var meta reality.RawObjectMeta
	require.NoError(t, pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ref, meta, err = ra.PutMeta(ctx, tx, raw)
		return err
	}))
	require.False(t, ref.Duplicate)
	require.Equal(t, archive.SHA256(raw.Body), ref.Hash)
	require.True(t, strings.HasPrefix(ref.URI, "s3://raw-events/raw/helius/wallet_event/v1/2026/09/05/12/"), ref.URI)
	require.Equal(t, "helius/wallet_event/v1/2026/09/05/12", meta.PartitionKey)
	require.Equal(t, ds.ID, meta.DataSourceID)
	require.Equal(t, sig1, meta.SourceEventID)
	require.Equal(t, uint64(42), *meta.Sequence)
	require.Equal(t, raw.Timestamps.SourceEventAt, meta.Timestamps.SourceEventAt)
	require.Equal(t, raw.Timestamps.PlatformReceivedAt, meta.Timestamps.PlatformReceivedAt)
	require.Equal(t, fixedNow, meta.IngestedAt)
	require.Equal(t, string(reality.RetentionRawMarketData), meta.RetentionClass)
	require.Equal(t, raw.Timestamps.PlatformReceivedAt.AddDate(0, 0, 90), meta.RetentionUntil, "retention comes from the data source's configured days")

	// Object metadata alone reconstructs provenance.
	loc, err := archive.ParseURI(ref.URI)
	require.NoError(t, err)
	head, err := mem.Head(ctx, loc)
	require.NoError(t, err)
	require.Equal(t, "helius", head.Metadata[archive.MetaProvider])
	require.Equal(t, sig1, head.Metadata[archive.MetaSourceEventID])
	require.Equal(t, "1", head.Metadata[archive.MetaSchemaVersion])
	require.Equal(t, ds.Code, head.Metadata[archive.MetaDataSource])
	require.Equal(t, raw.Timestamps.PlatformReceivedAt.Format(time.RFC3339Nano), head.Metadata[archive.MetaPlatformReceivedAt])
	require.Equal(t, fixedNow.Format(time.RFC3339Nano), head.Metadata[archive.MetaIngestedAt])
	require.Equal(t, ref.URI[strings.LastIndex(ref.URI, "?"):], "?versionId=v1")

	// A duplicate dedup key is not an error: the earlier object wins.
	dup := raw
	dup.Body = []byte(`{"signature":"` + sig1 + `","redelivered":true}`)
	var ref2 reality.ArchiveRef
	require.NoError(t, pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ref2, err = ra.Put(ctx, tx, dup)
		return err
	}))
	require.True(t, ref2.Duplicate)
	require.Equal(t, ref.ObjectID, ref2.ObjectID)
	require.Equal(t, 1, mem.Len(), "the duplicate was not written a second time")

	body, gotMeta, err := ra.Get(ctx, reality.ArchiveRef{ObjectID: ref.ObjectID})
	require.NoError(t, err)
	require.Equal(t, raw.Body, body)
	require.Equal(t, meta.ObjectID, gotMeta.ObjectID)
	require.NoError(t, ra.Verify(ctx, reality.ArchiveRef{URI: ref.URI}))

	// Corrupting one object makes Verify fail with the integrity code.
	require.True(t, mem.Corrupt(loc))
	err = ra.Verify(ctx, ref)
	require.Error(t, err)
	require.Equal(t, errs.CodeArchiveIntegrityViolation, errs.CodeOf(err))

	// A missing object is an integrity violation too, never "not found".
	audit := registerSource(t, pool, func(d *reality.DataSource) {
		d.RetentionClass = string(reality.RetentionSecurityAudit)
		d.RetentionDays = 2555
	})
	auditRaw := raw
	auditRaw.DataSource, auditRaw.DedupKey, auditRaw.SourceEventID = audit.Code, auditKey, auditKey
	var auditRef reality.ArchiveRef
	require.NoError(t, pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		auditRef, err = ra.Put(ctx, tx, auditRaw)
		return err
	}))
	require.True(t, strings.HasPrefix(auditRef.URI, "s3://audit-evidence/"), "audit classes go to the locked bucket: %s", auditRef.URI)
	auditLoc, _ := archive.ParseURI(auditRef.URI)
	auditHead, err := mem.Head(ctx, auditLoc)
	require.NoError(t, err)
	require.NotNil(t, auditHead.Retention, "audit objects carry Object Lock retention")
	require.Equal(t, raw.Timestamps.PlatformReceivedAt.AddDate(0, 0, 2555), auditHead.Retention.Until)
	require.ErrorIs(t, mem.Delete(ctx, auditLoc), archive.ErrRetentionLocked)

	// Immutability of the index and refusal of invalid input.
	_, err = pool.Exec(ctx, `UPDATE raw_archive_objects SET object_hash = $2 WHERE id = $1`, ref.ObjectID, make([]byte, 32))
	require.Error(t, err, "raw_archive_objects rows are immutable")
	err = pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ra.Put(ctx, tx, reality.RawObject{DataSource: ds.Code, Provider: "helius", EventType: "x", DedupKey: "k", SchemaVersion: 1})
		return err
	})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "empty body is not evidence")

	// Archive failure is surfaced, and no row is written.
	mem.FailPut(fmt.Errorf("minio down"))
	err = pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		r := raw
		r.DedupKey = sig9 + "/w1"
		_, err := ra.Put(ctx, tx, r)
		return err
	})
	require.Error(t, err)
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM raw_archive_objects WHERE dedup_key = $1`, sig9+"/w1").Scan(&n))
	require.Equal(t, 0, n, "no index row without a stored object")

	// ChainArchive adapter stores adapter responses under the data source.
	// chain event types are JSON-RPC method names, so they are folded into
	// the archive's lower-case segment alphabet at that boundary rather than
	// leaking camel case into object keys.
	mem.FailPut(nil)
	ca, err := reality.NewChainArchive(ra, pool, ds.Code)
	require.NoError(t, err)
	uri, err := ca.Store(ctx, chain.RawObject{Provider: "helius", EventType: "getTransaction", SourceEventID: sig1, Body: []byte(`{"jsonrpc":"2.0","id":"` + tok + `"}`), SchemaVersion: 1, PlatformReceivedAt: fixedNow})
	require.NoError(t, err)
	require.Contains(t, uri, "/helius/get_transaction/v1/", uri)
	require.NoError(t, ra.Verify(ctx, reality.ArchiveRef{URI: uri}))
	_, gotMeta, err = ra.Get(ctx, reality.ArchiveRef{URI: uri})
	require.NoError(t, err)
	require.Equal(t, "get_transaction", gotMeta.EventType, "the index records the folded event type")

	// A name that cannot be folded is refused, never truncated into a key.
	_, err = ca.Store(ctx, chain.RawObject{Provider: "helius", EventType: "get transaction", Body: []byte(`{}`), SchemaVersion: 1, PlatformReceivedAt: fixedNow})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// POINT_IN_TIME.md §2: Verify re-hashes archived objects on a schedule. The
// sweep is what the ingest worker schedules, so it must find every corrupted
// object in its window, not just the first, and must never report a window
// it could not actually read as clean.
func TestIntegration_RawArchive_VerifySweepFindsEveryCorruptedObject(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	clk := clock.NewFake(fixedNow)
	ds := registerSource(t, pool, nil)
	ra, mem := newRawArchive(t, pool, clk)
	tok := uniqueToken()

	put := func(sig string, received time.Time) reality.ArchiveRef {
		t.Helper()
		var ref reality.ArchiveRef
		require.NoError(t, pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			ref, err = ra.Put(ctx, tx, reality.RawObject{
				DataSource: ds.Code, Provider: "helius", EventType: "wallet_event", SourceEventID: sig, DedupKey: sig + "/w1",
				SchemaVersion: 1, ContentType: "application/json", Body: []byte(`{"signature":"` + sig + `"}`),
				Timestamps: reality.Timestamps{SourceEventAt: received, PlatformReceivedAt: received},
				Stream:     "wallet_events", Partition: "w1", Offset: sig,
			})
			return err
		}))
		return ref
	}
	inside := put("sweep-a-"+tok, fixedNow)
	put("sweep-b-"+tok, fixedNow.Add(time.Minute))
	outside := put("sweep-c-"+tok, fixedNow.Add(48*time.Hour))

	window := reality.Window{Start: fixedNow.Add(-time.Hour), End: fixedNow.Add(time.Hour)}
	report, err := ra.VerifySweep(ctx, ds.Code, window, 0)
	require.NoError(t, err)
	require.True(t, report.OK())
	require.Equal(t, 2, report.Checked, "only objects received inside the window are swept")
	require.Equal(t, ds.Code, report.DataSource)

	// An open-ended window reaches the later object too.
	report, err = ra.VerifySweep(ctx, ds.Code, reality.Window{Start: fixedNow.Add(-time.Hour)}, 0)
	require.NoError(t, err)
	require.Equal(t, 3, report.Checked)

	// Corrupting objects makes the sweep report each of them, and the sweep
	// keeps going rather than stopping at the first.
	for _, ref := range []reality.ArchiveRef{inside, outside} {
		loc, err := archive.ParseURI(ref.URI)
		require.NoError(t, err)
		require.True(t, mem.Corrupt(loc))
	}
	report, err = ra.VerifySweep(ctx, ds.Code, reality.Window{Start: fixedNow.Add(-time.Hour)}, 0)
	require.NoError(t, err)
	require.False(t, report.OK())
	require.Len(t, report.Violations, 2)
	corrupted := map[string]bool{}
	for _, v := range report.Violations {
		corrupted[v.ObjectID] = true
		require.NotEmpty(t, v.URI)
		require.Contains(t, v.Reason, string(errs.CodeArchiveIntegrityViolation), "the violation names the code an operator alerts on")
	}
	require.True(t, corrupted[inside.ObjectID] && corrupted[outside.ObjectID])

	// A window that names no data source, or ends before it starts, is a
	// validation error rather than an empty clean report.
	_, err = ra.VerifySweep(ctx, "", window, 0)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = ra.VerifySweep(ctx, ds.Code, reality.Window{}, 0)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = ra.VerifySweep(ctx, ds.Code, reality.Window{Start: fixedNow, End: fixedNow.Add(-time.Hour)}, 0)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// An indexed object that has disappeared from the store is a violation
	// too, never a quietly shorter report: an index row without bytes is
	// evidence that no longer exists.
	loc, err := archive.ParseURI(inside.URI)
	require.NoError(t, err)
	require.NoError(t, mem.Delete(ctx, loc))
	report, err = ra.VerifySweep(ctx, ds.Code, window, 0)
	require.NoError(t, err)
	require.Equal(t, 2, report.Checked)
	require.False(t, report.OK())
	var missing bool
	for _, v := range report.Violations {
		if v.ObjectID == inside.ObjectID {
			missing = true
			require.Contains(t, v.Reason, "missing from the store")
		}
	}
	require.True(t, missing)

	// limit bounds one sweep so a huge window cannot become an unbounded scan.
	report, err = ra.VerifySweep(ctx, ds.Code, reality.Window{Start: fixedNow.Add(-time.Hour)}, 1)
	require.NoError(t, err)
	require.Equal(t, 1, report.Checked)
}

func TestIntegration_Checkpoints_GapsResolutionAndContinuity(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	ds := registerSource(t, pool, nil)
	cps := reality.PgCheckpointStore{}
	operator := security.Principal{SubjectID: "op-1", ActorType: security.ActorOperator}
	agent := security.Principal{SubjectID: "agent-1", ActorType: security.ActorAgent}

	cp, err := cps.Load(ctx, pool, ds.Code, "wallet_events", "w1", "worker")
	require.NoError(t, err)
	require.False(t, cp.Found)
	require.Equal(t, ds.ID, cp.DataSourceID)

	seq := uint64(10)
	cp.LastSequence, cp.LastSourceOffset, cp.LastPlatformReceivedAt, cp.Status, cp.EventsSinceStart = &seq, "sig10", fixedNow, reality.CheckpointActive, 1
	require.NoError(t, pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cp, err = cps.Advance(ctx, tx, cp)
		return err
	}))
	require.True(t, cp.Found)
	require.Equal(t, int64(1), cp.Version)

	stale := cp
	stale.Version = 0
	err = pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := cps.Advance(ctx, tx, stale)
		return err
	})
	require.Equal(t, errs.CodeConflict, errs.CodeOf(err), "optimistic concurrency refuses a stale version")

	loaded, err := cps.Load(ctx, pool, ds.Code, "wallet_events", "w1", "worker")
	require.NoError(t, err)
	require.Equal(t, uint64(10), *loaded.LastSequence)
	require.Equal(t, "sig10", loaded.LastSourceOffset)
	require.Equal(t, fixedNow, loaded.LastPlatformReceivedAt)

	// An OPEN gap blocks continuity over its window.
	start, end := uint64(11), uint64(14)
	var gid reality.GapID
	require.NoError(t, pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		gid, err = cps.RecordGap(ctx, tx, reality.Gap{
			DataSourceID: ds.ID, CheckpointID: cp.ID, Stream: "wallet_events", Partition: "w1", Kind: reality.GapKindGap,
			ExpectedSequence: &start, ObservedSequence: &end, GapStartSequence: &start, GapEndSequence: &end,
			GapStartAt: fixedNow, GapEndAt: fixedNow.Add(10 * time.Second), Detail: map[string]any{"missing": 4},
		})
		return err
	}))
	cont, err := cps.Continuity(ctx, pool, ds.Code, reality.Window{Start: fixedNow.Add(5 * time.Second), End: fixedNow.Add(time.Minute)})
	require.NoError(t, err)
	require.False(t, cont.OK, "a strategy over the gap cannot pretend continuity")
	require.Equal(t, []string{"DATA_GAP:" + gid.String()}, cont.ImpurityReasons())
	cont, err = cps.Continuity(ctx, pool, ds.Code, reality.Window{Start: fixedNow.Add(11 * time.Second), End: fixedNow.Add(time.Minute)})
	require.NoError(t, err)
	require.True(t, cont.OK, "windows after the gap are continuous")

	err = pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return cps.Resolve(ctx, tx, gid, reality.ResolutionReplayed, agent)
	})
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "agents never resolve gaps")
	require.NoError(t, pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return cps.Resolve(ctx, tx, gid, reality.ResolutionReplayed, operator)
	}))
	g, err := cps.Get(ctx, pool, gid)
	require.NoError(t, err)
	require.Equal(t, reality.ResolutionReplayed, g.Resolution)
	require.Equal(t, "op-1", g.ResolvedByActorID)
	require.False(t, g.Blocking())
	cont, err = cps.Continuity(ctx, pool, ds.Code, reality.Window{Start: fixedNow, End: fixedNow.Add(time.Minute)})
	require.NoError(t, err)
	require.True(t, cont.OK, "a replayed gap no longer blocks")
	err = pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return cps.Resolve(ctx, tx, gid, reality.ResolutionAcknowledged, operator)
	})
	require.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err), "a resolved gap stays resolved")

	// UNRECOVERABLE keeps blocking; an open-ended SILENCE blocks everything after its start until closed.
	var unrec, silence reality.GapID
	require.NoError(t, pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if unrec, err = cps.RecordGap(ctx, tx, reality.Gap{DataSourceID: ds.ID, Stream: "wallet_events", Partition: "w1", Kind: reality.GapKindGap, GapStartAt: fixedNow.Add(2 * time.Minute), GapEndAt: fixedNow.Add(3 * time.Minute)}); err != nil {
			return err
		}
		if err := cps.Resolve(ctx, tx, unrec, reality.ResolutionUnrecoverable, operator); err != nil {
			return err
		}
		silence, err = cps.RecordGap(ctx, tx, reality.Gap{DataSourceID: ds.ID, Stream: "wallet_events", Partition: "w1", Kind: reality.GapKindSilence, GapStartAt: fixedNow.Add(10 * time.Minute)})
		return err
	}))
	gaps, err := cps.OpenGaps(ctx, pool, ds.Code, reality.Window{Start: fixedNow.Add(2*time.Minute + 30*time.Second), End: fixedNow.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, gaps, 2)
	require.Equal(t, unrec, gaps[0].ID)
	require.Equal(t, silence, gaps[1].ID)
	require.NoError(t, pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return cps.CloseGap(ctx, tx, silence, fixedNow.Add(12*time.Minute))
	}))
	gaps, err = cps.OpenGaps(ctx, pool, ds.Code, reality.Window{Start: fixedNow.Add(13 * time.Minute), End: fixedNow.Add(time.Hour)})
	require.NoError(t, err)
	require.Empty(t, gaps, "a closed silence no longer blocks later windows")

	// Rewind records a REPLAY marker and puts the checkpoint into REPLAYING.
	var replayed reality.Checkpoint
	var replayGap reality.GapID
	to := uint64(5)
	require.NoError(t, pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		replayed, replayGap, err = cps.Rewind(ctx, tx, loaded, &to, "sig5", operator, "operator backfill")
		return err
	}))
	require.Equal(t, reality.CheckpointReplaying, replayed.Status)
	require.Equal(t, uint64(5), *replayed.LastSequence)
	rg, err := cps.Get(ctx, pool, replayGap)
	require.NoError(t, err)
	require.Equal(t, reality.GapKindReplay, rg.Kind)
	require.Equal(t, reality.ResolutionAcknowledged, rg.Resolution)
	require.Equal(t, "operator backfill", rg.Detail["reason"])

	// RecordGap enforces the table's constraints (end before start).
	err = pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := cps.RecordGap(ctx, tx, reality.Gap{DataSourceID: ds.ID, Stream: "s", Kind: reality.GapKindGap, GapStartAt: fixedNow, GapEndAt: fixedNow.Add(-time.Second)})
		return err
	})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestIntegration_HealthSamples_WriteAndRead(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	ds := registerSource(t, pool, nil)
	th := provider.DefaultThresholds()
	reported := provider.Healthy
	sampler, err := reality.NewHealthSampler(th, func() provider.Health { return reported }, ds.ID, fixedNow)
	require.NoError(t, err)
	for i := 0; i < 20; i++ {
		sampler.Observe(fixedNow.Add(time.Duration(i)*time.Second), i%4 != 0, time.Duration(50+i*10)*time.Millisecond)
	}
	now := fixedNow.Add(20 * time.Second)
	s, err := sampler.Sample(ctx, "helius", reality.RoleData, now)
	require.NoError(t, err)
	require.Equal(t, int64(2500), s.ErrorRateBPS, "5 failures of 20 = 25%")
	require.Equal(t, provider.Degraded, s.State)
	require.Equal(t, []string{reality.ReasonErrorRateHigh}, s.ReasonCodes)
	require.Equal(t, 20, s.SampleCount)
	require.Greater(t, s.P99LatencyMS, s.P50LatencyMS)
	require.Equal(t, int64(1000), s.StalenessMS, "last success was one second ago")
	require.Equal(t, reality.HealthEvaluatorVersion, s.EvaluatorVersion)

	store := reality.PgHealthStore{MaxSampleAge: time.Minute}
	providerName := "helius-" + uniqueCode("h")
	s.Provider = providerName
	require.NoError(t, store.Insert(ctx, pool, s))
	require.Equal(t, errs.CodeInternal, errs.CodeOf(store.Insert(ctx, pool, s)), "a sample id is inserted once; rows are append-only evidence")
	cur, err := store.Current(ctx, pool, providerName, reality.RoleData, now.Add(10*time.Second))
	require.NoError(t, err)
	require.Equal(t, provider.Degraded, cur.State)
	require.True(t, cur.Usable, "degraded providers may still act")
	require.Equal(t, 10*time.Second, cur.SampleAge)

	old, err := store.Current(ctx, pool, providerName, reality.RoleData, now.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, provider.Unhealthy, old.State, "silence is not health")
	require.Contains(t, old.ReasonCodes, reality.ReasonNoRecentSample)
	require.False(t, old.Usable)

	none, err := store.Current(ctx, pool, "nobody-"+uniqueCode("x"), reality.RoleData, now)
	require.NoError(t, err)
	require.Equal(t, provider.Unhealthy, none.State)
	require.Equal(t, []string{reality.ReasonNoSamples}, none.ReasonCodes)

	reported = provider.Disabled
	s2, err := sampler.Sample(ctx, providerName, reality.RoleObservation, now)
	require.NoError(t, err)
	require.Equal(t, provider.Disabled, s2.State)
	require.Contains(t, s2.ReasonCodes, reality.ReasonOperatorDisabled)
	require.NoError(t, store.Insert(ctx, pool, s2))
	_, err = pool.Exec(ctx, `DELETE FROM provider_health_samples WHERE provider = $1`, providerName)
	require.Error(t, err, "samples are immutable evidence")

	bad := s
	bad.Role = "PSYCHIC"
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(store.Insert(ctx, pool, bad)))
	_, err = sampler.Sample(ctx, "", reality.RoleData, now)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// memSink records normalized events (the ClickHouse sink is exercised in
// clickhouse_integration_test.go).
type memSink struct {
	mu     sync.Mutex
	events []reality.NormalizedEvent
	fail   error
}

func (m *memSink) InsertNormalized(_ context.Context, events []reality.NormalizedEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.events = append(m.events, events...)
	return nil
}

func (m *memSink) all() []reality.NormalizedEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]reality.NormalizedEvent(nil), m.events...)
}

func TestIntegration_Pipeline_ArchiveNormalizePublishCheckpoint(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	clk := clock.NewFake(fixedNow)
	ds := registerSource(t, pool, func(d *reality.DataSource) { d.HeartbeatTimeout = 30 * time.Second })
	ra, mem := newRawArchive(t, pool, clk)
	bus, err := eventtest.NewMemoryBus("TEST")
	require.NoError(t, err)
	sink := &memSink{}
	norm, err := reality.NewChainNormalizer(ds.Code, reality.DedupProviderID, testPolicy)
	require.NoError(t, err)
	topic := "market.test." + uniqueCode("t")
	consumer := "worker-" + uniqueCode("c")
	var delivered []string
	var dmu sync.Mutex
	require.NoError(t, bus.Subscribe(ctx, topic, "consumer", func(_ context.Context, m event.Message) error {
		dmu.Lock()
		defer dmu.Unlock()
		delivered = append(delivered, m.Key)
		return nil
	}))
	p, err := reality.NewPipeline(reality.PipelineConfig{
		DataSource: ds.Code, Stream: reality.StreamWalletEvents, Consumer: consumer, Topic: topic,
		Detector: reality.DetectorOptions{ContiguousSequence: true},
	}, reality.PipelineDeps{DB: pool, Archive: ra, Normalizer: norm, Bus: bus, Sink: sink, Clock: clk})
	require.NoError(t, err)
	_, err = p.Ingest(ctx, reality.RawObject{})
	require.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err), "not started")
	require.NoError(t, p.Start(ctx))

	// Reconnect first (as Helius does), then events.
	require.NoError(t, p.Reconnect(ctx, "WalletAAA", fixedNow, "connected"))
	tok := uniqueToken()
	sig := func(base string) string { return base + "-" + tok }
	mk := func(base string, seq uint64, received time.Time) reality.RawObject {
		ev := sampleWalletEvent(received)
		ev.Observation.Signature = sig(base)
		ev.Sequence = &seq
		raw, err := reality.WalletEventRawObject(ev, ds.Code)
		require.NoError(t, err)
		return raw
	}
	clk.Set(fixedNow.Add(time.Second))
	res, err := p.Ingest(ctx, mk("sig10", 10, fixedNow.Add(time.Second)))
	require.NoError(t, err)
	require.False(t, res.Duplicate)
	require.Len(t, res.Events, 1)
	require.Empty(t, res.Findings)
	require.Equal(t, sig("sig10")+"/WalletAAA", res.Events[0].DedupID)
	require.Equal(t, res.ObjectID, res.Events[0].RawObjectID)

	// Redelivery: archived once, still published and sunk (at-least-once), deduped by consumers.
	res2, err := p.Ingest(ctx, mk("sig10", 10, fixedNow.Add(2*time.Second)))
	require.NoError(t, err)
	require.True(t, res2.Duplicate)
	require.Equal(t, res.ObjectID, res2.ObjectID)
	require.Equal(t, 1, mem.Len())
	require.Len(t, reality.Dedup(sink.all()), 1)
	require.Len(t, sink.all(), 2)

	// Sequence jump → GAP (open, checkpoint status GAP); going backwards → ORDERING_ANOMALY flagged.
	res3, err := p.Ingest(ctx, mk("sig14", 14, fixedNow.Add(3*time.Second)))
	require.NoError(t, err)
	require.Len(t, res3.Findings, 1)
	require.Equal(t, reality.GapKindGap, res3.Findings[0].Kind)
	cp, err := reality.PgCheckpointStore{}.Load(ctx, pool, ds.Code, reality.StreamWalletEvents, "WalletAAA", consumer)
	require.NoError(t, err)
	require.Equal(t, reality.CheckpointGap, cp.Status, "an open GAP is visible on the checkpoint")
	res4, err := p.Ingest(ctx, mk("sig12", 12, fixedNow.Add(4*time.Second)))
	require.NoError(t, err)
	require.Len(t, res4.Findings, 1)
	require.Equal(t, reality.GapKindOrderingAnomaly, res4.Findings[0].Kind)
	require.Equal(t, []string{reality.GapKindOrderingAnomaly}, res4.Events[0].Flags, "anomalous events are kept and flagged")

	cont, err := reality.PgCheckpointStore{}.Continuity(ctx, pool, ds.Code, reality.Window{Start: fixedNow, End: fixedNow.Add(time.Minute)})
	require.NoError(t, err)
	require.False(t, cont.OK, "the open GAP blocks the window")

	// Silence: no event for longer than the heartbeat → SILENCE opened; next event closes it.
	require.NoError(t, p.CheckSilence(ctx, fixedNow.Add(5*time.Minute)))
	sid, open := p.OpenSilence("WalletAAA")
	require.True(t, open)
	require.NoError(t, p.CheckSilence(ctx, fixedNow.Add(6*time.Minute)), "no second silence row while one is open")
	_, err = p.Ingest(ctx, mk("sig15", 15, fixedNow.Add(7*time.Minute)))
	require.NoError(t, err)
	_, open = p.OpenSilence("WalletAAA")
	require.False(t, open)
	sg, err := reality.PgCheckpointStore{}.Get(ctx, pool, sid)
	require.NoError(t, err)
	require.Equal(t, reality.ResolutionAcknowledged, sg.Resolution)
	require.Equal(t, fixedNow.Add(7*time.Minute), sg.GapEndAt)

	// Bus delivery: keyed by dedup id, every publish delivered.
	dmu.Lock()
	require.Equal(t, []string{sig("sig10") + "/WalletAAA", sig("sig10") + "/WalletAAA", sig("sig14") + "/WalletAAA", sig("sig12") + "/WalletAAA", sig("sig15") + "/WalletAAA"}, delivered)
	dmu.Unlock()
	st := p.Stats()
	require.Equal(t, int64(5), st.Ingested)
	require.Equal(t, int64(1), st.Duplicates)

	// A sink outage fails the ingest before the checkpoint moves, so a restart replays.
	sink.fail = fmt.Errorf("clickhouse down")
	_, err = p.Ingest(ctx, mk("sig16", 16, fixedNow.Add(8*time.Minute)))
	require.Error(t, err)
	cp, err = reality.PgCheckpointStore{}.Load(ctx, pool, ds.Code, reality.StreamWalletEvents, "WalletAAA", consumer)
	require.NoError(t, err)
	require.Equal(t, sig("sig15"), cp.LastSourceOffset, "checkpoint did not advance past the failed event")
	sink.fail = nil
	res5, err := p.Ingest(ctx, mk("sig16", 16, fixedNow.Add(8*time.Minute)))
	require.NoError(t, err)
	require.True(t, res5.Duplicate, "the raw object was archived before the failure and is reused")
	cp, err = reality.PgCheckpointStore{}.Load(ctx, pool, ds.Code, reality.StreamWalletEvents, "WalletAAA", consumer)
	require.NoError(t, err)
	require.Equal(t, sig("sig16"), cp.LastSourceOffset)
}

func TestIntegration_Pipeline_RunOverSolanaFakeProvider(t *testing.T) {
	pool := openTestDB(t)
	clk := clock.System()
	ds := registerSource(t, pool, nil)
	ra, _ := newRawArchive(t, pool, clk)
	bus, err := eventtest.NewMemoryBus("TEST")
	require.NoError(t, err)
	sink := &memSink{}
	norm, err := reality.NewChainNormalizer(ds.Code, reality.DedupProviderID, testPolicy)
	require.NoError(t, err)

	sim := chaintest.NewChain(clk)
	fake := sim.NewObserver("helius-fake", 0)
	wallet := "WalletRun" + uniqueCode("w")[len("w."):]
	src, err := reality.NewSolanaWalletSource(fake, []string{wallet}, ds.Code, clk)
	require.NoError(t, err)
	p, err := reality.NewPipeline(reality.PipelineConfig{
		DataSource: ds.Code, Stream: reality.StreamWalletEvents, Consumer: "run-" + uniqueCode("c"), Topic: "market.run." + uniqueCode("t"),
		SilenceCheckInterval: 50 * time.Millisecond,
	}, reality.PipelineDeps{DB: pool, Archive: ra, Normalizer: norm, Bus: bus, Sink: sink, Source: src, Clock: clk})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	// Land a swap touching the wallet, let the chain confirm it, then hint
	// the stream: the pipeline observes it through RPC, archives,
	// normalizes and sinks. (Streaming hints are disabled so the hint cannot
	// race ahead of confirmation and be dropped as not-yet-visible.)
	fake.SetStreaming(false)
	sim.Advance(1)
	landed := sim.Land(sim.Swap(wallet, "USDC", "SOL", money.QuantityFromInt64(1_000_000), money.QuantityFromInt64(5_000_000), money.QuantityFromInt64(5000)))
	sim.Advance(2)
	fake.Emit(chain.WalletEvent{Kind: chain.EventTransaction, Observation: chain.TxObservation{Signature: landed.Signature}})
	require.Eventually(t, func() bool { return len(sink.all()) >= 1 }, 10*time.Second, 50*time.Millisecond)
	got := sink.all()[0]
	require.Equal(t, wallet, got.Wallet)
	require.NoError(t, got.Timestamps.Validate())
	require.NoError(t, ra.Verify(context.Background(), reality.ArchiveRef{ObjectID: got.RawObjectID}))

	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("pipeline did not stop on cancel")
	}
	require.True(t, p.Stats().Started)
	require.GreaterOrEqual(t, p.Stats().Ingested, int64(1))
}
