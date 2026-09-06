package capital

import "github.com/nodal/controlplane/internal/id"

type (
	reservationKind    struct{}
	envelopeKind       struct{}
	withdrawalHoldKind struct{}
)

// ReservationID identifies an asset reservation.
type ReservationID = id.ID[reservationKind]

// EnvelopeID identifies a capital envelope.
type EnvelopeID = id.ID[envelopeKind]

// WithdrawalHoldID identifies a withdrawal hold.
type WithdrawalHoldID = id.ID[withdrawalHoldKind]

// NewReservationID returns a fresh reservation id.
func NewReservationID() ReservationID { return id.New[reservationKind]() }

// ParseReservationID parses the canonical form.
func ParseReservationID(s string) (ReservationID, error) { return id.Parse[reservationKind](s) }

// NewEnvelopeID returns a fresh envelope id.
func NewEnvelopeID() EnvelopeID { return id.New[envelopeKind]() }

// ParseEnvelopeID parses the canonical form.
func ParseEnvelopeID(s string) (EnvelopeID, error) { return id.Parse[envelopeKind](s) }

// NewWithdrawalHoldID returns a fresh hold id.
func NewWithdrawalHoldID() WithdrawalHoldID { return id.New[withdrawalHoldKind]() }

// ParseWithdrawalHoldID parses the canonical form.
func ParseWithdrawalHoldID(s string) (WithdrawalHoldID, error) {
	return id.Parse[withdrawalHoldKind](s)
}
