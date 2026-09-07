package nativemarket

import (
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

type marketKind struct{}
type quoteKind struct{}
type fillKind struct{}
type transitionKind struct{}
type alertKind struct{}

// MarketID identifies a native market.
type MarketID = id.ID[marketKind]

// NewMarketID returns a fresh market id.
func NewMarketID() MarketID { return id.New[marketKind]() }

// ParseMarketID parses the canonical form.
func ParseMarketID(s string) (MarketID, error) { return id.Parse[marketKind](s) }

// QuoteID identifies a recorded quote.
type QuoteID = id.ID[quoteKind]

// NewQuoteID returns a fresh quote id.
func NewQuoteID() QuoteID { return id.New[quoteKind]() }

// ParseQuoteID parses the canonical form.
func ParseQuoteID(s string) (QuoteID, error) { return id.Parse[quoteKind](s) }

// FillID identifies an executed trade.
type FillID = id.ID[fillKind]

// NewFillID returns a fresh fill id.
func NewFillID() FillID { return id.New[fillKind]() }

// TransitionID identifies a market status change.
type TransitionID = id.ID[transitionKind]

// NewTransitionID returns a fresh transition id.
func NewTransitionID() TransitionID { return id.New[transitionKind]() }

// AlertID identifies a surveillance alert.
type AlertID = id.ID[alertKind]

// NewAlertID returns a fresh alert id.
func NewAlertID() AlertID { return id.New[alertKind]() }

// Status is a market's trading state.
type Status string

// Market statuses.
const (
	// StatusPending is created but not yet open. Supply has been minted into
	// the pool; nobody can trade.
	StatusPending Status = "PENDING"
	StatusActive  Status = "ACTIVE"
	// StatusCloseOnly accepts sells and refuses buys, so holders can leave.
	StatusCloseOnly Status = "CLOSE_ONLY"
	// StatusHalted refuses everything. It traps holders, so it is for
	// investigations and is meant to be short.
	StatusHalted Status = "HALTED"
	// StatusFrozen is a stronger halt used during a dispute or an economic
	// incident, distinguished from HALTED so that the reason a market stopped
	// is visible in its status rather than only in a transition row.
	StatusFrozen   Status = "FROZEN"
	StatusDelisted Status = "DELISTED"
)

var allStatuses = []Status{
	StatusPending, StatusActive, StatusCloseOnly, StatusHalted, StatusFrozen, StatusDelisted,
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

// Accepts reports whether the market takes a trade on this side.
func (s Status) Accepts(side Side) bool {
	switch s {
	case StatusActive:
		return side == Buy || side == Sell
	case StatusCloseOnly:
		return side == Sell
	default:
		return false
	}
}

var statusTransitions = map[Status][]Status{
	StatusPending:   {StatusActive, StatusDelisted},
	StatusActive:    {StatusCloseOnly, StatusHalted, StatusFrozen},
	StatusCloseOnly: {StatusActive, StatusHalted, StatusFrozen, StatusDelisted},
	StatusHalted:    {StatusActive, StatusCloseOnly, StatusFrozen, StatusDelisted},
	StatusFrozen:    {StatusHalted, StatusCloseOnly, StatusDelisted},
	StatusDelisted:  {},
}

// CanTransition reports whether from → to is legal.
//
// FROZEN cannot go straight back to ACTIVE: an economic incident has to be
// stepped down through HALTED or CLOSE_ONLY, so resuming full trading is
// always a second, separate decision.
func CanTransition(from, to Status) bool {
	for _, t := range statusTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Market is a native_markets row together with its curve and fees.
type Market struct {
	ID            MarketID
	AssetID       assets.AssetID
	CreditAssetID assets.AssetID

	Curve Curve
	Fees  Fees

	Status      Status
	ActivatedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Quote is a recorded price observation. It is evidence, not a promise.
type Quote struct {
	ID              QuoteID
	MarketID        MarketID
	AccountID       accounts.AccountID
	Side            Side
	InputAmount     money.Quantity
	ExpectedOutput  money.Quantity
	PlatformFee     money.Quantity
	CreatorFee      money.Quantity
	SpotPriceBefore money.Quantity
	EffectivePrice  money.Quantity
	SlippageBPS     money.BPS
	StateVersion    int64
	ExpiresAt       time.Time
	CreatedAt       time.Time
}

// Expired reports whether the quote is past its expiry at now.
func (q Quote) Expired(now time.Time) bool { return !now.Before(q.ExpiresAt) }

// QuoteTTL is how long a quote stays presentable.
//
// Thirty seconds is long enough for a person to read a confirmation screen and
// short enough that the price they are reading is still roughly the price. It
// bounds nothing economically — execution re-prices regardless — but a stale
// quote shown as current is its own kind of dishonesty.
const QuoteTTL = 30 * time.Second

// CreateRequest opens a market for an already-drafted native asset.
type CreateRequest struct {
	AssetID       assets.AssetID
	CreditAssetID assets.AssetID
	CreatorID     accounts.AccountID

	// PoolSupply is what the curve sells: max supply less any allocations.
	PoolSupply money.Quantity
	// CreatorAllocation is minted directly to the creator, outside the curve.
	CreatorAllocation money.Quantity
	// TreasuryAllocation is minted to the platform, outside the curve.
	TreasuryAllocation money.Quantity

	// VirtualCreditReserve sets the opening price: the first unit costs
	// roughly VirtualCreditReserve / PoolSupply Credits.
	VirtualCreditReserve money.Quantity
	Fees                 Fees

	IdempotencyKey string
	EffectiveAt    time.Time
}

// Validate checks the request without touching the database.
func (r CreateRequest) Validate() error {
	if r.AssetID.IsZero() || r.CreditAssetID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "a market needs both its asset and the Credit asset")
	}
	if r.CreatorID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "a market needs its asset's creator")
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return errs.New(errs.CodeValidationFailed, "a market creation needs an idempotency key")
	}
	if r.EffectiveAt.IsZero() {
		return errs.New(errs.CodeValidationFailed, "a market creation needs effective_at")
	}
	if r.CreatorAllocation.IsNegative() || r.TreasuryAllocation.IsNegative() {
		return errs.New(errs.CodeValidationFailed, "allocations cannot be negative")
	}
	c := Curve{VirtualCreditReserve: r.VirtualCreditReserve, InitialAssetReserve: r.PoolSupply}
	if err := c.Validate(); err != nil {
		return err
	}
	return r.Fees.Validate()
}

// TotalSupply is everything minted at creation.
func (r CreateRequest) TotalSupply() money.Quantity {
	return r.PoolSupply.Add(r.CreatorAllocation).Add(r.TreasuryAllocation)
}

// QuoteRequest asks what a trade would do right now.
type QuoteRequest struct {
	MarketID  MarketID
	AccountID accounts.AccountID
	Side      Side
	// Amount is Credits for a BUY and asset units for a SELL.
	Amount money.Quantity
}

// Validate checks the request.
func (r QuoteRequest) Validate() error {
	if r.MarketID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "a quote needs a market")
	}
	if r.AccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "a quote needs an account")
	}
	if !r.Side.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "unknown side %q", r.Side)
	}
	if r.Amount.Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed, "a quote amount must be positive")
	}
	return nil
}

// ExecuteRequest is an order against a market.
type ExecuteRequest struct {
	MarketID  MarketID
	AccountID accounts.AccountID
	Side      Side
	Amount    money.Quantity

	// MinOutput is the caller's slippage protection: the trade is refused if
	// it would return less than this. It is checked against the freshly
	// computed fill, not against whatever a quote once said, because that is
	// the number the user actually agreed to.
	MinOutput money.Quantity

	// QuoteID optionally links the order to the quote the user was shown. It
	// is recorded for audit; it never supplies a price.
	QuoteID *QuoteID

	IdempotencyKey string
	EffectiveAt    time.Time
	CorrelationID  string
}

// Validate checks the request.
func (r ExecuteRequest) Validate() error {
	if r.MarketID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "an order needs a market")
	}
	if r.AccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "an order needs an account")
	}
	if !r.Side.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "unknown side %q", r.Side)
	}
	if r.Amount.Sign() <= 0 {
		return errs.New(errs.CodeValidationFailed, "an order amount must be positive")
	}
	if r.MinOutput.IsNegative() {
		return errs.New(errs.CodeValidationFailed, "minimum output cannot be negative")
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return errs.New(errs.CodeValidationFailed, "an order needs an idempotency key")
	}
	if r.EffectiveAt.IsZero() {
		return errs.New(errs.CodeValidationFailed, "an order needs effective_at")
	}
	return nil
}

// ExecuteResult is what a completed trade did.
type ExecuteResult struct {
	FillID   FillID
	MarketID MarketID
	Fill     Fill
	// Alerts are surveillance findings raised by this trade. They never block
	// it; they are recorded and surfaced to operators.
	Alerts []Alert
	// Existing is true when the idempotency key had already been used and this
	// is the original trade rather than a new one.
	Existing bool
}
