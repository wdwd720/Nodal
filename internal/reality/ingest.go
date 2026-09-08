package reality

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
)

// Bus header keys specific to normalized events (the event.Header* keys
// are reused where they apply).
const (
	HeaderDedupID             = "dedup_id"
	HeaderDecisionAvailableAt = "decision_available_at"
	HeaderRawObjectID         = "raw_object_id"
)

// SignalKind classifies what a RawSource emits.
type SignalKind string

// Signal kinds.
const (
	SignalRaw       SignalKind = "RAW"
	SignalReconnect SignalKind = "RECONNECT"
)

// Signal is one item from a RawSource: a raw payload to ingest or a
// reconnect marker for a partition.
type Signal struct {
	Kind      SignalKind
	Raw       RawObject
	Partition string
	At        time.Time
	Detail    string
}

// RawSource feeds the pipeline. Stream returns when ctx is done or emit
// returns an error (which it returns).
type RawSource interface {
	Stream(ctx context.Context, emit func(context.Context, Signal) error) error
}

// Publisher is the part of event.Bus the pipeline uses.
type Publisher interface {
	Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error
}

// PipelineConfig names the stream the pipeline serves.
type PipelineConfig struct {
	DataSource string
	Stream     string
	Consumer   string
	Topic      string
	Detector   DetectorOptions
	// SilenceCheckInterval is how often SILENCE is evaluated (default 5 s).
	SilenceCheckInterval time.Duration
	// RetryAttempts bounds retries of a failed ingest step before the
	// pipeline stops (default 5; the checkpoint makes the restart resume).
	RetryAttempts int
	RetryBackoff  time.Duration
}

// PipelineDeps are the injected collaborators.
type PipelineDeps struct {
	DB          *db.DB
	Archive     MetaArchive
	Normalizer  Normalizer
	Bus         Publisher
	Sink        EventSink
	Checkpoints PgCheckpointStore
	Sources     PgDataSourceStore
	Source      RawSource
	Clock       clock.Clock
	Logger      *slog.Logger
}

// Stats is a snapshot of pipeline counters for health endpoints.
type Stats struct {
	Ingested     int64
	Duplicates   int64
	Published    int64
	Findings     int64
	Errors       int64
	LastIngestAt time.Time
	LastError    string
	Started      bool
}

// IngestResult describes one Ingest call.
type IngestResult struct {
	ObjectID  string
	Duplicate bool
	Events    []NormalizedEvent
	Findings  []Finding
	GapIDs    []GapID
}

type partitionState struct {
	silenceGap  GapID
	silenceOpen bool
}

// Pipeline is the Ingestor: archive → normalize → publish → sink →
// checkpoint, at-least-once. Every step is idempotent downstream (archive
// dedup key, bus dedup id, ReplacingMergeTree, checkpoint version), so a
// crash between steps replays safely.
type Pipeline struct {
	cfg  PipelineConfig
	deps PipelineDeps
	log  *slog.Logger
	clk  clock.Clock
	det  Detector

	mu         sync.Mutex
	ds         DataSource
	started    bool
	partitions map[string]*partitionState
	stats      Stats
}

var _ Ingestor = (*Pipeline)(nil)

// NewPipeline validates configuration and dependencies.
func NewPipeline(cfg PipelineConfig, deps PipelineDeps) (*Pipeline, error) {
	switch {
	case cfg.DataSource == "" || cfg.Stream == "" || cfg.Consumer == "" || cfg.Topic == "":
		return nil, errors.New("reality: pipeline needs data source, stream, consumer and topic")
	case deps.DB == nil || deps.Archive == nil || deps.Normalizer == nil || deps.Bus == nil || deps.Sink == nil:
		return nil, errors.New("reality: pipeline needs db, archive, normalizer, bus and sink")
	}
	if cfg.SilenceCheckInterval <= 0 {
		cfg.SilenceCheckInterval = 5 * time.Second
	}
	if cfg.RetryAttempts <= 0 {
		cfg.RetryAttempts = 5
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 250 * time.Millisecond
	}
	if cfg.Detector.OrderingTolerance < 0 {
		return nil, errors.New("reality: ordering tolerance must not be negative")
	}
	clk := deps.Clock
	if clk == nil {
		clk = clock.System()
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Pipeline{
		cfg: cfg, deps: deps, clk: clk, partitions: map[string]*partitionState{},
		log: log.With(slog.String("data_source", cfg.DataSource), slog.String("stream", cfg.Stream), slog.String("consumer", cfg.Consumer)),
	}, nil
}

// Start loads the data source and builds the detector. It refuses a
// DISABLED source.
func (p *Pipeline) Start(ctx context.Context) error {
	ds, err := p.deps.Sources.Get(ctx, p.deps.DB, p.cfg.DataSource)
	if err != nil {
		return err
	}
	if ds.Status == SourceDisabled {
		return errs.New(errs.CodeUnsupported, "reality: data source is disabled").WithField("code", ds.Code)
	}
	opts := p.cfg.Detector
	if opts.HeartbeatTimeout <= 0 {
		opts.HeartbeatTimeout = ds.HeartbeatTimeout
	}
	det, err := NewDetector(opts)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ds, p.det, p.started = ds, det, true
	p.stats.Started = true
	return nil
}

// DataSource returns the loaded registration (zero before Start).
func (p *Pipeline) DataSource() DataSource {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ds
}

// Stats returns a snapshot of the counters.
func (p *Pipeline) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

func (p *Pipeline) requireStarted() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started {
		return errs.New(errs.CodeInvalidStateTransition, "reality: pipeline not started")
	}
	return nil
}

// Run implements Ingestor: it starts the pipeline, drives the source and
// evaluates silence periodically until ctx is done or the source fails.
func (p *Pipeline) Run(ctx context.Context) error {
	if p.deps.Source == nil {
		return errors.New("reality: pipeline has no raw source")
	}
	if err := p.Start(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.deps.Source.Stream(ctx, p.handle) }()
	ticker := time.NewTicker(p.cfg.SilenceCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return ctx.Err()
			}
			return err
		case <-ctx.Done():
			err := <-done
			if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return ctx.Err()
			}
			return err
		case <-ticker.C:
			if err := p.CheckSilence(ctx, p.clk.Now()); err != nil && ctx.Err() == nil {
				p.log.WarnContext(ctx, "silence check failed", slog.String("code", string(errs.CodeOf(err))))
			}
		}
	}
}

// handle processes one signal with bounded retries.
func (p *Pipeline) handle(ctx context.Context, sig Signal) error {
	var err error
	for attempt := 1; attempt <= p.cfg.RetryAttempts; attempt++ {
		switch sig.Kind {
		case SignalRaw:
			_, err = p.Ingest(ctx, sig.Raw)
		case SignalReconnect:
			err = p.Reconnect(ctx, sig.Partition, sig.At, sig.Detail)
		default:
			return errs.New(errs.CodeValidationFailed, "reality: unknown signal kind").WithField("kind", string(sig.Kind))
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		code := errs.CodeOf(err)
		if code == errs.CodeValidationFailed {
			// A malformed payload never succeeds on retry; it is archived
			// (when archivable) and skipped, never silently dropped.
			p.recordError(err)
			p.log.ErrorContext(ctx, "raw object rejected", slog.String("code", string(code)), slog.String("error", err.Error()))
			return nil
		}
		p.recordError(err)
		p.log.WarnContext(ctx, "ingest step failed; retrying", slog.Int("attempt", attempt), slog.String("code", string(code)))
		d := p.cfg.RetryBackoff << (attempt - 1)
		if d > 5*time.Second {
			d = 5 * time.Second
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return errs.Wrap(err, errs.CodeOf(err), "reality: ingest failed after retries")
}

func (p *Pipeline) recordError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stats.Errors++
	p.stats.LastError = string(errs.CodeOf(err))
}

// Ingest processes one raw object end to end.
func (p *Pipeline) Ingest(ctx context.Context, raw RawObject) (IngestResult, error) {
	if err := p.requireStarted(); err != nil {
		return IngestResult{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if raw.DataSource == "" {
		raw.DataSource = p.cfg.DataSource
	}
	if raw.DataSource != p.cfg.DataSource {
		return IngestResult{}, errs.New(errs.CodeValidationFailed, "reality: raw object belongs to another data source").WithField("data_source", raw.DataSource)
	}
	if raw.Stream == "" {
		raw.Stream = p.cfg.Stream
	}
	if raw.Partition == "" {
		raw.Partition = "0"
	}

	// 1. Archive before interpretation.
	var ref ArchiveRef
	var meta RawObjectMeta
	if err := p.deps.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var perr error
		ref, meta, perr = p.deps.Archive.PutMeta(ctx, tx, raw)
		return perr
	}); err != nil {
		return IngestResult{}, err
	}
	res := IngestResult{ObjectID: ref.ObjectID, Duplicate: ref.Duplicate}

	// 2. Normalize (pure), from the ARCHIVED receipt time rather than the wall
	//    clock (F-62).
	//
	//    The pipeline is at-least-once, and its own doc comment above claims
	//    every step is idempotent downstream because ClickHouse's
	//    ReplacingMergeTree collapses duplicates. It could not, for a reason
	//    that only shows up on a replay: PutMeta returns the ORIGINAL meta for
	//    an event already archived, so platform_received_at -- which is the
	//    engine's VERSION column -- is frozen, while a fresh clock read made
	//    normalized_at, and therefore decision_available_at, strictly later.
	//    The version did not move and the replaced column did, so two rows with
	//    the same sort key carried different values and which survived was
	//    decided at merge time rather than by the data. Worse, the table
	//    partitions on decision_available_at, so a replay landing in a
	//    different month produced two rows FINAL cannot collapse at all.
	//
	//    Deriving it from the archived receipt makes every pass produce
	//    byte-identical timestamps, which is what makes the replacement
	//    well-defined. It is also the more honest reading of the policy:
	//    AvailabilityPolicy already adds FeatureLatency and PipelineLatency as
	//    configured allowances for processing time, so a wall-clock
	//    normalized_at was charging real latency on top of the modelled
	//    latency, inconsistently and only on the first pass.
	events, err := p.deps.Normalizer.Normalize(meta, raw.Body, meta.Timestamps.PlatformReceivedAt)
	if err != nil {
		return res, err
	}

	// 3. Detect discontinuities against the checkpoint.
	cp, err := p.deps.Checkpoints.Load(ctx, p.deps.DB, p.cfg.DataSource, raw.Stream, raw.Partition, p.cfg.Consumer)
	if err != nil {
		return res, err
	}
	obs := Observation{Kind: ObservationEvent, Sequence: raw.Sequence, SourceEventAt: meta.Timestamps.SourceEventAt, PlatformReceivedAt: meta.Timestamps.PlatformReceivedAt, RawObjectID: meta.ObjectID}
	if obs.Sequence == nil && len(events) > 0 {
		obs.Sequence = events[0].Sequence
	}
	findings := p.det.Inspect(cp, obs)
	for _, f := range findings {
		if f.FlagEvent {
			for i := range events {
				events[i].Flags = append(events[i].Flags, f.Kind)
			}
		}
	}
	res.Events, res.Findings = events, findings

	// 4. Publish and sink (both idempotent on dedup id).
	for _, e := range events {
		value, err := e.Encode()
		if err != nil {
			return res, err
		}
		if err := p.deps.Bus.Publish(ctx, p.cfg.Topic, e.DedupID, value, busHeaders(e)); err != nil {
			return res, errs.Wrap(err, errs.CodeProviderUnavailable, "reality: publish normalized event")
		}
	}
	if err := p.deps.Sink.InsertNormalized(ctx, events); err != nil {
		return res, err
	}

	// 5. Record findings, close silence, advance the checkpoint.
	state := p.partition(raw.Partition)
	openGap := false
	if err := p.deps.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		res.GapIDs = res.GapIDs[:0]
		for _, f := range findings {
			gid, err := p.deps.Checkpoints.RecordGap(ctx, tx, p.gapFromFinding(f, cp, meta))
			if err != nil {
				return err
			}
			res.GapIDs = append(res.GapIDs, gid)
			if f.Kind == GapKindGap && f.Resolution == ResolutionOpen {
				openGap = true
			}
		}
		if state.silenceOpen {
			if err := p.deps.Checkpoints.CloseGap(ctx, tx, state.silenceGap, meta.Timestamps.PlatformReceivedAt); err != nil && errs.CodeOf(err) != errs.CodeInvalidStateTransition {
				return err
			}
		}
		next := cp
		if obs.Sequence != nil {
			next.LastSequence = obs.Sequence
		}
		if raw.Offset != "" {
			next.LastSourceOffset = raw.Offset
		}
		if !meta.Timestamps.SourceEventAt.IsZero() {
			next.LastSourceEventAt = meta.Timestamps.SourceEventAt
		}
		next.LastPlatformReceivedAt = meta.Timestamps.PlatformReceivedAt
		next.LastRawObjectID = meta.ObjectID
		next.EventsSinceStart++
		next.Status = CheckpointActive
		if openGap {
			next.Status = CheckpointGap
		}
		if next.ConnectedAt.IsZero() {
			next.ConnectedAt = meta.Timestamps.PlatformReceivedAt
		}
		_, err := p.deps.Checkpoints.Advance(ctx, tx, next)
		return err
	}); err != nil {
		return res, err
	}
	state.silenceOpen = false
	p.stats.Ingested++
	if ref.Duplicate {
		p.stats.Duplicates++
	}
	p.stats.Published += int64(len(events))
	p.stats.Findings += int64(len(findings))
	p.stats.LastIngestAt = p.clk.Now()
	return res, nil
}

// Reconnect records a (re)connection of a partition's stream.
func (p *Pipeline) Reconnect(ctx context.Context, partition string, at time.Time, detail string) error {
	if err := p.requireStarted(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if partition == "" {
		partition = "0"
	}
	if at.IsZero() {
		at = p.clk.Now()
	}
	cp, err := p.deps.Checkpoints.Load(ctx, p.deps.DB, p.cfg.DataSource, p.cfg.Stream, partition, p.cfg.Consumer)
	if err != nil {
		return err
	}
	findings := p.det.Inspect(cp, Observation{Kind: ObservationReconnect, PlatformReceivedAt: at.UTC(), Detail: detail})
	state := p.partition(partition)
	err = p.deps.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		for _, f := range findings {
			if _, err := p.deps.Checkpoints.RecordGap(ctx, tx, p.gapFromFinding(f, cp, RawObjectMeta{})); err != nil {
				return err
			}
		}
		if state.silenceOpen {
			if err := p.deps.Checkpoints.CloseGap(ctx, tx, state.silenceGap, at); err != nil && errs.CodeOf(err) != errs.CodeInvalidStateTransition {
				return err
			}
		}
		next := cp
		next.Status = CheckpointActive
		next.ConnectedAt = at.UTC()
		_, err := p.deps.Checkpoints.Advance(ctx, tx, next)
		return err
	})
	if err != nil {
		return err
	}
	state.silenceOpen = false
	p.stats.Findings += int64(len(findings))
	return nil
}

// CheckSilence records a SILENCE gap for every known partition whose stream
// is connected but silent beyond the heartbeat timeout.
func (p *Pipeline) CheckSilence(ctx context.Context, now time.Time) error {
	if err := p.requireStarted(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var firstErr error
	for partition, state := range p.partitions {
		if state.silenceOpen {
			continue
		}
		cp, err := p.deps.Checkpoints.Load(ctx, p.deps.DB, p.cfg.DataSource, p.cfg.Stream, partition, p.cfg.Consumer)
		if err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		f, ok := p.det.Silence(cp, now)
		if !ok {
			continue
		}
		err = p.deps.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			gid, err := p.deps.Checkpoints.RecordGap(ctx, tx, p.gapFromFinding(f, cp, RawObjectMeta{}))
			if err != nil {
				return err
			}
			state.silenceGap, state.silenceOpen = gid, true
			return nil
		})
		if err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		p.stats.Findings++
		p.log.WarnContext(ctx, "stream silence detected", slog.String("partition", partition), slog.String("gap_id", state.silenceGap.String()))
	}
	return firstErr
}

// OpenSilence reports the open SILENCE gap of a partition, if any.
func (p *Pipeline) OpenSilence(partition string) (GapID, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.partitions[partition]
	if !ok || !st.silenceOpen {
		return GapID{}, false
	}
	return st.silenceGap, true
}

func (p *Pipeline) partition(name string) *partitionState {
	st, ok := p.partitions[name]
	if !ok {
		st = &partitionState{}
		p.partitions[name] = st
	}
	return st
}

func (p *Pipeline) gapFromFinding(f Finding, cp Checkpoint, meta RawObjectMeta) Gap {
	detail := f.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	if meta.ObjectID != "" {
		detail["raw_object_id"] = meta.ObjectID
	}
	g := Gap{
		DataSourceID: p.ds.ID, DataSource: p.ds.Code, Stream: cp.Stream, Partition: cp.Partition, Kind: f.Kind,
		ExpectedSequence: f.ExpectedSequence, ObservedSequence: f.ObservedSequence, GapStartSequence: f.GapStartSequence, GapEndSequence: f.GapEndSequence,
		GapStartAt: f.GapStartAt, GapEndAt: f.GapEndAt, DetectedAt: p.clk.Now(), Resolution: f.Resolution, Detail: detail, CorrelationID: meta.CorrelationID,
	}
	if cp.Found {
		g.CheckpointID = cp.ID
	}
	return g
}

func busHeaders(e NormalizedEvent) map[string]string {
	return map[string]string{
		event.HeaderEventID:       e.EventID,
		event.HeaderEventType:     e.EventType,
		event.HeaderSchemaVersion: strconv.Itoa(e.SchemaVersion),
		event.HeaderSource:        e.Source,
		event.HeaderOccurredAt:    e.Timestamps.SourceEventAt.Format(time.RFC3339Nano),
		event.HeaderRecordedAt:    e.Timestamps.NormalizedAt.Format(time.RFC3339Nano),
		event.HeaderContentType:   event.ContentTypeJSON,
		HeaderDedupID:             e.DedupID,
		HeaderDecisionAvailableAt: e.Timestamps.DecisionAvailableAt.Format(time.RFC3339Nano),
		HeaderRawObjectID:         e.RawObjectID,
	}
}
