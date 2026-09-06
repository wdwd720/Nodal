package audit

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
)

// Writer appends events to the hash-chained log inside the caller's
// transaction, so an audit row commits with (and only with) the state change
// it describes.
type Writer interface {
	Append(ctx context.Context, tx pgx.Tx, e Event) (Appended, error)
}

// PGWriter is the PostgreSQL Writer. It holds no connection.
type PGWriter struct {
	buildVersion string
}

var _ Writer = (*PGWriter)(nil)

// NewWriter returns a writer that stamps every row with config.BuildVersion
// (set at link time; "dev" otherwise).
func NewWriter() *PGWriter {
	return &PGWriter{buildVersion: config.BuildVersion}
}

// NewWriterWithBuildVersion is NewWriter with an explicit build version, for
// tests and tooling that must reproduce a specific build's hashes.
func NewWriterWithBuildVersion(buildVersion string) *PGWriter {
	return &PGWriter{buildVersion: buildVersion}
}

const insertEventSQL = `
INSERT INTO audit_events (
    id, stream, stream_seq, actor_type, actor_id, action, resource_type, resource_id,
    before_hash, after_hash, request_id, correlation_id, policy_version, reason,
    source_ip, device, evidence_ref, build_version, payload, occurred_at, content_hash, prev_hash
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    $9, $10, $11, $12, $13, $14,
    $15::inet, $16, $17, $18, $19, $20, $21, $22
)`

// Append validates e, serializes writers of e.Stream with
// pg_advisory_xact_lock(hashtext(stream)), links the new row to the last one
// of the stream and inserts it. The advisory lock is released when tx ends,
// so the chain order equals commit order.
func (w *PGWriter) Append(ctx context.Context, tx pgx.Tx, e Event) (Appended, error) {
	if tx == nil {
		return Appended{}, errs.New(errs.CodeInternal, "audit: Append requires a transaction")
	}
	rec, err := e.record(w.buildVersion)
	if err != nil {
		return Appended{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, rec.Stream); err != nil {
		return Appended{}, errs.Wrap(err, errs.CodeInternal, "audit: lock stream")
	}
	var (
		lastSeq  int64
		lastHash []byte
	)
	err = tx.QueryRow(ctx,
		`SELECT stream_seq, content_hash FROM audit_events WHERE stream = $1 ORDER BY stream_seq DESC LIMIT 1`,
		rec.Stream).Scan(&lastSeq, &lastHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		lastSeq, lastHash = 0, nil
	case err != nil:
		return Appended{}, errs.Wrap(err, errs.CodeInternal, "audit: read stream head")
	}
	rec.StreamSeq = lastSeq + 1
	rec.PrevHash = nilIfEmpty(lastHash)
	sum, err := rec.hash()
	if err != nil {
		return Appended{}, errs.Wrap(err, errs.CodeInternal, "audit: hash record")
	}
	eid := NewEventID()
	_, err = tx.Exec(ctx, insertEventSQL,
		eid, rec.Stream, rec.StreamSeq, rec.ActorType, rec.ActorID, rec.Action, rec.ResourceType, rec.ResourceID,
		rec.BeforeHash, rec.AfterHash, rec.RequestID, rec.CorrelationID, rec.PolicyVersion, rec.Reason,
		rec.SourceIP, rec.Device, rec.EvidenceRef, rec.BuildVersion, []byte(rec.Payload), rec.OccurredAt, sum, rec.PrevHash,
	)
	if err != nil {
		return Appended{}, errs.Wrap(err, errs.CodeInternal, "audit: insert event")
	}
	return Appended{ID: eid, Stream: rec.Stream, StreamSeq: rec.StreamSeq, ContentHash: sum, PrevHash: rec.PrevHash}, nil
}
