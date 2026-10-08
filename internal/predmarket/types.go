package predmarket

import (
	"context"
	"crypto/ed25519"
	"time"

	"github.com/jackc/pgx/v5"

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

// OutcomeBooks is the internal order book as a market needs it, implemented
// by an adapter over the order book that the deployment wires in (this
// package never imports internal/orders). CancelResting cancels every resting
// order on the given outcome instruments, releasing the shares and collateral
// those orders reserve, in the caller's transaction, and returns how many it
// cancelled. It must be safe to call again: a second call finds nothing left
// to cancel. Everything it does must live in tx, because the caller may roll
// it back: a close, void or settlement that is refused afterwards undoes the
// cancellations together with everything else.
//
// Close and CloseDue call it after halting the outcome instruments, so no
// order rests on a closed market; a void calls it after halting them too, and
// a settlement calls it once more before checking that no share is still
// reserved. Without it, a settlement or void that finds an outcome share still
// reserved is refused, naming every reservation, until the orders holding them
// are cancelled through the order book.
type OutcomeBooks interface {
	CancelResting(ctx context.Context, tx pgx.Tx, outcomeInstruments []instruments.InstrumentID, by econ.Actor, reason string) (int, error)
}

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

// VoidableStatuses returns the statuses a market can be voided from: every
// status but SETTLED and VOIDED, in lifecycle order. It is the Go twin of
// prediction_void_proposals_market_status_check.
func VoidableStatuses() []MarketStatus {
	out := make([]MarketStatus, 0, len(allMarketStatuses))
	for _, s := range allMarketStatuses {
		if !s.Terminal() {
			out = append(out, s)
		}
	}
	return out
}

// ClaimActorTypes returns who may claim a held payout: the holder (USER) or
// a service acting for them. Never an agent or an operator. It is the Go twin
// of prediction_held_payout_claims_claimed_by_actor_type_check.
func ClaimActorTypes() []security.ActorType {
	return []security.ActorType{security.ActorUser, security.ActorService}
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
	marketKind       struct{}
	resolutionKind   struct{}
	disputeKind      struct{}
	settlementKind   struct{}
	voidProposalKind struct{}
	voidApprovalKind struct{}
	heldPayoutKind   struct{}
	heldClaimKind    struct{}
	observationKind  struct{}
)

// MarketID identifies a prediction market.
type MarketID = id.ID[marketKind]

// ResolutionID identifies a prediction_resolutions row.
type ResolutionID = id.ID[resolutionKind]

// DisputeID identifies a prediction_disputes row.
type DisputeID = id.ID[disputeKind]

// SettlementID identifies a prediction_settlements row.
type SettlementID = id.ID[settlementKind]

// VoidProposalID identifies a prediction_void_proposals row.
type VoidProposalID = id.ID[voidProposalKind]

// VoidApprovalID identifies a prediction_void_approvals row.
type VoidApprovalID = id.ID[voidApprovalKind]

// HeldPayoutID identifies a prediction_held_payouts row.
type HeldPayoutID = id.ID[heldPayoutKind]

// HeldPayoutClaimID identifies a prediction_held_payout_claims row.
type HeldPayoutClaimID = id.ID[heldClaimKind]

// ObservationID identifies a prediction_oracle_observations row.
type ObservationID = id.ID[observationKind]

// NewMarketID returns a fresh market id.
func NewMarketID() MarketID { return id.New[marketKind]() }

// ParseMarketID parses the canonical form.
func ParseMarketID(s string) (MarketID, error) { return id.Parse[marketKind](s) }

// ParseVoidProposalID parses the canonical form.
func ParseVoidProposalID(s string) (VoidProposalID, error) { return id.Parse[voidProposalKind](s) }

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
	// TotalPaid is everything the settlement allotted to holders: delivered
	// to their wallets, or HELD in the escrow for a holder it could not be
	// delivered to (TotalHeld of it). escrow_before = TotalPaid + Residual.
	TotalPaid      money.Quantity
	TotalHeld      money.Quantity
	Residual       money.Quantity
	HolderCount    int
	EvidenceRef    string
	EvidenceSHA256 []byte
	SettledBy      econ.Actor
	SettledAt      time.Time
}

// VoidProposal is a prediction_void_proposals row: one operator's proposal
// to void a market, with the reason and the archived evidence, made against
// the market as it stood (its status and its resolution in force). A void
// happens only when a different operator approves it.
type VoidProposal struct {
	ID             VoidProposalID
	MarketID       MarketID
	MarketStatus   MarketStatus
	ResolutionID   *ResolutionID
	Reason         string
	EvidenceRef    string
	EvidenceSHA256 []byte
	ProposedBy     econ.Actor
	ProposedAt     time.Time
	// Approval is set once a second operator approved the proposal, which
	// voided the market.
	Approval *VoidApproval
}

// VoidApproval is a prediction_void_approvals row.
type VoidApproval struct {
	ID         VoidApprovalID
	ProposalID VoidProposalID
	MarketID   MarketID
	Reason     string
	ApprovedBy econ.Actor
	ApprovedAt time.Time
}

// HeldPayout is a prediction_held_payouts row: a payout (or void refund) the
// settlement allotted to a holder it could not deliver to -- the route or
// Risk V2 refused the holder's own payout for the holder's own reasons (a
// frozen or closed account, an unverified identity). The holder's shares
// were retired like everyone else's and the amount stays in the market's
// PREDICTION_ESCROW until the holder claims it.
type HeldPayout struct {
	ID                   HeldPayoutID
	MarketID             MarketID
	SettlementID         SettlementID
	AccountID            accounts.AccountID
	Kind                 SettlementKind
	CollateralAssetID    assets.AssetID
	Amount               money.Quantity
	ReasonCodes          []string
	RefusedIntentID      econ.IntentID
	EffectID             econ.EffectID
	JournalTransactionID ledger.TransactionID
	EvidenceRef          string
	EvidenceSHA256       []byte
	HeldAt               time.Time
	// Claim is set once the holder claimed the payout.
	Claim *HeldPayoutClaim
}

// Claimed reports whether the held payout has been delivered.
func (h HeldPayout) Claimed() bool { return h.Claim != nil }

// HeldPayoutClaim is a prediction_held_payout_claims row: the delivery of a
// held payout to its holder's wallet.
type HeldPayoutClaim struct {
	ID                   HeldPayoutClaimID
	HeldPayoutID         HeldPayoutID
	MarketID             MarketID
	AccountID            accounts.AccountID
	Amount               money.Quantity
	IntentID             econ.IntentID
	EffectID             econ.EffectID
	JournalTransactionID ledger.TransactionID
	ClaimedBy            econ.Actor
	EvidenceRef          string
	EvidenceSHA256       []byte
	ClaimedAt            time.Time
}

// OracleObservation is a prediction_oracle_observations row: one signed
// sandbox oracle report and the operator who recorded it.
type OracleObservation struct {
	ID             ObservationID
	MarketID       MarketID
	OutcomeIndex   int
	Report         OracleReport
	ReportHash     []byte
	EvidenceSHA256 []byte
	RecordedBy     econ.Actor
	RecordedAt     time.Time
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
