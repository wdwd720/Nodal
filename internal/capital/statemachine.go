package capital

import "github.com/nodal/controlplane/internal/errs"

// reservationTransitions is the explicit legal transition table. ACTIVE is
// the only state with successors; every other state is terminal.
var reservationTransitions = map[ReservationStatus][]ReservationStatus{
	ReservationActive:   {ReservationConsumed, ReservationReleased, ReservationExpired},
	ReservationConsumed: {},
	ReservationReleased: {},
	ReservationExpired:  {},
}

// CanTransitionReservation reports whether from → to is legal.
func CanTransitionReservation(from, to ReservationStatus) bool {
	for _, t := range reservationTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// checkReservationTransition returns INVALID_STATE_TRANSITION with from/to
// fields when the transition is illegal. It is the single gate every
// mutating reservation method passes through, so a released or expired
// reservation can never be consumed (PART 21).
func checkReservationTransition(r Reservation, to ReservationStatus) error {
	if CanTransitionReservation(r.Status, to) {
		return nil
	}
	return errs.Newf(errs.CodeInvalidStateTransition, "reservation is %s; %s -> %s is not allowed", r.Status, r.Status, to).
		WithField("reservation_id", r.ID.String()).
		WithField("from", string(r.Status)).
		WithField("to", string(to))
}

// envelopeTransitions is the explicit legal transition table for envelope
// status. ACTIVE → EXHAUSTED is also taken by ApplyRealizedPnL when a loss
// limit is hit; returning to ACTIVE additionally requires the limits to no
// longer be breached and the validity window to be open (see SetStatus).
var envelopeTransitions = map[EnvelopeStatus][]EnvelopeStatus{
	EnvelopeDraft:     {EnvelopeActive, EnvelopeRevoked},
	EnvelopeActive:    {EnvelopePaused, EnvelopeExhausted, EnvelopeExpired, EnvelopeRevoked},
	EnvelopePaused:    {EnvelopeActive, EnvelopeExpired, EnvelopeRevoked},
	EnvelopeExhausted: {EnvelopeActive, EnvelopeExpired, EnvelopeRevoked},
	EnvelopeExpired:   {EnvelopeActive, EnvelopeRevoked},
	EnvelopeRevoked:   {},
}

// CanTransitionEnvelope reports whether from → to is legal.
func CanTransitionEnvelope(from, to EnvelopeStatus) bool {
	for _, t := range envelopeTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}
