package reality

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/security"
)

// Schema versions emitted by this package.
const (
	// NormalizedEventSchemaVersion is the schema of NormalizedEvent as
	// published on the bus and stored in ClickHouse.
	NormalizedEventSchemaVersion = 1
)

// Data source kinds (data_sources.kind).
const (
	KindMarketData         = "MARKET_DATA"
	KindOnchain            = "ONCHAIN"
	KindSocial             = "SOCIAL"
	KindWalletIntelligence = "WALLET_INTELLIGENCE"
	KindModel              = "MODEL"
	KindInternal           = "INTERNAL"
)

// Redistribution policies (data_sources.redistribution_policy).
const (
	RedistributionNone            = "NONE"
	RedistributionInternalOnly    = "INTERNAL_ONLY"
	RedistributionCustomerDisplay = "CUSTOMER_DISPLAY"
	RedistributionRedistributable = "REDISTRIBUTABLE"
)

// Historical-use permissions (data_sources.historical_use_permitted).
const (
	HistoricalUseYes     = "YES"
	HistoricalUseNo      = "NO"
	HistoricalUseUnknown = "UNKNOWN"
)

// Persistence capabilities (data_sources.persistence_capability).
const (
	PersistenceAllowed = "ALLOWED"
	PersistenceBlocked = "BLOCKED"
)

// Dedup strategies (data_sources.dedup_strategy; PART 199).
const (
	DedupProviderID    = "PROVIDER_ID"
	DedupCompositeHash = "COMPOSITE_HASH"
)

// Data source statuses.
const (
	SourceActive   = "ACTIVE"
	SourceDegraded = "DEGRADED"
	SourceDisabled = "DISABLED"
)

// Checkpoint statuses (ingest_checkpoints.status).
const (
	CheckpointActive       = "ACTIVE"
	CheckpointDisconnected = "DISCONNECTED"
	CheckpointReplaying    = "REPLAYING"
	CheckpointGap          = "GAP"
	CheckpointStopped      = "STOPPED"
)

// Gap kinds (stream_gaps.kind; POINT_IN_TIME.md §4).
const (
	GapKindGap              = "GAP"
	GapKindSilence          = "SILENCE"
	GapKindReconnect        = "RECONNECT"
	GapKindReplay           = "REPLAY"
	GapKindOrderingAnomaly  = "ORDERING_ANOMALY"
	ResolutionOpen          = "OPEN"
	ResolutionReplayed      = "REPLAYED"
	ResolutionUnrecoverable = "UNRECOVERABLE"
	ResolutionAcknowledged  = "ACKNOWLEDGED"
)

// Provider roles (provider_health_samples.role).
const (
	RoleData        = "DATA"
	RoleObservation = "OBSERVATION"
	RoleExecution   = "EXECUTION"
	RoleFunding     = "FUNDING"
	RoleWallet      = "WALLET"
	RoleModel       = "MODEL"
)

// Timestamps are the six knowledge-time instants of a datum (PART 74, 175).
// The first two are provider clocks and are never trusted; the rest are
// platform clocks. ProviderPublishedAt is zero when the provider reports
// none.
type Timestamps struct {
	SourceEventAt       time.Time `json:"source_event_at"`
	ProviderPublishedAt time.Time `json:"provider_published_at,omitzero"`
	PlatformReceivedAt  time.Time `json:"platform_received_at"`
	NormalizedAt        time.Time `json:"normalized_at"`
	FeatureAvailableAt  time.Time `json:"feature_available_at"`
	DecisionAvailableAt time.Time `json:"decision_available_at"`
}

// RawObject is a raw provider payload to archive before interpretation
// (POINT_IN_TIME.md §10).
type RawObject struct {
	DataSource    string
	Provider      string
	EventType     string
	SourceEventID string
	DedupKey      string
	SchemaVersion int
	ContentType   string
	Body          []byte
	// Timestamps: only SourceEventAt, ProviderPublishedAt and
	// PlatformReceivedAt are meaningful at archive time.
	Timestamps Timestamps
	// RetentionClass overrides the data source's class when set.
	RetentionClass string
	Stream         string
	Partition      string
	Offset         string
	Sequence       *uint64
	CorrelationID  string
}

// ArchiveRef references an archived raw object.
type ArchiveRef struct {
	ObjectID string
	URI      string
	Hash     []byte
	// Duplicate is true when an object with the same (provider, event
	// type, dedup key) had already been archived; ObjectID, URI and Hash
	// then describe that earlier object.
	Duplicate bool
}

// RawObjectMeta is the raw_archive_objects row.
type RawObjectMeta struct {
	ObjectID       string
	DataSourceID   string
	DataSource     string
	Provider       string
	EventType      string
	SourceEventID  string
	DedupKey       string
	SchemaVersion  int
	URI            string
	Hash           []byte
	Size           int64
	ContentType    string
	PartitionKey   string
	Stream         string
	Partition      string
	Offset         string
	Sequence       *uint64
	Timestamps     Timestamps // SourceEventAt, ProviderPublishedAt, PlatformReceivedAt
	IngestedAt     time.Time
	RetentionClass string
	RetentionUntil time.Time
	CorrelationID  string
}

// eventKind is the phantom kind of normalized event ids.
type eventKind struct{}

// EventID identifies a NormalizedEvent (UUIDv7).
type EventID = id.ID[eventKind]

// NewEventID mints a fresh EventID.
func NewEventID() EventID { return id.New[eventKind]() }

// NormalizedEvent is the canonical event envelope carried by Redpanda and
// stored in ClickHouse (POINT_IN_TIME.md §4).
type NormalizedEvent struct {
	EventID         string          `json:"event_id"`
	DedupID         string          `json:"dedup_id"`
	SchemaVersion   int             `json:"schema_version"`
	Source          string          `json:"source"`
	EventType       string          `json:"event_type"`
	Sequence        *uint64         `json:"sequence,omitempty"`
	SourcePartition string          `json:"source_partition"`
	SourceOffset    string          `json:"source_offset"`
	Timestamps      Timestamps      `json:"timestamps"`
	InstrumentID    string          `json:"instrument_id,omitempty"`
	AssetID         string          `json:"asset_id,omitempty"`
	Wallet          string          `json:"wallet,omitempty"`
	RawObjectID     string          `json:"raw_object_id"`
	RawObjectHash   []byte          `json:"raw_object_hash"`
	Payload         json.RawMessage `json:"payload"`
	// Flags carries detector annotations (for example ORDERING_ANOMALY);
	// flagged events are kept, never dropped.
	Flags []string `json:"flags,omitempty"`
}

// Normalizer turns one archived raw object into zero or more normalized
// events. It is pure given now: no clock, no I/O.
type Normalizer interface {
	Source() string
	Normalize(meta RawObjectMeta, body []byte, now time.Time) ([]NormalizedEvent, error)
}

// Archive is the raw archive contract (POINT_IN_TIME.md §10).
type Archive interface {
	Put(ctx context.Context, tx pgx.Tx, o RawObject) (ArchiveRef, error)
	Get(ctx context.Context, ref ArchiveRef) ([]byte, RawObjectMeta, error)
	Verify(ctx context.Context, ref ArchiveRef) error
}

// MetaArchive is an Archive that also returns the indexed row on Put, which
// the ingest pipeline needs to normalize without a second read.
type MetaArchive interface {
	Archive
	PutMeta(ctx context.Context, tx pgx.Tx, o RawObject) (ArchiveRef, RawObjectMeta, error)
}

// Ingestor runs an ingest pipeline until ctx is done.
type Ingestor interface {
	Run(ctx context.Context) error
}

// Checkpoint is the ingest position of one (source, stream, partition,
// consumer). Found is false for a position that was never persisted.
type Checkpoint struct {
	ID                     string
	DataSourceID           string
	DataSource             string
	Stream                 string
	Partition              string
	Consumer               string
	LastSequence           *uint64
	LastSourceOffset       string
	LastSourceEventAt      time.Time
	LastPlatformReceivedAt time.Time
	LastRawObjectID        string
	EventsSinceStart       int64
	Status                 string
	ConnectedAt            time.Time
	DisconnectedAt         time.Time
	Version                int64
	Found                  bool
}

// gapKind is the phantom kind of gap ids.
type gapKind struct{}

// GapID identifies a stream_gaps row.
type GapID = id.ID[gapKind]

// NewGapID mints a fresh GapID.
func NewGapID() GapID { return id.New[gapKind]() }

// Gap is a stream_gaps row.
type Gap struct {
	ID                  GapID
	DataSourceID        string
	DataSource          string
	CheckpointID        string
	Stream              string
	Partition           string
	Kind                string
	ExpectedSequence    *uint64
	ObservedSequence    *uint64
	GapStartSequence    *uint64
	GapEndSequence      *uint64
	GapStartAt          time.Time
	GapEndAt            time.Time // zero while open-ended
	DetectedAt          time.Time
	Resolution          string
	ResolvedAt          time.Time
	ResolvedByActorType string
	ResolvedByActorID   string
	Detail              map[string]any
	CorrelationID       string
}

// Blocking reports whether a strategy or backtest overlapping the gap must
// refuse continuity (OPEN or UNRECOVERABLE).
func (g Gap) Blocking() bool {
	return g.Resolution == ResolutionOpen || g.Resolution == ResolutionUnrecoverable
}

// Window is a closed time interval.
type Window struct {
	Start time.Time
	End   time.Time
}

// CheckpointStore persists ingest positions and gaps.
type CheckpointStore interface {
	Load(ctx context.Context, q db.Querier, source, stream, partition, consumer string) (Checkpoint, error)
	Advance(ctx context.Context, tx pgx.Tx, c Checkpoint) (Checkpoint, error)
	RecordGap(ctx context.Context, tx pgx.Tx, g Gap) (GapID, error)
	Resolve(ctx context.Context, tx pgx.Tx, gid GapID, resolution string, actor security.Principal) error
	CloseGap(ctx context.Context, tx pgx.Tx, gid GapID, endAt time.Time) error
	OpenGaps(ctx context.Context, q db.Querier, source string, w Window) ([]Gap, error)
}

// HealthSample is one provider_health_samples row (PART 79).
type HealthSample struct {
	ID               string
	Provider         string
	Role             string
	DataSourceID     string
	State            provider.Health
	ErrorRateBPS     int64
	P50LatencyMS     int64
	P99LatencyMS     int64
	StalenessMS      int64
	WindowMS         int64
	SampleCount      int
	ReasonCodes      []string
	EvaluatorVersion string
	EvaluatedAt      time.Time
}

// HealthState is the current view derived from the latest sample.
type HealthState struct {
	Provider    string
	Role        string
	State       provider.Health
	EvaluatedAt time.Time
	SampleAge   time.Duration
	ReasonCodes []string
	// Usable is false for UNHEALTHY and DISABLED providers (and when no
	// recent sample exists): the snapshotter treats their observations as
	// stale (PART 79).
	Usable bool
}

// HealthEvaluator produces samples.
type HealthEvaluator interface {
	Sample(ctx context.Context, providerName, role string, now time.Time) (HealthSample, error)
}

// HealthReader reads the current state.
type HealthReader interface {
	Current(ctx context.Context, q db.Querier, providerName, role string, now time.Time) (HealthState, error)
}

// DataSource is a data_sources row (PART 120, 122).
type DataSource struct {
	ID                     string
	Code                   string
	Provider               string
	Kind                   string
	RetentionClass         string
	RetentionDays          int
	RedistributionPolicy   string
	HistoricalUsePermitted string
	PersistenceCapability  string
	Environments           []string
	LicenseRef             string
	ContractRef            string
	DedupStrategy          string
	HeartbeatTimeout       time.Duration
	SupportsReplay         bool
	SupportsSequence       bool
	Status                 string
	CreatedByActorType     string
	CreatedByActorID       string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// DataSourceStore reads the registry.
type DataSourceStore interface {
	Get(ctx context.Context, q db.Querier, code string) (DataSource, error)
	List(ctx context.Context, q db.Querier) ([]DataSource, error)
}
