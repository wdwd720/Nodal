package predmarket

import (
	"crypto/ed25519"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// Fixed parameters of every market in this build.
const (
	// ShareDecimals is the precision of every outcome share asset, and the
	// precision a market's collateral asset must have.
	ShareDecimals uint8 = 6
	// shareUnitBaseUnits is one share, and one collateral unit, in base units.
	shareUnitBaseUnits int64 = 1_000_000
	// MinOutcomes and MaxOutcomes bound a market's outcome set. A posting
	// for a market of N outcomes carries at most 4N+4 entries, well inside
	// ledger.MaxEntries.
	MinOutcomes = 2
	MaxOutcomes = 64
	// MaxDisputeWindow bounds how long settlement may wait for disputes.
	MaxDisputeWindow = 30 * 24 * time.Hour
	// OracleMaxAge is the mark freshness every prediction instrument states
	// in its terms.
	OracleMaxAge = time.Minute
	// VenueCode names the venue in every prediction instrument's terms: the
	// platform's internal sandbox book.
	VenueCode = "NODAL_SANDBOX"
	// ReservationTTL bounds a capital reservation taken by an issuance or a
	// redemption. Both consume their reservation in the same transaction.
	ReservationTTL = 5 * time.Minute
)

// ShareUnit returns one share (and one collateral unit) in base units.
func ShareUnit() money.Quantity { return money.QuantityFromInt64(shareUnitBaseUnits) }

// MarketStatus is the lifecycle state of a market.
type MarketStatus string

// Market statuses.
const (
	StatusDraft     MarketStatus = "DRAFT"
	StatusOpen      MarketStatus = "OPEN"
	StatusClosed    MarketStatus = "CLOSED"
	StatusResolving MarketStatus = "RESOLVING"
	StatusResolved  MarketStatus = "RESOLVED"
	StatusDisputed  MarketStatus = "DISPUTED"
	StatusSettled   MarketStatus = "SETTLED"
	StatusVoided    MarketStatus = "VOIDED"
)

// statusNone is the from_status of a market's birth row. It is not a status a
// market can be in.
const statusNone MarketStatus = "NONE"

var allMarketStatuses = []MarketStatus{
	StatusDraft, StatusOpen, StatusClosed, StatusResolving, StatusResolved, StatusDisputed, StatusSettled, StatusVoided,
}

// AllMarketStatuses returns every market status in lifecycle order (a copy).
// It is the Go twin of prediction_markets_status_check.
func AllMarketStatuses() []MarketStatus { return append([]MarketStatus(nil), allMarketStatuses...) }

// Valid reports whether s is a declared status.
func (s MarketStatus) Valid() bool {
	for _, x := range allMarketStatuses {
		if x == s {
			return true
		}
	}
	return false
}

// Terminal reports whether no transition leaves s.
func (s MarketStatus) Terminal() bool { return s == StatusSettled || s == StatusVoided }

// marketTransitions is the explicit lifecycle table. It mirrors
// cp_prediction_market_transition_legal (migration 00749); a parity test
// compares the two.
var marketTransitions = map[MarketStatus][]MarketStatus{
	StatusDraft:     {StatusOpen, StatusVoided},
	StatusOpen:      {StatusClosed, StatusVoided},
	StatusClosed:    {StatusResolving, StatusVoided},
	StatusResolving: {StatusResolved, StatusVoided},
	StatusResolved:  {StatusSettled, StatusDisputed, StatusVoided},
	StatusDisputed:  {StatusResolved, StatusVoided},
}

// CanTransition reports whether from -> to is a legal lifecycle step.
func CanTransition(from, to MarketStatus) bool {
	for _, t := range marketTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// SupportedModes returns the modes a prediction market can run in. LIVE is
// absent here, in gates.AdvancedSpecs and in prediction_markets_mode_check.
func SupportedModes() []gates.Mode { return []gates.Mode{gates.ModePaper, gates.ModeSandbox} }

func supportedMode(m gates.Mode) bool {
	for _, x := range SupportedModes() {
		if x == m {
			return true
		}
	}
	return false
}

// LifecycleActorTypes returns the actor types a market transition may
// record: an operator, or the system scheduler that opens and closes markets
// on time. It is the Go twin of prediction_market_transitions_actor_type_check.
func LifecycleActorTypes() []security.ActorType {
	return []security.ActorType{security.ActorOperator, security.ActorSystem}
}

// ResolutionKind is how a resolution came to be.
type ResolutionKind string

// Resolution kinds.
const (
	// ResolutionOracle is the first resolution: a verified oracle report.
	ResolutionOracle ResolutionKind = "ORACLE"
	// ResolutionDisputeUpheld closes a dispute keeping the disputed outcome.
	ResolutionDisputeUpheld ResolutionKind = "DISPUTE_UPHELD"
	// ResolutionDisputeOverturned closes a dispute with a different outcome,
	// always backed by a verified oracle report.
	ResolutionDisputeOverturned ResolutionKind = "DISPUTE_OVERTURNED"
)

// AllResolutionKinds returns every resolution kind. It is the Go twin of
// prediction_resolutions_kind_check.
func AllResolutionKinds() []ResolutionKind {
	return []ResolutionKind{ResolutionOracle, ResolutionDisputeUpheld, ResolutionDisputeOverturned}
}

// SettlementKind distinguishes a payout from a void refund.
type SettlementKind string

// Settlement kinds.
const (
	SettlementPayout SettlementKind = "PAYOUT"
	SettlementRefund SettlementKind = "REFUND"
)

// AllSettlementKinds returns every settlement kind. It is the Go twin of
// prediction_settlements_kind_check.
func AllSettlementKinds() []SettlementKind {
	return []SettlementKind{SettlementPayout, SettlementRefund}
}

// RoundingPolicy names how a settlement turned share holdings into
// collateral.
type RoundingPolicy string

// Rounding policies.
const (
	// RoundingExact: a payout pays each winning share base unit exactly one
	// collateral base unit, so nothing is rounded and no residual exists.
	RoundingExact RoundingPolicy = "EXACT"
	// RoundingFloorPerHolder: a void refunds each whole share floor(U/N)
	// collateral base units (U the unit, N the outcome count); a holder of H
	// share base units across all outcomes is paid floor(H × floor(U/N) / U),
	// rounded once, toward zero, in favour of the escrow. What is not paid --
	// U mod N per complete set, plus less than one base unit per holder -- is
	// the disclosed residual.
	RoundingFloorPerHolder RoundingPolicy = "FLOOR_PER_HOLDER"
)

// AllRoundingPolicies returns every rounding policy. It is the Go twin of
// prediction_settlements_rounding_policy_check.
func AllRoundingPolicies() []RoundingPolicy {
	return []RoundingPolicy{RoundingExact, RoundingFloorPerHolder}
}

type (
	marketKind     struct{}
	resolutionKind struct{}
	disputeKind    struct{}
	settlementKind struct{}
)

// MarketID identifies a prediction market.
type MarketID = id.ID[marketKind]

// ResolutionID identifies a prediction_resolutions row.
type ResolutionID = id.ID[resolutionKind]

// DisputeID identifies a prediction_disputes row.
type DisputeID = id.ID[disputeKind]

// SettlementID identifies a prediction_settlements row.
type SettlementID = id.ID[settlementKind]

// NewMarketID returns a fresh market id.
func NewMarketID() MarketID { return id.New[marketKind]() }

// ParseMarketID parses the canonical form.
func ParseMarketID(s string) (MarketID, error) { return id.Parse[marketKind](s) }

// Outcome is one outcome of a market: its symbol, the SIMULATED share asset
// that represents it, and the instrument the internal book trades it on.
type Outcome struct {
	Index        int
	Symbol       string
	ShareAssetID assets.AssetID
	InstrumentID instruments.InstrumentID
}

// Market is a prediction_markets row with its outcome registry.
type Market struct {
	ID                  MarketID
	Symbol              string
	Question            string
	Mode                gates.Mode
	FacilityID          econ.FacilityID
	InstrumentID        instruments.InstrumentID
	CollateralAssetID   assets.AssetID
	Outcomes            []Outcome
	UnitQuantity        money.Quantity
	OpenAt              time.Time
	CloseAt             time.Time
	DisputeWindow       time.Duration
	OracleSource        string
	OraclePublicKey     ed25519.PublicKey
	DefinitionHash      []byte
	Status              MarketStatus
	CurrentResolutionID *ResolutionID
	CreatedBy           econ.Actor
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Facility returns the market's escrow facility as an econ.Facility value,
// enough to address its ledger accounts.
func (m Market) Facility() econ.Facility {
	return econ.Facility{ID: m.FacilityID, Kind: ledger.FacilityPredictionMarket}
}

// OutcomeBySymbol returns the outcome with the given symbol.
func (m Market) OutcomeBySymbol(symbol string) (Outcome, bool) {
	for _, o := range m.Outcomes {
		if o.Symbol == symbol {
			return o, true
		}
	}
	return Outcome{}, false
}

// Resolution is a prediction_resolutions row.
type Resolution struct {
	ID               ResolutionID
	MarketID         MarketID
	Kind             ResolutionKind
	OutcomeIndex     int
	SupersedesID     *ResolutionID
	DisputeID        *DisputeID
	ReportHash       []byte
	ReportSource     string
	ReportObservedAt *time.Time
	ReportSignature  []byte
	EvidenceRef      string
	EvidenceSHA256   []byte
	AcceptedBy       econ.Actor
	Reason           string
	AcceptedAt       time.Time
}

// Dispute is a prediction_disputes row: who disputed which resolution, why,
// and on what evidence.
type Dispute struct {
	ID             DisputeID
	MarketID       MarketID
	ResolutionID   ResolutionID
	RaisedBy       econ.Actor
	Reason         string
	EvidenceRef    string
	EvidenceSHA256 []byte
	RaisedAt       time.Time
}

// Settlement is a prediction_settlements row: the one record of how a market
// paid out (or refunded) its escrow.
type Settlement struct {
	ID                  SettlementID
	MarketID            MarketID
	Kind                SettlementKind
	ResolutionID        *ResolutionID
	WinningOutcomeIndex *int
	OutcomeCount        int
	UnitQuantity        money.Quantity
	RefundRatePerShare  *money.Quantity
	RoundingPolicy      RoundingPolicy
	EscrowBefore        money.Quantity
	TotalPaid           money.Quantity
	Residual            money.Quantity
	HolderCount         int
	EvidenceRef         string
	EvidenceSHA256      []byte
	SettledBy           econ.Actor
	SettledAt           time.Time
}

// Position is one row of the prediction_positions projection: a customer's
// WALLET balance of one outcome share.
type Position struct {
	MarketID      MarketID
	AccountID     accounts.AccountID
	OutcomeIndex  int
	OutcomeSymbol string
	ShareAssetID  assets.AssetID
	Quantity      money.Quantity
	AsOf          time.Time
}
