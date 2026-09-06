package proof

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

type (
	checkpointKind       struct{}
	checkpointStreamKind struct{}
	verificationRunKind  struct{}
)

// CheckpointID identifies one audit_checkpoints row.
type CheckpointID = id.ID[checkpointKind]

// NewCheckpointID returns a fresh checkpoint id.
func NewCheckpointID() CheckpointID { return id.New[checkpointKind]() }

// ParseCheckpointID parses the canonical form.
func ParseCheckpointID(s string) (CheckpointID, error) { return id.Parse[checkpointKind](s) }

// VerificationRunID identifies one audit_verification_runs row.
type VerificationRunID = id.ID[verificationRunKind]

// NewVerificationRunID returns a fresh run id.
func NewVerificationRunID() VerificationRunID { return id.New[verificationRunKind]() }

// StreamRange is the contiguous [FromSeq, ToSeq] slice of one stream a
// checkpoint covers. LastContentHash is audit_events.content_hash at ToSeq,
// binding the checkpoint to the hash chain itself and not only to the
// Merkle leaves.
type StreamRange struct {
	FromSeq         int64  `json:"from_seq"`
	ToSeq           int64  `json:"to_seq"`
	LastContentHash []byte `json:"last_content_hash"`
}

// Document is the signed projection of a checkpoint: exactly the fields
// whose canonical JSON digest the signature covers. Field names are the
// column names so an external verifier can rebuild it from SQL alone.
type Document struct {
	Seq            int64                  `json:"seq"`
	PrevRoot       []byte                 `json:"prev_root"`
	MerkleRoot     []byte                 `json:"merkle_root"`
	StreamsCovered map[string]StreamRange `json:"streams_covered"`
	LeafCount      int                    `json:"leaf_count"`
	BuildVersion   string                 `json:"build_version"`
}

// Canonical returns audit.CanonicalJSON(d).
func (d Document) Canonical() ([]byte, error) {
	b, err := audit.CanonicalJSON(d)
	if err != nil {
		return nil, fmt.Errorf("proof: canonical checkpoint: %w", err)
	}
	return b, nil
}

// Digest returns sha256(Canonical()): the only thing a Signer ever signs.
func (d Document) Digest() ([]byte, error) {
	b, err := d.Canonical()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// Streams returns the covered stream names in leaf order (bytewise).
func (d Document) Streams() []string {
	out := make([]string, 0, len(d.StreamsCovered))
	for s := range d.StreamsCovered {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Checkpoint is one audit_checkpoints row.
type Checkpoint struct {
	ID               CheckpointID
	PrevCheckpointID *CheckpointID
	Document
	Signature          []byte
	SigningKeyID       string
	SignatureAlgorithm string
	Signer             SignerKind
	ArchiveURI         string
	ArchiveSHA256      []byte
	CreatedAt          time.Time
}

// ArchivedCheckpoint is the object body written to the archive: the signed
// document, its digest and signature, and the row identity. AttestationRef
// is reserved for a future enclave attestation (PART 233) and is nil in V1.
type ArchivedCheckpoint struct {
	ID                 CheckpointID  `json:"id"`
	PrevCheckpointID   *CheckpointID `json:"prev_checkpoint_id"`
	Checkpoint         Document      `json:"checkpoint"`
	Digest             []byte        `json:"digest"`
	Signature          []byte        `json:"signature"`
	SigningKeyID       string        `json:"signing_key_id"`
	SignatureAlgorithm string        `json:"signature_algorithm"`
	Signer             SignerKind    `json:"signer"`
	CreatedAt          time.Time     `json:"created_at"`
	AttestationRef     *string       `json:"attestation_ref"`
}

// Archived builds the archive object for c. digest must be c.Digest().
func (c Checkpoint) Archived(digest []byte) ArchivedCheckpoint {
	return ArchivedCheckpoint{
		ID: c.ID, PrevCheckpointID: c.PrevCheckpointID, Checkpoint: c.Document, Digest: digest,
		Signature: c.Signature, SigningKeyID: c.SigningKeyID, SignatureAlgorithm: c.SignatureAlgorithm,
		Signer: c.Signer, CreatedAt: c.CreatedAt.UTC(),
	}
}

// ObjectKey is the archive key of checkpoint c under prefix:
// <prefix>/<seq, 12 digits>/<checkpoint id>.json. The id makes every attempt
// unique, so a retried run never collides with an orphaned object.
func ObjectKey(prefix string, c Checkpoint) string {
	if prefix == "" {
		prefix = DefaultKeyPrefix
	}
	return fmt.Sprintf("%s/%012d/%s.json", prefix, c.Seq, c.ID)
}

// Checkpointer defaults.
const (
	DefaultKeyPrefix       = "audit-checkpoints"
	DefaultMaxLeaves       = 500_000
	DefaultCandidateMargin = time.Hour

	// checkpointLockID is the pg_advisory_xact_lock key that serializes
	// checkpoint runs. It is far outside the int4 range of hashtext(), which
	// the audit writer uses for stream locks, so the two never collide.
	checkpointLockID int64 = 0x41554449545F4350 // "AUDIT_CP"
)

// CheckpointerOptions tune a Checkpointer. Zero values mean the defaults.
type CheckpointerOptions struct {
	// MaxLeaves caps the events of one checkpoint; a run that hits it reports
	// Truncated so the caller loops immediately instead of waiting.
	MaxLeaves int
	// Retention is passed to Archive.Put (nil: the bucket default).
	Retention *time.Duration
	// BuildVersion is stamped into every checkpoint (config.BuildVersion
	// when empty).
	BuildVersion string
	// KeyPrefix prefixes archive object keys (DefaultKeyPrefix when empty).
	KeyPrefix string
	// CandidateMargin is how far before the previous checkpoint's created_at
	// an incremental run looks for streams with new events (see Run).
	CandidateMargin time.Duration
}

func (o CheckpointerOptions) withDefaults() CheckpointerOptions {
	if o.MaxLeaves <= 0 {
		o.MaxLeaves = DefaultMaxLeaves
	}
	if o.BuildVersion == "" {
		o.BuildVersion = config.BuildVersion
	}
	if o.KeyPrefix == "" {
		o.KeyPrefix = DefaultKeyPrefix
	}
	if o.CandidateMargin <= 0 {
		o.CandidateMargin = DefaultCandidateMargin
	}
	return o
}

// Checkpointer builds, signs, archives and records checkpoints.
type Checkpointer struct {
	db      *db.DB
	signer  Signer
	archive Archive
	clk     clock.Clock
	opts    CheckpointerOptions
}

// NewCheckpointer wires a Checkpointer. Every dependency is required.
func NewCheckpointer(d *db.DB, signer Signer, archive Archive, clk clock.Clock, opts CheckpointerOptions) (*Checkpointer, error) {
	switch {
	case d == nil:
		return nil, errors.New("proof: checkpointer requires a database")
	case signer == nil:
		return nil, errors.New("proof: checkpointer requires a signer")
	case archive == nil:
		return nil, errors.New("proof: checkpointer requires an archive")
	case clk == nil:
		return nil, errors.New("proof: checkpointer requires a clock")
	}
	return &Checkpointer{db: d, signer: signer, archive: archive, clk: clk, opts: opts.withDefaults()}, nil
}

// SkipReason says why a run created no checkpoint.
type SkipReason string

// Skip reasons.
const (
	SkipInProgress  SkipReason = "in_progress"   // another worker holds the checkpoint lock
	SkipNoNewEvents SkipReason = "no_new_events" // every event is already covered
)

// CheckpointResult is the outcome of one run.
type CheckpointResult struct {
	Created    bool
	Skipped    SkipReason
	Truncated  bool
	Checkpoint Checkpoint
}

// Run creates at most one checkpoint over every event not yet covered. It is
// incremental: candidate streams are those with an event recorded after the
// previous checkpoint's created_at minus CandidateMargin (an index range
// scan), and for each candidate every event above the stream's watermark is
// taken, so a stream is never partially skipped. An event whose transaction
// stayed open longer than the margin before the previous run could be missed
// by the candidate scan until RunFull; the worker runs RunFull periodically.
//
// Two concurrent runs never both create a checkpoint: the second either
// finds the transaction-scoped advisory lock taken (SkipInProgress) or,
// after the first commits, finds nothing new (SkipNoNewEvents).
func (c *Checkpointer) Run(ctx context.Context) (CheckpointResult, error) { return c.run(ctx, false) }

// RunFull is Run with a full candidate scan: every stream whose head is
// above its watermark, regardless of recorded_at. It is used for the first
// checkpoint, the one-shot command, and the worker's periodic sweep.
func (c *Checkpointer) RunFull(ctx context.Context) (CheckpointResult, error) {
	return c.run(ctx, true)
}

func (c *Checkpointer) run(ctx context.Context, full bool) (CheckpointResult, error) {
	var res CheckpointResult
	err := c.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		res = CheckpointResult{}
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, checkpointLockID).Scan(&locked); err != nil {
			return errs.Wrap(err, errs.CodeInternal, "proof: acquire checkpoint lock")
		}
		if !locked {
			res.Skipped = SkipInProgress
			return nil
		}
		// created_at is the database's transaction time, the same clock that
		// stamps audit_events.recorded_at, so the incremental candidate scan
		// compares like with like.
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return errs.Wrap(err, errs.CodeInternal, "proof: read database time")
		}
		head, hasHead, err := readHead(ctx, tx)
		if err != nil {
			return err
		}
		var streams []string
		if full || !hasHead {
			streams, err = candidateStreamsFull(ctx, tx)
		} else {
			streams, err = candidateStreamsSince(ctx, tx, head.CreatedAt.Add(-c.opts.CandidateMargin))
		}
		if err != nil {
			return err
		}
		sort.Strings(streams)
		leaves, covered, truncated, err := c.collect(ctx, tx, streams)
		if err != nil {
			return err
		}
		if len(leaves) == 0 {
			res.Skipped = SkipNoNewEvents
			return nil
		}
		doc := Document{Seq: 1, MerkleRoot: NewTree(leaves).Root(), StreamsCovered: covered, LeafCount: len(leaves), BuildVersion: c.opts.BuildVersion}
		cp := Checkpoint{ID: NewCheckpointID(), CreatedAt: now.UTC()}
		if hasHead {
			doc.Seq = head.Seq + 1
			doc.PrevRoot = head.MerkleRoot
			prev := head.ID
			cp.PrevCheckpointID = &prev
		}
		cp.Document = doc
		digest, err := doc.Digest()
		if err != nil {
			return errs.Wrap(err, errs.CodeInternal, "proof: digest checkpoint")
		}
		sig, keyID, alg, err := c.signer.Sign(ctx, digest)
		if err != nil {
			return errs.Wrap(err, errs.CodeInternal, "proof: sign checkpoint")
		}
		if alg != AlgorithmECDSASHA256 || keyID == "" || len(sig) == 0 {
			return errs.New(errs.CodeInternal, "proof: signer returned an unsupported algorithm or an empty key id/signature")
		}
		cp.Signature, cp.SigningKeyID, cp.SignatureAlgorithm, cp.Signer = sig, keyID, alg, c.signer.Kind()
		body, err := audit.CanonicalJSON(cp.Archived(digest))
		if err != nil {
			return errs.Wrap(err, errs.CodeInternal, "proof: encode archive object")
		}
		// Archive first, then the row: a failed insert leaves an orphaned
		// object that no row references, which the verifier never reads.
		uri, sum, err := c.archive.Put(ctx, ObjectKey(c.opts.KeyPrefix, cp), body, c.opts.Retention)
		if err != nil {
			return errs.Wrap(err, errs.CodeProviderUnavailable, "proof: archive checkpoint")
		}
		cp.ArchiveURI, cp.ArchiveSHA256 = uri, sum
		if err := insertCheckpoint(ctx, tx, cp); err != nil {
			return err
		}
		res = CheckpointResult{Created: true, Truncated: truncated, Checkpoint: cp}
		return nil
	})
	return res, err
}

// collect gathers, per candidate stream in order, every event above the
// stream's watermark, up to MaxLeaves leaves in total.
func (c *Checkpointer) collect(ctx context.Context, q db.Querier, streams []string) ([][]byte, map[string]StreamRange, bool, error) {
	covered := map[string]StreamRange{}
	var leaves [][]byte
	truncated := false
	for _, s := range streams {
		remaining := c.opts.MaxLeaves - len(leaves)
		if remaining <= 0 {
			truncated = true
			break
		}
		wm, err := watermark(ctx, q, s)
		if err != nil {
			return nil, nil, false, err
		}
		hashes, broken, err := loadRange(ctx, q, s, wm+1, wm+int64(remaining)+1)
		if err != nil {
			return nil, nil, false, err
		}
		if broken != nil {
			return nil, nil, false, errs.Newf(errs.CodeConflict, "proof: stream %s is not contiguous at seq %d; refusing to checkpoint a broken chain", s, *broken).
				WithField("stream", s).WithField("stream_seq", *broken)
		}
		if len(hashes) > remaining {
			hashes = hashes[:remaining]
			truncated = true
		}
		if len(hashes) == 0 {
			continue
		}
		last := hashes[len(hashes)-1]
		covered[s] = StreamRange{FromSeq: wm + 1, ToSeq: wm + int64(len(hashes)), LastContentHash: last}
		leaves = append(leaves, hashes...)
		if truncated {
			break
		}
	}
	return leaves, covered, truncated, nil
}

type head struct {
	ID         CheckpointID
	Seq        int64
	MerkleRoot []byte
	CreatedAt  time.Time
}

func readHead(ctx context.Context, q db.Querier) (head, bool, error) {
	var h head
	err := q.QueryRow(ctx, `SELECT id, seq, merkle_root, created_at FROM audit_checkpoints ORDER BY seq DESC LIMIT 1`).
		Scan(&h.ID, &h.Seq, &h.MerkleRoot, &h.CreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return head{}, false, nil
	case err != nil:
		return head{}, false, errs.Wrap(err, errs.CodeInternal, "proof: read checkpoint head")
	}
	return h, true, nil
}

// watermark returns the highest checkpointed stream_seq of stream (0 when
// none).
func watermark(ctx context.Context, q db.Querier, stream string) (int64, error) {
	var wm int64
	err := q.QueryRow(ctx, `SELECT coalesce(max(to_seq), 0) FROM audit_checkpoint_streams WHERE stream = $1`, stream).Scan(&wm)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "proof: read stream watermark")
	}
	return wm, nil
}

func candidateStreamsFull(ctx context.Context, q db.Querier) ([]string, error) {
	return scanStrings(q.Query(ctx, `
SELECT e.stream FROM audit_events e
GROUP BY e.stream
HAVING max(e.stream_seq) > coalesce((SELECT max(s.to_seq) FROM audit_checkpoint_streams s WHERE s.stream = e.stream), 0)`))
}

func candidateStreamsSince(ctx context.Context, q db.Querier, since time.Time) ([]string, error) {
	return scanStrings(q.Query(ctx, `SELECT DISTINCT stream FROM audit_events WHERE recorded_at > $1`, since))
}

func scanStrings(rows pgx.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: list candidate streams")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "proof: scan stream")
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: iterate streams")
	}
	return out, nil
}

// loadRange returns content hashes of stream for stream_seq in [from, to]
// (bounded by what exists) in sequence order. If the rows are not
// contiguous from `from`, broken is the first offending sequence number and
// hashes holds the rows before it.
func loadRange(ctx context.Context, q db.Querier, stream string, from, to int64) (hashes [][]byte, broken *int64, err error) {
	rows, err := q.Query(ctx,
		`SELECT stream_seq, content_hash FROM audit_events WHERE stream = $1 AND stream_seq BETWEEN $2 AND $3 ORDER BY stream_seq`,
		stream, from, to)
	if err != nil {
		return nil, nil, errs.Wrap(err, errs.CodeInternal, "proof: read events")
	}
	defer rows.Close()
	expect := from
	for rows.Next() {
		var (
			seq  int64
			hash []byte
		)
		if err := rows.Scan(&seq, &hash); err != nil {
			return nil, nil, errs.Wrap(err, errs.CodeInternal, "proof: scan event")
		}
		if seq != expect || len(hash) != HashSize {
			return hashes, &seq, nil
		}
		hashes = append(hashes, hash)
		expect++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, errs.Wrap(err, errs.CodeInternal, "proof: iterate events")
	}
	return hashes, nil, nil
}

const insertCheckpointSQL = `
INSERT INTO audit_checkpoints (
    id, seq, prev_checkpoint_id, streams_covered, leaf_count, merkle_root, prev_root,
    signature, signing_key_id, signature_algorithm, signer, archive_uri, archive_sha256, build_version, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`

const insertCheckpointStreamsSQL = `
INSERT INTO audit_checkpoint_streams (id, checkpoint_id, stream, from_seq, to_seq, last_content_hash)
SELECT unnest($1::text[])::uuid, $2, unnest($3::text[]), unnest($4::bigint[]), unnest($5::bigint[]), unnest($6::bytea[])`

func insertCheckpoint(ctx context.Context, tx pgx.Tx, cp Checkpoint) error {
	coveredJSON, err := audit.CanonicalJSON(cp.StreamsCovered)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "proof: encode streams_covered")
	}
	var prevID *string
	if cp.PrevCheckpointID != nil {
		s := cp.PrevCheckpointID.String()
		prevID = &s
	}
	var build *string
	if cp.BuildVersion != "" {
		build = &cp.BuildVersion
	}
	if _, err := tx.Exec(ctx, insertCheckpointSQL,
		cp.ID, cp.Seq, prevID, coveredJSON, cp.LeafCount, cp.MerkleRoot, cp.PrevRoot,
		cp.Signature, cp.SigningKeyID, cp.SignatureAlgorithm, string(cp.Signer), cp.ArchiveURI, cp.ArchiveSHA256, build, cp.CreatedAt,
	); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "proof: insert checkpoint")
	}
	streams := cp.Streams()
	ids := make([]string, len(streams))
	froms := make([]int64, len(streams))
	tos := make([]int64, len(streams))
	lasts := make([][]byte, len(streams))
	for i, s := range streams {
		r := cp.StreamsCovered[s]
		ids[i] = id.New[checkpointStreamKind]().String()
		froms[i], tos[i], lasts[i] = r.FromSeq, r.ToSeq, r.LastContentHash
	}
	if _, err := tx.Exec(ctx, insertCheckpointStreamsSQL, ids, cp.ID, streams, froms, tos, lasts); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "proof: insert checkpoint streams")
	}
	return nil
}

const selectCheckpointsSQL = `
SELECT id, seq, prev_checkpoint_id, streams_covered, leaf_count, merkle_root, prev_root,
       signature, signing_key_id, signature_algorithm, signer, archive_uri, archive_sha256, build_version, created_at
FROM audit_checkpoints
WHERE seq > $1
ORDER BY seq
LIMIT $2`

// listCheckpoints returns up to limit checkpoints with seq > afterSeq, in
// order. Rows are fully read before returning so the caller can run further
// queries on the same connection.
func listCheckpoints(ctx context.Context, q db.Querier, afterSeq int64, limit int) ([]Checkpoint, error) {
	rows, err := q.Query(ctx, selectCheckpointsSQL, afterSeq, limit)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: list checkpoints")
	}
	defer rows.Close()
	var out []Checkpoint
	for rows.Next() {
		cp, err := scanCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: iterate checkpoints")
	}
	return out, nil
}

func scanCheckpoint(rows pgx.Rows) (Checkpoint, error) {
	var (
		cp      Checkpoint
		prevID  *CheckpointID
		covered []byte
		signer  string
		build   *string
	)
	if err := rows.Scan(
		&cp.ID, &cp.Seq, &prevID, &covered, &cp.LeafCount, &cp.MerkleRoot, &cp.PrevRoot,
		&cp.Signature, &cp.SigningKeyID, &cp.SignatureAlgorithm, &signer, &cp.ArchiveURI, &cp.ArchiveSHA256, &build, &cp.CreatedAt,
	); err != nil {
		return Checkpoint{}, errs.Wrap(err, errs.CodeInternal, "proof: scan checkpoint")
	}
	cp.PrevCheckpointID = prevID
	cp.Signer = SignerKind(signer)
	if build != nil {
		cp.BuildVersion = *build
	}
	if err := decodeCovered(covered, &cp.StreamsCovered); err != nil {
		return Checkpoint{}, err
	}
	cp.CreatedAt = cp.CreatedAt.UTC()
	return cp, nil
}

// forEachCheckpoint pages through every checkpoint in seq order and calls fn
// until it returns stop or an error.
func forEachCheckpoint(ctx context.Context, q db.Querier, fn func(cp Checkpoint) (stop bool, err error)) error {
	const page = 200
	after := int64(0)
	for {
		batch, err := listCheckpoints(ctx, q, after, page)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, cp := range batch {
			stop, err := fn(cp)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		}
		after = batch[len(batch)-1].Seq
	}
}

func countCheckpoints(ctx context.Context, q db.Querier) (int, error) {
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM audit_checkpoints`).Scan(&n); err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "proof: count checkpoints")
	}
	return n, nil
}
