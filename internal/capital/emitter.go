package capital

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/money"
)

// Emitter appends a domain event to the transactional outbox inside the
// caller's transaction. internal/event's Outbox satisfies it through a thin
// adapter; tests use a recording fake. An Emit failure fails the whole
// state change: a capital event is never lost or emitted without its state.
type Emitter interface {
	Emit(ctx context.Context, tx pgx.Tx, topic string, payload any) error
}

// Outbox topics emitted by this package.
const (
	TopicReservationCreated  = "capital.reservation.created"
	TopicReservationConsumed = "capital.reservation.consumed"
	TopicReservationReleased = "capital.reservation.released"
	TopicReservationExpired  = "capital.reservation.expired"
	TopicReservationLocked   = "capital.reservation.locked"

	TopicHoldPlaced   = "capital.hold.placed"
	TopicHoldReleased = "capital.hold.released"

	TopicEnvelopeCreated       = "capital.envelope.created"
	TopicEnvelopeUpdated       = "capital.envelope.updated"
	TopicEnvelopeStatusChanged = "capital.envelope.status_changed"
	TopicEnvelopePnLApplied    = "capital.envelope.pnl_applied"
	TopicEnvelopeExhausted     = "capital.envelope.exhausted"
	TopicEnvelopeUndeployed    = "capital.envelope.undeployed"
)

// ReservationEvent is the payload of every capital.reservation.* event.
type ReservationEvent struct {
	ReservationID    string            `json:"reservation_id"`
	AccountID        string            `json:"account_id"`
	AssetID          string            `json:"asset_id"`
	EnvelopeID       string            `json:"envelope_id,omitempty"`
	IntentID         string            `json:"intent_id,omitempty"`
	OrderID          string            `json:"order_id,omitempty"`
	Status           ReservationStatus `json:"status"`
	Quantity         money.Quantity    `json:"quantity"`
	ConsumedQuantity money.Quantity    `json:"consumed_quantity"`
	USD              money.USD         `json:"usd"`
	ConsumedUSD      money.USD         `json:"consumed_usd"`
	Reason           string            `json:"reason,omitempty"`
	ExpiresAt        time.Time         `json:"expires_at"`
	OccurredAt       time.Time         `json:"occurred_at"`
}

func newReservationEvent(r Reservation, reason string, at time.Time) ReservationEvent {
	ev := ReservationEvent{
		ReservationID:    r.ID.String(),
		AccountID:        r.AccountID.String(),
		AssetID:          r.AssetID.String(),
		IntentID:         r.IntentID,
		OrderID:          r.LockedByOrderID,
		Status:           r.Status,
		Quantity:         r.Quantity,
		ConsumedQuantity: r.ConsumedQuantity,
		USD:              r.USD,
		ConsumedUSD:      r.ConsumedUSD,
		Reason:           reason,
		ExpiresAt:        r.ExpiresAt,
		OccurredAt:       at,
	}
	if r.EnvelopeID != nil {
		ev.EnvelopeID = r.EnvelopeID.String()
	}
	return ev
}

// HoldEvent is the payload of capital.hold.* events.
type HoldEvent struct {
	HoldID     string         `json:"hold_id"`
	AccountID  string         `json:"account_id"`
	AssetID    string         `json:"asset_id"`
	Quantity   money.Quantity `json:"quantity"`
	Reason     string         `json:"reason"`
	DepositID  string         `json:"deposit_id,omitempty"`
	ExpiresAt  *time.Time     `json:"expires_at,omitempty"`
	ReleasedBy string         `json:"released_by,omitempty"`
	OccurredAt time.Time      `json:"occurred_at"`
}

func newHoldEvent(h WithdrawalHold, at time.Time) HoldEvent {
	return HoldEvent{
		HoldID: h.ID.String(), AccountID: h.AccountID.String(), AssetID: h.AssetID.String(),
		Quantity: h.Quantity, Reason: h.Reason, DepositID: h.DepositID, ExpiresAt: h.ExpiresAt,
		ReleasedBy: h.ReleasedBy, OccurredAt: at,
	}
}

// FieldChange is one entry of a capital_envelope_changes row: {from, to}.
type FieldChange struct {
	From any `json:"from"`
	To   any `json:"to"`
}

// EnvelopeEvent is the payload of every capital.envelope.* event. Changes
// is present on updated / status_changed / exhausted events.
type EnvelopeEvent struct {
	EnvelopeID      string                 `json:"envelope_id"`
	AccountID       string                 `json:"account_id"`
	AgentID         string                 `json:"agent_id"`
	Status          EnvelopeStatus         `json:"status"`
	Allocation      money.USD              `json:"allocation"`
	Available       money.USD              `json:"available"`
	Reserved        money.USD              `json:"reserved"`
	Deployed        money.USD              `json:"deployed"`
	RealizedPnL     money.USD              `json:"realized_pnl"`
	DailyLoss       money.USD              `json:"daily_loss"`
	CurrentDrawdown money.USD              `json:"current_drawdown"`
	PolicyVersion   string                 `json:"policy_version"`
	Version         int64                  `json:"version"`
	Changes         map[string]FieldChange `json:"changes,omitempty"`
	Reason          string                 `json:"reason,omitempty"`
	OccurredAt      time.Time              `json:"occurred_at"`
}

func newEnvelopeEvent(e Envelope, changes map[string]FieldChange, reason string, at time.Time) EnvelopeEvent {
	return EnvelopeEvent{
		EnvelopeID: e.ID.String(), AccountID: e.AccountID.String(), AgentID: e.AgentID, Status: e.Status,
		Allocation: e.Allocation, Available: e.Available, Reserved: e.Reserved, Deployed: e.Deployed,
		RealizedPnL: e.RealizedPnL, DailyLoss: e.DailyLoss, CurrentDrawdown: e.CurrentDrawdown,
		PolicyVersion: e.PolicyVersion, Version: e.Version, Changes: changes, Reason: reason, OccurredAt: at,
	}
}
