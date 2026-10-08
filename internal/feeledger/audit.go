package feeledger

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// SpreadCharge is one charged spread as the audit record states it: the
// booking, the disclosure it charged, the schedule version that priced it,
// the prices it moved between and the journal that carried it.
type SpreadCharge struct {
	BookingID            BookingID
	DisclosureID         DisclosureID
	AccountID            accounts.AccountID
	ScheduleKey          string
	ScheduleVersion      int
	ScheduleHash         string
	DisclosureHash       string
	Side                 fees.Side
	BaseQuantity         money.Quantity
	ReferencePrice       money.Price
	ExecutionPrice       money.Price
	SpreadBPS            money.BPS
	SpreadAmount         money.Quantity
	QuoteAsset           assets.AssetID
	BookingMode          BookingMode
	Status               BookingStatus
	JournalTransactionID ledger.TransactionID
	ReversalJournalID    *ledger.TransactionID
	BookedAt             time.Time
}

// SpreadFilter narrows SpreadAudit. Zero fields are ignored; Since is
// inclusive and Until exclusive on the booking time.
type SpreadFilter struct {
	QuoteAsset *assets.AssetID
	AccountID  *accounts.AccountID
	Since      time.Time
	Until      time.Time
	// IncludeReversed lists reversed charges too (they are marked so).
	IncludeReversed bool
	Limit           int
}

// SpreadAudit lists every charged spread the filter selects, oldest first,
// from fee_bookings and fee_quote_disclosures alone: each row names the
// disclosure (recomputable with VerifyQuoteRecord), the schedule version, the
// reference and execution prices and the journal. Together with the
// database's guarantee that a booking's journal carries exactly its amounts
// (FE004), it makes every unit of spread charged discoverable.
func (s *Service) SpreadAudit(ctx context.Context, q db.Querier, f SpreadFilter) ([]SpreadCharge, error) {
	limit := f.Limit
	if limit <= 0 || limit > 10_000 {
		limit = 1_000
	}
	var until any
	if !f.Until.IsZero() {
		until = f.Until.UTC()
	}
	rows, err := q.Query(ctx, `SELECT b.id, b.disclosure_id, b.account_id, s.schedule_key, s.version, d.schedule_hash, d.disclosure_hash,
		d.side, d.base_quantity::text, d.reference_mantissa::text, d.reference_scale, d.execution_mantissa::text, d.execution_scale,
		d.spread_bps, b.spread_amount::text, b.quote_asset_id, b.booking_mode, b.status, b.journal_transaction_id,
		b.reversal_journal_transaction_id, b.booked_at
		FROM fee_bookings b JOIN fee_quote_disclosures d ON d.id = b.disclosure_id JOIN fee_schedules s ON s.id = d.schedule_id
		WHERE b.spread_amount > 0
		  AND ($1::uuid IS NULL OR b.quote_asset_id = $1)
		  AND ($2::uuid IS NULL OR b.account_id = $2)
		  AND b.booked_at >= $3
		  AND ($4::timestamptz IS NULL OR b.booked_at < $4)
		  AND ($5 OR b.status = 'BOOKED')
		ORDER BY b.booked_at, b.id LIMIT $6`,
		f.QuoteAsset, f.AccountID, f.Since.UTC(), until, f.IncludeReversed, limit)
	if err != nil {
		return nil, dbErr("spread audit", err)
	}
	defer rows.Close()
	var out []SpreadCharge
	for rows.Next() {
		var c SpreadCharge
		var qty, refM, exeM, spread string
		var refScale, exeScale, bps int32
		if err := rows.Scan(&c.BookingID, &c.DisclosureID, &c.AccountID, &c.ScheduleKey, &c.ScheduleVersion, &c.ScheduleHash,
			&c.DisclosureHash, &c.Side, &qty, &refM, &refScale, &exeM, &exeScale, &bps, &spread, &c.QuoteAsset, &c.BookingMode,
			&c.Status, &c.JournalTransactionID, &c.ReversalJournalID, &c.BookedAt); err != nil {
			return nil, dbErr("scan spread audit", err)
		}
		vals := []*money.Quantity{&c.BaseQuantity, &c.ReferencePrice.Mantissa, &c.ExecutionPrice.Mantissa, &c.SpreadAmount}
		for i, src := range []string{qty, refM, exeM, spread} {
			v, err := money.ScanQuantity(src)
			if err != nil {
				return nil, errs.Wrap(err, errs.CodeInternal, "feeledger: stored spread amounts")
			}
			*vals[i] = v
		}
		c.ReferencePrice.Scale, c.ExecutionPrice.Scale = refScale, exeScale
		c.ReferencePrice.QuoteAsset, c.ExecutionPrice.QuoteAsset = c.QuoteAsset.String(), c.QuoteAsset.String()
		c.SpreadBPS = money.BPS(bps)
		c.BookedAt = c.BookedAt.UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// SpreadTotals sums the spread charged in one quote asset: what is booked,
// and what was reversed. Booked less reversed is what SPREAD_REVENUE holds
// from the fee ledger's bookings.
func (s *Service) SpreadTotals(ctx context.Context, q db.Querier, quoteAsset assets.AssetID) (booked, reversed money.Quantity, err error) {
	var b, r string
	if err := q.QueryRow(ctx, `SELECT coalesce(sum(spread_amount), 0)::text,
		coalesce(sum(spread_amount) FILTER (WHERE status = 'REVERSED'), 0)::text FROM fee_bookings WHERE quote_asset_id = $1`,
		quoteAsset).Scan(&b, &r); err != nil {
		return money.Quantity{}, money.Quantity{}, dbErr("spread totals", err)
	}
	if booked, err = money.ScanQuantity(b); err == nil {
		reversed, err = money.ScanQuantity(r)
	}
	if err != nil {
		return money.Quantity{}, money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "feeledger: spread totals")
	}
	return booked, reversed, nil
}
