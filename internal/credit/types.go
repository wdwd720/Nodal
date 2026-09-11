package credit

import (
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

type (
	lotKind               struct{}
	lotEventKind          struct{}
	fundingKind           struct{}
	fundingTransitionKind struct{}
)

// LotID identifies a provenance lot.
type LotID = id.ID[lotKind]

// NewLotID returns a fresh lot id.
func NewLotID() LotID { return id.New[lotKind]() }

// ParseLotID parses the canonical form.
func ParseLotID(s string) (LotID, error) { return id.Parse[lotKind](s) }

// LotEventID identifies one recorded change to a lot.
type LotEventID = id.ID[lotEventKind]

// NewLotEventID returns a fresh lot event id.
func NewLotEventID() LotEventID { return id.New[lotEventKind]() }

// FundingID identifies a credit purchase.
type FundingID = id.ID[fundingKind]

// NewFundingID returns a fresh funding id.
func NewFundingID() FundingID { return id.New[fundingKind]() }

// ParseFundingID parses the canonical form.
func ParseFundingID(s string) (FundingID, error) { return id.Parse[fundingKind](s) }

// FundingTransitionID identifies one recorded funding state change.
type FundingTransitionID = id.ID[fundingTransitionKind]

// NewFundingTransitionID returns a fresh funding transition id.
func NewFundingTransitionID() FundingTransitionID { return id.New[fundingTransitionKind]() }

// Reference names what produced or consumed value, e.g. {"credit_funding",
// "<uuid>"} or {"native_fill", "<uuid>"}.
type Reference struct {
	Type string
	ID   string
}

// Valid reports whether both halves are present.
func (r Reference) Valid() bool { return r.Type != "" && r.ID != "" }

// Lot is an immutable record of Credits issued to one account from one source.
//
// Quantity never changes. Remaining and Finality are the projection maintained
// by the database from the lot's append-only event stream.
type Lot struct {
	ID        LotID
	AccountID accounts.AccountID
	AssetID   assets.AssetID
	Origin    valuedomain.CreditOrigin

	// Quantity is what was issued and is immutable.
	Quantity money.Quantity
	// Remaining is what has not yet been consumed.
	Remaining money.Quantity
	// Finality is the current funding finality of the lot.
	Finality valuedomain.FundingFinality
	// InitialFinality is what it was issued at, kept so that a lot that
	// started REVERSIBLE and later SETTLED can be told from one that was
	// issued SETTLED.
	InitialFinality valuedomain.FundingFinality

	FundingReference *Reference
	JournalTxID      ledger.TransactionID

	IssuedByActorType string
	IssuedByActorID   string
	Reason            string
	CreatedAt         time.Time
	Version           int64
}

// Spendable reports whether the lot's remaining units may fund new activity.
func (l Lot) Spendable() bool { return l.Finality.Spendable() && l.Remaining.IsPositive() }

// AgeDays is how many whole days the lot has existed at now. It is the input
// to a policy's MinHoldDays.
func (l Lot) AgeDays(now time.Time) int {
	d := now.Sub(l.CreatedAt)
	if d < 0 {
		return 0
	}
	return int(d / (24 * time.Hour))
}

// consumptionRank orders origins from most restricted to least.
//
// This is a structural property of what the value IS, not of what a policy
// currently permits. Ordering by current payout eligibility would make the
// same spend consume different lots before and after a policy change, so a
// provenance question asked twice could get two answers. Rank is therefore
// fixed here and changing it is a versioned decision (DECISION_REGISTER).
var consumptionRank = map[valuedomain.CreditOrigin]int{
	valuedomain.OriginPromotional:           0,
	valuedomain.OriginCompetitionReward:     1,
	valuedomain.OriginAdminAdjustment:       2,
	valuedomain.OriginRefund:                3,
	valuedomain.OriginPurchased:             4,
	valuedomain.OriginProviderSettlement:    5,
	valuedomain.OriginMarketTradingProceeds: 6,
	valuedomain.OriginMarketCreatorEarning:  7,
	valuedomain.OriginAgentServiceEarning:   8,
	valuedomain.OriginDataSaleEarning:       9,
	valuedomain.OriginCreatorEarning:        10,
}

// ConsumptionRank returns the ordering position of an origin. Lower is
// consumed first. An unknown origin sorts last, so a newly added origin is
// preserved rather than silently spent before anything else.
func ConsumptionRank(o valuedomain.CreditOrigin) int {
	r, ok := consumptionRank[o]
	if !ok {
		return len(consumptionRank)
	}
	return r
}

// ConsumptionOrderSQL is the ORDER BY that implements ConsumptionRank inside
// the lot selection query. It is generated from the same map so the two cannot
// disagree, and a test asserts they agree for every declared origin.
func ConsumptionOrderSQL() string { return consumptionOrderSQL }

// Allocation is how much of one lot a single consumption took.
type Allocation struct {
	LotID    LotID
	Origin   valuedomain.CreditOrigin
	Finality valuedomain.FundingFinality
	Quantity money.Quantity
	EventID  LotEventID
}

// IssueRequest mints Credits into an account with a recorded provenance.
type IssueRequest struct {
	AccountID accounts.AccountID
	Quantity  money.Quantity
	Origin    valuedomain.CreditOrigin
	// Finality is the funding finality the lot starts at. Promotional grants
	// and admin adjustments are UNFUNDED; a card purchase is REVERSIBLE until
	// the dispute window closes; an internal earning inherits the finality of
	// the value that paid for it, which is why the caller states it rather
	// than this package guessing.
	Finality valuedomain.FundingFinality

	// Reference is the financial event this issuance records.
	Reference Reference
	// FundingReference points at the credit_fundings row, internal commerce
	// order or competition that produced the value. Optional for grants.
	FundingReference *Reference

	IdempotencyKey string
	Reason         string
	EffectiveAt    time.Time
	CorrelationID  string
}

// Validate checks the request without touching the database.
func (r IssueRequest) Validate() error {
	if r.AccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "credit: issue requires an account id")
	}
	if r.Quantity.Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed, "credit: issue quantity must be positive")
	}
	if !r.Origin.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "credit: unknown origin %q", r.Origin)
	}
	if !r.Finality.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "credit: unknown funding finality %q", r.Finality)
	}
	if r.Finality == valuedomain.FinalityReversed {
		return errs.New(errs.CodeValidationFailed, "credit: cannot issue a lot that is already reversed")
	}
	if !r.Reference.Valid() {
		return errs.New(errs.CodeValidationFailed, "credit: issue requires a financial event reference")
	}
	if r.FundingReference != nil && !r.FundingReference.Valid() {
		return errs.New(errs.CodeValidationFailed, "credit: funding reference must have both a type and an id")
	}
	if r.IdempotencyKey == "" {
		return errs.New(errs.CodeValidationFailed, "credit: issue requires an idempotency key")
	}
	if r.EffectiveAt.IsZero() {
		return errs.New(errs.CodeValidationFailed, "credit: issue requires effective_at")
	}
	// A promotional grant that claims to be backed by settled external funding
	// would be payout-eligible the moment a policy allowed its origin. The
	// pairing is checked here because nothing downstream re-derives it.
	if r.Origin == valuedomain.OriginPromotional && r.Finality != valuedomain.FinalityUnfunded {
		return errs.New(errs.CodeValidationFailed,
			"credit: promotional Credits are UNFUNDED by definition; nothing external backs them")
	}
	if r.Origin == valuedomain.OriginPurchased && r.Finality == valuedomain.FinalityUnfunded {
		return errs.New(errs.CodeValidationFailed,
			"credit: purchased Credits are backed by a payment and cannot be UNFUNDED")
	}
	return nil
}

// RecordLotRequest records provenance for units an already-posted journal
// transaction moved into an account.
type RecordLotRequest struct {
	AccountID        accounts.AccountID
	Quantity         money.Quantity
	Origin           valuedomain.CreditOrigin
	Finality         valuedomain.FundingFinality
	Reference        Reference
	FundingReference *Reference
	// JournalTxID is the posting that moved the units. The database refuses a
	// lot whose transaction did not touch this account and asset.
	JournalTxID ledger.TransactionID
	Reason      string
}

// Validate checks the request without touching the database.
func (r RecordLotRequest) Validate() error {
	if r.AccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "credit: recording a lot requires an account id")
	}
	if r.Quantity.Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed, "credit: lot quantity must be positive")
	}
	if !r.Origin.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "credit: unknown origin %q", r.Origin)
	}
	if !r.Finality.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "credit: unknown funding finality %q", r.Finality)
	}
	if r.Finality == valuedomain.FinalityReversed {
		return errs.New(errs.CodeValidationFailed, "credit: cannot record a lot that is already reversed")
	}
	if r.Origin == valuedomain.OriginPromotional && r.Finality != valuedomain.FinalityUnfunded {
		return errs.New(errs.CodeValidationFailed,
			"credit: promotional Credits are UNFUNDED by definition; nothing external backs them")
	}
	if r.Origin == valuedomain.OriginPurchased && r.Finality == valuedomain.FinalityUnfunded {
		return errs.New(errs.CodeValidationFailed,
			"credit: purchased Credits are backed by a payment and cannot be UNFUNDED")
	}
	if !r.Reference.Valid() {
		return errs.New(errs.CodeValidationFailed, "credit: recording a lot requires a financial event reference")
	}
	if r.FundingReference != nil && !r.FundingReference.Valid() {
		return errs.New(errs.CodeValidationFailed, "credit: funding reference must have both a type and an id")
	}
	if r.JournalTxID.IsZero() {
		return errs.New(errs.CodeValidationFailed,
			"credit: recording a lot requires the journal transaction that moved the units")
	}
	return nil
}

// ConsumeRequest allocates a quantity across an account's open lots.
//
// It must run in the same database transaction as the ledger posting that
// moved the units, and JournalTxID must be that posting.
type ConsumeRequest struct {
	AccountID   accounts.AccountID
	Quantity    money.Quantity
	JournalTxID ledger.TransactionID
	Reference   Reference
	Reason      string

	// RequireSpendableFinality, when true, refuses to consume lots whose
	// funding is DISPUTED or REVERSED. It is the normal case; payout
	// reservation sets it and so does ordinary spending. It is false only for
	// a reversal, which must be able to claw back value regardless.
	RequireSpendableFinality bool

	// AllowedOrigins, when non-empty, restricts consumption to these origins.
	// Payout reservation uses it to consume only value the policy permits, so
	// that a payout can never quietly take a promotional grant.
	AllowedOrigins []valuedomain.CreditOrigin

	// LotIDs, when non-empty, restricts consumption to these exact lots.
	//
	// A clawback is the case it exists for. A chargeback reverses ONE funding,
	// and the units it must destroy are the units THAT funding minted -- not
	// whichever lots sort first in consumption order, which is what an
	// unrestricted Consume takes and which is a promotional grant every time
	// (F-152). The origin filter is not enough: two purchases produce two lots
	// of the same origin, and a chargeback of one must not destroy the other.
	LotIDs []LotID
}

// Validate checks the request without touching the database.
func (r ConsumeRequest) Validate() error {
	if r.AccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "credit: consume requires an account id")
	}
	if r.Quantity.Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed, "credit: consume quantity must be positive")
	}
	if r.JournalTxID.IsZero() {
		return errs.New(errs.CodeValidationFailed,
			"credit: consume requires the journal transaction that moved the units")
	}
	if !r.Reference.Valid() {
		return errs.New(errs.CodeValidationFailed, "credit: consume requires a financial event reference")
	}
	for _, o := range r.AllowedOrigins {
		if !o.Valid() {
			return errs.Newf(errs.CodeValidationFailed, "credit: unknown allowed origin %q", o)
		}
	}
	for i, l := range r.LotIDs {
		if l.IsZero() {
			return errs.Newf(errs.CodeValidationFailed, "credit: consume lot restriction %d has no lot id", i)
		}
	}
	return nil
}

// RestoreRequest returns previously consumed units to their original lots.
//
// Restoration is by allocation, not by quantity: a failed payout returns the
// exact units it reserved, to the exact lots they came from, so provenance
// survives a round trip. Returning "500 Credits" without saying which lots
// would let a user launder a promotional grant into an earning by reserving a
// payout and cancelling it.
type RestoreRequest struct {
	Allocations []Allocation
	JournalTxID ledger.TransactionID
	Reference   Reference
	Reason      string
}

// Validate checks the request without touching the database.
func (r RestoreRequest) Validate() error {
	if len(r.Allocations) == 0 {
		return errs.New(errs.CodeValidationFailed, "credit: restore requires at least one allocation")
	}
	for i, a := range r.Allocations {
		if a.LotID.IsZero() {
			return errs.Newf(errs.CodeValidationFailed, "credit: restore allocation %d has no lot id", i)
		}
		if a.Quantity.Sign() <= 0 {
			return errs.Newf(errs.CodeValidationFailed, "credit: restore allocation %d quantity must be positive", i)
		}
	}
	if r.JournalTxID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "credit: restore requires a journal transaction")
	}
	if !r.Reference.Valid() {
		return errs.New(errs.CodeValidationFailed, "credit: restore requires a financial event reference")
	}
	return nil
}

// Balances is the breakdown PART XX requires the product to be able to show,
// so that a user is never told "18,500 Credits = $185 withdrawable" unless
// policy actually says so.
type Balances struct {
	// Gross is every remaining unit the account holds, regardless of state.
	Gross money.Quantity
	// Spendable is what can fund new internal activity now.
	Spendable money.Quantity
	// Frozen is value whose funding is under dispute.
	Frozen money.Quantity
	// Reversed is value whose funding was clawed back and which still has
	// remaining units recorded — a state that should be transient and is worth
	// surfacing when it is not.
	Reversed money.Quantity
	// PayoutEligible is what the supplied policy, verification level and
	// active capabilities together permit to be withdrawn right now.
	PayoutEligible money.Quantity
	// Ineligible is Gross minus PayoutEligible.
	Ineligible money.Quantity

	// ByOrigin and ByFinality break Gross down. They exist for operator tools
	// and for the user-facing explanation of WHY an amount is not withdrawable.
	ByOrigin   map[valuedomain.CreditOrigin]money.Quantity
	ByFinality map[valuedomain.FundingFinality]money.Quantity

	// IneligibleReasons counts how many distinct lots were refused for each
	// reason, so "why can I not withdraw" has a real answer.
	IneligibleReasons map[valuedomain.PermitReason]int

	// PolicyVersion and PolicyHash record which rules produced this answer.
	PolicyVersion string
	PolicyHash    string
}

// BalanceRequest is everything Balances needs. Nothing is discovered: the
// policy, the clock and the capability set are supplied by the caller, so the
// same inputs always produce the same breakdown.
type BalanceRequest struct {
	AccountID  accounts.AccountID
	Policy     valuedomain.Policy
	Verified   valuedomain.VerificationLevel
	ActiveCaps map[valuedomain.CapabilityKey]bool
	Now        time.Time
}
