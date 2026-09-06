package proof

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// FailureKind classifies the first failure of a verification run.
type FailureKind string

// Failure kinds, in the order the passes run.
const (
	FailStreamChain FailureKind = "stream_chain" // audit_events chain: hash, link or gap
	// FailCheckpointKeyUnknown: the checkpoint names a key id that is not in
	// the trusted key set. Distinct from FailCheckpointSignature on purpose:
	// this is a trust-configuration gap (typically a rotation whose key id
	// was never added), and reporting it as a bad signature would cry
	// tampering over a routine rotation.
	FailCheckpointKeyUnknown FailureKind = "checkpoint_key_unknown"
	// FailCheckpointKeyRevoked: the naming key was deliberately withdrawn.
	FailCheckpointKeyRevoked FailureKind = "checkpoint_key_revoked"
	FailCheckpointSignature  FailureKind = "checkpoint_signature" // signature over the canonical digest
	FailCheckpointChain      FailureKind = "checkpoint_chain"     // seq / prev_checkpoint_id / prev_root
	FailCoverage             FailureKind = "coverage"             // per-stream ranges not contiguous, or index disagrees
	FailCheckpointRoot       FailureKind = "checkpoint_root"      // recomputed root or leaf count differs
	FailArchiveObject        FailureKind = "archive_object"       // re-fetched object hash/content differs
)

// Failure names the first thing that did not verify.
type Failure struct {
	Kind          FailureKind `json:"kind"`
	Stream        string      `json:"stream,omitempty"`
	StreamSeq     *int64      `json:"stream_seq,omitempty"`
	CheckpointSeq *int64      `json:"checkpoint_seq,omitempty"`
	CheckpointID  string      `json:"checkpoint_id,omitempty"`
	SigningKeyID  string      `json:"signing_key_id,omitempty"`
	ArchiveURI    string      `json:"archive_uri,omitempty"`
	Reason        string      `json:"reason"`
}

// String renders the failure for logs and the CLI.
func (f Failure) String() string {
	s := string(f.Kind)
	if f.Stream != "" {
		s += " stream=" + f.Stream
	}
	if f.StreamSeq != nil {
		s += fmt.Sprintf(" stream_seq=%d", *f.StreamSeq)
	}
	if f.CheckpointSeq != nil {
		s += fmt.Sprintf(" checkpoint_seq=%d", *f.CheckpointSeq)
	}
	if f.CheckpointID != "" {
		s += " checkpoint_id=" + f.CheckpointID
	}
	if f.SigningKeyID != "" {
		s += " signing_key_id=" + f.SigningKeyID
	}
	if f.ArchiveURI != "" {
		s += " archive_uri=" + f.ArchiveURI
	}
	return s + ": " + f.Reason
}

// Report is the outcome of Verifier.VerifyAll, also persisted as an
// audit_verification_runs row.
type Report struct {
	RunID              VerificationRunID `json:"run_id"`
	OK                 bool              `json:"ok"`
	StartedAt          time.Time         `json:"started_at"`
	FinishedAt         time.Time         `json:"finished_at"`
	BuildVersion       string            `json:"build_version"`
	Streams            int               `json:"streams"`
	CheckedEvents      int64             `json:"checked_events"`
	CoveredEvents      int64             `json:"covered_events"`
	CheckedCheckpoints int               `json:"checked_checkpoints"`
	FirstFailure       *Failure          `json:"first_failure,omitempty"`
}

// Verifier errors: configuration problems, never verification outcomes.
var (
	ErrNoTrustedKeys = errors.New("proof: checkpoints exist but no trusted signing keys are configured to verify their signatures")
	ErrNoArchive     = errors.New("proof: checkpoints exist but no archive is configured to re-fetch their objects")
)

// VerifierOptions tune a Verifier.
type VerifierOptions struct {
	StreamsBatch int    // audit.Verifier batch size (audit.DefaultStreamsBatch when <= 0)
	BuildVersion string // stamped into the run row (config.BuildVersion when empty)
}

// Verifier re-derives everything from persisted rows and archived objects.
//
// keys is the set of signing keys trusted to have produced checkpoints. Every
// signature is verified with the key the checkpoint row names, resolved out
// of this set: a database whose history spans a key rotation carries
// checkpoints from several keys and verifies completely as long as each key
// id is still trusted. keys and archive may be empty/nil only for databases
// without checkpoints.
type Verifier struct {
	keys    *KeySet
	archive Archive
	clk     clock.Clock
	opts    VerifierOptions
}

// NewVerifier wires a Verifier.
func NewVerifier(keys *KeySet, archive Archive, clk clock.Clock, opts VerifierOptions) (*Verifier, error) {
	if clk == nil {
		return nil, errors.New("proof: verifier requires a clock")
	}
	if opts.BuildVersion == "" {
		opts.BuildVersion = config.BuildVersion
	}
	return &Verifier{keys: keys, archive: archive, clk: clk, opts: opts}, nil
}

// VerifyAll runs every pass, stops at the first failure, records the run and
// returns the report. A non-nil error means the run could not be carried
// out (database or configuration); a report with OK == false means the
// history did not verify.
func (v *Verifier) VerifyAll(ctx context.Context, q db.Querier) (Report, error) {
	rep := Report{RunID: NewVerificationRunID(), StartedAt: v.clk.Now(), BuildVersion: v.opts.BuildVersion}
	fail, err := v.check(ctx, q, &rep)
	if err != nil {
		return rep, err
	}
	rep.FirstFailure = fail
	rep.OK = fail == nil
	rep.FinishedAt = v.clk.Now()
	if rep.FinishedAt.Before(rep.StartedAt) {
		rep.FinishedAt = rep.StartedAt
	}
	if err := recordRun(ctx, q, rep); err != nil {
		return rep, err
	}
	return rep, nil
}

func (v *Verifier) check(ctx context.Context, q db.Querier, rep *Report) (*Failure, error) {
	// Pass 1: every stream's hash chain.
	reports, err := audit.NewVerifier().VerifyAll(ctx, q, v.opts.StreamsBatch)
	if err != nil {
		return nil, err
	}
	for _, r := range reports {
		rep.Streams++
		rep.CheckedEvents += int64(r.Events)
		if !r.OK {
			return &Failure{Kind: FailStreamChain, Stream: r.Stream, StreamSeq: r.BrokenAt, Reason: r.Reason}, nil
		}
	}
	n, err := countCheckpoints(ctx, q)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	if v.keys.Len() == 0 {
		return nil, ErrNoTrustedKeys
	}
	if v.archive == nil {
		return nil, ErrNoArchive
	}
	passes := []func(context.Context, db.Querier, *Report) (*Failure, error){
		v.passSignatures, v.passChain, v.passRoots, v.passArchive,
	}
	for _, pass := range passes {
		fail, err := pass(ctx, q, rep)
		if err != nil || fail != nil {
			return fail, err
		}
	}
	rep.CheckedCheckpoints = n
	return nil, nil
}

func at(cp Checkpoint) Failure {
	seq := cp.Seq
	return Failure{CheckpointSeq: &seq, CheckpointID: cp.ID.String()}
}

// passSignatures verifies every signature over the digest recomputed from
// the row's own columns, before anything else: an altered merkle_root,
// streams_covered, leaf_count, prev_root or build_version fails here.
//
// The verifying key is the one the row names, resolved out of the trusted
// key set, so history that spans a rotation verifies with the key that
// actually signed each checkpoint. An id that is not trusted, and one that
// has been revoked, each get their own failure kind: neither is evidence
// that a signature is forged.
func (v *Verifier) passSignatures(ctx context.Context, q db.Querier, _ *Report) (*Failure, error) {
	var fail *Failure
	err := forEachCheckpoint(ctx, q, func(cp Checkpoint) (bool, error) {
		f := at(cp)
		f.Kind, f.SigningKeyID = FailCheckpointSignature, cp.SigningKeyID
		switch {
		case cp.SignatureAlgorithm != AlgorithmECDSASHA256:
			f.Reason = fmt.Sprintf("unsupported signature_algorithm %q", cp.SignatureAlgorithm)
		case !cp.Signer.Valid():
			f.Reason = fmt.Sprintf("unknown signer %q", cp.Signer)
		default:
			digest, derr := cp.Digest()
			if derr != nil {
				f.Reason = "row cannot be canonicalized: " + derr.Error()
				break
			}
			switch verr := v.keys.Verify(ctx, digest, cp.Signature, cp.SigningKeyID); {
			case verr == nil:
			case errors.Is(verr, ErrKeyUnknown):
				f.Kind = FailCheckpointKeyUnknown
				f.Reason = "the signing key is not trusted, so the signature was never judged (add the key id to the trusted set, or investigate why an unknown key signed): " + verr.Error()
			case errors.Is(verr, ErrKeyRevoked):
				f.Kind = FailCheckpointKeyRevoked
				f.Reason = "the signing key has been revoked: " + verr.Error()
			default:
				f.Reason = "signature does not verify over the canonical digest: " + verr.Error()
			}
		}
		if f.Reason != "" {
			fail = &f
			return true, nil
		}
		return false, nil
	})
	return fail, err
}

// passChain checks seq contiguity from 1 and the prev_checkpoint_id /
// prev_root links.
func (v *Verifier) passChain(ctx context.Context, q db.Querier, _ *Report) (*Failure, error) {
	var (
		fail     *Failure
		expect   int64 = 1
		prevID   CheckpointID
		prevRoot []byte
	)
	err := forEachCheckpoint(ctx, q, func(cp Checkpoint) (bool, error) {
		f := at(cp)
		f.Kind = FailCheckpointChain
		switch {
		case cp.Seq != expect:
			f.Reason = fmt.Sprintf("sequence gap: expected checkpoint seq %d, found %d", expect, cp.Seq)
		case expect == 1 && (cp.PrevCheckpointID != nil || cp.PrevRoot != nil):
			f.Reason = "first checkpoint must not name a predecessor"
		case expect > 1 && (cp.PrevCheckpointID == nil || *cp.PrevCheckpointID != prevID):
			f.Reason = fmt.Sprintf("prev_checkpoint_id does not name checkpoint %d", expect-1)
		case expect > 1 && !bytes.Equal(cp.PrevRoot, prevRoot):
			f.Reason = fmt.Sprintf("prev_root does not equal the merkle_root of checkpoint %d", expect-1)
		case len(cp.MerkleRoot) != HashSize:
			f.Reason = "merkle_root is not a sha256 hash"
		}
		if f.Reason != "" {
			fail = &f
			return true, nil
		}
		prevID, prevRoot = cp.ID, cp.MerkleRoot
		expect++
		return false, nil
	})
	return fail, err
}

// passRoots recomputes every root from audit_events, checks the covered
// ranges are contiguous per stream across checkpoints, that last_content_hash
// matches the chain, and that the coverage index agrees with the signed
// streams_covered.
func (v *Verifier) passRoots(ctx context.Context, q db.Querier, rep *Report) (*Failure, error) {
	var fail *Failure
	watermarks := map[string]int64{}
	err := forEachCheckpoint(ctx, q, func(cp Checkpoint) (bool, error) {
		f, err := v.checkRoot(ctx, q, cp, watermarks, rep)
		if err != nil {
			return false, err
		}
		if f != nil {
			fail = f
			return true, nil
		}
		return false, nil
	})
	return fail, err
}

func (v *Verifier) checkRoot(ctx context.Context, q db.Querier, cp Checkpoint, watermarks map[string]int64, rep *Report) (*Failure, error) {
	base := at(cp)
	failWith := func(kind FailureKind, stream string, seq *int64, reason string) *Failure {
		f := base
		f.Kind, f.Stream, f.StreamSeq, f.Reason = kind, stream, seq, reason
		return &f
	}
	indexed, err := readCoverageIndex(ctx, q, cp.ID)
	if err != nil {
		return nil, err
	}
	if !coverageEqual(indexed, cp.StreamsCovered) {
		return failWith(FailCoverage, "", nil, "audit_checkpoint_streams disagrees with the signed streams_covered"), nil
	}
	if len(cp.StreamsCovered) == 0 {
		return failWith(FailCoverage, "", nil, "checkpoint covers no stream"), nil
	}
	leaves := make([][]byte, 0, cp.LeafCount)
	for _, s := range cp.Streams() {
		r := cp.StreamsCovered[s]
		from := r.FromSeq
		wm := watermarks[s]
		switch {
		case from != wm+1:
			return failWith(FailCoverage, s, &from, fmt.Sprintf("coverage is not contiguous: expected from_seq %d", wm+1)), nil
		case r.ToSeq < r.FromSeq:
			return failWith(FailCoverage, s, &from, "to_seq precedes from_seq"), nil
		case len(r.LastContentHash) != HashSize:
			return failWith(FailCoverage, s, &r.ToSeq, "last_content_hash is not a sha256 hash"), nil
		}
		hashes, broken, err := loadRange(ctx, q, s, r.FromSeq, r.ToSeq)
		if err != nil {
			return nil, err
		}
		if broken != nil {
			return failWith(FailCheckpointRoot, s, broken, "covered range is not contiguous in audit_events"), nil
		}
		if int64(len(hashes)) != r.ToSeq-r.FromSeq+1 {
			missing := r.FromSeq + int64(len(hashes))
			return failWith(FailCheckpointRoot, s, &missing, "covered event is missing from audit_events"), nil
		}
		if !bytes.Equal(hashes[len(hashes)-1], r.LastContentHash) {
			return failWith(FailCheckpointRoot, s, &r.ToSeq, "last_content_hash does not match the event's content_hash"), nil
		}
		leaves = append(leaves, hashes...)
		watermarks[s] = r.ToSeq
	}
	if len(leaves) != cp.LeafCount {
		return failWith(FailCheckpointRoot, "", nil, fmt.Sprintf("leaf_count %d but the covered ranges hold %d events", cp.LeafCount, len(leaves))), nil
	}
	if root := NewTree(leaves).Root(); !bytes.Equal(root, cp.MerkleRoot) {
		return failWith(FailCheckpointRoot, "", nil, "merkle_root does not match the root recomputed from audit_events"), nil
	}
	rep.CoveredEvents += int64(len(leaves))
	return nil, nil
}

// passArchive re-fetches every archived object and compares its sha256 and
// content with the row.
func (v *Verifier) passArchive(ctx context.Context, q db.Querier, _ *Report) (*Failure, error) {
	var fail *Failure
	err := forEachCheckpoint(ctx, q, func(cp Checkpoint) (bool, error) {
		f := at(cp)
		f.Kind, f.ArchiveURI = FailArchiveObject, cp.ArchiveURI
		f.Reason = v.checkObject(ctx, cp)
		if f.Reason != "" {
			fail = &f
			return true, nil
		}
		return false, nil
	})
	return fail, err
}

// checkObject returns the reason the archived object of cp does not verify,
// or "".
func (v *Verifier) checkObject(ctx context.Context, cp Checkpoint) string {
	body, err := v.archive.Get(ctx, cp.ArchiveURI)
	if err != nil {
		return "archived object cannot be fetched: " + err.Error()
	}
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], cp.ArchiveSHA256) {
		return "archived object sha256 does not match archive_sha256"
	}
	var arch ArchivedCheckpoint
	if err := json.Unmarshal(body, &arch); err != nil {
		return "archived object is not a checkpoint document: " + err.Error()
	}
	rowDigest, err := cp.Digest()
	if err != nil {
		return "row cannot be canonicalized: " + err.Error()
	}
	archDigest, err := arch.Checkpoint.Digest()
	if err != nil {
		return "archived document cannot be canonicalized: " + err.Error()
	}
	switch {
	case arch.ID != cp.ID:
		return "archived object names a different checkpoint id"
	case !bytes.Equal(archDigest, rowDigest) || !bytes.Equal(arch.Digest, rowDigest):
		return "archived document digest does not match the row"
	case !bytes.Equal(arch.Signature, cp.Signature):
		return "archived signature does not match the row"
	case arch.SigningKeyID != cp.SigningKeyID || arch.SignatureAlgorithm != cp.SignatureAlgorithm || arch.Signer != cp.Signer:
		return "archived signer metadata does not match the row"
	}
	return ""
}

func readCoverageIndex(ctx context.Context, q db.Querier, cpID CheckpointID) (map[string]StreamRange, error) {
	rows, err := q.Query(ctx, `SELECT stream, from_seq, to_seq, last_content_hash FROM audit_checkpoint_streams WHERE checkpoint_id = $1`, cpID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: read coverage index")
	}
	defer rows.Close()
	out := map[string]StreamRange{}
	for rows.Next() {
		var (
			s string
			r StreamRange
		)
		if err := rows.Scan(&s, &r.FromSeq, &r.ToSeq, &r.LastContentHash); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "proof: scan coverage index")
		}
		out[s] = r
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "proof: iterate coverage index")
	}
	return out, nil
}

func coverageEqual(a, b map[string]StreamRange) bool {
	if len(a) != len(b) {
		return false
	}
	for s, ra := range a {
		rb, ok := b[s]
		if !ok || ra.FromSeq != rb.FromSeq || ra.ToSeq != rb.ToSeq || !bytes.Equal(ra.LastContentHash, rb.LastContentHash) {
			return false
		}
	}
	return true
}

func decodeCovered(raw []byte, into *map[string]StreamRange) error {
	if err := json.Unmarshal(raw, into); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "proof: decode streams_covered")
	}
	if *into == nil {
		*into = map[string]StreamRange{}
	}
	return nil
}

func recordRun(ctx context.Context, q db.Querier, rep Report) error {
	var failure []byte
	if rep.FirstFailure != nil {
		b, err := json.Marshal(rep.FirstFailure)
		if err != nil {
			return errs.Wrap(err, errs.CodeInternal, "proof: encode failure")
		}
		failure = b
	}
	var build *string
	if rep.BuildVersion != "" {
		build = &rep.BuildVersion
	}
	_, err := q.Exec(ctx, `
INSERT INTO audit_verification_runs (id, started_at, finished_at, ok, checked_events, checked_checkpoints, first_failure, build_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		rep.RunID, rep.StartedAt, rep.FinishedAt, rep.OK, rep.CheckedEvents, rep.CheckedCheckpoints, failure, build)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "proof: record verification run")
	}
	return nil
}
