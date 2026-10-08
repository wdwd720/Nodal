package equities

import (
	"bytes"
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
)

// Every spread and fee an equity fill charges is booked through the fee
// ledger (internal/feeledger), in the transaction that applies the fill, once:
//
//   - The schedule. A settlement asset's schedule is fixed by the
//     deployment's configuration (Config.Schedules), so a fill makes that
//     exact schedule the fee-ledger version in force under its equities key
//     (FeeScheduleKey) before any charge is disclosed
//     (feeledger.EnsureSchedule) and prices at QuoteAt. The key names the
//     settlement asset and the schedule's hash: a configuration that changes
//     the schedule publishes under a new key, and one that changes it back
//     finds its own version still in force. Another version published under
//     the key refuses the fill rather than reprices it.
//   - The disclosure. The payer's quote is recorded in the fee ledger
//     (RecordQuote, keyed on the order's trade, bound to the payer) and must
//     be byte for byte the disclosure the fill stores and the customer
//     sees, so there is one disclosure, kept in both records.
//   - The booking. The charges are posted inside the fill's SIM_TRADE
//     journal with the fee ledger's own entries (TradeEntries,
//     feeledger.ChargeEntries: every component on its own pair) and the
//     disclosure is registered against that journal and the effect that
//     applied it (RecordEmbedded).
//
// The fee ledger books a charge only under SPREAD_ENGINE. An order whose
// schedule can charge its fill needs it in its mode when it is accepted, and
// a buy at the venue does not execute while it is refused (executionBlocked):
// a buy is new activity. A sell reduces risk, which no kill switch stops, so
// its charge is booked as risk-reducing (feeledger RiskReducing) and the sell
// executes whatever stops new activity. A fill that charges nothing books
// nothing. The database refuses an equity fill whose charges have no booking
// (00764, FW001).

// FeeScheduleKey is the fee-ledger key under which equities publishes the fee
// schedule of a settlement asset: "equities:<settlement asset id>:<schedule
// hash>".
func FeeScheduleKey(cash assets.AssetID, scheduleHash string) string {
	return "equities:" + cash.String() + ":" + scheduleHash
}

// scheduleCanCharge reports whether a fill priced under s can be charged
// anything at all: a spread, a fixed, percentage, venue or strategy fee, or a
// minimum fee, in the base schedule or in a volume tier. An equity schedule
// carries no network cost (NewService).
func scheduleCanCharge(s fees.Schedule) bool {
	if s.SpreadBPS > 0 || s.FixedFee.IsPositive() || s.PercentageFeeBPS > 0 || s.VenuePassThroughBPS > 0 ||
		s.StrategyFeeBPS > 0 || s.MinFee.IsPositive() {
		return true
	}
	for _, t := range s.Tiers {
		if (t.OverridesSpread && t.SpreadBPS > 0) || (t.OverridesPercentageFee && t.PercentageFeeBPS > 0) {
			return true
		}
	}
	return false
}

// canCharge reports whether a fill of the listing can be charged anything.
func (s *Service) canCharge(l Listing) bool {
	sch, ok := s.schedules[l.CashAsset.ID]
	return ok && scheduleCanCharge(sch)
}

// chargingBlocked reports whether the fee ledger would refuse a buy's charge
// in mode now (SPREAD_ENGINE not authorized or kill-switched), when the
// listing's schedule can charge the fill.
func (s *Service) chargingBlocked(ctx context.Context, q db.Querier, l Listing, mode gates.Mode) (bool, error) {
	if !s.canCharge(l) {
		return false, nil
	}
	d, err := s.auth.Decide(ctx, q, gates.SpreadEngine, mode)
	if err != nil {
		return false, err
	}
	return !d.Allowed, nil
}

// discloseFill makes the listing's schedule the fee-ledger version in force
// under its equities key and records the payer's disclosure of the fill's
// charges under it, which must be byte for byte the disclosure the fill
// stores (disclosure, disclosureHash), or the fill is refused.
func (s *Service) discloseFill(ctx context.Context, tx pgx.Tx, o Order, l Listing, sch fees.Schedule, in fees.QuoteInput,
	disclosure []byte, disclosureHash string, now time.Time,
) (feeledger.StoredQuote, error) {
	hash, err := sch.Hash()
	if err != nil {
		return feeledger.StoredQuote{}, err
	}
	key := FeeScheduleKey(l.CashAsset.ID, hash)
	pub, err := s.feeLedger.EnsureSchedule(ctx, tx, feeledger.PublishRequest{
		Key: key, Schedule: sch, Actor: s.actor,
		Reason: "the equities fee schedule " + sch.Version + " of settlement asset " + l.CashAsset.ID.String() +
			", fixed by the deployment's configuration",
	})
	if err != nil {
		return feeledger.StoredQuote{}, err
	}
	if pub.Hash != hash {
		return feeledger.StoredQuote{}, errs.New(errs.CodeInternal, "equities: the fee ledger published another schedule than the listing's").
			WithField("schedule_key", key)
	}
	payer, share := o.AccountID, l.ShareAsset.ID
	q, err := s.feeLedger.RecordQuote(ctx, tx, feeledger.QuoteRequest{
		ScheduleKey: key, At: feeledger.QuoteAt(pub, now), Input: in, Subsystem: econ.SubsystemEquities,
		IdempotencyKey: tradeKey(o.ID), AccountID: &payer, BaseAsset: &share, Mode: o.Mode, Actor: s.actor,
		RiskReducing: o.Side == SideSell,
	})
	if err != nil {
		return feeledger.StoredQuote{}, err
	}
	if q.ScheduleHash != hash || q.DisclosureHash != disclosureHash || !bytes.Equal(q.DisclosureBytes, disclosure) {
		return feeledger.StoredQuote{}, errs.New(errs.CodeInternal, "equities: the fee ledger disclosed another quote than the fill's").
			WithField("disclosure_id", q.ID.String()).WithField("order_id", o.ID.String())
	}
	return q, nil
}

// bookFill registers a fill's disclosed charges against the SIM_TRADE journal
// that posted them and the effect that applied it.
func (s *Service) bookFill(ctx context.Context, tx pgx.Tx, o Order, q feeledger.StoredQuote, journal ledger.TransactionID,
	effect econ.EffectID,
) error {
	iid := o.IntentID
	_, err := s.feeLedger.RecordEmbedded(ctx, tx, feeledger.EmbeddedRequest{
		DisclosureID: q.ID, AccountID: o.AccountID, JournalTransactionID: journal, EffectID: effect, IntentID: &iid,
		Mode: o.Mode, CorrelationID: o.CorrelationID, Actor: s.actor, RiskReducing: o.Side == SideSell,
	})
	return err
}
