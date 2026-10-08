package httpapi

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/money"
)

// FeesPort serves the fee ledger (internal/feeledger; ADR-0020): versioned fee
// schedules, quote disclosures, the bookings that charged them and the
// sandbox performance fee. Every unit behind it is SIMULATED. The fee ledger
// and the fee engine (internal/fees) decide every figure and every refusal;
// the adapter chooses the transaction and, where the fee ledger lists nothing
// (schedule keys, an account's quotes, bookings and periods), projects its
// tables read-only and reads each record back through the fee ledger, which
// re-proves its canonical form and hash.
type FeesPort interface {
	Schedules(ctx context.Context, cursor string, limit int) (FeeSchedulePage, error)
	// ScheduleHistory returns every version of a key, oldest first; NOT_FOUND
	// when the key has none.
	ScheduleHistory(ctx context.Context, key string) (FeeScheduleHistoryView, error)
	// PreviewQuote prices an order under the schedule in force without
	// recording anything.
	PreviewQuote(ctx context.Context, in FeeQuoteInput) (FeePreviewView, error)
	Quotes(ctx context.Context, account accounts.AccountID, cursor string, limit int) (FeeQuotePage, error)
	// Quote returns one stored disclosure with its verification from the record.
	Quote(ctx context.Context, id feeledger.DisclosureID) (FeeQuoteView, error)
	Bookings(ctx context.Context, account accounts.AccountID, cursor string, limit int) (FeeBookingPage, error)
	// Booking returns one booking with its disclosure and its verification.
	Booking(ctx context.Context, id feeledger.BookingID) (FeeBookingView, error)
	// Periods pages an account's performance periods, newest first; an empty
	// strategy lists every strategy.
	Periods(ctx context.Context, account accounts.AccountID, strategy string, cursor string, limit int) (FeePeriodPage, error)
	// Period returns one period, re-proved from its terms when it has a
	// statement.
	Period(ctx context.Context, id feeledger.PeriodID) (FeePeriodView, error)

	Publish(ctx context.Context, r feeledger.PublishRequest) (FeeScheduleVersionView, error)
	RecordQuote(ctx context.Context, r FeeQuoteCommand) (FeeQuoteView, error)
	ReverseBooking(ctx context.Context, r feeledger.ReverseRequest) (FeeBookingView, error)
	SpreadAudit(ctx context.Context, f feeledger.SpreadFilter) ([]feeledger.SpreadCharge, error)
	SpreadTotals(ctx context.Context, quoteAsset assets.AssetID) (FeeSpreadTotalsView, error)
	OpenPeriod(ctx context.Context, r feeledger.OpenPeriodRequest) (FeePeriodView, error)
	RecordFlow(ctx context.Context, r feeledger.FlowRequest) (feeledger.Flow, error)
	Accrue(ctx context.Context, r feeledger.AccrueRequest) (feeledger.Line, error)
	Crystallize(ctx context.Context, r feeledger.CrystallizeRequest) (FeePeriodView, error)
	CancelPeriod(ctx context.Context, id feeledger.PeriodID, reason string, actor econ.Actor) (FeePeriodView, error)
	ReverseCrystallization(ctx context.Context, r feeledger.ReversePeriodRequest) (FeePeriodView, error)
}

// FeeScheduleVersionView is one published version with its quote asset and
// whether it prices orders now.
type FeeScheduleVersionView struct {
	Schedule   feeledger.PublishedSchedule
	QuoteAsset TreasuryAsset
	InForce    bool
}

// FeeScheduleSummary is one schedule key: how many versions it has, its
// latest version and the version in force now (nil while every version takes
// effect later).
type FeeScheduleSummary struct {
	Key       string
	Latest    int
	Count     int
	Effective *FeeScheduleVersionView
}

// FeeSchedulePage is one page of schedule keys in key order.
type FeeSchedulePage struct {
	Items      []FeeScheduleSummary
	NextCursor string
}

// FeeScheduleHistoryView is every version of a key, oldest first, and the
// version in force now (0 when none is yet).
type FeeScheduleHistoryView struct {
	Key              string
	Versions         []FeeScheduleVersionView
	EffectiveVersion int
}

// FeeQuoteInput prices one order. ReferencePrice is the exact decimal price of
// one whole base unit in the schedule's quote asset; the adapter states it in
// that asset with the schedule's quote decimals. A zero At is now.
type FeeQuoteInput struct {
	ScheduleKey        string
	At                 time.Time
	Side               fees.Side
	BaseQty            money.Quantity
	BaseDecimals       uint8
	ReferencePrice     string
	TrailingVolume     money.USD
	HasSubscription    bool
	StrategyOriginated bool
}

// FeePreviewView is a quote the fee engine produced and verified under the
// schedule in force, never stored or charged.
type FeePreviewView struct {
	Schedule       feeledger.PublishedSchedule
	Disclosure     fees.Disclosure
	DisclosureHash string
	Charges        feeledger.Charges
}

// FeeQuoteCommand records a quote disclosure.
type FeeQuoteCommand struct {
	Input          FeeQuoteInput
	Subsystem      econ.Subsystem
	AccountID      *accounts.AccountID
	BaseAsset      *assets.AssetID
	Mode           gates.Mode
	Source         string
	IdempotencyKey string
	Actor          econ.Actor
}

// FeeVerification is what re-proving a record from itself found: Verified, or
// the stable code and reason of why it does not verify.
type FeeVerification struct {
	Verified bool
	Code     string
	Reason   string
	Detail   string
}

// FeeQuoteView is a stored disclosure; Verification is present on a read by id.
type FeeQuoteView struct {
	Quote        feeledger.StoredQuote
	Verification *FeeVerification
}

// FeeQuotePage is one page of an account's disclosures, newest first.
type FeeQuotePage struct {
	Items      []FeeQuoteView
	NextCursor string
}

// FeeBookingView is a booking with the disclosure it charged (its spread line)
// and, on a read by id, its verification.
type FeeBookingView struct {
	Booking      feeledger.Booking
	Quote        feeledger.StoredQuote
	Verification *FeeVerification
}

// FeeBookingPage is one page of an account's bookings, newest first.
type FeeBookingPage struct {
	Items      []FeeBookingView
	NextCursor string
}

// FeePeriodView is a performance period; Verification is present on a read by
// id of a period with a statement.
type FeePeriodView struct {
	Period       feeledger.Period
	Verification *FeeVerification
}

// FeePeriodPage is one page of an account's periods, newest first.
type FeePeriodPage struct {
	Items      []FeePeriodView
	NextCursor string
}

// FeeSpreadTotalsView is the spread booked and reversed in one quote asset.
type FeeSpreadTotalsView struct {
	QuoteAsset assets.AssetID
	Booked     money.Quantity
	Reversed   money.Quantity
}
