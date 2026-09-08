// Package commerce is the creator economy: users selling data, agent services,
// compute, research and templates to each other for Credits (gola.md PART XVII).
//
// # The one requirement that matters
//
// PART XVII asks for one thing above the rest: revenue earned by creator
// activity must have provenance distinct from speculative trading proceeds.
// That sentence is what the whole payout architecture rests on. Creator
// earnings are the origin most likely to be the FIRST thing a provider and
// counsel permit to be withdrawn, precisely because the user supplied
// something real for them; speculative proceeds are the last. If both arrived
// as undifferentiated "Credits", the distinction could never be made
// afterwards.
//
// So a product's Kind determines the provenance of the seller's earning, the
// mapping is fixed rather than a runtime choice, and the resulting origin is
// stored on the order rather than re-derived later.
//
// # A sale is atomic and has no lifecycle
//
// Credits move and the entitlement exists, in one transaction. There is no
// escrow, no delivery state machine and no dispute flow, and their absence is
// deliberate: holding value in a state nobody has decided the legal character
// of is worse than not holding it, and PART XVII does not ask for one.
//
// # What this package must never do
//
//   - Let an account buy from itself. That would manufacture withdrawable
//     provenance out of non-withdrawable Credits, which is the single most
//     valuable thing an attacker could do to this system. The database refuses
//     it; so does this package, before the database is reached.
//   - Let a seller change a published product's price, fee or kind. A buyer
//     agreed to terms.
//   - Infer an earning's provenance from anything but the product kind.
//   - Move Credits without a journal transaction in the same database
//     transaction.
package commerce

import (
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

type productKindTag struct{}
type orderKindTag struct{}

// ProductID identifies an internal product.
type ProductID = id.ID[productKindTag]

// NewProductID returns a fresh product id.
func NewProductID() ProductID { return id.New[productKindTag]() }

// ParseProductID parses the canonical form.
func ParseProductID(s string) (ProductID, error) { return id.Parse[productKindTag](s) }

// OrderID identifies an internal commerce order.
type OrderID = id.ID[orderKindTag]

// NewOrderID returns a fresh order id.
func NewOrderID() OrderID { return id.New[orderKindTag]() }

// ParseOrderID parses the canonical form.
func ParseOrderID(s string) (OrderID, error) { return id.Parse[orderKindTag](s) }

// Kind is what is being sold. It is the only input to the provenance decision.
type Kind string

// Product kinds.
const (
	// KindData is a dataset or a data feed.
	KindData Kind = "DATA"
	// KindAgentService is an agent performing work for another user.
	KindAgentService Kind = "AGENT_SERVICE"
	// KindCompute is inference or simulation capacity.
	KindCompute Kind = "COMPUTE"
	// KindStrategyTemplate is a reusable Strategy IR.
	KindStrategyTemplate Kind = "STRATEGY_TEMPLATE"
	// KindResearch is written analysis.
	KindResearch Kind = "RESEARCH"
	// KindAPIAccess is programmatic access to something the seller runs.
	KindAPIAccess Kind = "API_ACCESS"
	// KindCompetitionEntry is entry into a platform competition.
	KindCompetitionEntry Kind = "COMPETITION_ENTRY"
	// KindCreatorProduct is anything else a creator sells.
	KindCreatorProduct Kind = "CREATOR_PRODUCT"
)

var allKinds = []Kind{
	KindData, KindAgentService, KindCompute, KindStrategyTemplate,
	KindResearch, KindAPIAccess, KindCompetitionEntry, KindCreatorProduct,
}

// AllKinds returns every declared kind in declaration order (a copy).
func AllKinds() []Kind { return append([]Kind(nil), allKinds...) }

// Valid reports whether k is declared.
func (k Kind) Valid() bool {
	for _, x := range allKinds {
		if x == k {
			return true
		}
	}
	return false
}

func (k Kind) String() string { return string(k) }

// earningOrigin maps a product kind to the provenance the seller's Credits
// carry. This is the load-bearing table of the package.
//
// The three origins are separate because a payout policy will almost certainly
// treat them differently: selling a dataset and running an agent for somebody
// are different activities with different regulatory shapes, and collapsing
// them into one origin would force any future determination to cover both or
// neither.
var earningOrigin = map[Kind]valuedomain.CreditOrigin{
	KindData:             valuedomain.OriginDataSaleEarning,
	KindAgentService:     valuedomain.OriginAgentServiceEarning,
	KindCompute:          valuedomain.OriginCreatorEarning,
	KindStrategyTemplate: valuedomain.OriginCreatorEarning,
	KindResearch:         valuedomain.OriginCreatorEarning,
	KindAPIAccess:        valuedomain.OriginCreatorEarning,
	KindCompetitionEntry: valuedomain.OriginCreatorEarning,
	KindCreatorProduct:   valuedomain.OriginCreatorEarning,
}

// EarningOrigin returns the provenance a sale of this kind produces, and
// whether the kind is known.
//
// An unknown kind produces no origin rather than a default. A default would
// mean a new product kind silently inherited a payout treatment nobody chose
// for it, which is exactly how an origin ends up withdrawable by accident.
func EarningOrigin(k Kind) (valuedomain.CreditOrigin, bool) {
	o, ok := earningOrigin[k]
	return o, ok
}

// Status is a product's lifecycle state.
type Status string

// Product statuses.
const (
	StatusDraft     Status = "DRAFT"
	StatusActive    Status = "ACTIVE"
	StatusPaused    Status = "PAUSED"
	StatusWithdrawn Status = "WITHDRAWN"
)

var allStatuses = []Status{StatusDraft, StatusActive, StatusPaused, StatusWithdrawn}

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

// Sellable reports whether the product may be bought right now.
func (s Status) Sellable() bool { return s == StatusActive }

var statusTransitions = map[Status][]Status{
	StatusDraft:     {StatusActive, StatusWithdrawn},
	StatusActive:    {StatusPaused, StatusWithdrawn},
	StatusPaused:    {StatusActive, StatusWithdrawn},
	StatusWithdrawn: {},
}

// CanTransition reports whether from → to is legal. WITHDRAWN is terminal: a
// product that has been taken down and can come back is PAUSED, and the two
// mean different things to a buyer looking at their purchase history.
func CanTransition(from, to Status) bool {
	for _, t := range statusTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// SellerStatus is whether an account may sell.
type SellerStatus string

// Seller statuses.
const (
	SellerActive    SellerStatus = "ACTIVE"
	SellerSuspended SellerStatus = "SUSPENDED"
	SellerClosed    SellerStatus = "CLOSED"
)

// Valid reports whether s is declared.
func (s SellerStatus) Valid() bool {
	switch s {
	case SellerActive, SellerSuspended, SellerClosed:
		return true
	}
	return false
}

// CanSell reports whether an account in this state may take new orders.
func (s SellerStatus) CanSell() bool { return s == SellerActive }

// Seller is an account that has agreed to sell.
type Seller struct {
	AccountID       accounts.AccountID
	DisplayName     string
	Status          SellerStatus
	PayoutAccountID *accounts.AccountID
	SuspendedReason string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// EarningAccount is where this seller's earnings are attributed: the payout
// account when one is set, otherwise the selling account itself.
func (s Seller) EarningAccount() accounts.AccountID {
	if s.PayoutAccountID != nil {
		return *s.PayoutAccountID
	}
	return s.AccountID
}

// MaxPlatformFeeBPS caps the platform's share at 30%. A marketplace that can
// take more is not a marketplace, and a creator cannot be asked to agree to a
// share that leaves the transaction pointless.
const MaxPlatformFeeBPS money.BPS = 3_000

// Product is something for sale.
type Product struct {
	ID              ProductID
	SellerAccountID accounts.AccountID
	Kind            Kind
	Title           string
	Description     string
	Price           money.Quantity
	Version         int
	PlatformFeeBPS  money.BPS
	Status          Status
	Metadata        map[string]any
	PublishedAt     *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// TermsFrozen reports whether the product's price, fee and kind are fixed.
func (p Product) TermsFrozen() bool { return p.PublishedAt != nil }

// Split divides a price into the platform's fee and the seller's proceeds.
//
// The fee rounds DOWN, so the seller is never short by a rounding unit and the
// two halves always add back to exactly the price. The alternative — rounding
// the fee up — would take a sub-unit from a creator on every small sale, which
// over a marketplace's worth of transactions is a real transfer disguised as
// arithmetic.
func (p Product) Split() (fee, proceeds money.Quantity, err error) {
	if p.Price.Sign() <= 0 {
		return money.Quantity{}, money.Quantity{}, errs.New(errs.CodeValidationFailed,
			"a product price must be positive")
	}
	fee = p.Price.MulBPS(p.PlatformFeeBPS, money.RoundDown)
	proceeds = p.Price.Sub(fee)
	if proceeds.IsNegative() {
		return money.Quantity{}, money.Quantity{}, errs.New(errs.CodeInternal,
			"the platform fee exceeded the price")
	}
	return fee, proceeds, nil
}

// Validate checks a product definition.
func (p Product) Validate() error {
	var problems []string
	if p.SellerAccountID.IsZero() {
		problems = append(problems, "a product needs a seller")
	}
	if !p.Kind.Valid() {
		problems = append(problems, "unknown product kind "+string(p.Kind))
	} else if _, ok := EarningOrigin(p.Kind); !ok {
		// Unreachable while the table covers every kind, and checked anyway:
		// a kind with no declared provenance must never reach a sale.
		problems = append(problems, "product kind "+string(p.Kind)+" has no declared earning provenance")
	}
	if strings.TrimSpace(p.Title) == "" {
		problems = append(problems, "a product needs a title")
	}
	if len(p.Title) > 200 {
		problems = append(problems, "a title may be at most 200 characters")
	}
	if len(p.Description) > 5_000 {
		problems = append(problems, "a description may be at most 5,000 characters")
	}
	if p.Price.Sign() <= 0 {
		problems = append(problems, "a price must be positive")
	}
	if p.PlatformFeeBPS < 0 || p.PlatformFeeBPS > MaxPlatformFeeBPS {
		problems = append(problems, "the platform fee must be between 0 and 30%")
	}
	if p.Version < 1 {
		problems = append(problems, "a product version starts at 1")
	}
	if !p.Status.Valid() {
		problems = append(problems, "unknown status "+string(p.Status))
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid product definition").
			WithField("problems", problems)
	}
	return nil
}

// Order is a completed purchase.
type Order struct {
	ID               OrderID
	ProductID        ProductID
	ProductVersion   int
	BuyerAccountID   accounts.AccountID
	SellerAccountID  accounts.AccountID
	EarningAccountID accounts.AccountID

	Price          money.Quantity
	PlatformFee    money.Quantity
	SellerProceeds money.Quantity
	EarningOrigin  valuedomain.CreditOrigin

	JournalTxID    ledger.TransactionID
	IdempotencyKey string
	CreatedAt      time.Time
}

// PurchaseRequest is a buy.
type PurchaseRequest struct {
	ProductID      ProductID
	BuyerAccountID accounts.AccountID
	// ExpectedPrice is what the buyer was shown. The purchase is refused if it
	// no longer matches, so a price that changed between the listing and the
	// click is a refusal rather than a surprise charge.
	ExpectedPrice  money.Quantity
	IdempotencyKey string
	EffectiveAt    time.Time
	CorrelationID  string
}

// Validate checks the request without touching the database.
func (r PurchaseRequest) Validate() error {
	if r.ProductID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "a purchase needs a product")
	}
	if r.BuyerAccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "a purchase needs a buyer")
	}
	if r.ExpectedPrice.Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed, "a purchase must state the price the buyer agreed to")
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return errs.New(errs.CodeValidationFailed, "a purchase needs an idempotency key")
	}
	if r.EffectiveAt.IsZero() {
		return errs.New(errs.CodeValidationFailed, "a purchase needs effective_at")
	}
	return nil
}
