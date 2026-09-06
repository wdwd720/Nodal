package capital

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

var allReservationStatuses = []ReservationStatus{ReservationActive, ReservationConsumed, ReservationReleased, ReservationExpired}

func TestReservationTransitions_OnlyActiveHasSuccessors(t *testing.T) {
	legal := map[[2]ReservationStatus]bool{
		{ReservationActive, ReservationConsumed}: true,
		{ReservationActive, ReservationReleased}: true,
		{ReservationActive, ReservationExpired}:  true,
	}
	for _, from := range allReservationStatuses {
		for _, to := range allReservationStatuses {
			want := legal[[2]ReservationStatus{from, to}]
			assert.Equalf(t, want, CanTransitionReservation(from, to), "%s -> %s", from, to)
		}
	}
	assert.False(t, CanTransitionReservation("BOGUS", ReservationConsumed))
	assert.False(t, CanTransitionReservation(ReservationActive, "BOGUS"))
}

func TestReservationStatus_ValidAndFinal(t *testing.T) {
	for _, s := range allReservationStatuses {
		assert.True(t, s.Valid(), s)
		assert.Equal(t, s != ReservationActive, s.Final(), s)
	}
	assert.False(t, ReservationStatus("").Valid())
	assert.False(t, ReservationStatus("").Final())
}

// A released, expired or consumed reservation can never be consumed or
// released again: every non-ACTIVE source yields INVALID_STATE_TRANSITION
// with the from/to fields the API renders.
func TestCheckReservationTransition_NonActiveRejected(t *testing.T) {
	for _, from := range []ReservationStatus{ReservationConsumed, ReservationReleased, ReservationExpired} {
		for _, to := range []ReservationStatus{ReservationConsumed, ReservationReleased, ReservationExpired} {
			r := Reservation{ID: NewReservationID(), Status: from}
			err := checkReservationTransition(r, to)
			require.Errorf(t, err, "%s -> %s", from, to)
			assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
			e, ok := errs.As(err)
			require.True(t, ok)
			assert.Equal(t, string(from), e.Fields["from"])
			assert.Equal(t, string(to), e.Fields["to"])
			assert.Equal(t, r.ID.String(), e.Fields["reservation_id"])
		}
	}
	for _, to := range []ReservationStatus{ReservationConsumed, ReservationReleased, ReservationExpired} {
		assert.NoError(t, checkReservationTransition(Reservation{Status: ReservationActive}, to))
	}
}

func TestEnvelopeTransitions(t *testing.T) {
	all := []EnvelopeStatus{EnvelopeDraft, EnvelopeActive, EnvelopePaused, EnvelopeExhausted, EnvelopeExpired, EnvelopeRevoked}
	legal := map[[2]EnvelopeStatus]bool{
		{EnvelopeDraft, EnvelopeActive}:      true,
		{EnvelopeDraft, EnvelopeRevoked}:     true,
		{EnvelopeActive, EnvelopePaused}:     true,
		{EnvelopeActive, EnvelopeExhausted}:  true,
		{EnvelopeActive, EnvelopeExpired}:    true,
		{EnvelopeActive, EnvelopeRevoked}:    true,
		{EnvelopePaused, EnvelopeActive}:     true,
		{EnvelopePaused, EnvelopeExpired}:    true,
		{EnvelopePaused, EnvelopeRevoked}:    true,
		{EnvelopeExhausted, EnvelopeActive}:  true,
		{EnvelopeExhausted, EnvelopeExpired}: true,
		{EnvelopeExhausted, EnvelopeRevoked}: true,
		{EnvelopeExpired, EnvelopeActive}:    true,
		{EnvelopeExpired, EnvelopeRevoked}:   true,
	}
	for _, from := range all {
		assert.True(t, from.Valid())
		for _, to := range all {
			assert.Equalf(t, legal[[2]EnvelopeStatus{from, to}], CanTransitionEnvelope(from, to), "%s -> %s", from, to)
		}
	}
	// REVOKED is terminal; nothing leaves it.
	for _, to := range all {
		assert.False(t, CanTransitionEnvelope(EnvelopeRevoked, to), to)
	}
	assert.False(t, EnvelopeStatus("").Valid())
}
