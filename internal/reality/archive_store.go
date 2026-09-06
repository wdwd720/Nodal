package reality

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/archive"
	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// MaxRawObjectBytes bounds one raw payload.
const MaxRawObjectBytes = archive.MaxObjectBytes

// rawObjectKind is the phantom kind of raw object ids.
type rawObjectKind struct{}

// NewRawObjectID mints a raw object id.
func NewRawObjectID() id.ID[rawObjectKind] { return id.New[rawObjectKind]() }

// RawArchive is the Archive implementation: object store + raw_archive_objects
// index (POINT_IN_TIME.md §2). The object is written before the row so a row
// never references bytes that were not durably stored.
type RawArchive struct {
	store   archive.ObjectArchive
	layout  archive.Layout
	buckets config.ArchiveConfig
	sources PgDataSourceStore
	pool    *db.DB
	clk     clock.Clock
	log     *slog.Logger
}

var _ MetaArchive = (*RawArchive)(nil)

// NewRawArchive builds the archive. pool is used by Get and Verify, which run
// outside a caller transaction.
func NewRawArchive(store archive.ObjectArchive, buckets config.ArchiveConfig, pool *db.DB, clk clock.Clock, log *slog.Logger) (*RawArchive, error) {
	if store == nil {
		return nil, errors.New("reality: object archive is required")
	}
	if pool == nil {
		return nil, errors.New("reality: database is required")
	}
	if buckets.RawBucket == "" {
		return nil, errors.New("reality: raw bucket is required")
	}
	if clk == nil {
		clk = clock.System()
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &RawArchive{store: store, layout: archive.Layout{}, buckets: buckets, pool: pool, clk: clk, log: log}, nil
}

// Validate checks a RawObject before archiving.
func (o RawObject) Validate() error {
	fields := map[string]any{}
	if o.DataSource == "" {
		fields["data_source"] = "required"
	}
	// Provider and event type become key segments, so the layout alphabet
	// is part of this contract: rejecting here names the offending field
	// instead of surfacing an opaque provenance error from the layout.
	// Adapters over foreign namespaces fold their names with
	// archive.NormalizeSegment before they get here.
	if o.Provider == "" {
		fields["provider"] = "required"
	} else if !archive.ValidSegment(o.Provider) {
		fields["provider"] = "must match " + archive.SegmentPattern
	}
	if o.EventType == "" {
		fields["event_type"] = "required"
	} else if !archive.ValidSegment(o.EventType) {
		fields["event_type"] = "must match " + archive.SegmentPattern
	}
	if o.DedupKey == "" {
		fields["dedup_key"] = "required"
	}
	if o.SchemaVersion < 1 {
		fields["schema_version"] = "must be >= 1"
	}
	if len(o.Body) == 0 {
		fields["body"] = "required: an empty payload is not evidence"
	}
	if len(o.Body) > MaxRawObjectBytes {
		fields["body"] = "too large"
	}
	if o.Timestamps.PlatformReceivedAt.IsZero() {
		fields["platform_received_at"] = "required"
	}
	if o.RetentionClass != "" && !RetentionClass(o.RetentionClass).Valid() {
		fields["retention_class"] = "unknown"
	}
	if len(fields) > 0 {
		return errs.New(errs.CodeValidationFailed, "reality: invalid raw object").WithFields(fields)
	}
	return nil
}

// Put implements Archive.
func (a *RawArchive) Put(ctx context.Context, tx pgx.Tx, o RawObject) (ArchiveRef, error) {
	ref, _, err := a.PutMeta(ctx, tx, o)
	return ref, err
}

// PutMeta implements MetaArchive: archive the object (idempotent on
// provider, event type and dedup key) and return its index row.
func (a *RawArchive) PutMeta(ctx context.Context, tx pgx.Tx, o RawObject) (ArchiveRef, RawObjectMeta, error) {
	if err := o.Validate(); err != nil {
		return ArchiveRef{}, RawObjectMeta{}, err
	}
	ds, err := a.sources.Get(ctx, tx, o.DataSource)
	if err != nil {
		return ArchiveRef{}, RawObjectMeta{}, err
	}
	if ds.Status == SourceDisabled {
		return ArchiveRef{}, RawObjectMeta{}, errs.New(errs.CodeUnsupported, "reality: data source is disabled").WithField("code", ds.Code)
	}
	if existing, found, err := a.lookupDedup(ctx, tx, o.Provider, o.EventType, o.DedupKey); err != nil {
		return ArchiveRef{}, RawObjectMeta{}, err
	} else if found {
		return ArchiveRef{ObjectID: existing.ObjectID, URI: existing.URI, Hash: existing.Hash, Duplicate: true}, existing, nil
	}

	class := RetentionClass(o.RetentionClass)
	if class == "" {
		class = RetentionClass(ds.RetentionClass)
	}
	received := o.Timestamps.PlatformReceivedAt.UTC()
	now := a.clk.Now()
	until, err := RetentionUntil(ds.RetentionDays, received)
	if err != nil {
		return ArchiveRef{}, RawObjectMeta{}, err
	}
	bucket, locked, err := BucketFor(class, a.buckets)
	if err != nil {
		return ArchiveRef{}, RawObjectMeta{}, err
	}
	prov := archive.Provenance{
		DataSource: ds.Code, Provider: o.Provider, EventType: o.EventType, SourceEventID: o.SourceEventID,
		DedupKey: o.DedupKey, SchemaVersion: o.SchemaVersion, PlatformReceivedAt: received, IngestedAt: now,
		RetentionClass: string(class), Extension: extensionFor(o.ContentType),
	}
	key, err := a.layout.Key(prov)
	if err != nil {
		return ArchiveRef{}, RawObjectMeta{}, err
	}
	partition, err := a.layout.PartitionKey(prov)
	if err != nil {
		return ArchiveRef{}, RawObjectMeta{}, err
	}
	sum := archive.SHA256(o.Body)
	req := archive.PutRequest{
		Bucket: bucket, Key: key, Body: o.Body, ContentType: contentTypeOrJSON(o.ContentType),
		Metadata: a.layout.Metadata(prov, sum),
	}
	if locked {
		req.Retention = RetentionFor(class, until)
	}
	ref, err := a.store.Put(ctx, req)
	if err != nil {
		return ArchiveRef{}, RawObjectMeta{}, err
	}
	if !bytes.Equal(ref.SHA256, sum) {
		return ArchiveRef{}, RawObjectMeta{}, errs.New(errs.CodeArchiveIntegrityViolation, "reality: store reported a different sha256 than computed")
	}

	oid := NewRawObjectID()
	seq, err := sequenceToDB(o.Sequence)
	if err != nil {
		return ArchiveRef{}, RawObjectMeta{}, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO raw_archive_objects (id, data_source_id, provider, event_type, source_event_id, dedup_key, schema_version,
		object_uri, object_hash, size_bytes, content_type, partition_key, stream, source_partition, source_offset, sequence,
		source_event_at, provider_published_at, platform_received_at, ingested_at, retention_class, retention_until, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
		ON CONFLICT (provider, event_type, dedup_key) DO NOTHING`,
		oid, ds.ID, o.Provider, o.EventType, nilIfEmpty(o.SourceEventID), o.DedupKey, o.SchemaVersion,
		ref.URI, sum, ref.Size, req.ContentType, partition, nilIfEmpty(o.Stream), nilIfEmpty(o.Partition), nilIfEmpty(o.Offset), seq,
		nilIfZero(o.Timestamps.SourceEventAt), nilIfZero(o.Timestamps.ProviderPublishedAt), received, now, string(class), until, nilIfEmpty(o.CorrelationID))
	if err != nil {
		return ArchiveRef{}, RawObjectMeta{}, errs.Wrap(err, errs.CodeInternal, "reality: index raw object")
	}
	if tag.RowsAffected() == 0 {
		// Lost a race with a concurrent ingest of the same event: the
		// earlier row wins and the extra object stays as harmless evidence.
		existing, found, err := a.lookupDedup(ctx, tx, o.Provider, o.EventType, o.DedupKey)
		if err != nil {
			return ArchiveRef{}, RawObjectMeta{}, err
		}
		if !found {
			return ArchiveRef{}, RawObjectMeta{}, errs.New(errs.CodeInternal, "reality: raw object insert conflicted but no row exists")
		}
		return ArchiveRef{ObjectID: existing.ObjectID, URI: existing.URI, Hash: existing.Hash, Duplicate: true}, existing, nil
	}
	meta := RawObjectMeta{
		ObjectID: oid.String(), DataSourceID: ds.ID, DataSource: ds.Code, Provider: o.Provider, EventType: o.EventType,
		SourceEventID: o.SourceEventID, DedupKey: o.DedupKey, SchemaVersion: o.SchemaVersion, URI: ref.URI, Hash: sum, Size: ref.Size,
		ContentType: req.ContentType, PartitionKey: partition, Stream: o.Stream, Partition: o.Partition, Offset: o.Offset, Sequence: o.Sequence,
		Timestamps: Timestamps{SourceEventAt: utcOrZero(o.Timestamps.SourceEventAt), ProviderPublishedAt: utcOrZero(o.Timestamps.ProviderPublishedAt), PlatformReceivedAt: received},
		IngestedAt: now, RetentionClass: string(class), RetentionUntil: until, CorrelationID: o.CorrelationID,
	}
	return ArchiveRef{ObjectID: meta.ObjectID, URI: meta.URI, Hash: sum}, meta, nil
}

const rawObjectColumns = `r.id, r.data_source_id, d.code, r.provider, r.event_type, r.source_event_id, r.dedup_key, r.schema_version, r.object_uri,
	r.object_hash, r.size_bytes, r.content_type, r.partition_key, r.stream, r.source_partition, r.source_offset, r.sequence,
	r.source_event_at, r.provider_published_at, r.platform_received_at, r.ingested_at, r.retention_class, r.retention_until, r.correlation_id`

func scanRawObject(row pgx.Row) (RawObjectMeta, error) {
	var m RawObjectMeta
	var oid id.ID[rawObjectKind]
	var dsid id.ID[dataSourceKind]
	var sourceEventID, stream, partition, offset, corr *string
	var seq *int64
	var sourceAt, publishedAt *time.Time
	if err := row.Scan(&oid, &dsid, &m.DataSource, &m.Provider, &m.EventType, &sourceEventID, &m.DedupKey, &m.SchemaVersion, &m.URI,
		&m.Hash, &m.Size, &m.ContentType, &m.PartitionKey, &stream, &partition, &offset, &seq,
		&sourceAt, &publishedAt, &m.Timestamps.PlatformReceivedAt, &m.IngestedAt, &m.RetentionClass, &m.RetentionUntil, &corr); err != nil {
		return RawObjectMeta{}, err
	}
	m.ObjectID, m.DataSourceID = oid.String(), dsid.String()
	m.SourceEventID, m.Stream, m.Partition, m.Offset, m.CorrelationID = deref(sourceEventID), deref(stream), deref(partition), deref(offset), deref(corr)
	m.Sequence = sequenceFromDB(seq)
	if sourceAt != nil {
		m.Timestamps.SourceEventAt = sourceAt.UTC()
	}
	if publishedAt != nil {
		m.Timestamps.ProviderPublishedAt = publishedAt.UTC()
	}
	m.Timestamps.PlatformReceivedAt = m.Timestamps.PlatformReceivedAt.UTC()
	m.IngestedAt, m.RetentionUntil = m.IngestedAt.UTC(), m.RetentionUntil.UTC()
	return m, nil
}

func (a *RawArchive) lookupDedup(ctx context.Context, q db.Querier, provider, eventType, dedupKey string) (RawObjectMeta, bool, error) {
	m, err := scanRawObject(q.QueryRow(ctx, `SELECT `+rawObjectColumns+` FROM raw_archive_objects r JOIN data_sources d ON d.id = r.data_source_id
		WHERE r.provider = $1 AND r.event_type = $2 AND r.dedup_key = $3`, provider, eventType, dedupKey))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RawObjectMeta{}, false, nil
		}
		return RawObjectMeta{}, false, errs.Wrap(err, errs.CodeInternal, "reality: look up raw object")
	}
	return m, true, nil
}

// Meta loads the index row of an object id.
func (a *RawArchive) Meta(ctx context.Context, q db.Querier, objectID string) (RawObjectMeta, error) {
	m, err := scanRawObject(q.QueryRow(ctx, `SELECT `+rawObjectColumns+` FROM raw_archive_objects r JOIN data_sources d ON d.id = r.data_source_id WHERE r.id = $1`, objectID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RawObjectMeta{}, errs.New(errs.CodeNotFound, "reality: raw object not indexed").WithField("object_id", objectID)
		}
		return RawObjectMeta{}, errs.Wrap(err, errs.CodeInternal, "reality: load raw object")
	}
	return m, nil
}

// Get implements Archive: it returns the bytes only when they hash to the
// indexed value.
func (a *RawArchive) Get(ctx context.Context, ref ArchiveRef) ([]byte, RawObjectMeta, error) {
	meta, err := a.resolve(ctx, ref)
	if err != nil {
		return nil, RawObjectMeta{}, err
	}
	loc, err := archive.ParseURI(meta.URI)
	if err != nil {
		return nil, RawObjectMeta{}, err
	}
	obj, err := a.store.Get(ctx, loc)
	if err != nil {
		if errors.Is(err, archive.ErrNotFound) {
			return nil, RawObjectMeta{}, errs.Wrap(err, errs.CodeArchiveIntegrityViolation, "reality: indexed raw object is missing from the store").WithField("object_id", meta.ObjectID)
		}
		return nil, RawObjectMeta{}, err
	}
	if !bytes.Equal(obj.Ref.SHA256, meta.Hash) || obj.Ref.Size != meta.Size {
		return nil, RawObjectMeta{}, errs.New(errs.CodeArchiveIntegrityViolation, "reality: raw object bytes do not match the indexed hash").WithField("object_id", meta.ObjectID)
	}
	return obj.Body, meta, nil
}

// Verify implements Archive: re-hash the stored object and compare with the
// index (PART 123 periodic verification).
func (a *RawArchive) Verify(ctx context.Context, ref ArchiveRef) error {
	_, _, err := a.Get(ctx, ref)
	if err != nil && errs.CodeOf(err) == errs.CodeArchiveIntegrityViolation {
		a.log.ErrorContext(ctx, "archive integrity violation", slog.String("object_id", ref.ObjectID), slog.String("uri", ref.URI))
	}
	return err
}

// DefaultVerifyLimit bounds one VerifySweep when the caller names no limit.
const DefaultVerifyLimit = 1000

// VerifyViolation names one indexed object that no longer matches its index.
type VerifyViolation struct {
	ObjectID string `json:"object_id"`
	URI      string `json:"uri"`
	Reason   string `json:"reason"`
}

// VerifyReport is the result of one verification sweep.
type VerifyReport struct {
	DataSource string            `json:"data_source"`
	From       time.Time         `json:"from"`
	To         time.Time         `json:"to,omitzero"`
	Checked    int               `json:"checked"`
	Violations []VerifyViolation `json:"violations"`
}

// OK reports whether every object checked still hashes to its indexed value.
func (r VerifyReport) OK() bool { return len(r.Violations) == 0 }

// VerifySweep re-hashes every archived object of a data source whose
// platform_received_at falls inside w, which is what POINT_IN_TIME.md §2
// means by "Verify re-hashes objects on a schedule" (PART 123). A zero
// w.End leaves the window open-ended; limit bounds one sweep
// (DefaultVerifyLimit when not positive).
//
// A sweep never stops at the first violation: an operator needs the whole
// extent of the damage, not its first symptom. Only integrity failures
// become violations — a store that is merely unreachable is returned as an
// error, because "cannot check" must never be recorded as "checked and
// clean".
func (a *RawArchive) VerifySweep(ctx context.Context, source string, w Window, limit int) (VerifyReport, error) {
	if source == "" {
		return VerifyReport{}, errs.New(errs.CodeValidationFailed, "reality: data source code is required")
	}
	if w.Start.IsZero() {
		return VerifyReport{}, errs.New(errs.CodeValidationFailed, "reality: verification window needs a start")
	}
	if !w.End.IsZero() && w.End.Before(w.Start) {
		return VerifyReport{}, errs.New(errs.CodeValidationFailed, "reality: verification window ends before it starts")
	}
	if limit <= 0 {
		limit = DefaultVerifyLimit
	}
	rows, err := a.pool.Query(ctx, `SELECT r.id, r.object_uri FROM raw_archive_objects r
		JOIN data_sources d ON d.id = r.data_source_id
		WHERE d.code = $1 AND r.platform_received_at >= $2 AND ($3::timestamptz IS NULL OR r.platform_received_at <= $3)
		ORDER BY r.platform_received_at, r.id LIMIT $4`, source, w.Start.UTC(), nilIfZero(w.End), limit)
	if err != nil {
		return VerifyReport{}, errs.Wrap(err, errs.CodeInternal, "reality: list raw objects to verify")
	}
	type target struct{ id, uri string }
	var targets []target
	for rows.Next() {
		var t target
		var oid id.ID[rawObjectKind]
		if err := rows.Scan(&oid, &t.uri); err != nil {
			rows.Close()
			return VerifyReport{}, errs.Wrap(err, errs.CodeInternal, "reality: scan raw object to verify")
		}
		t.id = oid.String()
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return VerifyReport{}, errs.Wrap(err, errs.CodeInternal, "reality: list raw objects to verify")
	}

	report := VerifyReport{DataSource: source, From: w.Start.UTC(), To: utcOrZero(w.End), Checked: len(targets)}
	for _, t := range targets {
		err := a.Verify(ctx, ArchiveRef{ObjectID: t.id, URI: t.uri})
		if err == nil {
			continue
		}
		if errs.CodeOf(err) != errs.CodeArchiveIntegrityViolation {
			return VerifyReport{}, err
		}
		report.Violations = append(report.Violations, VerifyViolation{ObjectID: t.id, URI: t.uri, Reason: err.Error()})
	}
	if len(report.Violations) > 0 {
		a.log.ErrorContext(ctx, "archive verification found violations",
			slog.String("data_source", source), slog.Int("checked", report.Checked), slog.Int("violations", len(report.Violations)))
	}
	return report, nil
}

func (a *RawArchive) resolve(ctx context.Context, ref ArchiveRef) (RawObjectMeta, error) {
	if ref.ObjectID != "" {
		return a.Meta(ctx, a.pool, ref.ObjectID)
	}
	if ref.URI == "" {
		return RawObjectMeta{}, errs.New(errs.CodeValidationFailed, "reality: archive ref needs an object id or uri")
	}
	m, err := scanRawObject(a.pool.QueryRow(ctx, `SELECT `+rawObjectColumns+` FROM raw_archive_objects r JOIN data_sources d ON d.id = r.data_source_id WHERE r.object_uri = $1`, ref.URI))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RawObjectMeta{}, errs.New(errs.CodeNotFound, "reality: raw object not indexed")
		}
		return RawObjectMeta{}, errs.Wrap(err, errs.CodeInternal, "reality: load raw object")
	}
	return m, nil
}

// ChainArchive adapts RawArchive to chain.RawArchive so provider adapters
// (Helius, Solana RPC) archive every raw response under one data source.
// Each Store runs in its own transaction: an observation that cannot be
// evidenced is not an observation.
type ChainArchive struct {
	raw        *RawArchive
	pool       *db.DB
	dataSource string
}

var _ chain.RawArchive = (*ChainArchive)(nil)

// NewChainArchive binds the adapter to a registered data source code.
func NewChainArchive(raw *RawArchive, pool *db.DB, dataSource string) (*ChainArchive, error) {
	if raw == nil || pool == nil {
		return nil, errors.New("reality: raw archive and database are required")
	}
	if dataSource == "" {
		return nil, errors.New("reality: data source code is required")
	}
	return &ChainArchive{raw: raw, pool: pool, dataSource: dataSource}, nil
}

// Store implements chain.RawArchive.
//
// chain.RawObject.EventType is the adapter's own namespace — for Solana it
// is the JSON-RPC method, so "getTransaction" rather than the lower-case
// archive segment alphabet. It is folded to that alphabet here, at the one
// boundary where the foreign namespace enters, so every archived key stays
// in the documented layout and the fold is deterministic and reversible by
// inspection ("getTransaction" is archived as "get_transaction"). A name
// that cannot be folded is refused, never truncated.
func (c *ChainArchive) Store(ctx context.Context, obj chain.RawObject) (string, error) {
	dedup := obj.DedupKey
	if dedup == "" {
		dedup = fmt.Sprintf("%x", archive.SHA256(obj.Body))
	}
	providerSeg, err := archive.NormalizeSegment(obj.Provider)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeValidationFailed, "reality: chain provider is not archivable").WithField("provider", obj.Provider)
	}
	eventSeg, err := archive.NormalizeSegment(obj.EventType)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeValidationFailed, "reality: chain event type is not archivable").WithField("event_type", obj.EventType)
	}
	raw := RawObject{
		DataSource: c.dataSource, Provider: providerSeg, EventType: eventSeg, SourceEventID: obj.SourceEventID,
		DedupKey: dedup, SchemaVersion: maxInt(obj.SchemaVersion, 1), ContentType: obj.ContentType, Body: obj.Body,
		Timestamps: Timestamps{PlatformReceivedAt: obj.PlatformReceivedAt},
	}
	if obj.ProviderPublishedAt != nil {
		raw.Timestamps.ProviderPublishedAt = *obj.ProviderPublishedAt
	}
	if raw.Timestamps.PlatformReceivedAt.IsZero() {
		raw.Timestamps.PlatformReceivedAt = c.raw.clk.Now()
	}
	var ref ArchiveRef
	err = c.pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var perr error
		ref, perr = c.raw.Put(ctx, tx, raw)
		return perr
	})
	if err != nil {
		return "", err
	}
	return ref.URI, nil
}

func extensionFor(contentType string) string {
	switch contentType {
	case "", "application/json", "text/json":
		return "json"
	case "text/plain":
		return "txt"
	case "text/html":
		return "html"
	default:
		return "bin"
	}
}

func contentTypeOrJSON(ct string) string {
	if ct == "" {
		return "application/json"
	}
	return ct
}

func nilIfZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

// sequenceToDB converts a stream sequence to the bigint column, refusing
// values that do not fit.
func sequenceToDB(seq *uint64) (*int64, error) {
	if seq == nil {
		return nil, nil
	}
	if *seq > math.MaxInt64 {
		return nil, errs.New(errs.CodeOverflow, "reality: sequence exceeds int64")
	}
	v := int64(*seq)
	return &v, nil
}

func sequenceFromDB(v *int64) *uint64 {
	if v == nil || *v < 0 {
		return nil
	}
	u := uint64(*v)
	return &u
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
