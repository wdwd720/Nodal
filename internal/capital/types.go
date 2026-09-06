package capital

import (
	"fmt"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// MaxReservationTTL bounds ReserveRequest.TTL. A reservation that could
// outlive a trading day is a bug in the caller, not a feature.
const MaxReservationTTL = 24 * time.Hour

// ReservationStatus is the reservation lifecycle state.
type ReservationStatus string

// Reservation statuses. ACTIVE is the only non-final state.
const (
	ReservationActive   ReservationStatus = "ACTIVE"
	ReservationConsumed ReservationStatus = "CONSUMED"
	ReservationReleased ReservationStatus = "RELEASED"
	ReservationExpired  ReservationStatus = "EXPIRED"
)

// Valid reports whether s is a declared status.
func (s ReservationStatus) Valid() bool {
	switch s {
	case ReservationActive, ReservationConsumed, ReservationReleased, ReservationExpired:
		return true
	}
	return false
}

// Final reports whether s is terminal.
func (s ReservationStatus) Final() bool { return s.Valid() && s != ReservationActive }

// Reservation mirrors one asset_reservations row: an exact quantity of one
// asset set aside for an intent, and (optionally) the USD budget it holds
// against a capital envelope.
type Reservation struct {
	ID               ReservationID
	AccountID        accounts.AccountID
	AssetID          assets.AssetID
	EnvelopeID       *EnvelopeID
	IntentID         string // uuid text; "" when not attached to an intent
	LockedByOrderID  string // uuid text; "" when no order holds the reservation
	ActorType        security.ActorType
	ActorID          string
	Quantity         money.Quantity
	ConsumedQuantity money.Quantity
	USD              money.USD
	ConsumedUSD      money.USD
	Status           ReservationStatus
	Reason           string
	ReleaseReason    string
	IdempotencyKey   string
	CorrelationID    string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	ConsumedAt       *time.Time
	ReleasedAt       *time.Time
}

// Remaining returns the unconsumed quantity (Quantity − ConsumedQuantity).
func (r Reservation) Remaining() money.Quantity { return r.Quantity.Sub(r.ConsumedQuantity) }

// RemainingUSD returns the unconsumed USD budget (USD − ConsumedUSD).
func (r Reservation) RemainingUSD() (money.USD, error) { return r.USD.Sub(r.ConsumedUSD) }

// IsLocked reports whether an order holds the reservation (expiry skips it).
func (r Reservation) IsLocked() bool { return r.LockedByOrderID != "" }

// ReservationTotals mirrors one asset_reservation_totals row: the row locked
// FOR UPDATE to serialize reservations for (account, asset). Reserved is
// Σ(quantity − consumed_quantity) over ACTIVE reservations.
type ReservationTotals struct {
	AccountID accounts.AccountID
	AssetID   assets.AssetID
	Reserved  money.Quantity
	Version   int64
	UpdatedAt time.Time
}

// WithdrawalHold mirrors one withdrawal_holds row. An active hold reduces
// the quantity available for reservation and withdrawal (PART 27).
type WithdrawalHold struct {
	ID         WithdrawalHoldID
	AccountID  accounts.AccountID
	AssetID    assets.AssetID
	Quantity   money.Quantity
	Reason     string
	DepositID  string // uuid text; "" when not tied to a deposit
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	ReleasedAt *time.Time
	ReleasedBy string
}

// ActiveAt reports whether the hold still binds at now: not released and
// either never expiring or expiring after now.
func (h WithdrawalHold) ActiveAt(now time.Time) bool {
	if h.ReleasedAt != nil {
		return false
	}
	return h.ExpiresAt == nil || h.ExpiresAt.After(now)
}

// EnvelopeStatus is the capital envelope lifecycle state (PART 24).
type EnvelopeStatus string

// Envelope statuses.
const (
	EnvelopeDraft     EnvelopeStatus = "DRAFT"
	EnvelopeActive    EnvelopeStatus = "ACTIVE"
	EnvelopePaused    EnvelopeStatus = "PAUSED"
	EnvelopeExhausted EnvelopeStatus = "EXHAUSTED"
	EnvelopeExpired   EnvelopeStatus = "EXPIRED"
	EnvelopeRevoked   EnvelopeStatus = "REVOKED"
)

// Valid reports whether s is a declared status.
func (s EnvelopeStatus) Valid() bool {
	switch s {
	case EnvelopeDraft, EnvelopeActive, EnvelopePaused, EnvelopeExhausted, EnvelopeExpired, EnvelopeRevoked:
		return true
	}
	return false
}

// Envelope mirrors one capital_envelopes row. Authority fields (Allocation,
// Max*, Allowed*, PolicyVersion, Status, EffectiveAt, ExpiresAt) change only
// through EnvelopeService. Available, Reserved and Deployed are the budget
// flow and always satisfy Available + Reserved + Deployed == Allocation.
type Envelope struct {
	ID                  EnvelopeID
	AccountID           accounts.AccountID
	AgentID             string // uuid text; the agents table lives in a later migration range
	StrategyVersionID   string // uuid text
	SettlementAssetID   assets.AssetID
	Allocation          money.USD
	Available           money.USD
	Reserved            money.USD
	Deployed            money.USD
	RealizedPnL         money.USD
	RealizedLoss        money.USD
	CurrentDrawdown     money.USD
	DailyLoss           money.USD
	DailyLossResetAt    time.Time
	MaxDailyLoss        money.USD
	MaxDrawdown         money.USD
	MaxSingleTrade      money.USD
	MaxPosition         money.USD
	AllowedInstruments  []string // uuid text
	AllowedAssetClasses []string
	AllowedVenues       []string
	MaxModelSpend       money.USD
	MaxDataSpend        money.USD
	MaxOrderRatePerHour int32
	PolicyVersion       string
	Status              EnvelopeStatus
	EffectiveAt         time.Time
	ExpiresAt           *time.Time
	Version             int64
	CreatedByActorType  security.ActorType
	CreatedByActorID    string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// ExpiredAt reports whether the validity window has closed at now.
func (e Envelope) ExpiredAt(now time.Time) bool {
	return e.ExpiresAt != nil && !e.ExpiresAt.After(now)
}

// UsableAt reports whether the envelope can back a new reservation at now:
// ACTIVE, effective, and not expired.
func (e Envelope) UsableAt(now time.Time) bool {
	return e.Status == EnvelopeActive && !e.EffectiveAt.After(now) && !e.ExpiredAt(now)
}

// Validate checks the structural invariants required to create an envelope.
// Budget flow fields must be at their initial values (Available ==
// Allocation, everything else zero); Create normalises an unset Available.
func (e Envelope) Validate() error {
	var problems []string
	if e.AccountID.IsZero() {
		problems = append(problems, "account_id required")
	}
	if _, err := id.ParseAny(e.AgentID); err != nil {
		problems = append(problems, "agent_id must be a canonical uuid")
	}
	if _, err := id.ParseAny(e.StrategyVersionID); err != nil {
		problems = append(problems, "strategy_version_id must be a canonical uuid")
	}
	if e.SettlementAssetID.IsZero() {
		problems = append(problems, "settlement_asset_id required")
	}
	if e.Allocation.IsNegative() {
		problems = append(problems, "allocation must not be negative")
	}
	if !e.Available.Equal(e.Allocation) {
		problems = append(problems, "available must equal allocation at creation")
	}
	for _, f := range []struct {
		name string
		v    money.USD
	}{
		{"reserved", e.Reserved},
		{"deployed", e.Deployed},
		{"realized_pnl", e.RealizedPnL},
		{"realized_loss", e.RealizedLoss},
		{"current_drawdown", e.CurrentDrawdown},
		{"daily_loss", e.DailyLoss},
	} {
		if !f.v.IsZero() {
			problems = append(problems, f.name+" must be zero at creation")
		}
	}
	for _, f := range []struct {
		name string
		v    money.USD
	}{
		{"max_daily_loss", e.MaxDailyLoss},
		{"max_drawdown", e.MaxDrawdown},
		{"max_single_trade", e.MaxSingleTrade},
		{"max_position", e.MaxPosition},
		{"max_model_spend", e.MaxModelSpend},
		{"max_data_spend", e.MaxDataSpend},
	} {
		if f.v.IsNegative() {
			problems = append(problems, f.name+" must not be negative")
		}
	}
	if e.MaxOrderRatePerHour < 0 {
		problems = append(problems, "max_order_rate_per_hour must not be negative")
	}
	problems = append(problems, validateAllowlists(e.AllowedInstruments, e.AllowedAssetClasses, e.AllowedVenues)...)
	if strings.TrimSpace(e.PolicyVersion) == "" {
		problems = append(problems, "policy_version required")
	}
	switch e.Status {
	case EnvelopeDraft, EnvelopeActive:
	default:
		problems = append(problems, fmt.Sprintf("status must be DRAFT or ACTIVE at creation, got %q", e.Status))
	}
	if e.EffectiveAt.IsZero() {
		problems = append(problems, "effective_at required")
	}
	if e.ExpiresAt != nil && !e.EffectiveAt.IsZero() && !e.ExpiresAt.After(e.EffectiveAt) {
		problems = append(problems, "expires_at must be after effective_at")
	}
	if e.CreatedByActorType == security.ActorAgent {
		problems = append(problems, "an agent cannot create an envelope")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid envelope").WithField("problems", problems)
	}
	return nil
}

// validateAllowlists checks the three allowlists; instruments must be uuids
// and every entry must be non-blank.
func validateAllowlists(instruments, classes, venues []string) []string {
	var problems []string
	for _, s := range instruments {
		if _, err := id.ParseAny(s); err != nil {
			problems = append(problems, "allowed_instruments entries must be canonical uuids")
			break
		}
	}
	for _, s := range classes {
		if strings.TrimSpace(s) == "" {
			problems = append(problems, "allowed_asset_classes entries must be non-blank")
			break
		}
	}
	for _, s := range venues {
		if strings.TrimSpace(s) == "" {
			problems = append(problems, "allowed_venues entries must be non-blank")
			break
		}
	}
	return problems
}

// EnvelopeAuthorityPatch names the authority fields an administrator may
// change through EnvelopeService.Update. A nil pointer leaves the field
// untouched. Status is not part of the patch: it changes through SetStatus
// so every status change is an explicit, reasoned transition. Budget flow
// fields (available, reserved, deployed) and P&L fields are never patchable.
//
// Reason and ApprovalID are change metadata recorded in the audit row, not
// envelope fields.
type EnvelopeAuthorityPatch struct {
	Allocation          *money.USD
	MaxDailyLoss        *money.USD
	MaxDrawdown         *money.USD
	MaxSingleTrade      *money.USD
	MaxPosition         *money.USD
	AllowedInstruments  *[]string
	AllowedAssetClasses *[]string
	AllowedVenues       *[]string
	MaxModelSpend       *money.USD
	MaxDataSpend        *money.USD
	MaxOrderRatePerHour *int32
	PolicyVersion       *string
	EffectiveAt         *time.Time
	ExpiresAt           *time.Time
	ClearExpiresAt      bool // remove the expiry; mutually exclusive with ExpiresAt

	Reason     string
	ApprovalID string // uuid text; optional
}

// IsEmpty reports whether the patch names no field.
func (p EnvelopeAuthorityPatch) IsEmpty() bool {
	return p.Allocation == nil && p.MaxDailyLoss == nil && p.MaxDrawdown == nil && p.MaxSingleTrade == nil &&
		p.MaxPosition == nil && p.AllowedInstruments == nil && p.AllowedAssetClasses == nil && p.AllowedVenues == nil &&
		p.MaxModelSpend == nil && p.MaxDataSpend == nil && p.MaxOrderRatePerHour == nil && p.PolicyVersion == nil &&
		p.EffectiveAt == nil && p.ExpiresAt == nil && !p.ClearExpiresAt
}

// Validate checks the patch in isolation (window consistency against the
// current envelope is checked when it is applied).
func (p EnvelopeAuthorityPatch) Validate() error {
	var problems []string
	if p.IsEmpty() {
		problems = append(problems, "patch names no field")
	}
	if strings.TrimSpace(p.Reason) == "" {
		problems = append(problems, "reason required")
	}
	if p.ApprovalID != "" {
		if _, err := id.ParseAny(p.ApprovalID); err != nil {
			problems = append(problems, "approval_id must be a canonical uuid")
		}
	}
	for _, f := range []struct {
		name string
		v    *money.USD
	}{
		{"allocation", p.Allocation},
		{"max_daily_loss", p.MaxDailyLoss},
		{"max_drawdown", p.MaxDrawdown},
		{"max_single_trade", p.MaxSingleTrade},
		{"max_position", p.MaxPosition},
		{"max_model_spend", p.MaxModelSpend},
		{"max_data_spend", p.MaxDataSpend},
	} {
		if f.v != nil && f.v.IsNegative() {
			problems = append(problems, f.name+" must not be negative")
		}
	}
	if p.MaxOrderRatePerHour != nil && *p.MaxOrderRatePerHour < 0 {
		problems = append(problems, "max_order_rate_per_hour must not be negative")
	}
	var instruments, classes, venues []string
	if p.AllowedInstruments != nil {
		instruments = *p.AllowedInstruments
	}
	if p.AllowedAssetClasses != nil {
		classes = *p.AllowedAssetClasses
	}
	if p.AllowedVenues != nil {
		venues = *p.AllowedVenues
	}
	problems = append(problems, validateAllowlists(instruments, classes, venues)...)
	if p.PolicyVersion != nil && strings.TrimSpace(*p.PolicyVersion) == "" {
		problems = append(problems, "policy_version must be non-blank")
	}
	if p.EffectiveAt != nil && p.EffectiveAt.IsZero() {
		problems = append(problems, "effective_at must be set")
	}
	if p.ExpiresAt != nil && p.ClearExpiresAt {
		problems = append(problems, "expires_at and clear_expires_at are mutually exclusive")
	}
	if p.ExpiresAt != nil && p.ExpiresAt.IsZero() {
		problems = append(problems, "expires_at must be set (use clear_expires_at to remove it)")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid envelope patch").WithField("problems", problems)
	}
	return nil
}

// ReserveRequest is the fixed input contract of Reserver.Reserve
// (FINANCIAL_MODEL §7).
type ReserveRequest struct {
	AccountID      string
	AssetID        assets.AssetID
	Quantity       money.Quantity
	USDMinor       int64
	EnvelopeID     *EnvelopeID
	IntentID       string
	ActorType      security.ActorType
	ActorID        string
	IdempotencyKey string
	TTL            time.Duration
	Reason         string
}

// Validate checks the request before any database work.
func (r ReserveRequest) Validate() error {
	var problems []string
	if _, err := accounts.ParseAccountID(r.AccountID); err != nil || r.AccountID == "" {
		problems = append(problems, "account_id must be a canonical uuid")
	}
	if r.AssetID.IsZero() {
		problems = append(problems, "asset_id required")
	}
	if !r.Quantity.IsPositive() {
		problems = append(problems, "quantity must be positive")
	}
	if r.USDMinor < 0 {
		problems = append(problems, "usd_minor must not be negative")
	}
	if r.EnvelopeID != nil && r.EnvelopeID.IsZero() {
		problems = append(problems, "envelope_id must be set when present")
	}
	if r.IntentID != "" {
		if _, err := id.ParseAny(r.IntentID); err != nil {
			problems = append(problems, "intent_id must be a canonical uuid")
		}
	}
	if !r.ActorType.Valid() {
		problems = append(problems, fmt.Sprintf("unknown actor type %q", r.ActorType))
	}
	if strings.TrimSpace(r.ActorID) == "" {
		problems = append(problems, "actor_id required")
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		problems = append(problems, "idempotency_key required")
	}
	if r.TTL <= 0 {
		problems = append(problems, "ttl must be positive")
	} else if r.TTL > MaxReservationTTL {
		problems = append(problems, "ttl exceeds "+MaxReservationTTL.String())
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid reserve request").WithField("problems", problems)
	}
	return nil
}

// Purpose and its constants are defined by the buying-power engine and
// re-exported from buyingpower_types.go.
