//go:build integration

package proof

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Shared with internal/db, internal/audit and test/integration/migrations:
// schema-mutating suites hold pg_advisory_lock(424242) exclusively; this
// suite holds it shared. Run against an isolated database
// (scripts/testdb -name proof).
const testAdvisoryLockID = 424242

// Checkpoints and their archived objects are append-only evidence: they
// outlive the test process exactly as they outlive a production deployment.
// So the suite must not mint throwaway keys or an in-memory archive — a
// second run against the same database would then meet checkpoints signed by
// a key it no longer has and objects that no longer exist, and would report
// tampering that never happened. Both live in per-database directories that
// survive the process, and every key found there is trusted (the same
// discipline production needs across a rotation, D-029).
//
// Overridable so the same key and archive can be handed to
// `audit-worker verify` after a run:
//
//	CP_AUDIT_LOCAL_SIGNING_KEY_REF=file://<dir>/active.pem
//	CP_AUDIT_LOCAL_RETIRED_KEY_REFS=file://<dir>/rotated-<id>.pem,...
//	CP_AUDIT_ARCHIVE_DIR=<archive dir>
const (
	envTestKeyDir    = "CP_TEST_PROOF_KEY_DIR"
	envTestArchiveDr = "CP_TEST_PROOF_ARCHIVE_DIR"
	activeKeyFile    = "active.pem"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
	testSigner     *LocalECDSASigner // the active key: signs every checkpoint this run creates
	testKeyDir     string
	testArchive    *DirArchive
	testBuild      = "proof-itest"

	testKeysMu sync.Mutex
	testKeys   *KeySet // every key in testKeyDir, active first; rebuilt by rotateSigner
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	lockConn, err := pgx.Connect(ctx, testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "proof integration: connect for advisory lock:", err)
		return 1
	}
	defer func() { _ = lockConn.Close(ctx) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock_shared($1)", testAdvisoryLockID); err != nil {
		fmt.Fprintln(os.Stderr, "proof integration: advisory lock:", err)
		return 1
	}
	defer func() { _, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock_shared($1)", testAdvisoryLockID) }()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "proof integration: migrate up:", err)
		return 1
	}
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "proof-itest", MaxConns: 32})
	if err != nil {
		fmt.Fprintln(os.Stderr, "proof integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	if err := setupSignerAndArchive(); err != nil {
		fmt.Fprintln(os.Stderr, "proof integration:", err)
		return 1
	}
	return m.Run()
}

// testDataDir returns a directory private to this test database, stable
// across processes: <temp>/cp-proof-<kind>/<database name>.
func testDataDir(kind string) (string, error) {
	u, err := url.Parse(testAppURL)
	if err != nil {
		return "", fmt.Errorf("parse CP_TEST_DATABASE_URL: %w", err)
	}
	name := strings.Trim(u.Path, "/")
	if name == "" {
		return "", errors.New("CP_TEST_DATABASE_URL names no database")
	}
	return filepath.Join(os.TempDir(), "cp-proof-"+kind, name), nil
}

// setupSignerAndArchive loads (or creates) this database's signing keys and
// opens its archive directory. Every key in the directory is trusted: the
// active one signs, the rotated ones still verify the checkpoints they
// signed in an earlier run.
func setupSignerAndArchive() error {
	dir := os.Getenv(envTestKeyDir)
	if dir == "" {
		var err error
		if dir, err = testDataDir("keys"); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	testKeyDir = dir

	activePath := filepath.Join(dir, activeKeyFile)
	if _, err := os.Stat(activePath); errors.Is(err, os.ErrNotExist) {
		if err := writeNewKeyFile(activePath); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	active, err := loadKeyFile(activePath)
	if err != nil {
		return err
	}
	testSigner = active
	if err := reloadTestKeys(); err != nil {
		return err
	}

	archiveDir := os.Getenv(envTestArchiveDr)
	if archiveDir == "" {
		if archiveDir, err = testDataDir("archive"); err != nil {
			return err
		}
	}
	testArchive, err = NewDirArchive(config.EnvTest, archiveDir)
	return err
}

// writeNewKeyFile generates a P-256 key and stores it, owner-readable only.
func writeNewKeyFile(path string) error {
	k, err := GenerateLocalKey()
	if err != nil {
		return err
	}
	pemText, err := MarshalLocalKeyPEM(k)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(pemText), 0o600)
}

func loadKeyFile(path string) (*LocalECDSASigner, error) {
	b, err := os.ReadFile(path) //nolint:gosec // test-only key directory
	if err != nil {
		return nil, err
	}
	key, err := ParseLocalKeyPEM(string(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return NewLocalECDSASigner(config.EnvTest, key)
}

// testKeyEntries reads every *.pem in the key directory: active.pem active,
// the rest retired. The directory accumulates keys across runs, exactly as an
// operator's trusted set accumulates across rotations.
func testKeyEntries() ([]TrustedKey, error) {
	entries, err := os.ReadDir(testKeyDir)
	if err != nil {
		return nil, err
	}
	var keys []TrustedKey
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".pem" {
			continue
		}
		s, err := loadKeyFile(filepath.Join(testKeyDir, e.Name()))
		if err != nil {
			return nil, err
		}
		status := KeyRetired
		if e.Name() == activeKeyFile {
			status = KeyActive
		}
		keys = append(keys, s.TrustedKey(status))
	}
	return keys, nil
}

// reloadTestKeys rebuilds the trusted set from the key directory.
func reloadTestKeys() error {
	keys, err := testKeyEntries()
	if err != nil {
		return err
	}
	ks, err := NewKeySet(keys...)
	if err != nil {
		return err
	}
	testKeysMu.Lock()
	testKeys = ks
	testKeysMu.Unlock()
	return nil
}

func trustedKeys() *KeySet {
	testKeysMu.Lock()
	defer testKeysMu.Unlock()
	return testKeys
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test (provision one with `go run ./scripts/testdb -name proof`)")
	}
}

func migrateConn(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testMigrateURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

var t0 = time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)

func freshStream() string { return audit.AccountStream(uuid.NewString()) }

func sampleEvent(stream string, i int) audit.Event {
	return audit.Event{
		Stream: stream, ActorType: "USER", ActorID: "user-1", Action: "proof.test", ResourceType: "test", ResourceID: fmt.Sprintf("res-%d", i),
		CorrelationID: "corr-" + stream, Payload: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)), OccurredAt: t0.Add(time.Duration(i) * time.Second),
	}
}

func appendEvent(t *testing.T, e audit.Event) audit.Appended {
	t.Helper()
	var out audit.Appended
	require.NoError(t, testDB.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = audit.NewWriterWithBuildVersion(testBuild).Append(ctx, tx, e)
		return err
	}))
	return out
}

func appendEvents(t *testing.T, stream string, n int) []audit.Appended {
	t.Helper()
	out := make([]audit.Appended, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, appendEvent(t, sampleEvent(stream, i)))
	}
	return out
}

func newCheckpointer(t *testing.T, opts CheckpointerOptions) *Checkpointer {
	t.Helper()
	if opts.BuildVersion == "" {
		opts.BuildVersion = testBuild
	}
	cp, err := NewCheckpointer(testDB, testSigner, testArchive, clock.System(), opts)
	require.NoError(t, err)
	return cp
}

func newVerifier(t *testing.T) *Verifier {
	t.Helper()
	return newVerifierWithKeys(t, trustedKeys())
}

func newVerifierWithKeys(t *testing.T, keys *KeySet) *Verifier {
	t.Helper()
	v, err := NewVerifier(keys, testArchive, clock.System(), VerifierOptions{BuildVersion: testBuild})
	require.NoError(t, err)
	return v
}

// rotateSigner retires the current active key and promotes a fresh one, the
// way an operator rotates: the outgoing key file stays in the directory (and
// therefore in the trusted set) so the checkpoints it signed keep verifying,
// here and in every later run against this database.
func rotateSigner(t *testing.T) *LocalECDSASigner {
	t.Helper()
	retiredPath := filepath.Join(testKeyDir, "rotated-"+strings.TrimPrefix(testSigner.KeyID(), "local-test:")+".pem")
	if _, err := os.Stat(retiredPath); errors.Is(err, os.ErrNotExist) {
		b, rerr := os.ReadFile(filepath.Join(testKeyDir, activeKeyFile)) //nolint:gosec // test-only key directory
		require.NoError(t, rerr)
		require.NoError(t, os.WriteFile(retiredPath, b, 0o600))
	} else {
		require.NoError(t, err)
	}
	require.NoError(t, writeNewKeyFile(filepath.Join(testKeyDir, activeKeyFile)))
	next, err := loadKeyFile(filepath.Join(testKeyDir, activeKeyFile))
	require.NoError(t, err)
	previous := testSigner
	testSigner = next
	require.NoError(t, reloadTestKeys())
	require.NotEqual(t, previous.KeyID(), testSigner.KeyID())
	return previous
}

func verifyAll(t *testing.T) Report {
	t.Helper()
	rep, err := newVerifier(t).VerifyAll(context.Background(), testDB)
	require.NoError(t, err)
	return rep
}

func requireVerifies(t *testing.T) Report {
	t.Helper()
	rep := verifyAll(t)
	if !rep.OK {
		require.Fail(t, "history must verify", "%s", rep.FirstFailure)
	}
	return rep
}

func requireFails(t *testing.T, kind FailureKind) Failure {
	t.Helper()
	rep := verifyAll(t)
	require.False(t, rep.OK, "expected a %s failure", kind)
	require.NotNil(t, rep.FirstFailure)
	assert.Equal(t, kind, rep.FirstFailure.Kind, "%s", rep.FirstFailure)
	var (
		ok      bool
		failure []byte
	)
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT ok, first_failure FROM audit_verification_runs WHERE id = $1`, rep.RunID).Scan(&ok, &failure))
	assert.False(t, ok, "the failed run is recorded")
	assert.Contains(t, string(failure), string(kind))
	return *rep.FirstFailure
}

func cpCount(t *testing.T) int {
	t.Helper()
	n, err := countCheckpoints(context.Background(), testDB)
	require.NoError(t, err)
	return n
}

func runFull(t *testing.T) Checkpoint {
	t.Helper()
	res, err := newCheckpointer(t, CheckpointerOptions{}).RunFull(context.Background())
	require.NoError(t, err)
	require.True(t, res.Created, "skipped: %s", res.Skipped)
	return res.Checkpoint
}

// withTriggerDisabled runs fn as the table owner with an immutability
// trigger off: something cp_app can never do (see the privilege test).
func withTriggerDisabled(t *testing.T, conn *pgx.Conn, table, trigger string, fn func()) {
	t.Helper()
	ctx := context.Background()
	_, err := conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DISABLE TRIGGER %s`, table, trigger))
	require.NoError(t, err)
	defer func() {
		_, err := conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ENABLE TRIGGER %s`, table, trigger))
		require.NoError(t, err)
	}()
	fn()
}

// tamper executes sql as the owner with trigger disabled and registers
// restore to run at cleanup the same way.
func tamper(t *testing.T, conn *pgx.Conn, table, trigger, sql string, args []any, restoreSQL string, restoreArgs []any) {
	t.Helper()
	ctx := context.Background()
	withTriggerDisabled(t, conn, table, trigger, func() {
		_, err := conn.Exec(ctx, sql, args...)
		require.NoError(t, err)
	})
	t.Cleanup(func() {
		withTriggerDisabled(t, conn, table, trigger, func() {
			_, err := conn.Exec(ctx, restoreSQL, restoreArgs...)
			require.NoError(t, err)
		})
	})
}

// archivePath maps a file:// archive URI back to a filesystem path.
func archivePath(t *testing.T, uri string) string {
	t.Helper()
	u, err := url.Parse(uri)
	require.NoError(t, err)
	p := u.Path
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' { // /C:/... on Windows
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

// tamperArchive rewrites the archived object at uri and restores it at
// cleanup.
func tamperArchive(t *testing.T, uri string, fn func([]byte) []byte) {
	t.Helper()
	path := archivePath(t, uri)
	original, err := os.ReadFile(path) //nolint:gosec // test-only archive directory
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0o600))
	require.NoError(t, os.WriteFile(path, fn(append([]byte(nil), original...)), 0o600))
	t.Cleanup(func() {
		require.NoError(t, os.WriteFile(path, original, 0o600))
		require.NoError(t, os.Chmod(path, 0o440))
	})
}

// removeArchiveObject deletes the archived object and restores it at cleanup
// (a lost WORM object).
func removeArchiveObject(t *testing.T, uri string) {
	t.Helper()
	path := archivePath(t, uri)
	original, err := os.ReadFile(path) //nolint:gosec // test-only archive directory
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0o600))
	require.NoError(t, os.Remove(path))
	t.Cleanup(func() {
		require.NoError(t, os.WriteFile(path, original, 0o440))
	})
}

func leavesFromDB(t *testing.T, covered map[string]StreamRange) [][]byte {
	t.Helper()
	leaves, _, err := loadLeaves(context.Background(), testDB, covered)
	require.NoError(t, err)
	return leaves
}

// ---- tests -----------------------------------------------------------------

func TestIntegration_CleanCheckpointVerifiesAndRecordsRun(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	before := cpCount(t)
	a, b := freshStream(), freshStream()
	evA := appendEvents(t, a, 5)
	evB := appendEvents(t, b, 3)

	cp := runFull(t)
	assert.Equal(t, before+1, cpCount(t))
	assert.Equal(t, SignerLocalTest, cp.Signer)
	assert.Equal(t, testSigner.KeyID(), cp.SigningKeyID)
	assert.Equal(t, AlgorithmECDSASHA256, cp.SignatureAlgorithm)
	assert.Equal(t, testBuild, cp.BuildVersion)
	assert.Len(t, cp.MerkleRoot, HashSize)
	assert.Len(t, cp.ArchiveSHA256, HashSize)
	assert.NotEmpty(t, cp.ArchiveURI)
	assert.GreaterOrEqual(t, cp.LeafCount, 8)
	assert.Equal(t, StreamRange{FromSeq: 1, ToSeq: 5, LastContentHash: evA[4].ContentHash}, cp.StreamsCovered[a])
	assert.Equal(t, StreamRange{FromSeq: 1, ToSeq: 3, LastContentHash: evB[2].ContentHash}, cp.StreamsCovered[b])

	// Determinism: the root equals the tree over the content hashes read back
	// from audit_events in (stream bytewise, stream_seq) order.
	leaves := leavesFromDB(t, cp.StreamsCovered)
	assert.Equal(t, cp.LeafCount, len(leaves))
	assert.Equal(t, cp.MerkleRoot, NewTree(leaves).Root())
	assert.Equal(t, cp.MerkleRoot, NewTree(leaves).Root(), "same events, same root")
	// The stored row equals the returned checkpoint and its signature verifies.
	rows, err := listCheckpoints(ctx, testDB, cp.Seq-1, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	stored := rows[0]
	assert.Equal(t, cp.ID, stored.ID)
	assert.Equal(t, cp.StreamsCovered, stored.StreamsCovered)
	assert.Equal(t, cp.MerkleRoot, stored.MerkleRoot)
	assert.Equal(t, cp.Signature, stored.Signature)
	digest, err := stored.Digest()
	require.NoError(t, err)
	assert.NoError(t, testSigner.Verify(ctx, digest, stored.Signature, stored.SigningKeyID))

	// The archived object is the canonical document + signature, and its hash is the row's.
	body, err := testArchive.Get(ctx, cp.ArchiveURI)
	require.NoError(t, err)
	sum := sha256.Sum256(body)
	assert.Equal(t, cp.ArchiveSHA256, sum[:])
	var arch ArchivedCheckpoint
	require.NoError(t, json.Unmarshal(body, &arch))
	assert.Equal(t, cp.ID, arch.ID)
	assert.Equal(t, digest, arch.Digest)
	assert.Equal(t, cp.Signature, arch.Signature)
	assert.Nil(t, arch.AttestationRef)
	assert.Equal(t, testSigner.KeyID(), arch.SigningKeyID, "the object names the key, like the row")

	rep := requireVerifies(t)
	assert.GreaterOrEqual(t, rep.CheckedEvents, int64(8))
	assert.GreaterOrEqual(t, rep.CoveredEvents, int64(8))
	assert.Equal(t, before+1, rep.CheckedCheckpoints)
	assert.False(t, rep.FinishedAt.Before(rep.StartedAt))
	var (
		ok       bool
		events   int64
		cps      int
		failure  []byte
		buildVer string
	)
	require.NoError(t, testDB.QueryRow(ctx, `SELECT ok, checked_events, checked_checkpoints, first_failure, build_version FROM audit_verification_runs WHERE id = $1`, rep.RunID).
		Scan(&ok, &events, &cps, &failure, &buildVer))
	assert.True(t, ok)
	assert.Equal(t, rep.CheckedEvents, events)
	assert.Equal(t, rep.CheckedCheckpoints, cps)
	assert.Nil(t, failure)
	assert.Equal(t, testBuild, buildVer)
}

func TestIntegration_CheckpointsChainAndCoverContiguously(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	a := freshStream()
	appendEvents(t, a, 4)
	first := runFull(t)

	// Nothing new: no checkpoint.
	cp := newCheckpointer(t, CheckpointerOptions{})
	res, err := cp.Run(ctx)
	require.NoError(t, err)
	assert.False(t, res.Created)
	assert.Equal(t, SkipNoNewEvents, res.Skipped)
	res, err = cp.RunFull(ctx)
	require.NoError(t, err)
	assert.Equal(t, SkipNoNewEvents, res.Skipped)

	// New events on the old stream and on a new one: the incremental run
	// continues exactly where the previous checkpoint stopped.
	more := appendEvents(t, a, 3) // seqs 5..7 (sampleEvent numbering restarts; seq is the writer's)
	b := freshStream()
	evB := appendEvents(t, b, 2)
	res, err = cp.Run(ctx)
	require.NoError(t, err)
	require.True(t, res.Created, "skipped: %s", res.Skipped)
	second := res.Checkpoint
	assert.Equal(t, first.Seq+1, second.Seq)
	require.NotNil(t, second.PrevCheckpointID)
	assert.Equal(t, first.ID, *second.PrevCheckpointID)
	assert.Equal(t, first.MerkleRoot, second.PrevRoot)
	assert.Equal(t, StreamRange{FromSeq: 5, ToSeq: 7, LastContentHash: more[2].ContentHash}, second.StreamsCovered[a], "continues at the previous to_seq + 1")
	assert.Equal(t, StreamRange{FromSeq: 1, ToSeq: 2, LastContentHash: evB[1].ContentHash}, second.StreamsCovered[b])
	assert.NotEqual(t, first.MerkleRoot, second.MerkleRoot)
	requireVerifies(t)

	// Leaf order is bytewise by stream then seq, independent of insertion order.
	streams := second.Streams()
	assert.True(t, sort.StringsAreSorted(streams))
}

func TestIntegration_ConcurrentRunsProduceOneCheckpoint(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	appendEvents(t, freshStream(), 6)
	before := cpCount(t)
	cp := newCheckpointer(t, CheckpointerOptions{})

	const n = 3
	var start, wg sync.WaitGroup
	start.Add(1)
	results := make([]CheckpointResult, n)
	errors := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			results[i], errors[i] = cp.Run(ctx)
		}(i)
	}
	start.Done()
	wg.Wait()
	created := 0
	for i := range n {
		require.NoError(t, errors[i], "goroutine %d", i)
		if results[i].Created {
			created++
		} else {
			assert.Contains(t, []SkipReason{SkipInProgress, SkipNoNewEvents}, results[i].Skipped, "goroutine %d", i)
		}
	}
	assert.Equal(t, 1, created, "exactly one goroutine creates the checkpoint")
	assert.Equal(t, before+1, cpCount(t))
	requireVerifies(t)
}

func TestIntegration_MaxLeavesTruncatesAndResumes(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	a, b := freshStream(), freshStream()
	appendEvents(t, a, 4)
	appendEvents(t, b, 3)
	cp := newCheckpointer(t, CheckpointerOptions{MaxLeaves: 3})
	covered := map[string]int64{}
	var runs int
	for {
		res, err := cp.RunFull(ctx)
		require.NoError(t, err)
		if !res.Created {
			break
		}
		runs++
		assert.LessOrEqual(t, res.Checkpoint.LeafCount, 3)
		for s, r := range res.Checkpoint.StreamsCovered {
			if s == a || s == b {
				assert.Equal(t, covered[s]+1, r.FromSeq, "contiguous across checkpoint boundaries")
				covered[s] = r.ToSeq
			}
		}
		if !res.Truncated {
			break
		}
		require.Less(t, runs, 10)
	}
	assert.Equal(t, int64(4), covered[a])
	assert.Equal(t, int64(3), covered[b])
	assert.GreaterOrEqual(t, runs, 3)
	requireVerifies(t)
}

func TestIntegration_RefusesToCheckpointABrokenChain(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	admin := migrateConn(t)
	stream := freshStream()
	appendEvents(t, stream, 4)
	before := cpCount(t)
	// Remove seq 2 before any checkpoint covers it (restored at cleanup).
	_, err := admin.Exec(ctx, `CREATE TEMP TABLE proof_bk_gap AS SELECT * FROM audit_events WHERE stream = $1 AND stream_seq = 2`, stream)
	require.NoError(t, err)
	tamper(t, admin, "audit_events", "audit_events_immutable",
		`DELETE FROM audit_events WHERE stream = $1 AND stream_seq = 2`, []any{stream},
		`INSERT INTO audit_events SELECT * FROM proof_bk_gap`, nil)
	_, err = newCheckpointer(t, CheckpointerOptions{}).RunFull(ctx)
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	assert.Contains(t, err.Error(), stream)
	assert.Equal(t, before, cpCount(t), "no checkpoint row was written")
}

func TestIntegration_TamperingIsDetected(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	stream := freshStream()
	events := appendEvents(t, stream, 6)
	cp := runFull(t)
	requireVerifies(t)

	t.Run("a: audit row modified after checkpointing -> stream chain at that seq", func(t *testing.T) {
		admin := migrateConn(t)
		tamper(t, admin, "audit_events", "audit_events_immutable",
			`UPDATE audit_events SET reason = 'edited after the fact' WHERE stream = $1 AND stream_seq = 4`, []any{stream},
			`UPDATE audit_events SET reason = NULL WHERE stream = $1 AND stream_seq = 4`, []any{stream})
		f := requireFails(t, FailStreamChain)
		assert.Equal(t, stream, f.Stream)
		require.NotNil(t, f.StreamSeq)
		assert.Equal(t, int64(4), *f.StreamSeq)
		assert.Contains(t, f.Reason, "content_hash")
	})
	t.Run("b: bit flipped in the archived object -> object hash", func(t *testing.T) {
		tamperArchive(t, cp.ArchiveURI, func(b []byte) []byte { b[len(b)/2] ^= 0x01; return b })
		f := requireFails(t, FailArchiveObject)
		assert.Equal(t, cp.ArchiveURI, f.ArchiveURI)
		assert.Equal(t, cp.ID.String(), f.CheckpointID)
		assert.Contains(t, f.Reason, "sha256")
	})
	t.Run("b2: archived object replaced by a well-formed document with the same hash mismatch -> object", func(t *testing.T) {
		tamperArchive(t, cp.ArchiveURI, func(b []byte) []byte {
			var arch ArchivedCheckpoint
			require.NoError(t, json.Unmarshal(b, &arch))
			arch.Checkpoint.LeafCount++
			out, err := json.Marshal(arch)
			require.NoError(t, err)
			return out
		})
		f := requireFails(t, FailArchiveObject)
		assert.Contains(t, f.Reason, "sha256")
	})
	t.Run("b3: archived object lost -> object", func(t *testing.T) {
		removeArchiveObject(t, cp.ArchiveURI)
		f := requireFails(t, FailArchiveObject)
		assert.Contains(t, f.Reason, "cannot be fetched")
	})
	t.Run("c: merkle_root altered in the row -> signature", func(t *testing.T) {
		admin := migrateConn(t)
		forged := sha256.Sum256([]byte("forged root"))
		tamper(t, admin, "audit_checkpoints", "audit_checkpoints_immutable",
			`UPDATE audit_checkpoints SET merkle_root = $2 WHERE id = $1`, []any{cp.ID, forged[:]},
			`UPDATE audit_checkpoints SET merkle_root = $2 WHERE id = $1`, []any{cp.ID, cp.MerkleRoot})
		f := requireFails(t, FailCheckpointSignature)
		assert.Equal(t, cp.ID.String(), f.CheckpointID)
		require.NotNil(t, f.CheckpointSeq)
		assert.Equal(t, cp.Seq, *f.CheckpointSeq)
		assert.Contains(t, f.Reason, "signature")
	})
	t.Run("c2: streams_covered altered in the row -> signature", func(t *testing.T) {
		admin := migrateConn(t)
		var original []byte
		require.NoError(t, admin.QueryRow(ctx, `SELECT streams_covered FROM audit_checkpoints WHERE id = $1`, cp.ID).Scan(&original))
		altered := map[string]StreamRange{}
		for s, r := range cp.StreamsCovered {
			altered[s] = r
		}
		r := altered[stream]
		r.ToSeq = 5
		altered[stream] = r
		alteredJSON, err := json.Marshal(altered)
		require.NoError(t, err)
		tamper(t, admin, "audit_checkpoints", "audit_checkpoints_immutable",
			`UPDATE audit_checkpoints SET streams_covered = $2 WHERE id = $1`, []any{cp.ID, alteredJSON},
			`UPDATE audit_checkpoints SET streams_covered = $2 WHERE id = $1`, []any{cp.ID, original})
		requireFails(t, FailCheckpointSignature)
	})
	t.Run("c3: signature forged with another key -> signature", func(t *testing.T) {
		admin := migrateConn(t)
		otherKey, err := GenerateLocalKey()
		require.NoError(t, err)
		other, err := NewLocalECDSASigner(config.EnvTest, otherKey)
		require.NoError(t, err)
		digest, err := cp.Digest()
		require.NoError(t, err)
		forgedSig, _, _, err := other.Sign(ctx, digest)
		require.NoError(t, err)
		// The forger keeps the row's key id (a foreign key id fails on the id alone).
		tamper(t, admin, "audit_checkpoints", "audit_checkpoints_immutable",
			`UPDATE audit_checkpoints SET signature = $2 WHERE id = $1`, []any{cp.ID, forgedSig},
			`UPDATE audit_checkpoints SET signature = $2 WHERE id = $1`, []any{cp.ID, cp.Signature})
		f := requireFails(t, FailCheckpointSignature)
		assert.Contains(t, f.Reason, "does not verify")
	})
	t.Run("d: audit row deleted from the middle -> stream chain gap", func(t *testing.T) {
		admin := migrateConn(t)
		_, err := admin.Exec(ctx, `CREATE TEMP TABLE proof_bk_mid AS SELECT * FROM audit_events WHERE stream = $1 AND stream_seq = 3`, stream)
		require.NoError(t, err)
		tamper(t, admin, "audit_events", "audit_events_immutable",
			`DELETE FROM audit_events WHERE stream = $1 AND stream_seq = 3`, []any{stream},
			`INSERT INTO audit_events SELECT * FROM proof_bk_mid`, nil)
		f := requireFails(t, FailStreamChain)
		assert.Equal(t, stream, f.Stream)
		require.NotNil(t, f.StreamSeq)
		assert.Equal(t, int64(4), *f.StreamSeq, "reported at the first surviving row after the gap")
		assert.Contains(t, f.Reason, "sequence gap")
	})
	t.Run("e: tail of a covered stream truncated -> checkpoint root, not the chain", func(t *testing.T) {
		// Deleting the last row leaves a perfectly valid shorter chain; only
		// the checkpoint knows the stream once reached seq 6.
		admin := migrateConn(t)
		_, err := admin.Exec(ctx, `CREATE TEMP TABLE proof_bk_tail AS SELECT * FROM audit_events WHERE stream = $1 AND stream_seq = 6`, stream)
		require.NoError(t, err)
		tamper(t, admin, "audit_events", "audit_events_immutable",
			`DELETE FROM audit_events WHERE stream = $1 AND stream_seq = 6`, []any{stream},
			`INSERT INTO audit_events SELECT * FROM proof_bk_tail`, nil)
		f := requireFails(t, FailCheckpointRoot)
		assert.Equal(t, stream, f.Stream)
		require.NotNil(t, f.StreamSeq)
		assert.Equal(t, int64(6), *f.StreamSeq)
		assert.Contains(t, f.Reason, "missing")
	})
	t.Run("f: coverage index altered -> coverage", func(t *testing.T) {
		admin := migrateConn(t)
		tamper(t, admin, "audit_checkpoint_streams", "audit_checkpoint_streams_immutable",
			`UPDATE audit_checkpoint_streams SET to_seq = to_seq - 1 WHERE checkpoint_id = $1 AND stream = $2`, []any{cp.ID, stream},
			`UPDATE audit_checkpoint_streams SET to_seq = to_seq + 1 WHERE checkpoint_id = $1 AND stream = $2`, []any{cp.ID, stream})
		f := requireFails(t, FailCoverage)
		assert.Contains(t, f.Reason, "disagrees")
	})
	t.Run("g: rows re-hashed consistently but the checkpoint remembers the original -> checkpoint root", func(t *testing.T) {
		// An attacker who rewrites seq 6 AND recomputes its content_hash (no
		// later row exists to break the prev_hash link) defeats the chain
		// alone; the signed checkpoint still holds the original hash.
		admin := migrateConn(t)
		var prev []byte
		require.NoError(t, testDB.QueryRow(ctx, `SELECT content_hash FROM audit_events WHERE stream = $1 AND stream_seq = 5`, stream).Scan(&prev))
		edited := sampleEvent(stream, 6)
		edited.Reason = "rewritten"
		newHash, err := audit.HashEvent(edited, 6, prev, testBuild)
		require.NoError(t, err)
		tamper(t, admin, "audit_events", "audit_events_immutable",
			`UPDATE audit_events SET reason = 'rewritten', content_hash = $2 WHERE stream = $1 AND stream_seq = 6`, []any{stream, newHash},
			`UPDATE audit_events SET reason = NULL, content_hash = $2 WHERE stream = $1 AND stream_seq = 6`, []any{stream, events[5].ContentHash})
		f := requireFails(t, FailCheckpointRoot)
		assert.Equal(t, stream, f.Stream)
		assert.Contains(t, f.Reason, "last_content_hash")
	})
	// Every tamper was undone: the history verifies again.
	requireVerifies(t)
}

func TestIntegration_CheckpointRowsAreImmutableAndChained(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	appendEvents(t, freshStream(), 2)
	cp := runFull(t)

	// cp_app has no UPDATE/DELETE privilege at all.
	for _, stmt := range []string{
		`UPDATE audit_checkpoints SET build_version = 'x' WHERE id = $1`,
		`DELETE FROM audit_checkpoints WHERE id = $1`,
		`UPDATE audit_checkpoint_streams SET to_seq = 1 WHERE checkpoint_id = $1`,
		`DELETE FROM audit_checkpoint_streams WHERE checkpoint_id = $1`,
	} {
		_, err := testDB.Exec(ctx, stmt, cp.ID)
		require.Error(t, err, stmt)
		assert.True(t, db.IsInsufficientPrivilege(err), "%s: SQLSTATE %q", stmt, db.SQLState(err))
	}
	_, err := testDB.Exec(ctx, `DELETE FROM audit_verification_runs`)
	require.Error(t, err)
	assert.True(t, db.IsInsufficientPrivilege(err))

	// Even the owner is stopped by the forbid_mutation triggers.
	admin := migrateConn(t)
	for _, stmt := range []string{
		`UPDATE audit_checkpoints SET build_version = 'x' WHERE id = $1`,
		`DELETE FROM audit_checkpoints WHERE id = $1`,
		`UPDATE audit_checkpoint_streams SET to_seq = 1 WHERE checkpoint_id = $1`,
	} {
		_, err := admin.Exec(ctx, stmt, cp.ID)
		require.Error(t, err, stmt)
		assert.True(t, db.IsImmutableRow(err), "%s: SQLSTATE %q: %v", stmt, db.SQLState(err), err)
	}

	// The chain trigger refuses an out-of-order or mis-linked checkpoint even
	// from a role that can INSERT.
	forged := Checkpoint{
		ID: NewCheckpointID(), Document: cp.Document, Signature: cp.Signature, SigningKeyID: cp.SigningKeyID,
		SignatureAlgorithm: cp.SignatureAlgorithm, Signer: cp.Signer, ArchiveURI: cp.ArchiveURI, ArchiveSHA256: cp.ArchiveSHA256, CreatedAt: cp.CreatedAt,
	}
	prev := cp.ID
	forged.PrevCheckpointID = &prev
	forged.PrevRoot = cp.MerkleRoot
	cases := map[string]func(*Checkpoint){
		"seq gap":             func(c *Checkpoint) { c.Seq = cp.Seq + 2 },
		"wrong predecessor":   func(c *Checkpoint) { c.Seq = cp.Seq + 1; other := NewCheckpointID(); c.PrevCheckpointID = &other },
		"wrong prev_root":     func(c *Checkpoint) { c.Seq = cp.Seq + 1; c.PrevRoot = testDigest("x") },
		"duplicate seq":       func(c *Checkpoint) { c.Seq = cp.Seq },
		"first with a parent": func(c *Checkpoint) { c.Seq = 1 },
	}
	for name, mutate := range cases {
		f := forged
		mutate(&f)
		err := testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error { return insertCheckpoint(ctx, tx, f) })
		require.Error(t, err, name)
		state := db.SQLState(err)
		assert.True(t, state == "AU002" || state == db.SQLStateUniqueViolation || state == db.SQLStateCheckViolation || state == db.SQLStateForeignKeyViolation, "%s: SQLSTATE %q: %v", name, state, err)
	}
	requireVerifies(t)
}

func TestIntegration_VerifierNeedsKeysAndArchiveOnceCheckpointsExist(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	appendEvents(t, freshStream(), 1)
	runFull(t)
	for name, keys := range map[string]*KeySet{"nil key set": nil, "empty key set": mustKeySet(t)} {
		v, err := NewVerifier(keys, testArchive, clock.System(), VerifierOptions{})
		require.NoError(t, err, name)
		_, err = v.VerifyAll(ctx, testDB)
		assert.ErrorIs(t, err, ErrNoTrustedKeys, name)
	}
	v, err := NewVerifier(trustedKeys(), nil, clock.System(), VerifierOptions{})
	require.NoError(t, err)
	_, err = v.VerifyAll(ctx, testDB)
	assert.ErrorIs(t, err, ErrNoArchive)
	// None of these is recorded as a verification run: nothing was verified.
	var n int
	require.NoError(t, testDB.QueryRow(ctx, `SELECT count(*) FROM audit_verification_runs WHERE ok = false AND first_failure IS NULL`).Scan(&n))
	assert.Zero(t, n)
}

func mustKeySet(t *testing.T, keys ...TrustedKey) *KeySet {
	t.Helper()
	ks, err := NewKeySet(keys...)
	require.NoError(t, err)
	return ks
}

// keysWith derives a key set from the full trusted set by changing one key:
// status "" drops it entirely, KeyRevoked withdraws it. Deriving (rather than
// naming keys explicitly) keeps these cases about the one key under test even
// though the database accumulates checkpoints from every earlier run's keys.
func keysWith(t *testing.T, keyID string, status KeyStatus) *KeySet {
	t.Helper()
	entries, err := testKeyEntries()
	require.NoError(t, err)
	var out []TrustedKey
	found := false
	for _, k := range entries {
		if k.KeyID != keyID {
			out = append(out, k)
			continue
		}
		found = true
		switch status {
		case "": // dropped
		case KeyRevoked:
			out = append(out, RevokedKey(keyID))
		default:
			k.Status = status
			out = append(out, k)
		}
	}
	require.True(t, found, "key %s is not in the test key directory", keyID)
	return mustKeySet(t, out...)
}

// TestIntegration_KeyRotation is the case that broke this suite before D-029:
// checkpoints are append-only and outlive their signing key, so verification
// must use the key each row names, out of a trusted set.
func TestIntegration_KeyRotation(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	stream := freshStream()
	appendEvents(t, stream, 3)
	before := runFull(t)
	requireVerifies(t)

	// Rotate. The outgoing key signs nothing further but stays trusted.
	retired := rotateSigner(t)
	require.NotEqual(t, retired.KeyID(), testSigner.KeyID())
	appendEvents(t, stream, 2)
	after := runFull(t)
	assert.Equal(t, retired.KeyID(), before.SigningKeyID)
	assert.Equal(t, testSigner.KeyID(), after.SigningKeyID, "new checkpoints carry the new key")
	assert.Equal(t, before.MerkleRoot, after.PrevRoot, "the checkpoint chain is unbroken across the rotation")

	t.Run("both keys trusted: a rotation is not an incident", func(t *testing.T) {
		rep := requireVerifies(t)
		assert.GreaterOrEqual(t, rep.CheckedCheckpoints, 2)
		st, ok := trustedKeys().Status(retired.KeyID())
		assert.True(t, ok)
		assert.Equal(t, KeyRetired, st)
		st, ok = trustedKeys().Status(testSigner.KeyID())
		assert.True(t, ok)
		assert.Equal(t, KeyActive, st)
	})

	t.Run("the retired key is dropped from the set: unknown, not tampered", func(t *testing.T) {
		rep, err := newVerifierWithKeys(t, keysWith(t, retired.KeyID(), "")).VerifyAll(ctx, testDB)
		require.NoError(t, err)
		require.False(t, rep.OK)
		require.NotNil(t, rep.FirstFailure)
		assert.Equal(t, FailCheckpointKeyUnknown, rep.FirstFailure.Kind, "%s", rep.FirstFailure)
		assert.NotEqual(t, FailCheckpointSignature, rep.FirstFailure.Kind)
		assert.Equal(t, retired.KeyID(), rep.FirstFailure.SigningKeyID, "the report names the key that is missing")
		assert.Contains(t, rep.FirstFailure.Reason, "not trusted")
		assert.Contains(t, rep.FirstFailure.Reason, "never judged")
	})

	t.Run("the retired key is revoked: its own reason, not unknown", func(t *testing.T) {
		rep, err := newVerifierWithKeys(t, keysWith(t, retired.KeyID(), KeyRevoked)).VerifyAll(ctx, testDB)
		require.NoError(t, err)
		require.False(t, rep.OK)
		require.NotNil(t, rep.FirstFailure)
		assert.Equal(t, FailCheckpointKeyRevoked, rep.FirstFailure.Kind, "%s", rep.FirstFailure)
		assert.Equal(t, retired.KeyID(), rep.FirstFailure.SigningKeyID)
		// The named checkpoint is one the revoked key signed: the first in seq
		// order, which is at or before the last pre-rotation checkpoint.
		assert.NotEmpty(t, rep.FirstFailure.CheckpointID)
		require.NotNil(t, rep.FirstFailure.CheckpointSeq)
		assert.LessOrEqual(t, *rep.FirstFailure.CheckpointSeq, before.Seq)
		assert.Contains(t, rep.FirstFailure.Reason, "revoked")
	})

	t.Run("revocation of the active key overrides its trust", func(t *testing.T) {
		rep, err := newVerifierWithKeys(t, keysWith(t, testSigner.KeyID(), KeyRevoked)).VerifyAll(ctx, testDB)
		require.NoError(t, err)
		require.False(t, rep.OK)
		require.NotNil(t, rep.FirstFailure)
		assert.Equal(t, FailCheckpointKeyRevoked, rep.FirstFailure.Kind, "%s", rep.FirstFailure)
		assert.Equal(t, testSigner.KeyID(), rep.FirstFailure.SigningKeyID)
	})

	t.Run("a trusted key still catches a forged signature", func(t *testing.T) {
		// Widening trust must not weaken the signature check: with both keys
		// trusted, a signature forged by a third key on a row that names a
		// trusted key is still checkpoint_signature.
		admin := migrateConn(t)
		stranger, err := GenerateLocalKey()
		require.NoError(t, err)
		other, err := NewLocalECDSASigner(config.EnvTest, stranger)
		require.NoError(t, err)
		digest, err := after.Digest()
		require.NoError(t, err)
		forged, _, _, err := other.Sign(ctx, digest)
		require.NoError(t, err)
		tamper(t, admin, "audit_checkpoints", "audit_checkpoints_immutable",
			`UPDATE audit_checkpoints SET signature = $2 WHERE id = $1`, []any{after.ID, forged},
			`UPDATE audit_checkpoints SET signature = $2 WHERE id = $1`, []any{after.ID, after.Signature})
		f := requireFails(t, FailCheckpointSignature)
		assert.Equal(t, testSigner.KeyID(), f.SigningKeyID)
		assert.Contains(t, f.Reason, "does not verify")
	})

	requireVerifies(t)
}

// ---- proof bundle ----------------------------------------------------------

type bundleFixture struct {
	account                                                                      accounts.AccountID
	intentID, planID, quoteID, orderID, attemptID, fillID, riskID, eligID, resID string
	decisionID, resultID, journalID, walletID                                    string
	corr                                                                         string
	intentHash, planHash, quoteHash, signedTxHash                                []byte
	events                                                                       []audit.Appended
}

func newBundleFixture(t *testing.T) *bundleFixture {
	t.Helper()
	ctx := context.Background()
	f := &bundleFixture{}
	arepo := accounts.NewRepository()
	u, err := arepo.CreateUser(ctx, testDB, "proof-itest", "sub-"+uuid.NewString(), nil)
	require.NoError(t, err)
	acct, err := arepo.CreateAccount(ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	f.account = acct.ID
	suffix := uuid.NewString()[24:]
	usdc, err := assets.NewRepository().Create(ctx, testDB, assets.Asset{
		Chain: "solana-devnet", MintAddress: "usdc-" + suffix, Kind: assets.KindSPLToken, ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "USDC", Name: "USD Coin", Decimals: 6,
		IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	sol, err := assets.NewRepository().Create(ctx, testDB, assets.Asset{
		Chain: "solana-devnet", MintAddress: "sol-" + suffix, Kind: assets.KindSPLToken, ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "SOL", Name: "Solana", Decimals: 9,
		RiskClass: assets.RiskMajor, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	var (
		inst    instruments.Instrument
		listing instruments.VenueListing
	)
	irepo := instruments.NewRepository()
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inst, err = irepo.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: sol.ID, Quote: usdc.ID, Settlement: usdc.ID, CanonicalName: "SOL/USDC-" + suffix, RiskClass: assets.RiskMajor,
			Status: assets.StatusActive, ActiveFrom: time.Now().UTC().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		venue, err := irepo.CreateVenue(ctx, tx, instruments.Venue{Code: "JUP-" + suffix, Name: "Jupiter", Kind: instruments.VenueDEXAggregator, Chain: "solana-devnet", Status: instruments.VenueActive})
		if err != nil {
			return err
		}
		listing, err = irepo.CreateListing(ctx, tx, instruments.VenueListing{
			VenueID: venue.ID, InstrumentID: inst.ID, VenueNativeID: "SOL-USDC", Network: "solana-devnet", BaseMint: sol.MintAddress, QuoteMint: usdc.MintAddress,
			BasePrecision: 9, QuotePrecision: 6, MinNotionalQuote: money.QuantityFromInt64(1_000_000), Status: instruments.VenueActive,
		})
		return err
	}))

	newID := func() string { return id.New[id.Any]().String() }
	f.intentID, f.planID, f.quoteID, f.orderID, f.attemptID, f.fillID = newID(), newID(), newID(), newID(), newID(), newID()
	f.riskID, f.eligID, f.resID, f.decisionID, f.resultID, f.walletID = newID(), newID(), newID(), newID(), newID(), newID()
	f.corr = "corr-" + suffix
	h := func(s string) []byte { sum := sha256.Sum256([]byte(s)); return sum[:] }
	f.intentHash, f.planHash, f.quoteHash, f.signedTxHash = h("intent"+suffix), h("plan"+suffix), h("quote"+suffix), h("signed"+suffix)
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := testDB.Exec(ctx, sql, args...)
		require.NoError(t, err, sql)
	}
	exec(`INSERT INTO eligibility_decisions (id, account_id, intent_id, context_kind, eligible, policy_version, context_hash, evaluated_at, correlation_id)
	      VALUES ($1,$2,NULL,'TRADE',true,'elig-v1',$3,now(),$4)`, f.eligID, f.account, h("elig"+suffix), f.corr)
	exec(`INSERT INTO risk_decisions (id, intent_id, account_id, stage, policy_version, policy_hash, account_snapshot, market_snapshot, decision, evaluator_version, evaluated_at, correlation_id)
	      VALUES ($1,NULL,$2,'PRE_TRADE','risk-v1',$3,'{}','{}','ALLOW','eval-1',now(),$4)`, f.riskID, f.account, h("risk"+suffix), f.corr)
	exec(`INSERT INTO trade_intents (id, account_id, actor_type, actor_id, action, instrument_id, notional_usd_minor, constraints, requested_at, idempotency_key, correlation_id, mode, status, content_hash, eligibility_decision_id, risk_decision_id)
	      VALUES ($1,$2,'USER','user-1','ACQUIRE_NOTIONAL',$3,10000,'{}',now(),$4,$5,'LIVE','PLANNED',$6,$7,$8)`,
		f.intentID, f.account, inst.ID, "idem-"+suffix, f.corr, f.intentHash, f.eligID, f.riskID)
	// The decisions were inserted before the intent existed (FK); bind them now via their own tables? They are immutable,
	// so intent_id stays NULL there and the bundle must still reach them through the intent's decision ids and the correlation id.
	exec(`INSERT INTO asset_reservations (id, account_id, asset_id, intent_id, actor_type, actor_id, quantity, usd_minor, status, idempotency_key, expires_at)
	      VALUES ($1,$2,$3,$4,'USER','user-1',100000000,10000,'ACTIVE',$5,now() + interval '1 hour')`, f.resID, f.account, usdc.ID, f.intentID, "res-"+suffix)
	exec(`INSERT INTO quotes (id, intent_id, provider, instrument_id, venue_listing_id, side, input_asset_id, input_quantity, output_asset_id, expected_output, minimum_output,
	        effective_price_mantissa, effective_price_scale, price_impact_bps, slippage_bps, received_at, expires_at, route_hash, raw_response_hash, raw_response_ref)
	      VALUES ($1,$2,'jupiter',$3,$4,'BUY',$5,100000000,$6,666666666,660000000,150,0,5,50,now(),now() + interval '30 seconds',$7,$8,'s3://evidence/quote')`,
		f.quoteID, f.intentID, inst.ID, listing.ID, usdc.ID, sol.ID, h("route"+suffix), f.quoteHash)
	exec(`INSERT INTO execution_plans (id, intent_id, version, planner_version, status, hard_constraints, plan_hash, risk_decision_id, quote_id, approved_at)
	      VALUES ($1,$2,1,'planner-1','APPROVED','{}',$3,$4,$5,now())`, f.planID, f.intentID, f.planHash, f.riskID, f.quoteID)
	exec(`UPDATE trade_intents SET plan_id = $2 WHERE id = $1`, f.intentID, f.planID)
	exec(`INSERT INTO wallets (id, account_id, provider, provider_wallet_id, chain, address, kind, status)
	      VALUES ($1,$2,'privy',$3,'solana-devnet',$4,'EMBEDDED_DELEGATED','ACTIVE')`, f.walletID, f.account, "pw-"+suffix, "addr-"+suffix)
	exec(`INSERT INTO orders (id, intent_id, plan_id, account_id, instrument_id, venue_listing_id, side, mode, status, input_asset_id, input_quantity, output_asset_id, min_output_quantity, reservation_id, quote_id, correlation_id)
	      VALUES ($1,$2,$3,$4,$5,$6,'BUY','LIVE','FILLED',$7,100000000,$8,660000000,$9,$10,$11)`,
		f.orderID, f.intentID, f.planID, f.account, inst.ID, listing.ID, usdc.ID, sol.ID, f.resID, f.quoteID, f.corr)
	exec(`UPDATE trade_intents SET order_id = $2 WHERE id = $1`, f.intentID, f.orderID)
	exec(`INSERT INTO execution_attempts (id, order_id, plan_id, attempt_no, wallet_id, provider, quote_id, unsigned_tx_hash, signed_tx_hash, tx_signature, status, finality, correlation_id, submitted_at, finalized_at)
	      VALUES ($1,$2,$3,1,$4,'jupiter',$5,$6,$7,$8,'FINALIZED','FINALIZED',$9,now(),now())`,
		f.attemptID, f.orderID, f.planID, f.walletID, f.quoteID, h("unsigned"+suffix), f.signedTxHash, "sig-"+suffix, f.corr)
	exec(`INSERT INTO signing_decisions (id, attempt_id, plan_id, intent_id, risk_decision_id, wallet_id, expected_tx_hash, inspected_tx_hash, decision, checks, inspector_version, requested_by_service, decided_at)
	      VALUES ($1,$2,$3,$4,$5,$6,$7,$7,'APPROVED','{}','inspector-1','execution-worker',now())`,
		f.decisionID, f.attemptID, f.planID, f.intentID, f.riskID, f.walletID, h("unsigned"+suffix))
	exec(`UPDATE execution_attempts SET signing_decision_id = $2 WHERE id = $1`, f.attemptID, f.decisionID)
	exec(`INSERT INTO signing_results (id, decision_id, attempt_id, wallet_id, provider, idempotency_key, signed_tx, signed_tx_hash, signature, retry_class, signed_at)
	      VALUES ($1,$2,$3,$4,'privy',$5,$6,$7,$8,'UNKNOWN_EFFECT_WRITE',now())`,
		f.resultID, f.decisionID, f.attemptID, f.walletID, "sign-"+suffix, []byte("signed-tx-bytes"), f.signedTxHash, bytes.Repeat([]byte{0xab}, 64))

	// A real journal transaction through the ledger service.
	svc := ledger.NewService(clock.System(), testBuild)
	posting, err := ledger.FundingSettledPosting(ledger.FundingInputs{AccountID: f.account, DepositID: "dep-" + suffix, AssetID: usdc.ID, Quantity: money.QuantityFromInt64(100_000_000), EffectiveAt: time.Now().UTC(), CorrelationID: f.corr})
	require.NoError(t, err)
	posted, err := svc.PostInTx(ctx, testDB, posting)
	require.NoError(t, err)
	f.journalID = posted.TransactionID.String()
	exec(`INSERT INTO fills (id, order_id, attempt_id, account_id, venue, external_fill_id, tx_signature, input_asset_id, input_quantity, output_asset_id, output_quantity,
	        effective_price_mantissa, effective_price_scale, source, finality, observed_at, journal_transaction_id)
	      VALUES ($1,$2,$3,$4,'JUPITER',$5,$6,$7,100000000,$8,666666666,150,0,'PROVIDER','CONFIRMED',now(),$9)`,
		f.fillID, f.orderID, f.attemptID, f.account, "ext-"+suffix, "sig-"+suffix, usdc.ID, sol.ID, f.journalID)

	// The audit events that recorded the chain, on the account stream and the system stream.
	stream := audit.AccountStream(f.account.String())
	ev := func(rt, rid, action, corr string) audit.Event {
		return audit.Event{Stream: stream, ActorType: "USER", ActorID: u.ID.String(), Action: action, ResourceType: rt, ResourceID: rid, CorrelationID: corr, OccurredAt: time.Now().UTC()}
	}
	for _, e := range []audit.Event{
		ev("eligibility_decision", f.eligID, "eligibility.evaluated", f.corr),
		ev("risk_decision", f.riskID, "risk.evaluated", f.corr),
		ev("trade_intent", f.intentID, "intent.received", f.corr),
		ev("reservation", f.resID, "capital.reserved", f.corr), // reached through the correlation id only
		ev("execution_plan", f.planID, "plan.approved", f.corr),
		ev("order", f.orderID, "order.created", f.corr),
		ev("fill", f.fillID, "fill.recorded", f.corr),
		ev("unrelated", newID(), "noise", ""), // must not appear
	} {
		f.events = append(f.events, appendEvent(t, e))
	}
	sys := audit.Event{Stream: audit.SystemStream, ActorType: "SYSTEM", ActorID: "ledger", Action: "ledger.posted", ResourceType: "journal_transaction", ResourceID: f.journalID, OccurredAt: time.Now().UTC()}
	f.events = append(f.events, appendEvent(t, sys))
	return f
}

func TestIntegration_BundleCollectsEveryLinkWithMembershipProofs(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	f := newBundleFixture(t)
	cp := runFull(t)
	requireVerifies(t)

	bundler, err := NewBundler(clock.System(), testBuild)
	require.NoError(t, err)
	ref, err := ResolveRef(ctx, testDB, f.fillID)
	require.NoError(t, err)
	assert.Equal(t, BundleRef{FillID: f.fillID}, ref)
	b, err := bundler.Bundle(ctx, testDB, ref)
	require.NoError(t, err)

	require.NotNil(t, b.Intent)
	assert.Equal(t, f.intentID, b.Intent.ID)
	assert.Equal(t, f.intentHash, b.Intent.ContentHash)
	assert.Equal(t, f.corr, b.Intent.CorrelationID)
	require.NotNil(t, b.Plan)
	assert.Equal(t, f.planHash, b.Plan.PlanHash)
	assert.Equal(t, "planner-1", b.Plan.PlannerVersion)
	require.NotNil(t, b.Quote)
	assert.Equal(t, f.quoteHash, b.Quote.RawResponseHash)
	assert.Len(t, b.Quote.RouteHash, 32)
	require.NotNil(t, b.Order)
	assert.Equal(t, f.orderID, b.Order.ID)
	require.Len(t, b.Attempts, 1)
	assert.Equal(t, f.signedTxHash, b.Attempts[0].SignedTxHash)
	require.NotNil(t, b.Attempts[0].TxSignature)
	assert.Equal(t, "sig-"+f.corr[len("corr-"):], *b.Attempts[0].TxSignature)
	require.NotNil(t, b.Attempts[0].SigningDecision)
	assert.Equal(t, f.decisionID, b.Attempts[0].SigningDecision.ID)
	require.NotNil(t, b.Attempts[0].SigningResult)
	assert.Len(t, b.Attempts[0].SigningResult.Signature, 64)
	require.Len(t, b.Fills, 1)
	assert.Equal(t, f.fillID, b.Fills[0].ID)
	require.Len(t, b.Journal, 1)
	assert.Equal(t, f.journalID, b.Journal[0].ID)
	var journalHash []byte
	require.NoError(t, testDB.QueryRow(ctx, `SELECT content_hash FROM journal_transactions WHERE id = $1`, f.journalID).Scan(&journalHash))
	assert.Equal(t, journalHash, b.Journal[0].ContentHash)
	require.Len(t, b.Risk, 1, "reached through the intent's risk_decision_id")
	assert.Equal(t, f.riskID, b.Risk[0].ID)
	require.Len(t, b.Eligibility, 1)
	assert.Equal(t, f.eligID, b.Eligibility[0].ID)
	assert.Nil(t, b.Prediction)
	assert.Nil(t, b.Model)
	assert.Nil(t, b.StrategyVersion)
	joined := fmt.Sprint(b.Absent)
	assert.Contains(t, joined, "prediction:")
	assert.Contains(t, joined, "strategy_version:")
	assert.Contains(t, joined, "model:")
	assert.NotContains(t, joined, "plan:")
	assert.NotContains(t, joined, "quote:")
	assert.NotContains(t, joined, "journal_transactions")

	// Audit events: every recording event, none of the noise, all with verified membership.
	wantIDs := map[string]bool{}
	for _, e := range f.events[:7] {
		wantIDs[e.ID.String()] = true
	}
	wantIDs[f.events[8].ID.String()] = true
	gotIDs := map[string]bool{}
	for _, e := range b.AuditEvents {
		gotIDs[e.ID] = true
		assert.Len(t, e.ContentHash, 32)
		require.NotNil(t, e.Checkpoint, "event %s %s is checkpointed", e.Action, e.ResourceID)
		assert.True(t, e.Checkpoint.Verified, "%s: %s", e.Action, e.Checkpoint.Error)
		assert.Equal(t, cp.ID.String(), e.Checkpoint.CheckpointID)
		assert.Equal(t, cp.MerkleRoot, e.Checkpoint.MerkleRoot)
		assert.NoError(t, VerifyMembership(cp.MerkleRoot, e.ContentHash, e.Checkpoint.Proof), "independent re-verification")
		assert.Equal(t, cp.LeafCount, e.Checkpoint.Proof.LeafCount)
	}
	assert.Equal(t, wantIDs, gotIDs)
	assert.False(t, gotIDs[f.events[7].ID.String()], "unrelated event excluded")

	// The bundle is JSON and round-trips.
	raw, err := json.Marshal(b)
	require.NoError(t, err)
	var back Bundle
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, b.Intent.ContentHash, back.Intent.ContentHash)
	assert.Len(t, back.AuditEvents, len(b.AuditEvents))

	// By intent id: the same chain.
	ref2, err := ResolveRef(ctx, testDB, f.intentID)
	require.NoError(t, err)
	assert.Equal(t, BundleRef{IntentID: f.intentID}, ref2)
	b2, err := bundler.Bundle(ctx, testDB, ref2)
	require.NoError(t, err)
	require.Len(t, b2.Fills, 1)
	assert.Equal(t, f.fillID, b2.Fills[0].ID)
	assert.Equal(t, b.Order.ID, b2.Order.ID)
	assert.Len(t, b2.AuditEvents, len(b.AuditEvents))

	// An event recorded after the checkpoint is present but not yet checkpointed.
	late := appendEvent(t, audit.Event{Stream: audit.AccountStream(f.account.String()), ActorType: "USER", ActorID: "u", Action: "fill.settled", ResourceType: "fill", ResourceID: f.fillID, OccurredAt: time.Now().UTC()})
	b3, err := bundler.Bundle(ctx, testDB, ref)
	require.NoError(t, err)
	var found bool
	for _, e := range b3.AuditEvents {
		if e.ID == late.ID.String() {
			found = true
			assert.Nil(t, e.Checkpoint, "not covered by any checkpoint yet")
		}
	}
	assert.True(t, found)

	// Unknown and malformed references.
	_, err = ResolveRef(ctx, testDB, id.New[id.Any]().String())
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	_, err = ResolveRef(ctx, testDB, "not-an-id")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = bundler.Bundle(ctx, testDB, BundleRef{})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = bundler.Bundle(ctx, testDB, BundleRef{FillID: f.fillID, IntentID: f.intentID})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = bundler.Bundle(ctx, testDB, BundleRef{FillID: id.New[id.Any]().String()})
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}
