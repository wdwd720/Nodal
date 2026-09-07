package nativeasset

import (
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

type transitionKind struct{}

// TransitionID identifies one recorded status change.
type TransitionID = id.ID[transitionKind]

// NewTransitionID returns a fresh transition id.
func NewTransitionID() TransitionID { return id.New[transitionKind]() }

// Status is the lifecycle state of a native asset (PART XIII).
type Status string

// Statuses.
const (
	// StatusDraft is being composed by its creator. Nothing is tradable and
	// the economics can still change.
	StatusDraft Status = "DRAFT"
	// StatusPendingReview has been submitted and is awaiting moderation.
	StatusPendingReview Status = "PENDING_REVIEW"
	// StatusActive is live: the market accepts buys and sells, and the
	// economics are frozen.
	StatusActive Status = "ACTIVE"
	// StatusCloseOnly lets holders sell and nobody buy.
	StatusCloseOnly Status = "CLOSE_ONLY"
	// StatusHalted stops all trading in both directions. It is for
	// investigations, and it is the only state that traps holders, so it is
	// meant to be short.
	StatusHalted Status = "HALTED"
	// StatusDelisted is terminal.
	StatusDelisted Status = "DELISTED"
	// StatusRejected is terminal: moderation refused it before it ever went
	// live, so nobody holds any.
	StatusRejected Status = "REJECTED"
)

var allStatuses = []Status{
	StatusDraft, StatusPendingReview, StatusActive, StatusCloseOnly,
	StatusHalted, StatusDelisted, StatusRejected,
}

// AllStatuses returns every declared status in declaration order (a copy).
func AllStatuses() []Status { return append([]Status(nil), allStatuses...) }

// Valid reports whether s is declared.
func (s Status) Valid() bool {
	for _, x := range allStatuses {
		if x == s {
			return true
		}
	}
	return false
}

func (s Status) String() string { return string(s) }

// statusTransitions is the explicit legal transition table.
var statusTransitions = map[Status][]Status{
	StatusDraft:         {StatusPendingReview, StatusRejected},
	StatusPendingReview: {StatusActive, StatusRejected, StatusDraft},
	StatusActive:        {StatusCloseOnly, StatusHalted},
	StatusCloseOnly:     {StatusActive, StatusHalted, StatusDelisted},
	StatusHalted:        {StatusActive, StatusCloseOnly, StatusDelisted},
	StatusDelisted:      {},
	StatusRejected:      {},
}

// CanTransition reports whether from → to is legal.
//
// ACTIVE cannot go straight to DELISTED: a live market must pass through
// CLOSE_ONLY or HALTED first, so holders are either given the chance to exit
// or the halt is a recorded, reviewable decision.
func CanTransition(from, to Status) bool {
	for _, t := range statusTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// AllowsBuy reports whether new exposure may be opened.
func (s Status) AllowsBuy() bool { return s == StatusActive }

// AllowsSell reports whether holders may reduce or exit.
func (s Status) AllowsSell() bool { return s == StatusActive || s == StatusCloseOnly }

// Terminal reports whether no further transition is possible.
func (s Status) Terminal() bool { return s == StatusDelisted || s == StatusRejected }

// ModerationState is the content verdict on an asset's user-supplied fields.
type ModerationState string

// Moderation states.
const (
	ModerationPending  ModerationState = "PENDING"
	ModerationApproved ModerationState = "APPROVED"
	ModerationRejected ModerationState = "REJECTED"
	// ModerationFlagged is approved-with-concern: the asset may trade, and a
	// human is expected to look. It exists so that a borderline case is not
	// forced into either a block or a silent pass.
	ModerationFlagged ModerationState = "FLAGGED"
)

// Valid reports whether m is declared.
func (m ModerationState) Valid() bool {
	switch m {
	case ModerationPending, ModerationApproved, ModerationRejected, ModerationFlagged:
		return true
	}
	return false
}

// PermitsActivation reports whether an asset in this moderation state may go
// live. FLAGGED does, because it is a request for review and not a refusal.
func (m ModerationState) PermitsActivation() bool {
	return m == ModerationApproved || m == ModerationFlagged
}

// PolicyProfile is the per-asset legal and product position (PART XXXIV).
//
// Every field is an explicit stated position. There is no "unset": a fresh
// asset is created with the conservative profile, and moving any field
// requires a policy decision that is recorded.
type PolicyProfile struct {
	// InternalOnly is always true in this build and the database refuses
	// false. It is a field rather than an assumption so that the day it
	// becomes configurable, every caller already reads it.
	InternalOnly bool
	// Transferable is whether a holder may send units to another user
	// outside a market trade. Default false: peer transfer is how an internal
	// asset becomes a payment instrument.
	Transferable bool
	// CashoutEligible is whether proceeds from this asset may ever reach the
	// payout engine. Default false.
	CashoutEligible bool
	// CreatorEarningEligible is whether the creator's fee income from this
	// asset may reach the payout engine. Default false.
	CreatorEarningEligible bool
	// MarketProceedsEligible is whether a holder's sale proceeds may reach
	// the payout engine. Default false; this is the most legally sensitive
	// switch in the product.
	MarketProceedsEligible bool
	// MinimumAge is the age policy for holding this asset.
	MinimumAge int
	// JurisdictionPolicy names the machine-readable policy the legal router
	// applies. It is a name, not a conclusion.
	JurisdictionPolicy string
	// MarketingRestrictions names what may not be claimed about the asset.
	MarketingRestrictions string
}

// ConservativePolicy is the profile every new asset is created with: internal
// only, non-transferable, nothing cashable, adult, conservative jurisdiction
// handling, no return claims.
func ConservativePolicy() PolicyProfile {
	return PolicyProfile{
		InternalOnly:           true,
		Transferable:           false,
		CashoutEligible:        false,
		CreatorEarningEligible: false,
		MarketProceedsEligible: false,
		MinimumAge:             18,
		JurisdictionPolicy:     "DEFAULT_CONSERVATIVE",
		MarketingRestrictions:  "NO_RETURN_CLAIMS",
	}
}

// Validate checks the profile is one this build can honour.
func (p PolicyProfile) Validate() error {
	if !p.InternalOnly {
		return errs.New(errs.CodeUnsupported,
			"native assets are internal-only in this build; making one external is a legal decision, not a column write")
	}
	if p.MinimumAge < 0 || p.MinimumAge > 120 {
		return errs.New(errs.CodeValidationFailed, "minimum age is out of range")
	}
	if strings.TrimSpace(p.JurisdictionPolicy) == "" {
		return errs.New(errs.CodeValidationFailed, "a jurisdiction policy must be named, even if it is the default")
	}
	if strings.TrimSpace(p.MarketingRestrictions) == "" {
		return errs.New(errs.CodeValidationFailed, "marketing restrictions must be stated")
	}
	// Cashout eligibility is meaningless without the corresponding proceeds
	// switch, and shipping the pair inconsistent would let one look enabled
	// while the other governs.
	if (p.CreatorEarningEligible || p.MarketProceedsEligible) && !p.CashoutEligible {
		return errs.New(errs.CodeValidationFailed,
			"an asset whose earnings or proceeds are payout-eligible must itself be cashout-eligible")
	}
	return nil
}

// SupplyModel is how many units exist and who holds them at launch.
type SupplyModel struct {
	// MaxSupply is the total units ever minted, in base units. It is fixed at
	// creation and frozen at activation.
	MaxSupply money.Quantity
	// CreatorAllocation is minted directly to the creator, outside the curve.
	// It is visible on the market page precisely because it is the number a
	// buyer most needs to see (PART LIV).
	CreatorAllocation money.Quantity
	// TreasuryAllocation is minted to the platform, outside the curve.
	TreasuryAllocation money.Quantity
}

// PoolSupply is what the curve actually sells: everything not pre-allocated.
func (s SupplyModel) PoolSupply() money.Quantity {
	return s.MaxSupply.Sub(s.CreatorAllocation).Sub(s.TreasuryAllocation)
}

// MaxCreatorAllocationBPS caps a creator's pre-allocation at 20% of supply.
//
// There is no honest market in an asset whose creator holds most of it: the
// creator's own selling is then the dominant price signal. The cap is a
// product control and does not make anything legal that was not.
const MaxCreatorAllocationBPS money.BPS = 2_000

// Validate checks the supply model.
func (s SupplyModel) Validate() error {
	if s.MaxSupply.Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed, "max supply must be positive")
	}
	if s.CreatorAllocation.IsNegative() || s.TreasuryAllocation.IsNegative() {
		return errs.New(errs.CodeValidationFailed, "allocations cannot be negative")
	}
	allocated := s.CreatorAllocation.Add(s.TreasuryAllocation)
	if allocated.Cmp(s.MaxSupply) > 0 {
		return errs.New(errs.CodeValidationFailed, "allocations exceed max supply")
	}
	if s.PoolSupply().Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed,
			"allocations leave nothing for the market to sell")
	}
	cap := s.MaxSupply.MulBPS(MaxCreatorAllocationBPS, money.RoundDown)
	if s.CreatorAllocation.Cmp(cap) > 0 {
		return errs.Newf(errs.CodeValidationFailed,
			"creator allocation %s exceeds the %s cap of %s units; a market whose creator holds most of the supply is not a market",
			s.CreatorAllocation, MaxCreatorAllocationBPS, cap)
	}
	return nil
}

// Asset is a native_assets row joined with its registry identity.
type Asset struct {
	AssetID          assets.AssetID
	CreatorAccountID accounts.AccountID

	Name        string
	Symbol      string
	Description string
	ImageURL    string
	Metadata    map[string]any

	Status          Status
	Supply          SupplyModel
	Policy          PolicyProfile
	Moderation      ModerationState
	ModerationNotes string

	EconomicsLockedAt *time.Time
	ActivatedAt       *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// EconomicsFrozen reports whether the asset's supply, symbol and policy can
// still change.
func (a Asset) EconomicsFrozen() bool { return a.EconomicsLockedAt != nil }
