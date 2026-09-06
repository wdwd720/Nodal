package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"time"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// DefaultStreamsBatch is the number of distinct streams VerifyAll checks per
// query when the caller passes a non-positive batch size.
const DefaultStreamsBatch = 100

// Report is the outcome of verifying one stream. Events counts the rows
// examined; when OK is false, BrokenAt is the stream_seq of the first row
// whose sequence, prev_hash link or content_hash does not verify and Reason
// says which.
type Report struct {
	Stream   string
	Events   int
	OK       bool
	BrokenAt *int64
	Reason   string
}

// Verifier recomputes hash chains from persisted rows only. It holds no
// state and is safe for concurrent use.
type Verifier struct{}

// NewVerifier returns a Verifier.
func NewVerifier() *Verifier { return &Verifier{} }

const selectStreamSQL = `
SELECT stream, stream_seq, actor_type, actor_id, action, resource_type, resource_id,
       before_hash, after_hash, request_id, correlation_id, policy_version, reason,
       host(source_ip), device, evidence_ref, build_version, payload, occurred_at, content_hash, prev_hash
FROM audit_events
WHERE stream = $1
ORDER BY stream_seq`

// VerifyStream walks the stream in sequence order and checks, for every row,
// that stream_seq is the previous one plus 1 (starting at 1), that prev_hash
// equals the previous row's content_hash (NULL for the first row) and that
// content_hash equals the recomputed hash of the row. It stops at the first
// failure. An empty stream verifies as OK with zero events.
func (v *Verifier) VerifyStream(ctx context.Context, q db.Querier, stream string) (Report, error) {
	rep := Report{Stream: stream}
	rows, err := q.Query(ctx, selectStreamSQL, stream)
	if err != nil {
		return rep, errs.Wrap(err, errs.CodeInternal, "audit: read stream")
	}
	defer rows.Close()

	var (
		expectSeq int64 = 1
		lastHash  []byte
	)
	for rows.Next() {
		var (
			rec         hashRecord
			sourceIP    *string
			payload     []byte
			contentHash []byte
		)
		if err := rows.Scan(
			&rec.Stream, &rec.StreamSeq, &rec.ActorType, &rec.ActorID, &rec.Action, &rec.ResourceType, &rec.ResourceID,
			&rec.BeforeHash, &rec.AfterHash, &rec.RequestID, &rec.CorrelationID, &rec.PolicyVersion, &rec.Reason,
			&sourceIP, &rec.Device, &rec.EvidenceRef, &rec.BuildVersion, &payload, &rec.OccurredAt, &contentHash, &rec.PrevHash,
		); err != nil {
			return rep, errs.Wrap(err, errs.CodeInternal, "audit: scan event")
		}
		rep.Events++
		seq := rec.StreamSeq
		if seq != expectSeq {
			return broken(rep, seq, fmt.Sprintf("sequence gap: expected stream_seq %d, found %d", expectSeq, seq)), nil
		}
		if !bytes.Equal(nilIfEmpty(rec.PrevHash), lastHash) {
			return broken(rep, seq, "prev_hash does not match the previous content_hash"), nil
		}
		if err := rec.normalizeFromRow(sourceIP, payload); err != nil {
			return broken(rep, seq, err.Error()), nil
		}
		sum, err := rec.hash()
		if err != nil {
			return broken(rep, seq, "record cannot be hashed: "+err.Error()), nil
		}
		if !bytes.Equal(sum, contentHash) {
			return broken(rep, seq, "content_hash does not match the recomputed hash"), nil
		}
		lastHash = sum
		expectSeq++
	}
	if err := rows.Err(); err != nil {
		return rep, errs.Wrap(err, errs.CodeInternal, "audit: iterate stream")
	}
	rep.OK = true
	return rep, nil
}

// VerifyAll verifies every stream, reading distinct stream names in batches
// of streamsBatch (DefaultStreamsBatch when non-positive). Reports are in
// stream-name order.
func (v *Verifier) VerifyAll(ctx context.Context, q db.Querier, streamsBatch int) ([]Report, error) {
	if streamsBatch <= 0 {
		streamsBatch = DefaultStreamsBatch
	}
	var reports []Report
	after := ""
	for {
		streams, err := listStreams(ctx, q, after, streamsBatch)
		if err != nil {
			return reports, err
		}
		if len(streams) == 0 {
			return reports, nil
		}
		for _, s := range streams {
			rep, err := v.VerifyStream(ctx, q, s)
			if err != nil {
				return reports, err
			}
			reports = append(reports, rep)
		}
		after = streams[len(streams)-1]
	}
}

func listStreams(ctx context.Context, q db.Querier, after string, limit int) ([]string, error) {
	rows, err := q.Query(ctx,
		`SELECT DISTINCT stream FROM audit_events WHERE stream > $1 ORDER BY stream LIMIT $2`, after, limit)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "audit: list streams")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "audit: scan stream")
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "audit: iterate streams")
	}
	return out, nil
}

// normalizeFromRow applies to a scanned row the same normalization the
// writer applied before hashing: canonical payload text, canonical IP text,
// UTC microsecond time, nil for empty byte strings.
func (r *hashRecord) normalizeFromRow(sourceIP *string, payload []byte) error {
	canonical, err := CanonicalJSON(json.RawMessage(payload))
	if err != nil {
		return fmt.Errorf("stored payload is not canonical JSON: %w", err)
	}
	r.Payload = canonical
	if sourceIP != nil {
		addr, err := netip.ParseAddr(*sourceIP)
		if err != nil {
			return fmt.Errorf("stored source_ip is not an address: %w", err)
		}
		r.SourceIP = optional(addr.String())
	}
	r.OccurredAt = r.OccurredAt.UTC().Truncate(time.Microsecond)
	r.BeforeHash = nilIfEmpty(r.BeforeHash)
	r.AfterHash = nilIfEmpty(r.AfterHash)
	r.PrevHash = nilIfEmpty(r.PrevHash)
	return nil
}

func broken(rep Report, seq int64, reason string) Report {
	rep.OK = false
	rep.BrokenAt = &seq
	rep.Reason = reason
	return rep
}
