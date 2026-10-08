package feeledger

import (
	"errors"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
)

// Every enumeration below has a SQL twin in migration 00746 (a CHECK (col IN
// (...)) constraint), and each AllXxx function returns exactly that list.

// SupportedModes are the modes a booking and a performance period run in
// (fee_bookings.mode, performance_fee_periods.mode): the simulated modes
// SPREAD_ENGINE and PERFORMANCE_FEES implement.
func SupportedModes() []gates.Mode { return []gates.Mode{gates.ModePaper, gates.ModeSandbox} }

func supportedMode(m gates.Mode) bool {
	for _, x := range SupportedModes() {
		if x == m {
			return true
		}
	}
	return false
}

// AllSides returns both quote sides (fee_quote_disclosures.side).
func AllSides() []fees.Side { return []fees.Side{fees.SideBuy, fees.SideSell} }

// BookingMode says who posted a booking's journal.
type BookingMode string

// Booking modes.
const (
	// BookingPosted: the fee ledger posted its own journal under its own
	// FEES effect.
	BookingPosted BookingMode = "POSTED"
	// BookingEmbedded: the charges are inside the caller's journal (a trade's
	// SIM_TRADE), applied by the caller's effect; the fee ledger verified and
	// registered them.
	BookingEmbedded BookingMode = "EMBEDDED"
)

// AllBookingModes returns both (fee_bookings.booking_mode).
func AllBookingModes() []BookingMode { return []BookingMode{BookingPosted, BookingEmbedded} }

// BookingStatus is the state of a booking.
type BookingStatus string

// Booking statuses.
const (
	BookingBooked   BookingStatus = "BOOKED"
	BookingReversed BookingStatus = "REVERSED"
)

// AllBookingStatuses returns both (fee_bookings.status).
func AllBookingStatuses() []BookingStatus { return []BookingStatus{BookingBooked, BookingReversed} }

// AllBookingKinds are the journal kinds a fee can be booked under
// (fee_bookings.journal_kind): the simulated-only kinds of the economic events
// a disclosed fee can accompany.
func AllBookingKinds() []ledger.Kind {
	return []ledger.Kind{
		ledger.KindSimTrade, ledger.KindP2PTransfer, ledger.KindLiquidation, ledger.KindOptionPremium,
		ledger.KindDerivativeMarginMove, ledger.KindMarginBorrow, ledger.KindEquitySettlement, ledger.KindStableIssuance,
		ledger.KindStableRedemption, ledger.KindBridgeSourceFinalized, ledger.KindPredictionSetIssuance,
		ledger.KindSandboxWithdrawal,
	}
}

func bookingKind(k ledger.Kind) bool {
	for _, x := range AllBookingKinds() {
		if x == k {
			return true
		}
	}
	return false
}

// PeriodStatus is the state of a performance fee period.
type PeriodStatus string

// Period statuses.
const (
	PeriodOpen         PeriodStatus = "OPEN"
	PeriodCrystallized PeriodStatus = "CRYSTALLIZED"
	PeriodCancelled    PeriodStatus = "CANCELLED"
	PeriodReversed     PeriodStatus = "REVERSED"
)

// AllPeriodStatuses returns every status (performance_fee_periods.status).
func AllPeriodStatuses() []PeriodStatus {
	return []PeriodStatus{PeriodOpen, PeriodCrystallized, PeriodCancelled, PeriodReversed}
}

// periodTransitions mirrors the status machine the database enforces (FE009).
var periodTransitions = map[PeriodStatus][]PeriodStatus{
	PeriodOpen:         {PeriodCrystallized, PeriodCancelled},
	PeriodCrystallized: {PeriodReversed},
}

// CanTransitionPeriod reports whether a period may move from -> to.
func CanTransitionPeriod(from, to PeriodStatus) bool {
	for _, t := range periodTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// AllLineKinds returns every performance line kind
// (performance_fee_lines.kind).
func AllLineKinds() []fees.PerformanceLineKind {
	return []fees.PerformanceLineKind{fees.LineAccrual, fees.LineReversal, fees.LineNoChange, fees.LineCrystallization}
}

// Effect kinds of this package.
const (
	EffectFeeCharge        = "FEE_CHARGE"
	EffectFeeRefund        = "FEE_REFUND"
	EffectPerfAccrual      = "PERF_FEE_ACCRUAL"
	EffectPerfCrystallize  = "PERF_FEE_CRYSTALLIZATION"
	referenceCharge        = "fee_charge"
	referenceChargeReverse = "fee_charge_reversal"
	referencePerfAccrual   = "performance_fee_accrual"
	referencePerfCharge    = "performance_fee_crystallization"
	referencePerfReversal  = "performance_fee_reversal"
)

// Stable reason codes, carried in the "reason" field of an error.
const (
	ReasonNothingToBook       = "FEES_NOTHING_TO_BOOK"
	ReasonQuoteExpired        = "FEES_QUOTE_EXPIRED"
	ReasonPayerMismatch       = "FEES_PAYER_NOT_THE_DISCLOSED"
	ReasonRecordDoesNotVerify = "FEES_RECORD_DOES_NOT_VERIFY"
	ReasonJournalMismatch     = "FEES_JOURNAL_DOES_NOT_CARRY_THE_CHARGES"
	ReasonKindNotFeeBearing   = "FEES_KIND_NOT_FEE_BEARING"
	ReasonScheduleNotFound    = "FEES_NO_EFFECTIVE_SCHEDULE"
	ReasonScheduleNotInForce  = "FEES_SCHEDULE_NOT_IN_FORCE"
	ReasonBackdated           = "FEES_SCHEDULE_BACKDATED"
	ReasonPeriodNotEnded      = "FEES_PERIOD_NOT_ENDED"
	ReasonPeriodContinued     = "FEES_PERIOD_ALREADY_CONTINUED"
	ReasonChainBroken         = "FEES_PERIOD_CHAIN_BROKEN"
)

// rejection builds the error of a refused request with its stable reason.
func rejection(code errs.Code, reason, detail string) *errs.Error {
	return errs.New(code, "feeledger: "+detail).WithField("reason", reason)
}

// ReasonOf returns the stable reason an error from this package carries, or
// "" when it carries none.
func ReasonOf(err error) string {
	var e *errs.Error
	if !errors.As(err, &e) || e.Fields == nil {
		return ""
	}
	r, _ := e.Fields["reason"].(string)
	return r
}

// Identifier kinds.
type (
	scheduleKind   struct{}
	disclosureKind struct{}
	bookingKindID  struct{}
	periodKind     struct{}
	flowKind       struct{}
	lineKind       struct{}
)

// ScheduleID identifies a published schedule version.
type ScheduleID = id.ID[scheduleKind]

// DisclosureID identifies a stored quote disclosure.
type DisclosureID = id.ID[disclosureKind]

// BookingID identifies a booking.
type BookingID = id.ID[bookingKindID]

// PeriodID identifies a performance fee period.
type PeriodID = id.ID[periodKind]

// FlowID identifies a capital flow of a period.
type FlowID = id.ID[flowKind]

// LineID identifies a performance line.
type LineID = id.ID[lineKind]

// ParseDisclosureID parses the canonical form.
func ParseDisclosureID(s string) (DisclosureID, error) { return id.Parse[disclosureKind](s) }

// ParsePeriodID parses the canonical form.
func ParsePeriodID(s string) (PeriodID, error) { return id.Parse[periodKind](s) }
