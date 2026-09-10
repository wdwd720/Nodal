//go:build integration

package proof

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
)

// PgArchive is the archive of a deployment with no object store, so every
// property below is a property the S3 implementation got from the storage
// engine and this one has to get from somewhere else. The tests are written
// against a real database for that reason: the guarantee lives in migration
// 00730 -- the unique key, the CHECK constraints, the grant, the guard trigger
// -- at least as much as it lives in the Go, and a fake would assert only that
// this package agrees with itself.
//
// The guard trigger and the table name are named here on purpose. If a later
// migration renames or drops either, these tests fail loudly rather than
// quietly passing against an archive that no longer refuses anything.
const (
	pgArchiveTriggerName = "provider_evidence_guard"
	pgArchiveKeyPrefix   = "pgarchive-itest"
)

func newPgArchive(t *testing.T) *PgArchive {
	t.Helper()
	requireEnv(t)
	a, err := NewPgArchive(testDB)
	require.NoError(t, err)
	return a
}

// freshArchiveKey returns a key no other test and no earlier run has used. The
// archive is append-only and outlives the process exactly as it does in
// production, so a test that reused a key would be asserting on the object some
// previous run left behind.
func freshArchiveKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s/%s/%s.json", pgArchiveKeyPrefix, t.Name(), uuid.NewString())
}

// rewriteStoredObject performs a write that the whole design exists to prevent,
// and can only do it by stepping outside that design twice: as the table's
// owner (the application role has no UPDATE grant) and with the guard trigger
// disabled. That is the point -- it is the shape of the only actor still able
// to alter archived evidence once this table is in place, and it is what a test
// of "Get refuses altered evidence" has to impersonate to have anything to
// detect.
func rewriteStoredObject(t *testing.T, key, sql string, args ...any) {
	t.Helper()
	conn := migrateConn(t)
	withTriggerDisabled(t, conn, pgArchiveTable, pgArchiveTriggerName, func() {
		tag, err := conn.Exec(context.Background(), sql, args...)
		require.NoError(t, err)
		require.EqualValues(t, 1, tag.RowsAffected(), "expected to alter exactly the object under %s", key)
	})
}

func TestIntegration_PgArchiveReturnsExactlyTheBytesItWasGiven(t *testing.T) {
	a := newPgArchive(t)
	ctx := context.Background()
	key := freshArchiveKey(t)
	// bytea rather than text, and the body is chosen to prove it: archived
	// evidence is whatever the provider sent, which includes byte sequences no
	// text column could hold.
	body := append([]byte(`{"provider":"stripe","event":"charge.succeeded"}`), 0x00, 0xff)

	retention := 24 * time.Hour
	uri, sum, err := a.Put(ctx, key, body, &retention)
	require.NoError(t, err)

	// The digest is the caller's receipt: it is what a verifier compares an
	// object against months later, so Put returning anything other than the
	// hash of what was stored would make every later verification a lie.
	want := sha256.Sum256(body)
	assert.Equal(t, want[:], sum)
	assert.Equal(t, "pg://"+pgArchiveTable+"/"+key, uri)

	got, err := a.Get(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, body, got)

	// byte_len is what an operator sums to ask how much of a 0.5 GB database
	// the archive is using; it is worth nothing if it can drift from the body.
	var byteLen int
	var storedSum []byte
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT byte_len, sha256 FROM provider_evidence WHERE key = $1`, key).Scan(&byteLen, &storedSum))
	assert.Equal(t, len(body), byteLen)
	assert.Equal(t, want[:], storedSum)
}

func TestIntegration_PgArchiveKeepsTheFirstObjectWrittenUnderAKey(t *testing.T) {
	a := newPgArchive(t)
	ctx := context.Background()
	key := freshArchiveKey(t)
	original := []byte(`{"amount":1000,"currency":"usd"}`)
	forgery := []byte(`{"amount":1,"currency":"usd"}`)

	uri, sum, err := a.Put(ctx, key, original, nil)
	require.NoError(t, err)

	_, _, err = a.Put(ctx, key, forgery, nil)
	assert.ErrorIs(t, err, ErrObjectExists, "a live key must not accept a second, different object")

	// The refusal is only worth having if it also left the original alone. A
	// store that reports ErrObjectExists and overwrites anyway would pass an
	// assertion on the error and lose the evidence.
	got, err := a.Get(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, original, got)
	again := sha256.Sum256(got)
	assert.Equal(t, sum, again[:])
}

// A retry that carries the same bytes is refused too, which is worth a test of
// its own because it is the surprising half of the contract and the half a
// later change is most likely to "fix".
//
// The reason is DirArchive. It refuses on O_CREATE|O_EXCL, which never looks at
// the bytes, and prooftest.MemArchive refuses identical bytes as well. Archive
// has one documented contract -- "implementations must refuse to overwrite an
// existing key" -- and an implementation that quietly succeeded where the other
// two fail would make the answer to a retry depend on which archive the
// deployment wired. If this behaviour is ever changed, it must be changed in
// all three at once, and this test is where that argument starts.
func TestIntegration_PgArchiveRefusesEvenAReplayOfIdenticalBytes(t *testing.T) {
	a := newPgArchive(t)
	ctx := context.Background()
	key := freshArchiveKey(t)
	body := []byte(`{"provider_event_id":"evt_1","attempt":1}`)

	uri, _, err := a.Put(ctx, key, body, nil)
	require.NoError(t, err)

	_, _, err = a.Put(ctx, key, append([]byte(nil), body...), nil)
	require.ErrorIs(t, err, ErrObjectExists)
	assert.Contains(t, err.Error(), "identical bytes",
		"the message is what tells an operator a retry apart from a collision")

	got, err := a.Get(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, body, got)

	var rows int
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT count(*) FROM provider_evidence WHERE key = $1`, key).Scan(&rows))
	assert.Equal(t, 1, rows, "the replay must not have stored a second copy")
}

func TestIntegration_PgArchiveDistinguishesAMissingObjectFromTheWrongStore(t *testing.T) {
	a := newPgArchive(t)
	ctx := context.Background()

	missing := "pg://" + pgArchiveTable + "/" + freshArchiveKey(t)
	_, err := a.Get(ctx, missing)
	assert.ErrorIs(t, err, ErrObjectNotFound)

	// These are not missing objects, they are questions asked of the wrong
	// store, and reporting them as ErrObjectNotFound would tell a verifier that
	// evidence had gone missing when nothing of the sort had happened.
	for _, uri := range []string{
		"s3://bucket/audit-checkpoints/1.json",
		"file:///tmp/audit/1.json",
		"pg://some_other_table/audit-checkpoints/1.json",
		"pg://" + pgArchiveTable + "/",
		"",
	} {
		_, err := a.Get(ctx, uri)
		require.Error(t, err, "uri %q", uri)
		assert.NotErrorIs(t, err, ErrObjectNotFound, "uri %q", uri)
	}
}

// The one that matters most on this tier. Postgres cannot promise the bytes are
// unchanged the way S3 Object Lock can, so PgArchive checks on every read; an
// archive that hands back silently-altered evidence is worse than one that
// fails, because the altered evidence is believed.
func TestIntegration_PgArchiveRefusesToHandBackAlteredEvidence(t *testing.T) {
	a := newPgArchive(t)
	ctx := context.Background()
	key := freshArchiveKey(t)
	body := []byte(`{"signature_verified":true,"amount":1000}`)

	uri, _, err := a.Put(ctx, key, body, nil)
	require.NoError(t, err)

	// Same length, one byte different: the byte_len CHECK still holds, so this
	// is the alteration that leaves every other column looking correct.
	altered := append([]byte(nil), body...)
	altered[0] ^= 0xff
	rewriteStoredObject(t, key, `UPDATE provider_evidence SET body = $1 WHERE key = $2`, altered, key)

	got, err := a.Get(ctx, uri)
	require.ErrorIs(t, err, ErrObjectCorrupt)
	assert.Nil(t, got, "altered bytes must not be returned alongside the error either")

	// Corrupting the digest instead of the body is the same failure seen from
	// the other side, and is what an attacker who could rewrite a row would try
	// second, having found that changing the body alone is detected.
	rewriteStoredObject(t, key, `UPDATE provider_evidence SET body = $1, sha256 = $2 WHERE key = $3`,
		body, make([]byte, 32), key)
	_, err = a.Get(ctx, uri)
	require.ErrorIs(t, err, ErrObjectCorrupt)

	// And the refusal is a fact about the row, not a latch: an object put back
	// exactly as it was reads normally again. Otherwise a single bad read would
	// condemn evidence that is intact.
	sum := sha256.Sum256(body)
	rewriteStoredObject(t, key, `UPDATE provider_evidence SET body = $1, sha256 = $2 WHERE key = $3`,
		body, sum[:], key)
	restored, err := a.Get(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, body, restored)
}

// The write-once property of this archive is not a property of the storage
// engine -- Postgres has no Object Lock -- it is a property of the grant in
// 00730 and of the guard trigger. So it has to be observed, as the role the
// application actually connects as, or the claim in PgArchive's doc comment is
// just a comment.
func TestIntegration_TheApplicationRoleCannotRewriteOrDeleteArchivedEvidence(t *testing.T) {
	a := newPgArchive(t)
	ctx := context.Background()
	key := freshArchiveKey(t)
	body := []byte(`{"provider":"stripe","evidence":"raw"}`)

	uri, _, err := a.Put(ctx, key, body, nil)
	require.NoError(t, err)

	// testDB is the cp_app pool: this is the whole blast radius of a defect in
	// this package or of somebody holding the application's credential.
	for _, stmt := range []string{
		`UPDATE provider_evidence SET body = '\x00'::bytea WHERE key = $1`,
		`UPDATE provider_evidence SET sha256 = '\x00'::bytea WHERE key = $1`,
		`UPDATE provider_evidence SET retain_until = NULL WHERE key = $1`,
		`DELETE FROM provider_evidence WHERE key = $1`,
	} {
		_, err := testDB.Exec(ctx, stmt, key)
		require.Error(t, err, "statement was allowed: %s", stmt)
		assert.True(t, db.IsMutationForbidden(err), "%s: %v", stmt, err)
	}

	got, err := a.Get(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

// The guard trigger covers what the grant cannot: roles wider than cp_app, and
// any future grant somebody widens without reading the migration. Retention is
// the part of Object Lock it can emulate -- an object may not be deleted before
// its deadline, and an object with no deadline may not be deleted at all.
func TestIntegration_TheGuardTriggerRefusesTheOwnerToo(t *testing.T) {
	a := newPgArchive(t)
	ctx := context.Background()
	key := freshArchiveKey(t)
	retention := 24 * time.Hour
	_, _, err := a.Put(ctx, key, []byte(`{"under":"retention"}`), &retention)
	require.NoError(t, err)

	conn := migrateConn(t)
	_, err = conn.Exec(ctx, `UPDATE provider_evidence SET body = $1 WHERE key = $2`, []byte("x"), key)
	require.Error(t, err, "the owning role must not be able to rewrite evidence either")
	assert.True(t, db.IsImmutableRow(err), "%v", err)

	_, err = conn.Exec(ctx, `DELETE FROM provider_evidence WHERE key = $1`, key)
	require.Error(t, err, "an object inside its retention window must not be deletable")
	assert.True(t, db.IsImmutableRow(err), "%v", err)

	// No retention means no expiry, which the guard reads as "never", because
	// the alternative reading -- "no deadline, so delete freely" -- turns a
	// missing argument into the destruction of evidence.
	forever := freshArchiveKey(t)
	_, _, err = a.Put(ctx, forever, []byte(`{"retention":null}`), nil)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `DELETE FROM provider_evidence WHERE key = $1`, forever)
	require.Error(t, err)
	assert.True(t, db.IsImmutableRow(err), "%v", err)
}

// Retention is stored as a deadline rather than a duration, and the deadline is
// computed from the database's own now(). A deadline measured by the writer's
// clock is measured by a different clock on every machine in the fleet, which
// is the defect 00729 exists to undo; asserting the interval exactly is only
// possible because both columns come from the same transaction timestamp.
func TestIntegration_PgArchiveStoresTheRetentionDeadlineItWasGiven(t *testing.T) {
	a := newPgArchive(t)
	ctx := context.Background()

	retained := freshArchiveKey(t)
	retention := 24 * time.Hour
	_, _, err := a.Put(ctx, retained, []byte(`{"retained":true}`), &retention)
	require.NoError(t, err)

	var seconds int64
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT EXTRACT(EPOCH FROM (retain_until - created_at))::bigint FROM provider_evidence WHERE key = $1`,
		retained).Scan(&seconds))
	assert.EqualValues(t, retention.Seconds(), seconds)

	var afterNow bool
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT retain_until > now() FROM provider_evidence WHERE key = $1`, retained).Scan(&afterNow))
	assert.True(t, afterNow, "a 24 hour retention that has already expired retains nothing")

	unbounded := freshArchiveKey(t)
	_, _, err = a.Put(ctx, unbounded, []byte(`{"retained":"forever"}`), nil)
	require.NoError(t, err)
	var isNull bool
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT retain_until IS NULL FROM provider_evidence WHERE key = $1`, unbounded).Scan(&isNull))
	assert.True(t, isNull, "nil retention means no expiry, not a zero deadline")

	// A non-positive retention would be a deadline in the past: a row that
	// reads as retained while permitting an immediate delete. Refusing it is
	// the only answer that does not quietly mean its own opposite.
	for _, d := range []time.Duration{0, -time.Second} {
		_, _, err := a.Put(ctx, freshArchiveKey(t), []byte("x"), &d)
		assert.Error(t, err, "retention %s", d)
	}
}

// Round-tripping the URI matters because the URI is the only handle a caller
// keeps: audit_checkpoints.archive_uri stores it, and a verifier months later
// has nothing else to ask the archive with.
func TestIntegration_PgArchiveURIsRoundTripAwkwardKeys(t *testing.T) {
	a := newPgArchive(t)
	ctx := context.Background()

	for _, suffix := range []string{
		"plain.json",
		"a key with spaces.json",
		"question?and#hash.json",
		"percent%20already.json",
		"unicode-éè.json",
	} {
		key := freshArchiveKey(t) + "/" + suffix
		body := []byte(suffix)
		uri, _, err := a.Put(ctx, key, body, nil)
		require.NoError(t, err, "key %q", key)
		got, err := a.Get(ctx, uri)
		require.NoError(t, err, "uri %q", uri)
		assert.Equal(t, body, got, "key %q", key)
	}
}

func TestIntegration_PgArchiveRefusesWhatItCannotStore(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()

	// A nil querier is a composition-root mistake, and the only moment it can
	// be caught is construction: after that it is a nil dereference inside a
	// Put that a caller believed had archived something.
	a, err := NewPgArchive(nil)
	assert.Nil(t, a)
	assert.Error(t, err)

	archive := newPgArchive(t)
	for name, key := range map[string]string{
		"empty":     "",
		"nul byte":  "audit\x00checkpoints/1.json",
		"over long": strings.Repeat("k", maxPgKeyBytes+1),
	} {
		_, _, err := archive.Put(ctx, key, []byte("x"), nil)
		assert.Error(t, err, "key: %s", name)
	}

	// The cap is a bound on how much of a 0.5 GB database one body can take,
	// so it has to be refused before the INSERT rather than after.
	_, _, err = archive.Put(ctx, freshArchiveKey(t), make([]byte, MaxPgObjectBytes+1), nil)
	assert.Error(t, err, "an object over the size cap must not reach the database")

	// An empty object is odd but legitimate, and DirArchive stores it as a
	// zero-byte file. It is called out because a nil []byte reaches Postgres as
	// NULL, which the NOT NULL column would refuse for the wrong reason.
	uri, sum, err := archive.Put(ctx, freshArchiveKey(t), nil, nil)
	require.NoError(t, err)
	empty := sha256.Sum256([]byte{})
	assert.Equal(t, empty[:], sum)
	got, err := archive.Get(ctx, uri)
	require.NoError(t, err)
	assert.Empty(t, got)
}
