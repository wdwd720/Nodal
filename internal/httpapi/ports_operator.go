package httpapi

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuation"
)

// The operator console (ADR-0020): one read of what needs an operator now,
// the sandbox reference marks every simulated valuation depends on, and the
// tips of the simulated chains. Everything here reads the database or the
// devnet host at the instant of the request; nothing is cached, estimated or
// filled in.

// OperatorPort serves /advanced/ops.
type OperatorPort interface {
	Overview(ctx context.Context) (OpsOverview, error)
	// RecordMark records one reference mark of a SIMULATED asset quoted in
	// the sandbox dollar, with its audit event. The adapter refuses every
	// observation the readers could not use (see ReferenceMarkCommand).
	RecordMark(ctx context.Context, r ReferenceMarkCommand) (ReferenceMarkRecorded, error)
	// Marks lists the latest marks of an asset in the sandbox dollar, newest
	// observation first.
	Marks(ctx context.Context, asset assets.AssetID, limit int) (ReferenceMarkList, error)
	// Chains reports each simulated chain's tip; UNSUPPORTED when this
	// deployment runs no devnet host.
	Chains(ctx context.Context) (OpsChains, error)
}

// ReferenceMarkCommand is an operator's price observation as stated. Price
// is the decimal exactly as typed (sandbox dollars per one whole unit); the
// adapter refuses a non-SIMULATED asset, the sandbox dollar itself, a quote
// other than the sandbox dollar, a price that is not positive or has more
// than ReferenceMarkMaxScale fractional digits, and an observation in the
// future or older than the maximum mark age. A nil ObservedAt is now.
type ReferenceMarkCommand struct {
	AssetID       assets.AssetID
	QuoteAssetID  *assets.AssetID
	Price         string
	Source        string
	ObservedAt    *time.Time
	Actor         econ.Actor
	CorrelationID string
}

// ReferenceMark is one asset_prices row of an asset in the sandbox dollar,
// with its age at the time of the read.
type ReferenceMark struct {
	ID           valuation.PriceID
	AssetID      assets.AssetID
	QuoteAssetID assets.AssetID
	Mantissa     money.Quantity
	Scale        int32
	Source       string
	ObservedAt   time.Time
	ReceivedAt   time.Time
	// Age is the read time minus ObservedAt (negative for a future
	// observation, which every reader ignores).
	Age    time.Duration
	Stale  bool
	Future bool
}

// ReferenceMarkRecorded is the answer to RecordMark.
type ReferenceMarkRecorded struct {
	Mark        ReferenceMark
	AssetSymbol string
	QuoteSymbol string
	// Duplicate is true when the same observation was already recorded.
	Duplicate bool
	MaxAge    time.Duration
}

// ReferenceMarkList is the latest marks of one asset.
type ReferenceMarkList struct {
	Asset  assets.Asset
	Quote  assets.Asset
	MaxAge time.Duration
	AsOf   time.Time
	Items  []ReferenceMark
}

// OpsOverview is the console's one read.
type OpsOverview struct {
	GeneratedAt time.Time
	Environment string
	MaxMarkAge  time.Duration
	// KillSwitches are the active ones.
	KillSwitches   []killswitch.Switch
	StableAssets   []OpsStableAsset
	BridgeRoutes   []OpsBridgeRoute
	Reconciliation []OpsReconciliationKind
	DualControl    OpsDualControl
	Oracle         []OpsOracleAsset
	// ChainsConfigured says whether a devnet host is wired (Chains answers).
	ChainsConfigured bool
}

// OpsReconciliationRecord is one reconciliation_records row, summarized.
type OpsReconciliationRecord struct {
	ID            uuid.UUID
	Kind          string
	Status        string
	Material      bool
	BlocksNewRisk bool
	OpenedAt      time.Time
	ResolvedAt    *time.Time
}

// OpsPegAlert is a stable asset's latest peg alert.
type OpsPegAlert struct {
	Kind           string
	Severity       string
	BreakerTripped bool
	ObservedAt     time.Time
}

// OpsStableAsset is one stable asset's registry status, its latest peg alert
// and its latest STABLE_RESERVE reconciliation, if any.
type OpsStableAsset struct {
	ID                 uuid.UUID
	Name               string
	AssetID            uuid.UUID
	ChainID            string
	Status             string
	LatestAlert        *OpsPegAlert
	LatestSupplyRecord *OpsReconciliationRecord
}

// OpsBridgeRoute is one bridge route's status and its unresolved BRIDGE
// reconciliation, if any.
type OpsBridgeRoute struct {
	ID                 uuid.UUID
	Name               string
	SourceChain        string
	DestinationChain   string
	Status             string
	OpenReconciliation *OpsReconciliationRecord
}

// OpsReconciliationKind counts the unresolved records of one advanced kind.
type OpsReconciliationKind struct {
	Kind          string
	Unresolved    int64
	Material      int64
	BlocksNewRisk int64
	OldestOpened  *time.Time
	// OldestAge is the age of the oldest unresolved record at GeneratedAt.
	OldestAge *time.Duration
}

// OpsDualControl counts the decisions awaiting a second operator.
type OpsDualControl struct {
	TransferReversalsPending int64
	MarginCasesInReview      int64
	MarginResolutionsPending int64
}

// OpsOracleAsset is a held SIMULATED asset and its latest mark in the
// sandbox dollar (nil ObservedAt: no mark at all).
type OpsOracleAsset struct {
	AssetID    assets.AssetID
	Symbol     string
	ObservedAt *time.Time
	Source     string
	Age        *time.Duration
	Stale      bool
}

// OpsChains is the tip of every chain of the devnet host.
type OpsChains struct {
	Network    string
	ObservedAt time.Time
	Items      []OpsChainTip
}

// OpsChainTip is one chain's canonical head.
type OpsChainTip struct {
	ChainID             string
	Slot                uint64
	Height              uint64
	Hash                string
	Final               bool
	FinalizedHeight     uint64
	LastBlockProducedAt *time.Time
}
