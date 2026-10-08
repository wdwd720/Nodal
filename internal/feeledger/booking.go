package feeledger

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// ChargeEntries are the journal entries of a disclosure's charges to a payer,
// every component on its own pair (ADVANCED_FINANCIAL_MODEL §5):
//
//	spread:   C: Dr FEES_SPREAD / Cr WALLET;   P: Dr PLATFORM_FEE_RECEIVABLE / Cr SPREAD_REVENUE
//	platform: C: Dr FEES_PLATFORM / Cr WALLET; P: Dr PLATFORM_FEE_RECEIVABLE / Cr PLATFORM_FEE_REVENUE
//	strategy: C: Dr FEES_PLATFORM / Cr WALLET; P: Dr PLATFORM_FEE_RECEIVABLE / Cr PLATFORM_FEE_REVENUE (its own pair)
//	venue:    C: Dr FEES_VENUE / Cr WALLET     (passed through to the venue, not platform revenue)
//	network:  C: Dr FEES_NETWORK / Cr WALLET   (in the network asset; passed through)
//
// Each party balances on its own books per asset. A caller that posts fees
// inside its own journal (RecordEmbedded) uses exactly these entries.
func ChargeEntries(payer accounts.AccountID, c Charges) []ledger.Entry {
	var out []ledger.Entry
	revenue := func(expense, income ledger.Code, amount money.Quantity) {
		if !amount.IsPositive() {
			return
		}
		out = append(out,
			ledger.Entry{Account: ledger.CustomerAccount(payer, expense, c.QuoteAsset), Side: ledger.Debit, Quantity: amount},
			ledger.Entry{Account: ledger.CustomerAccount(payer, ledger.CodeWallet, c.QuoteAsset), Side: ledger.Credit, Quantity: amount},
			ledger.Entry{Account: ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, c.QuoteAsset), Side: ledger.Debit, Quantity: amount},
			ledger.Entry{Account: ledger.PlatformAccount(income, c.QuoteAsset), Side: ledger.Credit, Quantity: amount})
	}
	passThrough := func(expense ledger.Code, asset assets.AssetID, amount money.Quantity) {
		if !amount.IsPositive() {
			return
		}
		out = append(out,
			ledger.Entry{Account: ledger.CustomerAccount(payer, expense, asset), Side: ledger.Debit, Quantity: amount},
			ledger.Entry{Account: ledger.CustomerAccount(payer, ledger.CodeWallet, asset), Side: ledger.Credit, Quantity: amount})
	}
	revenue(ledger.CodeFeesSpread, ledger.CodeSpreadRevenue, c.Spread)
	revenue(ledger.CodeFeesPlatform, ledger.CodePlatformFeeRevenue, c.Platform)
	revenue(ledger.CodeFeesPlatform, ledger.CodePlatformFeeRevenue, c.Strategy)
	passThrough(ledger.CodeFeesVenue, c.QuoteAsset, c.Venue)
	passThrough(ledger.CodeFeesNetwork, c.NetworkAsset, c.Network)
	return out
}

// refundEntries mirror ChargeEntries: the payer is made whole and the
// platform's revenue and receivable reduced, component by component.
func refundEntries(payer accounts.AccountID, c Charges) []ledger.Entry {
	charged := ChargeEntries(payer, c)
	out := make([]ledger.Entry, 0, len(charged))
	for _, e := range charged {
		e.Side = e.Side.Opposite()
		out = append(out, e)
	}
	return out
}

// Booking is one charge of a disclosure to its payer.
type Booking struct {
	ID                   BookingID
	DisclosureID         DisclosureID
	AccountID            accounts.AccountID
	Mode                 gates.Mode
	BookingMode          BookingMode
	Kind                 ledger.Kind
	JournalTransactionID ledger.TransactionID
	EffectID             econ.EffectID
	IntentID             *econ.IntentID
	Charges              Charges
	Status               BookingStatus
	EvidenceRef          string
	EvidenceSHA256       []byte
	BookedAt             time.Time
	ReversalJournalID    *ledger.TransactionID
	ReversalEffectID     *econ.EffectID
	ReversalReasonCode   string
	ReversedAt           *time.Time
}

// BookRequest charges a stored disclosure to its payer.
type BookRequest struct {
	DisclosureID DisclosureID
	AccountID    accounts.AccountID
	// Kind is the journal kind of the economic event the fee accompanies
	// (AllBookingKinds), e.g. SIM_TRADE for a trade's fee.
	Kind          ledger.Kind
	IntentID      *econ.IntentID
	Mode          gates.Mode
	CorrelationID string
	Actor         econ.Actor
}

// Book charges a disclosure to its payer as one exactly-once FEES effect
// (key "fees:charge:<disclosure id>"), every component on its own pair of
// accounts (ChargeEntries). The stored disclosure is re-verified from the
// record first, an expired quote or another payer is refused, and the payer's
// wallet is reserved for the charge (naming the economic intent it serves)
// before anything is posted, so a fee never spends what another operation
// holds. A replay returns the first booking.
func (s *Service) Book(ctx context.Context, tx pgx.Tx, r BookRequest) (Booking, error) {
	if err := requireActor(r.Actor); err != nil {
		return Booking{}, err
	}
	if !bookingKind(r.Kind) {
		return Booking{}, rejection(errs.CodeValidationFailed, ReasonKindNotFeeBearing, "a fee is booked under the kind of the event it accompanies")
	}
	if err := s.requireMode(ctx, tx, gates.SpreadEngine, r.Mode); err != nil {
		return Booking{}, err
	}
	key := "fees:charge:" + r.DisclosureID.String()
	if prior, found, err := s.bookingWhere(ctx, tx, `disclosure_id = $1`, r.DisclosureID); err != nil {
		return Booking{}, err
	} else if found {
		if prior.BookingMode != BookingPosted || prior.AccountID != r.AccountID || prior.Kind != r.Kind {
			return Booking{}, errs.New(errs.CodeInvalidIdempotencyReuse, "feeledger: this disclosure was booked differently").
				WithField("booking_id", prior.ID.String())
		}
		return prior, nil
	}
	if _, err := s.VerifyQuoteRecord(ctx, tx, r.DisclosureID); err != nil {
		return Booking{}, err
	}
	stored, err := s.Quote(ctx, tx, r.DisclosureID)
	if err != nil {
		return Booking{}, err
	}
	now := s.clk.Now().UTC()
	if err := chargeable(stored, r.AccountID, now); err != nil {
		return Booking{}, err
	}
	c := stored.Charges
	var intent string
	if r.IntentID != nil {
		intent = r.IntentID.String()
	}
	eff, existed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: key, Subsystem: econ.SubsystemFees, Kind: EffectFeeCharge, IntentID: r.IntentID, AccountID: r.AccountID.String(),
		Request: map[string]any{"disclosure_id": r.DisclosureID.String(), "disclosure_hash": stored.DisclosureHash,
			"payer": r.AccountID.String(), "kind": string(r.Kind), "intent_id": intent},
		PostsLedger: true, Actor: s.actor, CorrelationID: r.CorrelationID,
	})
	if err != nil {
		return Booking{}, err
	}
	if existed {
		if eff.Status == econ.EffectApplied {
			b, found, err := s.bookingWhere(ctx, tx, `disclosure_id = $1`, r.DisclosureID)
			if err != nil {
				return Booking{}, err
			}
			if found {
				return b, nil
			}
		}
		return Booking{}, errs.Newf(errs.CodeReconciliationRequired, "feeledger: the charge of this disclosure is %s; it is reconciled, never retried", eff.Status).
			WithField("effect_id", eff.ID.String())
	}
	// Reserve what the charge takes from the payer's wallet, per asset.
	reservations := map[assets.AssetID]money.Quantity{}
	reservations[c.QuoteAsset] = c.Spread.Add(c.Platform).Add(c.Strategy).Add(c.Venue)
	if c.Network.IsPositive() {
		reservations[c.NetworkAsset] = reservations[c.NetworkAsset].Add(c.Network)
	}
	var held []capital.Reservation
	for _, asset := range sortedAssets(reservations) {
		qty := reservations[asset]
		if !qty.IsPositive() {
			continue
		}
		res, err := s.capital.Reserve(ctx, tx, capital.ReserveRequest{
			AccountID: r.AccountID.String(), AssetID: asset, Quantity: qty, EconomicIntentID: intent,
			ActorType: s.actor.Type, ActorID: s.actor.ID, IdempotencyKey: key + ":" + asset.String(), TTL: time.Minute,
			Reason: "disclosed fee charge (disclosure " + r.DisclosureID.String() + ")",
		})
		if err != nil {
			return Booking{}, err
		}
		held = append(held, res)
	}
	posted, err := s.ledger.Post(s.postingContext(ctx), tx, ledger.Posting{
		Kind: r.Kind, IdempotencyKey: key, Reference: ledger.FinancialEventReference{Type: referenceCharge, ID: r.DisclosureID.String()},
		EffectiveAt: now, Description: "disclosed simulated fee charge: every component on its own account",
		CorrelationID: r.CorrelationID, Entries: ChargeEntries(r.AccountID, c),
		Metadata: map[string]any{
			"disclosure_id": r.DisclosureID.String(), "disclosure_hash": stored.DisclosureHash, "schedule_key": stored.ScheduleKey,
			"schedule_version": stored.ScheduleVersion, "schedule_hash": stored.ScheduleHash, "spread": c.Spread.String(),
			"platform_fee": c.Platform.String(), "strategy_fee": c.Strategy.String(), "venue_fee": c.Venue.String(),
			"network_fee": c.Network.String(),
		},
	})
	if err != nil {
		return Booking{}, err
	}
	reservationID := ""
	for _, res := range held {
		if _, err := s.capital.ConsumeFinal(ctx, tx, res.ID, res.Quantity, 0, ""); err != nil {
			return Booking{}, err
		}
		if reservationID == "" {
			reservationID = res.ID.String()
		}
	}
	b := Booking{
		ID: id.New[bookingKindID](), DisclosureID: r.DisclosureID, AccountID: r.AccountID, Mode: r.Mode, BookingMode: BookingPosted,
		Kind: r.Kind, JournalTransactionID: posted.TransactionID, IntentID: r.IntentID, Charges: c, Status: BookingBooked,
		BookedAt: now,
	}
	ev, err := s.bookingEvidence(ctx, tx, b, stored)
	if err != nil {
		return Booking{}, err
	}
	applied, err := s.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{
		JournalTransactionID: &posted.TransactionID, ReservationID: reservationID, Evidence: &ev, Actor: s.actor,
		Reason: "disclosed fee charge posted",
	})
	if err != nil {
		return Booking{}, err
	}
	b.EffectID = applied.ID
	if err := s.insertBooking(ctx, tx, &b, ev, r.CorrelationID); err != nil {
		return Booking{}, err
	}
	return b, nil
}

// chargeable refuses a quote that cannot be charged to payer now.
func chargeable(q StoredQuote, payer accounts.AccountID, now time.Time) error {
	if q.AccountID != nil && *q.AccountID != payer {
		return rejection(errs.CodeForbidden, ReasonPayerMismatch, "the quote was disclosed to another account")
	}
	if now.After(q.ValidUntil) {
		return rejection(errs.CodeQuoteExpired, ReasonQuoteExpired, "the quote expired before it was booked")
	}
	if q.Charges.IsZero() {
		return rejection(errs.CodeValidationFailed, ReasonNothingToBook, "the disclosure charges nothing")
	}
	return nil
}

func sortedAssets(m map[assets.AssetID]money.Quantity) []assets.AssetID {
	out := make([]assets.AssetID, 0, len(m))
	for a := range m {
		out = append(out, a)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].String() < out[j-1].String(); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (s *Service) bookingEvidence(ctx context.Context, tx pgx.Tx, b Booking, q StoredQuote) (econ.EvidenceRef, error) {
	return econ.RecordEvidence(ctx, tx, econ.Evidence{
		Key: "fees/charge/" + b.DisclosureID.String(),
		Document: map[string]any{
			"kind": "fees.charge.v1", "booking_id": b.ID.String(), "booking_mode": string(b.BookingMode),
			"disclosure_id": b.DisclosureID.String(), "disclosure_hash": q.DisclosureHash, "schedule_key": q.ScheduleKey,
			"schedule_version": q.ScheduleVersion, "schedule_hash": q.ScheduleHash, "payer": b.AccountID.String(),
			"journal_kind": string(b.Kind), "journal_transaction_id": b.JournalTransactionID.String(),
			"reference_price": q.Disclosure.ReferencePrice, "execution_price": q.Disclosure.ExecutionPrice,
			"effective_price": q.Disclosure.EffectivePrice, "spread_bps": q.Disclosure.Spread.BPS, "spread": b.Charges.Spread.String(),
			"platform_fee": b.Charges.Platform.String(), "strategy_fee": b.Charges.Strategy.String(),
			"venue_fee": b.Charges.Venue.String(), "network_fee": b.Charges.Network.String(),
			"network_asset": b.Charges.NetworkAsset.String(), "quote_asset": b.Charges.QuoteAsset.String(), "value_domain": "SIMULATED",
		},
	})
}

func (s *Service) insertBooking(ctx context.Context, tx pgx.Tx, b *Booking, ev econ.EvidenceRef, correlation string) error {
	b.EvidenceRef, b.EvidenceSHA256 = ev.URI, ev.SHA256
	var networkAsset any
	if b.Charges.Network.IsPositive() {
		networkAsset = b.Charges.NetworkAsset
	}
	if _, err := tx.Exec(ctx, `INSERT INTO fee_bookings (id, disclosure_id, account_id, mode, booking_mode, journal_kind,
		journal_transaction_id, effect_id, intent_id, quote_asset_id, network_fee_asset_id, spread_amount, platform_fee, strategy_fee,
		venue_fee, network_fee, status, evidence_ref, evidence_sha256, booked_at, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::numeric,$13::numeric,$14::numeric,$15::numeric,$16::numeric,'BOOKED',$17,$18,$19,
		        NULLIF($20,''))`,
		b.ID, b.DisclosureID, b.AccountID, b.Mode, b.BookingMode, b.Kind, b.JournalTransactionID, b.EffectID, b.IntentID,
		b.Charges.QuoteAsset, networkAsset, b.Charges.Spread.String(), b.Charges.Platform.String(), b.Charges.Strategy.String(),
		b.Charges.Venue.String(), b.Charges.Network.String(), ev.URI, ev.SHA256, b.BookedAt, correlation); err != nil {
		return dbErr("record fee booking", err)
	}
	return nil
}

// EmbeddedRequest registers charges a caller already posted inside its own
// journal.
type EmbeddedRequest struct {
	DisclosureID         DisclosureID
	AccountID            accounts.AccountID
	JournalTransactionID ledger.TransactionID
	// EffectID is the caller's APPLIED effect that applied the journal.
	EffectID      econ.EffectID
	IntentID      *econ.IntentID
	Mode          gates.Mode
	CorrelationID string
	Actor         econ.Actor
}

// RecordEmbedded registers a disclosure whose charges the caller posted in
// its own journal (for example the fee legs of a paper fill's SIM_TRADE): the
// stored disclosure is re-verified, the journal must be of a fee-bearing kind
// and debit the payer exactly each disclosed component on its own FEES_*
// account and credit the platform the revenue, and the effect must have
// applied that journal. A disclosure is registered at most once.
func (s *Service) RecordEmbedded(ctx context.Context, tx pgx.Tx, r EmbeddedRequest) (Booking, error) {
	if err := requireActor(r.Actor); err != nil {
		return Booking{}, err
	}
	if err := s.requireMode(ctx, tx, gates.SpreadEngine, r.Mode); err != nil {
		return Booking{}, err
	}
	if prior, found, err := s.bookingWhere(ctx, tx, `disclosure_id = $1`, r.DisclosureID); err != nil {
		return Booking{}, err
	} else if found {
		if prior.BookingMode != BookingEmbedded || prior.JournalTransactionID != r.JournalTransactionID || prior.AccountID != r.AccountID {
			return Booking{}, errs.New(errs.CodeInvalidIdempotencyReuse, "feeledger: this disclosure was booked differently").
				WithField("booking_id", prior.ID.String())
		}
		return prior, nil
	}
	if _, err := s.VerifyQuoteRecord(ctx, tx, r.DisclosureID); err != nil {
		return Booking{}, err
	}
	stored, err := s.Quote(ctx, tx, r.DisclosureID)
	if err != nil {
		return Booking{}, err
	}
	now := s.clk.Now().UTC()
	if err := chargeable(stored, r.AccountID, now); err != nil {
		return Booking{}, err
	}
	jt, err := s.ledger.Transaction(ctx, tx, r.JournalTransactionID)
	if err != nil {
		return Booking{}, err
	}
	if !bookingKind(jt.Kind) || jt.ReversalOf != nil {
		return Booking{}, rejection(errs.CodeValidationFailed, ReasonKindNotFeeBearing, "the journal is not of a fee-bearing kind")
	}
	if err := carriesCharges(jt, r.AccountID, stored.Charges); err != nil {
		return Booking{}, err
	}
	eff, err := s.effects.Get(ctx, tx, r.EffectID)
	if err != nil {
		return Booking{}, err
	}
	if eff.Status != econ.EffectApplied || eff.JournalTransactionID == nil || *eff.JournalTransactionID != r.JournalTransactionID {
		return Booking{}, rejection(errs.CodeValidationFailed, ReasonJournalMismatch, "the effect did not apply this journal")
	}
	b := Booking{
		ID: id.New[bookingKindID](), DisclosureID: r.DisclosureID, AccountID: r.AccountID, Mode: r.Mode, BookingMode: BookingEmbedded,
		Kind: jt.Kind, JournalTransactionID: r.JournalTransactionID, EffectID: r.EffectID, IntentID: r.IntentID,
		Charges: stored.Charges, Status: BookingBooked, BookedAt: now,
	}
	ev, err := s.bookingEvidence(ctx, tx, b, stored)
	if err != nil {
		return Booking{}, err
	}
	if err := s.insertBooking(ctx, tx, &b, ev, r.CorrelationID); err != nil {
		return Booking{}, err
	}
	return b, nil
}

// carriesCharges proves a journal charges the payer exactly c, each
// component on its own FEES_* account, and credits the platform at least the
// revenue (a journal can carry several payers' fees).
func carriesCharges(jt ledger.Transaction, payer accounts.AccountID, c Charges) error {
	var spread, platform, venue, network, spreadRevenue, feeRevenue money.Quantity
	for _, e := range jt.Entries {
		a := e.Account
		switch {
		case a.OwnerType == ledger.OwnerCustomer && strings.EqualFold(a.OwnerID, payer.String()) && e.Side == ledger.Debit:
			switch {
			case a.Code == ledger.CodeFeesSpread && a.AssetID == c.QuoteAsset:
				spread = spread.Add(e.Quantity)
			case a.Code == ledger.CodeFeesPlatform && a.AssetID == c.QuoteAsset:
				platform = platform.Add(e.Quantity)
			case a.Code == ledger.CodeFeesVenue && a.AssetID == c.QuoteAsset:
				venue = venue.Add(e.Quantity)
			case a.Code == ledger.CodeFeesNetwork && a.AssetID == c.NetworkAsset:
				network = network.Add(e.Quantity)
			}
		case a.OwnerType == ledger.OwnerPlatform && e.Side == ledger.Credit && a.AssetID == c.QuoteAsset:
			switch a.Code {
			case ledger.CodeSpreadRevenue:
				spreadRevenue = spreadRevenue.Add(e.Quantity)
			case ledger.CodePlatformFeeRevenue:
				feeRevenue = feeRevenue.Add(e.Quantity)
			}
		}
	}
	revenue := c.Platform.Add(c.Strategy)
	if !spread.Equal(c.Spread) || !platform.Equal(revenue) || !venue.Equal(c.Venue) || !network.Equal(c.Network) ||
		spreadRevenue.Cmp(c.Spread) < 0 || feeRevenue.Cmp(revenue) < 0 {
		return rejection(errs.CodeValidationFailed, ReasonJournalMismatch, "the journal does not carry exactly the disclosed charges").
			WithField("journal_transaction_id", jt.ID.String())
	}
	return nil
}

// ReverseRequest reverses a booking.
type ReverseRequest struct {
	BookingID  BookingID
	ReasonCode string
	Reason     string
	Actor      econ.Actor
}

var reasonCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)

// Reverse compensates a booking by a COMPENSATION journal (reversal_of the
// booking's journal, with a reason code) that returns exactly its charges:
// the payer is made whole and the platform's revenue reduced, component by
// component. A POSTED booking's own effect is COMPENSATED by it; an EMBEDDED
// booking's refund is its own FEES effect (the trade's effect stands). History
// is never edited: the booking becomes REVERSED with its transition row.
func (s *Service) Reverse(ctx context.Context, tx pgx.Tx, r ReverseRequest) (Booking, error) {
	if err := requireActor(r.Actor); err != nil {
		return Booking{}, err
	}
	if !reasonCodePattern.MatchString(r.ReasonCode) || strings.TrimSpace(r.Reason) == "" {
		return Booking{}, errs.New(errs.CodeValidationFailed, "feeledger: a reversal needs a reason code and a reason")
	}
	b, found, err := s.bookingWhere(ctx, tx, `id = $1 FOR UPDATE`, r.BookingID)
	if err != nil {
		return Booking{}, err
	}
	if !found {
		return Booking{}, errs.New(errs.CodeNotFound, "feeledger: booking not found").WithField("booking_id", r.BookingID.String())
	}
	if b.Status == BookingReversed {
		return b, nil
	}
	now := s.clk.Now().UTC()
	posting := ledger.Posting{
		Kind: ledger.KindCompensation, IdempotencyKey: "fees:charge-reversal:" + b.ID.String(),
		Reference:   ledger.FinancialEventReference{Type: referenceChargeReverse, ID: b.ID.String()},
		EffectiveAt: now, Description: "reversal of a disclosed simulated fee charge", ReversalOf: &b.JournalTransactionID,
		Entries:  refundEntries(b.AccountID, b.Charges),
		Metadata: map[string]any{ledger.MetadataReasonCode: r.ReasonCode, "booking_id": b.ID.String()},
	}
	var reversalEffect econ.EffectID
	switch b.BookingMode {
	case BookingPosted:
		posted, err := s.ledger.Post(s.postingContext(ctx), tx, posting)
		if err != nil {
			return Booking{}, err
		}
		if _, err := s.effects.Compensate(ctx, tx, b.EffectID, &posted.TransactionID, s.actor, r.ReasonCode+": "+r.Reason); err != nil {
			return Booking{}, err
		}
		reversalEffect = b.EffectID
		b.ReversalJournalID = &posted.TransactionID
	default:
		eff, existed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
			Key: "fees:refund:" + b.ID.String(), Subsystem: econ.SubsystemFees, Kind: EffectFeeRefund, IntentID: b.IntentID,
			AccountID: b.AccountID.String(), Request: map[string]any{"booking_id": b.ID.String(), "reason_code": r.ReasonCode},
			PostsLedger: true, Actor: s.actor,
		})
		if err != nil {
			return Booking{}, err
		}
		if existed {
			return Booking{}, errs.New(errs.CodeConflict, "feeledger: this booking's refund was already claimed").WithField("effect_id", eff.ID.String())
		}
		posted, err := s.ledger.Post(s.postingContext(ctx), tx, posting)
		if err != nil {
			return Booking{}, err
		}
		applied, err := s.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{JournalTransactionID: &posted.TransactionID, Actor: s.actor,
			Reason: r.ReasonCode + ": " + r.Reason})
		if err != nil {
			return Booking{}, err
		}
		reversalEffect = applied.ID
		b.ReversalJournalID = &posted.TransactionID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO fee_booking_transitions (id, booking_id, from_status, to_status, actor_type, actor_id,
		reason, occurred_at) VALUES ($1,$2,'BOOKED','REVERSED',$3,$4,$5,$6)`,
		id.New[id.Any](), b.ID, r.Actor.Type, r.Actor.ID, r.ReasonCode+": "+r.Reason, now); err != nil {
		return Booking{}, dbErr("record booking transition", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE fee_bookings SET status = 'REVERSED', reversal_journal_transaction_id = $2,
		reversal_effect_id = $3, reversal_reason_code = $4, reversed_by_actor_type = $5, reversed_by_actor_id = $6, reversed_at = $7
		WHERE id = $1`, b.ID, *b.ReversalJournalID, reversalEffect, r.ReasonCode, r.Actor.Type, r.Actor.ID, now); err != nil {
		return Booking{}, dbErr("reverse fee booking", err)
	}
	b.Status, b.ReversalEffectID, b.ReversalReasonCode, b.ReversedAt = BookingReversed, &reversalEffect, r.ReasonCode, &now
	return b, nil
}

const bookingColumns = `id, disclosure_id, account_id, mode, booking_mode, journal_kind, journal_transaction_id, effect_id, intent_id,
	quote_asset_id, network_fee_asset_id, spread_amount::text, platform_fee::text, strategy_fee::text, venue_fee::text,
	network_fee::text, status, evidence_ref, evidence_sha256, booked_at, reversal_journal_transaction_id, reversal_effect_id,
	coalesce(reversal_reason_code, ''), reversed_at`

func (s *Service) bookingWhere(ctx context.Context, q db.Querier, where string, args ...any) (Booking, bool, error) {
	var b Booking
	var network *assets.AssetID
	var v [5]string
	err := q.QueryRow(ctx, `SELECT `+bookingColumns+` FROM fee_bookings WHERE `+where, args...).Scan(&b.ID, &b.DisclosureID,
		&b.AccountID, &b.Mode, &b.BookingMode, &b.Kind, &b.JournalTransactionID, &b.EffectID, &b.IntentID, &b.Charges.QuoteAsset,
		&network, &v[0], &v[1], &v[2], &v[3], &v[4], &b.Status, &b.EvidenceRef, &b.EvidenceSHA256, &b.BookedAt,
		&b.ReversalJournalID, &b.ReversalEffectID, &b.ReversalReasonCode, &b.ReversedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, false, nil
	}
	if err != nil {
		return Booking{}, false, dbErr("load fee booking", err)
	}
	for i, dst := range []*money.Quantity{&b.Charges.Spread, &b.Charges.Platform, &b.Charges.Strategy, &b.Charges.Venue, &b.Charges.Network} {
		x, err := money.ScanQuantity(v[i])
		if err != nil {
			return Booking{}, false, errs.Wrap(err, errs.CodeInternal, "feeledger: stored booking amounts")
		}
		*dst = x
	}
	if network != nil {
		b.Charges.NetworkAsset = *network
		b.Charges.NetworkInQuote = *network == b.Charges.QuoteAsset
	}
	b.BookedAt = b.BookedAt.UTC()
	return b, true, nil
}

// Booking returns one booking.
func (s *Service) Booking(ctx context.Context, q db.Querier, bid BookingID) (Booking, error) {
	b, found, err := s.bookingWhere(ctx, q, `id = $1`, bid)
	if err != nil {
		return Booking{}, err
	}
	if !found {
		return Booking{}, errs.New(errs.CodeNotFound, "feeledger: booking not found").WithField("booking_id", bid.String())
	}
	return b, nil
}

// VerifyBooking re-proves a booking end to end from the record: the stored
// disclosure verifies against its schedule, the booking's amounts are the
// disclosure's, and its journal carries exactly those charges.
func (s *Service) VerifyBooking(ctx context.Context, q db.Querier, bid BookingID) error {
	b, err := s.Booking(ctx, q, bid)
	if err != nil {
		return err
	}
	d, err := s.VerifyQuoteRecord(ctx, q, b.DisclosureID)
	if err != nil {
		return err
	}
	c := ChargesOf(d)
	if !c.Spread.Equal(b.Charges.Spread) || !c.Platform.Equal(b.Charges.Platform) || !c.Strategy.Equal(b.Charges.Strategy) ||
		!c.Venue.Equal(b.Charges.Venue) || !c.Network.Equal(b.Charges.Network) {
		return rejection(errs.CodeValidationFailed, ReasonRecordDoesNotVerify, "the booking's amounts are not its disclosure's")
	}
	jt, err := s.ledger.Transaction(ctx, q, b.JournalTransactionID)
	if err != nil {
		return err
	}
	return carriesCharges(jt, b.AccountID, c)
}
