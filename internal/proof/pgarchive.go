package proof

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
)

// ErrObjectCorrupt is returned by PgArchive.Get when the bytes stored under a
// key no longer hash to the digest stored beside them. It is deliberately not
// folded into ErrObjectNotFound: a missing object is an accident, an altered
// one is an incident, and a caller that cannot tell them apart will retry the
// second as if it were the first.
var ErrObjectCorrupt = errors.New("proof: archive object does not match its stored digest")

const (
	// pgArchiveScheme and pgArchiveTable make up a PgArchive URI:
	// pg://provider_evidence/<key>. The table is the host component so a URI
	// carries everything needed to read the object back, the way s3://bucket/key
	// names its bucket.
	pgArchiveScheme = "pg"
	pgArchiveTable  = "provider_evidence"

	// MaxPgObjectBytes is the largest object PgArchive will store. The S3
	// archive allows 64 MiB per object; this one shares a 0.5 GB database with
	// the entire ledger, so it refuses at 8 MiB instead. Every object actually
	// archived today -- a signed checkpoint document, a provider webhook body --
	// is kilobytes, so this cap is not a budget to spend but a bound on how much
	// damage one pathological body can do to a tier that cannot grow.
	MaxPgObjectBytes = 8 << 20

	// maxPgKeyBytes keeps a key inside the btree limit of the unique index on
	// provider_evidence.key. Refusing it here produces an error about the key;
	// letting the index refuse it produces one about the index.
	maxPgKeyBytes = 1024
)

// providerEvidenceKind types the surrogate id of an archived object.
type providerEvidenceKind struct{}

// PgArchive is a PostgreSQL Archive: objects are rows in provider_evidence
// (migration 00730), written create-only, addressed as
// pg://provider_evidence/<key> URIs. Unlike DirArchive it is not a
// LOCAL/TEST/DEV double -- it is the archive the object-storage-free launch
// tier actually runs on, because that tier has no bucket to put an S3 Object
// Lock on.
//
// What it does NOT provide, stated plainly because the interface it satisfies
// was written around a store that does: WORM. PostgreSQL has no storage-level
// retention lock and no way to build one. Nothing here stops a superuser, the
// table owner, or anyone who can reach the data directory from rewriting a row;
// S3 Object Lock in COMPLIANCE mode stops exactly those people.
//
// The write-once property comes from the privilege model instead. 00730 grants
// the application role INSERT and SELECT on provider_evidence and nothing else,
// so an UPDATE or a DELETE issued by anything holding the application's
// credential -- a defect in this package, or somebody who has stolen the
// credential -- is refused by the privilege system before it reaches a row, and
// a guard trigger refuses it again for the roles the grant does not cover. That
// is a real control, and it is a different one: it binds the application rather
// than the storage.
//
// Because the engine cannot promise the bytes are unchanged, Get checks rather
// than assumes. Every read re-hashes the body against the digest stored with it
// and returns ErrObjectCorrupt instead of the bytes when they disagree. An
// archive that hands back silently-altered evidence is worse than one that
// fails, because the altered evidence is believed.
type PgArchive struct {
	q db.Querier
}

var _ Archive = (*PgArchive)(nil)

// NewPgArchive returns an Archive backed by q, which may be a *db.DB or a
// pgx.Tx: Put and Get are each a single statement, so an object written inside
// a caller's transaction is archived exactly when that transaction commits.
func NewPgArchive(q db.Querier) (*PgArchive, error) {
	if q == nil {
		return nil, errors.New("proof: PgArchive requires a querier")
	}
	return &PgArchive{q: q}, nil
}

const pgArchiveInsertSQL = `
INSERT INTO provider_evidence (id, key, body, sha256, byte_len, retain_until)
VALUES ($1, $2, $3, $4, $5, now() + ($6::bigint * INTERVAL '1 microsecond'))
ON CONFLICT (key) DO NOTHING
RETURNING sha256`

const pgArchiveDigestSQL = `SELECT sha256 FROM provider_evidence WHERE key = $1`

const pgArchiveSelectSQL = `SELECT body, sha256 FROM provider_evidence WHERE key = $1`

// Put implements Archive. It is create-only: the first Put of a key stores the
// object, and every later Put of that key is refused -- including one carrying
// byte-for-byte identical bytes. Nothing is ever overwritten.
//
// The two collisions are told apart in the ERROR rather than only in its
// message: identical bytes give ErrObjectExistsIdentical, different bytes give
// ErrObjectExists, and errors.Is matches ErrObjectExists for both. All three
// implementations of the interface answer a retry the same way, which is what
// the earlier version of this comment was protecting; what changed is that a
// caller can now act on the distinction rather than read it in a log. Both
// refusals also return the URI and digest of the object already stored.
//
// That earlier comment claimed no caller in the tree was inconvenienced by
// refusing identical bytes. It was wrong, and expensively: the webhook
// evidence pipeline archives the raw delivery BEFORE the inbox deduplicates
// it, and its key is the event id plus the payload hash -- so every retry a
// provider makes lands on the same key with the same bytes. Every one became a
// 503, which is itself a request to retry, so a provider doing exactly what
// its delivery contract says would be told to try again forever.
//
// retention is honoured as a retain_until timestamp computed from the
// database's own now(), not from this process's clock: a deadline measured by
// the writer's clock is measured by a different clock on every machine in the
// fleet, which is the mistake 00729 exists to undo. A nil retention means no
// expiry, and the guard trigger reads that as "never delete".
func (a *PgArchive) Put(ctx context.Context, key string, body []byte, retention *time.Duration) (string, []byte, error) {
	if err := validatePgKey(key); err != nil {
		return "", nil, err
	}
	if len(body) > MaxPgObjectBytes {
		return "", nil, fmt.Errorf("proof: archive put: %s is %d bytes, over the %d byte limit", key, len(body), MaxPgObjectBytes)
	}
	if body == nil {
		// pgx encodes a nil []byte as SQL NULL and body is NOT NULL, so this
		// would fail as a constraint violation rather than storing what the
		// caller asked for. An empty object is odd but legitimate, and
		// DirArchive stores it as a zero-byte file rather than refusing.
		body = []byte{}
	}
	micros, err := retentionMicros(retention)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(body)

	var stored []byte
	err = a.q.QueryRow(ctx, pgArchiveInsertSQL, id.New[providerEvidenceKind](), key, body, sum[:], len(body), micros).Scan(&stored)
	if err == nil {
		// The digest returned is the one the database now holds rather than the
		// one this process computed: it is what a later Get is checked against.
		return pgObjectURI(key), stored, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", nil, fmt.Errorf("proof: archive put: %w", err)
	}

	// ON CONFLICT DO NOTHING returned no row, so the key is taken. Read the
	// stored digest in a second statement, which takes a fresh snapshot and so
	// sees a row committed by a concurrent writer that the INSERT's snapshot did
	// not, and say which kind of collision this is.
	var existing []byte
	if err := a.q.QueryRow(ctx, pgArchiveDigestSQL, key).Scan(&existing); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The unique key refused the INSERT and a moment later no row is
			// there. Nothing in this schema deletes an archived object, so this
			// means something with more privilege than the application is
			// operating on the archive while it is being written to.
			return "", nil, fmt.Errorf("proof: archive put: %s was refused as an existing key but no object is stored under it", key)
		}
		return "", nil, fmt.Errorf("proof: archive put: %w", err)
	}
	if bytes.Equal(existing, sum[:]) {
		// The URI and digest of what is already stored come back with the
		// refusal. A caller for whom a replay is the provider delivery
		// contract rather than an anomaly can use them; one that treats every
		// ErrObjectExists as fatal is unchanged.
		return pgObjectURI(key), existing, fmt.Errorf("%w: %s", ErrObjectExistsIdentical, key)
	}
	return pgObjectURI(key), existing, fmt.Errorf("%w: %s (a different object is already stored under this key)", ErrObjectExists, key)
}

// Get implements Archive. It returns ErrObjectNotFound for a key nothing was
// ever stored under, and ErrObjectCorrupt -- never the bytes -- when what is
// stored no longer hashes to the digest stored with it. The engine cannot
// promise the row is unchanged (see the type comment), so this is where that
// promise is actually kept.
func (a *PgArchive) Get(ctx context.Context, uri string) ([]byte, error) {
	key, err := pgObjectKey(uri)
	if err != nil {
		return nil, err
	}
	var body, stored []byte
	if err := a.q.QueryRow(ctx, pgArchiveSelectSQL, key).Scan(&body, &stored); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, uri)
		}
		return nil, fmt.Errorf("proof: archive get: %w", err)
	}
	if body == nil {
		body = []byte{}
	}
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], stored) {
		return nil, fmt.Errorf("%w: %s: stored digest %x, but the %d stored bytes hash to %x", ErrObjectCorrupt, uri, stored, len(body), sum[:])
	}
	return body, nil
}

// retentionMicros renders a retention as the microseconds the INSERT adds to
// the database's now(). A non-positive duration is refused rather than stored:
// it would write a deadline that has already passed, which reads as "retained"
// while permitting an immediate delete, and a control that means the opposite
// of what it says is worse than no control at all.
func retentionMicros(retention *time.Duration) (*int64, error) {
	if retention == nil {
		return nil, nil
	}
	if *retention <= 0 {
		return nil, fmt.Errorf("proof: archive put: retention must be positive, got %s", *retention)
	}
	micros := retention.Microseconds()
	return &micros, nil
}

// pgObjectURI renders the URI of key. url.URL does the escaping, so a key
// containing a character that means something in a URI ('?', '#', a space)
// round-trips through pgObjectKey unchanged.
func pgObjectURI(key string) string {
	u := url.URL{Scheme: pgArchiveScheme, Host: pgArchiveTable, Path: "/" + key}
	return u.String()
}

// pgObjectKey is the inverse, and is strict on purpose: a URI naming another
// scheme or another table is a caller reading the wrong archive, which must not
// be reported as ErrObjectNotFound -- "the object is not there" and "you asked
// the wrong store" are different answers, and only one of them means evidence
// has gone missing.
func pgObjectKey(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != pgArchiveScheme {
		return "", fmt.Errorf("proof: archive get: not a %s:// URI: %q", pgArchiveScheme, uri)
	}
	if u.Host != pgArchiveTable {
		return "", fmt.Errorf("proof: archive get: %q does not address the %s table", uri, pgArchiveTable)
	}
	key := strings.TrimPrefix(u.Path, "/")
	if err := validatePgKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// validatePgKey rejects the keys that cannot be stored or cannot round-trip: an
// empty one names no object, a NUL byte is not storable in a text column at
// all, and an over-long key would be refused by the unique index instead.
func validatePgKey(key string) error {
	switch {
	case key == "":
		return errors.New("proof: empty archive key")
	case len(key) > maxPgKeyBytes:
		return fmt.Errorf("proof: archive key is %d bytes, over the %d byte limit", len(key), maxPgKeyBytes)
	case !utf8.ValidString(key):
		return errors.New("proof: archive key is not valid utf-8")
	case strings.ContainsRune(key, 0):
		return errors.New("proof: archive key contains a NUL byte")
	}
	return nil
}
