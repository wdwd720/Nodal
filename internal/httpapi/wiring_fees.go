package httpapi

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/money"
)

// feesAdapter serves FeesPort over the fee ledger.
//
// Every command runs one fee-ledger operation in its own READ COMMITTED
// transaction; every read runs in one read-only REPEATABLE READ snapshot. The
// lists the fee ledger does not offer (schedule keys, an account's quotes,
// bookings and periods) select ids read-only and read every record back
// through the fee ledger, which decodes it and proves it is the canonical form
// its hash names.
type feesAdapter struct {
	svc    *feeledger.Service
	assets *assets.Repository
	db     *db.DB
	clk    clock.Clock
}

var _ FeesPort = feesAdapter{}

func (a feesAdapter) read(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, fn)
}

func (a feesAdapter) write(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

// feeVerification turns what re-proving a record returned into its verdict.
// A record that does not verify is a finding, reported with its stable code
// and reason; any other failure (the record could not be read at all) is the
// read's error.
func feeVerification(err error) (*FeeVerification, error) {
	if err == nil {
		return &FeeVerification{Verified: true}, nil
	}
	reason := feeledger.ReasonOf(err)
	ee, ok := errs.As(err)
	if !ok {
		return nil, err
	}
	switch {
	case reason == feeledger.ReasonRecordDoesNotVerify, reason == feeledger.ReasonJournalMismatch,
		ee.Code == errs.CodeValidationFailed:
		return &FeeVerification{Verified: false, Code: string(ee.Code), Reason: reason, Detail: ee.Detail}, nil
	}
	return nil, err
}

// effective returns the version of a key in force at an instant, or nil when
// none is yet.
func (a feesAdapter) effective(ctx context.Context, q db.Querier, key string, at time.Time) (*feeledger.PublishedSchedule, error) {
	p, err := a.svc.EffectiveSchedule(ctx, q, key, at)
	if feeledger.ReasonOf(err) == feeledger.ReasonScheduleNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (a feesAdapter) versionView(ctx context.Context, q db.Querier, known TreasuryAssets, p feeledger.PublishedSchedule, inForce bool) (FeeScheduleVersionView, error) {
	if err := treasuryDescribe(ctx, q, a.assets, known, p.QuoteAsset); err != nil {
		return FeeScheduleVersionView{}, err
	}
	return FeeScheduleVersionView{Schedule: p, QuoteAsset: known[p.QuoteAsset], InForce: inForce}, nil
}

// --- schedules ----------------------------------------------------------------

const feeScheduleKeysSQL = `SELECT schedule_key, max(version), count(*) FROM fee_schedules
	WHERE ($1::text IS NULL OR schedule_key > $1::text)
	GROUP BY schedule_key ORDER BY schedule_key LIMIT $2`

func (a feesAdapter) Schedules(ctx context.Context, cursor string, limit int) (FeeSchedulePage, error) {
	c, ok, err := decodeCursor(cursor)
	if err != nil {
		return FeeSchedulePage{}, err
	}
	var after *string
	if ok {
		after = &c.ID
	}
	limit = clampLimit(limit)
	var out FeeSchedulePage
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, feeScheduleKeysSQL, after, limit+1)
		if err != nil {
			return err
		}
		var items []FeeScheduleSummary
		for rows.Next() {
			var s FeeScheduleSummary
			if err := rows.Scan(&s.Key, &s.Latest, &s.Count); err != nil {
				rows.Close()
				return err
			}
			items = append(items, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		out = FeeSchedulePage{}
		if len(items) > limit {
			items = items[:limit]
			out.NextCursor = encodeCursor(keysetCursor{ID: items[limit-1].Key})
		}
		now := a.clk.Now()
		known := TreasuryAssets{}
		for i := range items {
			eff, err := a.effective(ctx, tx, items[i].Key, now)
			if err != nil {
				return err
			}
			if eff != nil {
				v, err := a.versionView(ctx, tx, known, *eff, true)
				if err != nil {
					return err
				}
				items[i].Effective = &v
			}
		}
		out.Items = items
		return nil
	})
	return out, err
}

func (a feesAdapter) ScheduleHistory(ctx context.Context, key string) (FeeScheduleHistoryView, error) {
	var out FeeScheduleHistoryView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		versions, err := a.svc.ScheduleHistory(ctx, tx, key)
		if err != nil {
			return err
		}
		if len(versions) == 0 {
			return errs.New(errs.CodeNotFound, "no fee schedule is published under this key").WithField("schedule_key", key)
		}
		eff, err := a.effective(ctx, tx, key, a.clk.Now())
		if err != nil {
			return err
		}
		out = FeeScheduleHistoryView{Key: key}
		if eff != nil {
			out.EffectiveVersion = eff.Version
		}
		known := TreasuryAssets{}
		for _, p := range versions {
			v, err := a.versionView(ctx, tx, known, p, eff != nil && eff.ID == p.ID)
			if err != nil {
				return err
			}
			out.Versions = append(out.Versions, v)
		}
		return nil
	})
	return out, err
}

func (a feesAdapter) Publish(ctx context.Context, r feeledger.PublishRequest) (FeeScheduleVersionView, error) {
	var out FeeScheduleVersionView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		p, err := a.svc.PublishSchedule(ctx, tx, r)
		if err != nil {
			return err
		}
		eff, err := a.effective(ctx, tx, p.Key, a.clk.Now())
		if err != nil {
			return err
		}
		out, err = a.versionView(ctx, tx, TreasuryAssets{}, p, eff != nil && eff.ID == p.ID)
		return err
	})
	return out, err
}

// --- quotes ------------------------------------------------------------------------

// quoteInput states the input in the schedule's quote asset and decimals: the
// reference price is the exact decimal the caller stated, observed at the
// quote's instant.
func quoteInput(in FeeQuoteInput, sched feeledger.PublishedSchedule, source string, observed time.Time) (fees.QuoteInput, error) {
	price, err := money.PriceFromDecimalString(in.ReferencePrice, sched.QuoteAsset.String(), source, observed)
	if err != nil {
		return fees.QuoteInput{}, validationError("reference_price", "reference_price is a plain decimal string in the schedule's quote asset")
	}
	return fees.QuoteInput{
		Side: in.Side, BaseQty: in.BaseQty, BaseDecimals: in.BaseDecimals, QuoteDecimals: sched.Schedule.QuoteDecimals,
		ReferencePrice: price, TrailingVolume: in.TrailingVolume, HasSubscription: in.HasSubscription,
		StrategyOriginated: in.StrategyOriginated,
	}, nil
}

// PreviewQuote prices an order through the fee engine exactly as the fee
// ledger's RecordQuote does -- the schedule in force, fees.VerifyQuote,
// fees.VerifyDisclosure -- and stores nothing.
func (a feesAdapter) PreviewQuote(ctx context.Context, in FeeQuoteInput) (FeePreviewView, error) {
	var out FeePreviewView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := a.clk.Now().UTC()
		at := in.At.UTC()
		if in.At.IsZero() {
			at = now
		}
		sched, err := a.svc.EffectiveSchedule(ctx, tx, in.ScheduleKey, at)
		if err != nil {
			return err
		}
		input, err := quoteInput(in, sched, "stated-by-requester", at)
		if err != nil {
			return err
		}
		q, err := sched.Schedule.Quote(input)
		if err != nil {
			return err
		}
		if err := fees.VerifyQuote(sched.Schedule, input, q); err != nil {
			return errs.Wrap(err, errs.CodeInternal, "a quote does not verify against its schedule")
		}
		d := q.Disclosure()
		if err := fees.VerifyDisclosure(sched.Schedule, d); err != nil {
			return errs.Wrap(err, errs.CodeInternal, "a disclosure does not verify against its schedule")
		}
		hash, err := d.Hash()
		if err != nil {
			return err
		}
		out = FeePreviewView{Schedule: sched, Disclosure: d, DisclosureHash: hash, Charges: feeledger.ChargesOf(d)}
		return nil
	})
	return out, err
}

func (a feesAdapter) RecordQuote(ctx context.Context, r FeeQuoteCommand) (FeeQuoteView, error) {
	var out FeeQuoteView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		at := r.Input.At.UTC()
		if r.Input.At.IsZero() {
			at = a.clk.Now().UTC()
		}
		sched, err := a.svc.EffectiveSchedule(ctx, tx, r.Input.ScheduleKey, at)
		if err != nil {
			return err
		}
		source := r.Source
		if source == "" {
			source = "operator-stated"
		}
		input, err := quoteInput(r.Input, sched, source, at)
		if err != nil {
			return err
		}
		q, err := a.svc.RecordQuote(ctx, tx, feeledger.QuoteRequest{
			ScheduleKey: r.Input.ScheduleKey, At: r.Input.At, Input: input, Subsystem: r.Subsystem, IdempotencyKey: r.IdempotencyKey,
			AccountID: r.AccountID, BaseAsset: r.BaseAsset, Mode: r.Mode, Actor: r.Actor,
		})
		if err != nil {
			return err
		}
		out = FeeQuoteView{Quote: q}
		return nil
	})
	return out, err
}

const feeQuotesSQL = `SELECT id, quoted_at FROM fee_quote_disclosures WHERE account_id = $1
	AND ($2::timestamptz IS NULL OR (quoted_at, id) < ($2::timestamptz, $3::uuid))
	ORDER BY quoted_at DESC, id DESC LIMIT $4`

// feeKey is one row of an id listing: the record's id and the instant it is
// ordered by.
type feeKey[T any] struct {
	id T
	at time.Time
}

func feeKeys[T any](ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]feeKey[T], error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []feeKey[T]
	for rows.Next() {
		var k feeKey[T]
		if err := rows.Scan(&k.id, &k.at); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// feePage trims a listing fetched with limit+1 rows and returns the cursor of
// the next page.
func feePage[T interface{ String() string }](keys []feeKey[T], limit int) ([]feeKey[T], string) {
	if len(keys) <= limit {
		return keys, ""
	}
	keys = keys[:limit]
	last := keys[limit-1]
	return keys, encodeCursor(keysetCursor{At: last.at.UTC(), ID: last.id.String()})
}

func (a feesAdapter) Quotes(ctx context.Context, account accounts.AccountID, cursor string, limit int) (FeeQuotePage, error) {
	at, after, err := marginCursor(cursor)
	if err != nil {
		return FeeQuotePage{}, err
	}
	limit = clampLimit(limit)
	var out FeeQuotePage
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		keys, err := feeKeys[feeledger.DisclosureID](ctx, tx, feeQuotesSQL, account, at, after, limit+1)
		if err != nil {
			return err
		}
		keys, next := feePage(keys, limit)
		out = FeeQuotePage{NextCursor: next}
		for _, k := range keys {
			q, err := a.svc.Quote(ctx, tx, k.id)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, FeeQuoteView{Quote: q})
		}
		return nil
	})
	return out, err
}

func (a feesAdapter) Quote(ctx context.Context, did feeledger.DisclosureID) (FeeQuoteView, error) {
	var out FeeQuoteView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q, err := a.svc.Quote(ctx, tx, did)
		if err != nil {
			return err
		}
		_, verr := a.svc.VerifyQuoteRecord(ctx, tx, did)
		v, err := feeVerification(verr)
		if err != nil {
			return err
		}
		out = FeeQuoteView{Quote: q, Verification: v}
		return nil
	})
	return out, err
}

// --- bookings ----------------------------------------------------------------------

const feeBookingsSQL = `SELECT id, booked_at FROM fee_bookings WHERE account_id = $1
	AND ($2::timestamptz IS NULL OR (booked_at, id) < ($2::timestamptz, $3::uuid))
	ORDER BY booked_at DESC, id DESC LIMIT $4`

// bookingView is a booking with the disclosure it charged.
func (a feesAdapter) bookingView(ctx context.Context, q db.Querier, b feeledger.Booking) (FeeBookingView, error) {
	stored, err := a.svc.Quote(ctx, q, b.DisclosureID)
	if err != nil {
		return FeeBookingView{}, err
	}
	return FeeBookingView{Booking: b, Quote: stored}, nil
}

func (a feesAdapter) Bookings(ctx context.Context, account accounts.AccountID, cursor string, limit int) (FeeBookingPage, error) {
	at, after, err := marginCursor(cursor)
	if err != nil {
		return FeeBookingPage{}, err
	}
	limit = clampLimit(limit)
	var out FeeBookingPage
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		keys, err := feeKeys[feeledger.BookingID](ctx, tx, feeBookingsSQL, account, at, after, limit+1)
		if err != nil {
			return err
		}
		keys, next := feePage(keys, limit)
		out = FeeBookingPage{NextCursor: next}
		for _, k := range keys {
			b, err := a.svc.Booking(ctx, tx, k.id)
			if err != nil {
				return err
			}
			v, err := a.bookingView(ctx, tx, b)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, v)
		}
		return nil
	})
	return out, err
}

func (a feesAdapter) Booking(ctx context.Context, bid feeledger.BookingID) (FeeBookingView, error) {
	var out FeeBookingView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		b, err := a.svc.Booking(ctx, tx, bid)
		if err != nil {
			return err
		}
		v, err := a.bookingView(ctx, tx, b)
		if err != nil {
			return err
		}
		if v.Verification, err = feeVerification(a.svc.VerifyBooking(ctx, tx, bid)); err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}

func (a feesAdapter) ReverseBooking(ctx context.Context, r feeledger.ReverseRequest) (FeeBookingView, error) {
	var out FeeBookingView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		b, err := a.svc.Reverse(ctx, tx, r)
		if err != nil {
			return err
		}
		out, err = a.bookingView(ctx, tx, b)
		return err
	})
	return out, err
}

func (a feesAdapter) SpreadAudit(ctx context.Context, f feeledger.SpreadFilter) ([]feeledger.SpreadCharge, error) {
	var out []feeledger.SpreadCharge
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		c, err := a.svc.SpreadAudit(ctx, tx, f)
		out = c
		return err
	})
	return out, err
}

func (a feesAdapter) SpreadTotals(ctx context.Context, quoteAsset assets.AssetID) (FeeSpreadTotalsView, error) {
	var out FeeSpreadTotalsView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		booked, reversed, err := a.svc.SpreadTotals(ctx, tx, quoteAsset)
		out = FeeSpreadTotalsView{QuoteAsset: quoteAsset, Booked: booked, Reversed: reversed}
		return err
	})
	return out, err
}

// --- performance periods --------------------------------------------------------------

const feePeriodsSQL = `SELECT id, created_at FROM performance_fee_periods WHERE account_id = $1
	AND ($2::text = '' OR strategy_ref = $2::text)
	AND ($3::timestamptz IS NULL OR (created_at, id) < ($3::timestamptz, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $5`

func (a feesAdapter) Periods(ctx context.Context, account accounts.AccountID, strategy string, cursor string, limit int) (FeePeriodPage, error) {
	at, after, err := marginCursor(cursor)
	if err != nil {
		return FeePeriodPage{}, err
	}
	limit = clampLimit(limit)
	var out FeePeriodPage
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		keys, err := feeKeys[feeledger.PeriodID](ctx, tx, feePeriodsSQL, account, strategy, at, after, limit+1)
		if err != nil {
			return err
		}
		keys, next := feePage(keys, limit)
		out = FeePeriodPage{NextCursor: next}
		for _, k := range keys {
			p, err := a.svc.Period(ctx, tx, k.id)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, FeePeriodView{Period: p})
		}
		return nil
	})
	return out, err
}

func (a feesAdapter) Period(ctx context.Context, pid feeledger.PeriodID) (FeePeriodView, error) {
	var out FeePeriodView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		p, err := a.svc.Period(ctx, tx, pid)
		if err != nil {
			return err
		}
		out = FeePeriodView{Period: p}
		if p.Statement != nil {
			if out.Verification, err = feeVerification(a.svc.VerifyPeriod(ctx, tx, pid)); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func (a feesAdapter) period(ctx context.Context, run func(ctx context.Context, tx pgx.Tx) (feeledger.Period, error)) (FeePeriodView, error) {
	var out FeePeriodView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		p, err := run(ctx, tx)
		out = FeePeriodView{Period: p}
		return err
	})
	return out, err
}

func (a feesAdapter) OpenPeriod(ctx context.Context, r feeledger.OpenPeriodRequest) (FeePeriodView, error) {
	return a.period(ctx, func(ctx context.Context, tx pgx.Tx) (feeledger.Period, error) { return a.svc.OpenPeriod(ctx, tx, r) })
}

func (a feesAdapter) RecordFlow(ctx context.Context, r feeledger.FlowRequest) (feeledger.Flow, error) {
	var out feeledger.Flow
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		f, err := a.svc.RecordFlow(ctx, tx, r)
		out = f
		return err
	})
	return out, err
}

func (a feesAdapter) Accrue(ctx context.Context, r feeledger.AccrueRequest) (feeledger.Line, error) {
	var out feeledger.Line
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		l, err := a.svc.Accrue(ctx, tx, r)
		out = l
		return err
	})
	return out, err
}

func (a feesAdapter) Crystallize(ctx context.Context, r feeledger.CrystallizeRequest) (FeePeriodView, error) {
	return a.period(ctx, func(ctx context.Context, tx pgx.Tx) (feeledger.Period, error) { return a.svc.Crystallize(ctx, tx, r) })
}

func (a feesAdapter) CancelPeriod(ctx context.Context, pid feeledger.PeriodID, reason string, actor econ.Actor) (FeePeriodView, error) {
	return a.period(ctx, func(ctx context.Context, tx pgx.Tx) (feeledger.Period, error) {
		return a.svc.CancelPeriod(ctx, tx, pid, reason, actor)
	})
}

func (a feesAdapter) ReverseCrystallization(ctx context.Context, r feeledger.ReversePeriodRequest) (FeePeriodView, error) {
	return a.period(ctx, func(ctx context.Context, tx pgx.Tx) (feeledger.Period, error) {
		return a.svc.ReverseCrystallization(ctx, tx, r)
	})
}
