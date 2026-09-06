package reality

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

// checkpointKind is the phantom kind of checkpoint ids.
type checkpointKind struct{}

// PgCheckpointStore is the Postgres CheckpointStore.
type PgCheckpointStore struct {
	sources PgDataSourceStore
}

var _ CheckpointStore = PgCheckpointStore{}

const checkpointColumns = `c.id, c.data_source_id, d.code, c.stream, c.source_partition, c.consumer, c.last_sequence, c.last_source_offset,
	c.last_source_event_at, c.last_platform_received_at, c.last_raw_object_id, c.events_since_start, c.status, c.connected_at, c.disconnected_at, c.version`

func scanCheckpoint(row pgx.Row) (Checkpoint, error) {
	var c Checkpoint
	var cid id.ID[checkpointKind]
	var dsid id.ID[dataSourceKind]
	var seq *int64
	var offset *string
	var lastSource, lastReceived, connected, disconnected *time.Time
	var rawID *id.ID[rawObjectKind]
	if err := row.Scan(&cid, &dsid, &c.DataSource, &c.Stream, &c.Partition, &c.Consumer, &seq, &offset,
		&lastSource, &lastReceived, &rawID, &c.EventsSinceStart, &c.Status, &connected, &disconnected, &c.Version); err != nil {
		return Checkpoint{}, err
	}
	c.ID, c.DataSourceID = cid.String(), dsid.String()
	c.LastSequence = sequenceFromDB(seq)
	c.LastSourceOffset = deref(offset)
	c.LastSourceEventAt, c.LastPlatformReceivedAt = derefTime(lastSource), derefTime(lastReceived)
	c.ConnectedAt, c.DisconnectedAt = derefTime(connected), derefTime(disconnected)
	if rawID != nil {
		c.LastRawObjectID = rawID.String()
	}
	c.Found = true
	return c, nil
}

// Load implements CheckpointStore. A position that was never persisted is
// returned with Found == false, Version 0 and the data source resolved.
func (s PgCheckpointStore) Load(ctx context.Context, q db.Querier, source, stream, partition, consumer string) (Checkpoint, error) {
	if source == "" || stream == "" || consumer == "" {
		return Checkpoint{}, errs.New(errs.CodeValidationFailed, "reality: source, stream and consumer are required")
	}
	if partition == "" {
		partition = "0"
	}
	ds, err := s.sources.Get(ctx, q, source)
	if err != nil {
		return Checkpoint{}, err
	}
	c, err := scanCheckpoint(q.QueryRow(ctx, `SELECT `+checkpointColumns+` FROM ingest_checkpoints c JOIN data_sources d ON d.id = c.data_source_id
		WHERE c.data_source_id = $1 AND c.stream = $2 AND c.source_partition = $3 AND c.consumer = $4`, ds.ID, stream, partition, consumer))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Checkpoint{DataSourceID: ds.ID, DataSource: source, Stream: stream, Partition: partition, Consumer: consumer, Status: CheckpointStopped}, nil
		}
		return Checkpoint{}, errs.Wrap(err, errs.CodeInternal, "reality: load checkpoint")
	}
	return c, nil
}

// Advance implements CheckpointStore with optimistic concurrency: the row
// is inserted when c.Version == 0 and it does not exist, or updated when its
// stored version equals c.Version. Anything else is CONFLICT (another
// consumer instance advanced the same position).
func (s PgCheckpointStore) Advance(ctx context.Context, tx pgx.Tx, c Checkpoint) (Checkpoint, error) {
	if c.DataSourceID == "" || c.Stream == "" || c.Consumer == "" {
		return Checkpoint{}, errs.New(errs.CodeValidationFailed, "reality: checkpoint needs data source, stream and consumer")
	}
	if c.Partition == "" {
		c.Partition = "0"
	}
	if !oneOf(c.Status, CheckpointActive, CheckpointDisconnected, CheckpointReplaying, CheckpointGap, CheckpointStopped) {
		return Checkpoint{}, errs.New(errs.CodeValidationFailed, "reality: unknown checkpoint status").WithField("status", c.Status)
	}
	if c.EventsSinceStart < 0 {
		return Checkpoint{}, errs.New(errs.CodeValidationFailed, "reality: events_since_start must not be negative")
	}
	seq, err := sequenceToDB(c.LastSequence)
	if err != nil {
		return Checkpoint{}, err
	}
	var rawID *string
	if c.LastRawObjectID != "" {
		rawID = &c.LastRawObjectID
	}
	newID := id.New[checkpointKind]()
	row := tx.QueryRow(ctx, `INSERT INTO ingest_checkpoints (id, data_source_id, stream, source_partition, consumer, last_sequence, last_source_offset,
		last_source_event_at, last_platform_received_at, last_raw_object_id, events_since_start, status, connected_at, disconnected_at, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,1)
		ON CONFLICT (data_source_id, stream, source_partition, consumer) DO UPDATE SET
			last_sequence = EXCLUDED.last_sequence, last_source_offset = EXCLUDED.last_source_offset,
			last_source_event_at = EXCLUDED.last_source_event_at, last_platform_received_at = EXCLUDED.last_platform_received_at,
			last_raw_object_id = EXCLUDED.last_raw_object_id, events_since_start = EXCLUDED.events_since_start, status = EXCLUDED.status,
			connected_at = EXCLUDED.connected_at, disconnected_at = EXCLUDED.disconnected_at, version = ingest_checkpoints.version + 1
		WHERE ingest_checkpoints.version = $15
		RETURNING id, version`,
		newID, c.DataSourceID, c.Stream, c.Partition, c.Consumer, seq, nilIfEmpty(c.LastSourceOffset),
		nilIfZero(c.LastSourceEventAt), nilIfZero(c.LastPlatformReceivedAt), rawID, c.EventsSinceStart, c.Status,
		nilIfZero(c.ConnectedAt), nilIfZero(c.DisconnectedAt), c.Version)
	var cid id.ID[checkpointKind]
	var version int64
	if err := row.Scan(&cid, &version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Checkpoint{}, errs.New(errs.CodeConflict, "reality: checkpoint advanced concurrently").WithField("expected_version", c.Version)
		}
		return Checkpoint{}, errs.Wrap(err, errs.CodeInternal, "reality: advance checkpoint")
	}
	c.ID, c.Version, c.Found = cid.String(), version, true
	return c, nil
}

// Rewind moves a checkpoint backwards for recovery and records a REPLAY gap
// acknowledged by the acting operator or system (POINT_IN_TIME.md §4).
func (s PgCheckpointStore) Rewind(ctx context.Context, tx pgx.Tx, c Checkpoint, toSequence *uint64, toOffset string, actor security.Principal, reason string) (Checkpoint, GapID, error) {
	if err := checkResolver(actor); err != nil {
		return Checkpoint{}, GapID{}, err
	}
	if !c.Found {
		return Checkpoint{}, GapID{}, errs.New(errs.CodeInvalidStateTransition, "reality: cannot rewind a checkpoint that was never advanced")
	}
	now := time.Now().UTC()
	g := Gap{
		DataSourceID: c.DataSourceID, CheckpointID: c.ID, Stream: c.Stream, Partition: c.Partition, Kind: GapKindReplay,
		ExpectedSequence: c.LastSequence, ObservedSequence: toSequence, GapStartSequence: toSequence, GapEndSequence: c.LastSequence,
		GapStartAt: firstNonZero(c.LastPlatformReceivedAt, now), GapEndAt: now, Resolution: ResolutionAcknowledged,
		ResolvedAt: now, ResolvedByActorType: string(actor.ActorType), ResolvedByActorID: actor.SubjectID,
		Detail: map[string]any{"reason": reason, "from_offset": c.LastSourceOffset, "to_offset": toOffset},
	}
	gid, err := s.RecordGap(ctx, tx, g)
	if err != nil {
		return Checkpoint{}, GapID{}, err
	}
	c.LastSequence, c.LastSourceOffset, c.Status = toSequence, toOffset, CheckpointReplaying
	c, err = s.Advance(ctx, tx, c)
	return c, gid, err
}

// RecordGap implements CheckpointStore.
func (s PgCheckpointStore) RecordGap(ctx context.Context, tx pgx.Tx, g Gap) (GapID, error) {
	if g.DataSourceID == "" || g.Stream == "" {
		return GapID{}, errs.New(errs.CodeValidationFailed, "reality: gap needs data source and stream")
	}
	if !oneOf(g.Kind, GapKindGap, GapKindSilence, GapKindReconnect, GapKindReplay, GapKindOrderingAnomaly) {
		return GapID{}, errs.New(errs.CodeValidationFailed, "reality: unknown gap kind").WithField("kind", g.Kind)
	}
	if g.Resolution == "" {
		g.Resolution = ResolutionOpen
	}
	if !oneOf(g.Resolution, ResolutionOpen, ResolutionReplayed, ResolutionUnrecoverable, ResolutionAcknowledged) {
		return GapID{}, errs.New(errs.CodeValidationFailed, "reality: unknown gap resolution").WithField("resolution", g.Resolution)
	}
	if g.GapStartAt.IsZero() {
		return GapID{}, errs.New(errs.CodeValidationFailed, "reality: gap_start_at is required")
	}
	if !g.GapEndAt.IsZero() && g.GapEndAt.Before(g.GapStartAt) {
		return GapID{}, errs.New(errs.CodeValidationFailed, "reality: gap_end_at precedes gap_start_at")
	}
	if g.Partition == "" {
		g.Partition = "0"
	}
	if g.Resolution != ResolutionOpen {
		if g.ResolvedByActorID == "" {
			g.ResolvedByActorType, g.ResolvedByActorID = string(security.ActorSystem), "reality"
		}
		if g.ResolvedAt.IsZero() {
			g.ResolvedAt = firstNonZero(g.GapEndAt, g.GapStartAt)
		}
		if g.ResolvedByActorType != string(security.ActorSystem) && g.ResolvedByActorType != string(security.ActorOperator) {
			return GapID{}, errs.New(errs.CodeForbidden, "reality: only OPERATOR or SYSTEM may resolve gaps")
		}
	}
	detail := g.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return GapID{}, errs.Wrap(err, errs.CodeValidationFailed, "reality: gap detail is not json")
	}
	exp, err := sequenceToDB(g.ExpectedSequence)
	if err != nil {
		return GapID{}, err
	}
	obs, err := sequenceToDB(g.ObservedSequence)
	if err != nil {
		return GapID{}, err
	}
	start, err := sequenceToDB(g.GapStartSequence)
	if err != nil {
		return GapID{}, err
	}
	end, err := sequenceToDB(g.GapEndSequence)
	if err != nil {
		return GapID{}, err
	}
	gid := NewGapID()
	var checkpointID *string
	if g.CheckpointID != "" {
		checkpointID = &g.CheckpointID
	}
	_, err = tx.Exec(ctx, `INSERT INTO stream_gaps (id, data_source_id, checkpoint_id, stream, source_partition, kind, expected_sequence, observed_sequence,
		gap_start_sequence, gap_end_sequence, gap_start_at, gap_end_at, detected_at, resolution, resolved_at, resolved_by_actor_type, resolved_by_actor_id, detail, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,COALESCE($13, now()),$14,$15,$16,$17,$18,$19)`,
		gid, g.DataSourceID, checkpointID, g.Stream, g.Partition, g.Kind, exp, obs, start, end, g.GapStartAt.UTC(), nilIfZero(g.GapEndAt), nilIfZero(g.DetectedAt),
		g.Resolution, nilIfZero(g.ResolvedAt), nilIfEmpty(g.ResolvedByActorType), nilIfEmpty(g.ResolvedByActorID), detailJSON, nilIfEmpty(g.CorrelationID))
	if err != nil {
		if db.IsCheckViolation(err) {
			return GapID{}, errs.Wrap(err, errs.CodeValidationFailed, "reality: gap violates a constraint").WithField("constraint", db.ConstraintName(err))
		}
		return GapID{}, errs.Wrap(err, errs.CodeInternal, "reality: record gap")
	}
	return gid, nil
}

// Resolve implements CheckpointStore. Only OPERATOR and SYSTEM actors may
// resolve, and only an OPEN gap can be resolved.
func (s PgCheckpointStore) Resolve(ctx context.Context, tx pgx.Tx, gid GapID, resolution string, actor security.Principal) error {
	if err := checkResolver(actor); err != nil {
		return err
	}
	if !oneOf(resolution, ResolutionReplayed, ResolutionUnrecoverable, ResolutionAcknowledged) {
		return errs.New(errs.CodeValidationFailed, "reality: resolution must be REPLAYED, UNRECOVERABLE or ACKNOWLEDGED")
	}
	tag, err := tx.Exec(ctx, `UPDATE stream_gaps SET resolution = $2, resolved_at = now(), resolved_by_actor_type = $3, resolved_by_actor_id = $4,
		gap_end_at = COALESCE(gap_end_at, now()) WHERE id = $1 AND resolution = 'OPEN'`, gid, resolution, string(actor.ActorType), actor.SubjectID)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "reality: resolve gap")
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeInvalidStateTransition, "reality: gap is not open").WithField("gap_id", gid.String())
	}
	return nil
}

// CloseGap implements CheckpointStore: it ends an open SILENCE (or any open
// gap) at endAt, acknowledged by the system, once data flows again.
func (s PgCheckpointStore) CloseGap(ctx context.Context, tx pgx.Tx, gid GapID, endAt time.Time) error {
	if endAt.IsZero() {
		return errs.New(errs.CodeValidationFailed, "reality: gap end is required")
	}
	tag, err := tx.Exec(ctx, `UPDATE stream_gaps SET gap_end_at = GREATEST(gap_start_at, $2), resolution = 'ACKNOWLEDGED', resolved_at = $2,
		resolved_by_actor_type = 'SYSTEM', resolved_by_actor_id = 'reality' WHERE id = $1 AND resolution = 'OPEN'`, gid, endAt.UTC())
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "reality: close gap")
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeInvalidStateTransition, "reality: gap is not open").WithField("gap_id", gid.String())
	}
	return nil
}

const gapColumns = `g.id, g.data_source_id, d.code, g.checkpoint_id, g.stream, g.source_partition, g.kind, g.expected_sequence, g.observed_sequence,
	g.gap_start_sequence, g.gap_end_sequence, g.gap_start_at, g.gap_end_at, g.detected_at, g.resolution, g.resolved_at, g.resolved_by_actor_type,
	g.resolved_by_actor_id, g.detail, g.correlation_id`

func scanGap(row pgx.Row) (Gap, error) {
	var g Gap
	var dsid id.ID[dataSourceKind]
	var cpid *id.ID[checkpointKind]
	var exp, obs, start, end *int64
	var endAt, resolvedAt *time.Time
	var actorType, actorID, corr *string
	var detail []byte
	if err := row.Scan(&g.ID, &dsid, &g.DataSource, &cpid, &g.Stream, &g.Partition, &g.Kind, &exp, &obs,
		&start, &end, &g.GapStartAt, &endAt, &g.DetectedAt, &g.Resolution, &resolvedAt, &actorType, &actorID, &detail, &corr); err != nil {
		return Gap{}, err
	}
	g.DataSourceID = dsid.String()
	if cpid != nil {
		g.CheckpointID = cpid.String()
	}
	g.ExpectedSequence, g.ObservedSequence = sequenceFromDB(exp), sequenceFromDB(obs)
	g.GapStartSequence, g.GapEndSequence = sequenceFromDB(start), sequenceFromDB(end)
	g.GapStartAt, g.DetectedAt = g.GapStartAt.UTC(), g.DetectedAt.UTC()
	g.GapEndAt, g.ResolvedAt = derefTime(endAt), derefTime(resolvedAt)
	g.ResolvedByActorType, g.ResolvedByActorID, g.CorrelationID = deref(actorType), deref(actorID), deref(corr)
	if len(detail) > 0 {
		_ = json.Unmarshal(detail, &g.Detail) // jsonb is always valid json
	}
	return g, nil
}

// Get loads a gap.
func (s PgCheckpointStore) Get(ctx context.Context, q db.Querier, gid GapID) (Gap, error) {
	g, err := scanGap(q.QueryRow(ctx, `SELECT `+gapColumns+` FROM stream_gaps g JOIN data_sources d ON d.id = g.data_source_id WHERE g.id = $1`, gid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Gap{}, errs.New(errs.CodeNotFound, "reality: gap not found")
		}
		return Gap{}, errs.Wrap(err, errs.CodeInternal, "reality: load gap")
	}
	return g, nil
}

// OpenGaps implements CheckpointStore: every OPEN or UNRECOVERABLE gap of the
// source whose window overlaps w (an open-ended gap overlaps everything
// after its start). Strategies over such a window must skip with
// MISSING_DEPENDENCY; backtests are labeled DATA_GAP:<id>.
func (s PgCheckpointStore) OpenGaps(ctx context.Context, q db.Querier, source string, w Window) ([]Gap, error) {
	if w.End.IsZero() || w.Start.IsZero() || w.End.Before(w.Start) {
		return nil, errs.New(errs.CodeValidationFailed, "reality: window must have start <= end")
	}
	rows, err := q.Query(ctx, `SELECT `+gapColumns+` FROM stream_gaps g JOIN data_sources d ON d.id = g.data_source_id
		WHERE d.code = $1 AND g.resolution IN ('OPEN','UNRECOVERABLE') AND g.gap_start_at <= $3 AND (g.gap_end_at IS NULL OR g.gap_end_at >= $2)
		ORDER BY g.gap_start_at, g.id`, source, w.Start.UTC(), w.End.UTC())
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "reality: query gaps")
	}
	defer rows.Close()
	var out []Gap
	for rows.Next() {
		g, err := scanGap(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "reality: scan gap")
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "reality: query gaps")
	}
	return out, nil
}

// Continuity is the answer to "may a dependency window pretend continuity".
type Continuity struct {
	OK   bool
	Gaps []Gap
}

// ImpurityReasons renders the DATA_GAP:<id> labels of the blocking gaps
// (sorted, for backtest manifests).
func (c Continuity) ImpurityReasons() []string {
	out := make([]string, 0, len(c.Gaps))
	for _, g := range c.Gaps {
		out = append(out, "DATA_GAP:"+g.ID.String())
	}
	return out
}

// Continuity reports whether source has no blocking gap over w.
func (s PgCheckpointStore) Continuity(ctx context.Context, q db.Querier, source string, w Window) (Continuity, error) {
	gaps, err := s.OpenGaps(ctx, q, source, w)
	if err != nil {
		return Continuity{}, err
	}
	return Continuity{OK: len(gaps) == 0, Gaps: gaps}, nil
}

func checkResolver(actor security.Principal) error {
	switch actor.ActorType {
	case security.ActorOperator, security.ActorSystem:
		if actor.SubjectID == "" {
			return errs.New(errs.CodeValidationFailed, "reality: resolving actor needs a subject id")
		}
		return nil
	}
	return errs.New(errs.CodeForbidden, "reality: only OPERATOR or SYSTEM actors may resolve gaps").WithField("actor_type", string(actor.ActorType))
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}
