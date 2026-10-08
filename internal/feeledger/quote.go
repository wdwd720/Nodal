package feeledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
)

// Charges is everything a disclosure charges, component by component, as a
// ledger books it. Spread, Platform, Strategy and Venue are in the quote
// asset; Network is in NetworkAsset, which may be another asset.
type Charges struct {
	QuoteAsset assets.AssetID
	Spread     money.Quantity
	// Platform is the fixed fee, the percentage fee and any minimum-fee
	// top-up: the platform's explicit fee revenue.
	Platform money.Quantity
	// Strategy is the strategy fee, booked as its own pair.
	Strategy       money.Quantity
	Venue          money.Quantity
	Network        money.Quantity
	NetworkAsset   assets.AssetID
	NetworkInQuote bool
}

// ChargesOf reads the charges out of a disclosure. It reads only the
// disclosure: a caller that wants them proven verifies the disclosure first.
func ChargesOf(d fees.Disclosure) Charges {
	c := Charges{QuoteAsset: d.QuoteAsset, Spread: d.Spread.Amount, Platform: d.Fees.MinimumTopUp,
		Network: d.Network.Amount, NetworkAsset: d.Network.Asset, NetworkInQuote: d.Network.InQuoteAsset}
	for _, l := range d.Fees.Lines {
		switch l.Component {
		case fees.ComponentFixedFee, fees.ComponentPercentageFee:
			c.Platform = c.Platform.Add(l.Charged)
		case fees.ComponentStrategyFee:
			c.Strategy = c.Strategy.Add(l.Charged)
		case fees.ComponentVenuePassThrough:
			c.Venue = c.Venue.Add(l.Charged)
		}
	}
	return c
}

// InQuoteAsset is the total charged in the quote asset.
func (c Charges) InQuoteAsset() money.Quantity {
	t := c.Spread.Add(c.Platform).Add(c.Strategy).Add(c.Venue)
	if c.NetworkInQuote {
		t = t.Add(c.Network)
	}
	return t
}

// IsZero reports whether nothing at all is charged.
func (c Charges) IsZero() bool {
	return c.Spread.IsZero() && c.Platform.IsZero() && c.Strategy.IsZero() && c.Venue.IsZero() && c.Network.IsZero()
}

// StoredQuote is one stored quote disclosure.
type StoredQuote struct {
	ID              DisclosureID
	ScheduleID      ScheduleID
	ScheduleKey     string
	ScheduleVersion int
	ScheduleHash    string
	Subsystem       econ.Subsystem
	IdempotencyKey  string
	AccountID       *accounts.AccountID
	BaseAsset       *assets.AssetID
	Side            fees.Side
	Disclosure      fees.Disclosure
	DisclosureBytes []byte
	DisclosureHash  string
	Charges         Charges
	QuotedAt        time.Time
	ValidUntil      time.Time
}

// ReferencePrice, ExecutionPrice and EffectivePrice are the disclosed prices.
func (q StoredQuote) ReferencePrice() money.Price { return q.Disclosure.ReferencePrice.Price() }

// ExecutionPrice is the reference price moved by exactly the spread.
func (q StoredQuote) ExecutionPrice() money.Price { return q.Disclosure.ExecutionPrice.Price() }

// EffectivePrice is the all-in price per unit, for display.
func (q StoredQuote) EffectivePrice() money.Price { return q.Disclosure.EffectivePrice.Price() }

// QuoteRequest prices one order under the schedule in force at an instant.
type QuoteRequest struct {
	ScheduleKey string
	// At is the instant whose schedule prices the quote; zero means now.
	At    time.Time
	Input fees.QuoteInput
	// Subsystem and IdempotencyKey identify the quote: the same pair always
	// names the same disclosure.
	Subsystem      econ.Subsystem
	IdempotencyKey string
	// AccountID binds the quote to its payer when known: only that account
	// can then be charged it.
	AccountID *accounts.AccountID
	// BaseAsset is recorded with the quote; a disclosure itself names only
	// the quote asset.
	BaseAsset *assets.AssetID
	Mode      gates.Mode
	Actor     econ.Actor
	// RiskReducing says the quote prices a charge of a risk-reducing
	// operation (a liquidation's fee): a SPREAD_ENGINE kill switch or an
	// inactive gate does not refuse it (see EmbeddedRequest.RiskReducing).
	RiskReducing bool
}

// RecordQuote prices an order, proves the quote and its disclosure against
// the schedule (fees.VerifyQuote, fees.VerifyDisclosure), and stores the
// disclosure. It is idempotent on (subsystem, idempotency key): the same
// request returns the stored quote, another request under the key is
// refused.
func (s *Service) RecordQuote(ctx context.Context, tx pgx.Tx, r QuoteRequest) (StoredQuote, error) {
	if err := requireActor(r.Actor); err != nil {
		return StoredQuote{}, err
	}
	if err := validKey(r.IdempotencyKey); err != nil {
		return StoredQuote{}, err
	}
	if !r.Subsystem.Valid() {
		return StoredQuote{}, errs.Newf(errs.CodeValidationFailed, "feeledger: unknown subsystem %q", r.Subsystem)
	}
	if err := s.requireCharge(ctx, tx, r.Mode, r.RiskReducing); err != nil {
		return StoredQuote{}, err
	}
	now := s.clk.Now().UTC()
	at := r.At.UTC()
	if r.At.IsZero() {
		at = now
	}
	sched, err := s.EffectiveSchedule(ctx, tx, r.ScheduleKey, at)
	if err != nil {
		return StoredQuote{}, err
	}
	reqHash, err := econ.RequestHash(map[string]any{
		"schedule_id": sched.ID.String(), "input": fees.NewDisclosedInput(r.Input), "account": optionalString(r.AccountID),
		"base_asset": optionalString(r.BaseAsset),
	})
	if err != nil {
		return StoredQuote{}, err
	}
	if prior, found, err := s.quoteWhere(ctx, tx, `d.subsystem = $1 AND d.idempotency_key = $2`, r.Subsystem, r.IdempotencyKey); err != nil {
		return StoredQuote{}, err
	} else if found {
		var stored []byte
		if err := tx.QueryRow(ctx, `SELECT request_hash FROM fee_quote_disclosures WHERE id = $1`, prior.ID).Scan(&stored); err != nil {
			return StoredQuote{}, dbErr("load quote request", err)
		}
		if !bytes.Equal(stored, reqHash) {
			return StoredQuote{}, errs.New(errs.CodeInvalidIdempotencyReuse, "feeledger: this idempotency key was used for a different quote").
				WithField("disclosure_id", prior.ID.String())
		}
		return prior, nil
	}
	quote, err := sched.Schedule.Quote(r.Input)
	if err != nil {
		return StoredQuote{}, err
	}
	if err := fees.VerifyQuote(sched.Schedule, r.Input, quote); err != nil {
		return StoredQuote{}, errs.Wrap(err, errs.CodeInternal, "feeledger: a quote does not verify against its schedule").
			WithField("reason", ReasonRecordDoesNotVerify)
	}
	d := quote.Disclosure()
	if err := fees.VerifyDisclosure(sched.Schedule, d); err != nil {
		return StoredQuote{}, errs.Wrap(err, errs.CodeInternal, "feeledger: a disclosure does not verify against its schedule").
			WithField("reason", ReasonRecordDoesNotVerify)
	}
	body, err := d.Canonical()
	if err != nil {
		return StoredQuote{}, err
	}
	hash, err := d.Hash()
	if err != nil {
		return StoredQuote{}, err
	}
	c := ChargesOf(d)
	out := StoredQuote{
		ID: id.New[disclosureKind](), ScheduleID: sched.ID, ScheduleKey: sched.Key, ScheduleVersion: sched.Version,
		ScheduleHash: sched.Hash, Subsystem: r.Subsystem, IdempotencyKey: r.IdempotencyKey, AccountID: r.AccountID,
		BaseAsset: r.BaseAsset, Side: r.Input.Side, Disclosure: d, DisclosureBytes: body, DisclosureHash: hash, Charges: c,
		QuotedAt: now, ValidUntil: now.Add(s.quoteTTL),
	}
	var networkAsset any
	if !c.Network.IsZero() {
		networkAsset = c.NetworkAsset
	}
	if _, err := tx.Exec(ctx, `INSERT INTO fee_quote_disclosures (id, schedule_id, schedule_hash, subsystem, idempotency_key,
		request_hash, account_id, base_asset_id, quote_asset_id, side, base_quantity, reference_mantissa, reference_scale,
		execution_mantissa, execution_scale, effective_mantissa, effective_scale, notional_at_reference, notional_at_execution,
		spread_bps, spread_amount, platform_fee, strategy_fee, venue_fee, explicit_fees_total, network_fee_asset_id, network_fee,
		network_in_quote, slippage_reserve, total_debit, total_credit, max_total_fee_bps, max_bps_amount, max_total_fee,
		disclosure, disclosure_hash, quoted_at, valid_until)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::numeric,$12::numeric,$13,$14::numeric,$15,$16::numeric,$17,$18::numeric,
		        $19::numeric,$20,$21::numeric,$22::numeric,$23::numeric,$24::numeric,$25::numeric,$26,$27::numeric,$28,$29::numeric,
		        $30::numeric,$31::numeric,$32,$33::numeric,$34::numeric,$35,$36,$37,$38)`,
		out.ID, sched.ID, sched.Hash, r.Subsystem, r.IdempotencyKey, reqHash, r.AccountID, r.BaseAsset, d.QuoteAsset, d.Input.Side,
		d.Input.BaseQty.String(), d.ReferencePrice.Mantissa.String(), d.ReferencePrice.Scale, d.ExecutionPrice.Mantissa.String(),
		d.ExecutionPrice.Scale, d.EffectivePrice.Mantissa.String(), d.EffectivePrice.Scale, d.NotionalAtReference.String(),
		d.NotionalAtExecution.String(), int(d.Spread.BPS), c.Spread.String(), c.Platform.String(), c.Strategy.String(),
		c.Venue.String(), d.Fees.Total.String(), networkAsset, c.Network.String(), c.NetworkInQuote,
		d.SlippageReserve.Amount.String(), d.TotalDebit.String(), d.TotalCredit.String(), int(d.Fees.MaxTotalFeeBPS),
		d.Fees.MaxBPSAmount.String(), d.Fees.MaxTotalFee.String(), body, hash, now, out.ValidUntil); err != nil {
		return StoredQuote{}, dbErr("record quote disclosure", err)
	}
	return out, nil
}

func optionalString[T interface{ String() string }](v *T) string {
	if v == nil {
		return ""
	}
	return (*v).String()
}

const quoteColumns = `d.id, d.schedule_id, s.schedule_key, s.version, d.schedule_hash, d.subsystem, d.idempotency_key, d.account_id,
	d.base_asset_id, d.side, d.disclosure, d.disclosure_hash, d.quoted_at, d.valid_until`

func (s *Service) quoteWhere(ctx context.Context, q db.Querier, where string, args ...any) (StoredQuote, bool, error) {
	var out StoredQuote
	err := q.QueryRow(ctx, `SELECT `+quoteColumns+` FROM fee_quote_disclosures d JOIN fee_schedules s ON s.id = d.schedule_id
		WHERE `+where, args...).Scan(&out.ID, &out.ScheduleID, &out.ScheduleKey, &out.ScheduleVersion, &out.ScheduleHash,
		&out.Subsystem, &out.IdempotencyKey, &out.AccountID, &out.BaseAsset, &out.Side, &out.DisclosureBytes, &out.DisclosureHash,
		&out.QuotedAt, &out.ValidUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredQuote{}, false, nil
	}
	if err != nil {
		return StoredQuote{}, false, dbErr("load quote disclosure", err)
	}
	d, err := decodeDisclosure(out.DisclosureBytes, out.DisclosureHash)
	if err != nil {
		return StoredQuote{}, false, err
	}
	out.Disclosure, out.Charges = d, ChargesOf(d)
	out.QuotedAt, out.ValidUntil = out.QuotedAt.UTC(), out.ValidUntil.UTC()
	return out, true, nil
}

// Quote returns one stored quote disclosure.
func (s *Service) Quote(ctx context.Context, q db.Querier, did DisclosureID) (StoredQuote, error) {
	out, found, err := s.quoteWhere(ctx, q, `d.id = $1`, did)
	if err != nil {
		return StoredQuote{}, err
	}
	if !found {
		return StoredQuote{}, errs.New(errs.CodeNotFound, "feeledger: quote disclosure not found").WithField("disclosure_id", did.String())
	}
	return out, nil
}

// decodeDisclosure decodes a stored disclosure and proves it is the
// canonical form its hash names.
func decodeDisclosure(body []byte, hash string) (fees.Disclosure, error) {
	var d fees.Disclosure
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return fees.Disclosure{}, rejection(errs.CodeInternal, ReasonRecordDoesNotVerify, "a stored disclosure is not a disclosure")
	}
	again, err := d.Canonical()
	if err != nil {
		return fees.Disclosure{}, err
	}
	if !bytes.Equal(again, body) {
		return fees.Disclosure{}, rejection(errs.CodeInternal, ReasonRecordDoesNotVerify, "a stored disclosure is not in canonical form")
	}
	if h, err := d.Hash(); err != nil || h != hash {
		return fees.Disclosure{}, rejection(errs.CodeInternal, ReasonRecordDoesNotVerify, "a stored disclosure does not match its hash")
	}
	return d, nil
}

// VerifyQuoteRecord is what an auditor runs on a stored disclosure: the
// schedule its row names is decoded and proven canonical, the disclosure is
// decoded and proven canonical and hashed as recorded, and fees.
// VerifyDisclosure recomputes the quote from the schedule and the disclosed
// input and requires the disclosure to be exactly that quote's. A disclosure
// edited in any way -- one unit of spread, a rate, a rounding mode -- fails.
func (s *Service) VerifyQuoteRecord(ctx context.Context, q db.Querier, did DisclosureID) (fees.Disclosure, error) {
	stored, err := s.Quote(ctx, q, did)
	if err != nil {
		return fees.Disclosure{}, err
	}
	sched, err := s.Schedule(ctx, q, stored.ScheduleID)
	if err != nil {
		return fees.Disclosure{}, err
	}
	if sched.Hash != stored.ScheduleHash || stored.Disclosure.ScheduleHash != sched.Hash {
		return fees.Disclosure{}, rejection(errs.CodeValidationFailed, ReasonRecordDoesNotVerify, "the disclosure does not name its schedule")
	}
	if err := fees.VerifyDisclosure(sched.Schedule, stored.Disclosure); err != nil {
		return fees.Disclosure{}, errs.Wrap(err, errs.CodeValidationFailed, "feeledger: the stored disclosure does not verify against its schedule").
			WithField("reason", ReasonRecordDoesNotVerify)
	}
	return stored.Disclosure, nil
}
