package orders

import (
	"context"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// Every fee this package charges is booked through the fee ledger
// (internal/feeledger), in the transaction of the fill that charges it, once:
//
//   - The internal book charges each customer party of a fill the explicit fee
//     of its role's schedule (maker or taker). The schedules are frozen on the
//     book when it opens (OB008) and published then as the fee-ledger versions
//     of BookScheduleKey; a fill re-ensures that the version in force is still
//     exactly the book's schedule (feeledger.EnsureSchedule) and is refused
//     otherwise, because a buy's reservation was sized from that schedule.
//     Each paying party gets its own quote disclosure (RecordQuote, keyed on
//     the trade and the role), its fee is posted inside the trade's SIM_TRADE
//     journal with the disclosure's own entries (feeledger.ChargeEntries), and
//     the disclosure is registered against that journal and the trade's
//     effect (RecordEmbedded).
//   - The paper venue charges whatever schedule version is in force under its
//     key (PaperScheduleKey) at the fill instant, which an operator manages
//     through the fee ledger; the fill's disclosure is recorded and booked the
//     same way against its own SIM_TRADE journal and effect.
//
// A fill that charges nothing books nothing. A house party pays no fee. The
// database refuses a fill row whose fee has no booking (00761, FW001).

// FeeRole is the role whose schedule prices a party's fee on the internal
// book.
type FeeRole string

// Fee roles.
const (
	FeeMaker FeeRole = "maker"
	FeeTaker FeeRole = "taker"
)

// BookScheduleKey is the fee-ledger key under which an instrument's internal
// book publishes the schedule of one role: "orders:book:<instrument id>:maker"
// or ":taker".
func BookScheduleKey(inst instruments.InstrumentID, role FeeRole) string {
	return "orders:book:" + inst.String() + ":" + string(role)
}

// PaperScheduleKeyPrefix is the default prefix of the paper venue's schedule
// keys.
const PaperScheduleKeyPrefix = "orders:paper"

// paperKeyPrefixPattern bounds a prefix so that a whole key (prefix, colon,
// instrument id) is a valid fee-ledger schedule key.
var paperKeyPrefixPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:/-]{0,90}$`)

// PaperScheduleKey is the fee-ledger key of an instrument's paper-venue
// schedule under a key prefix: "<prefix>:<instrument id>".
func PaperScheduleKey(prefix string, inst instruments.InstrumentID) string {
	return prefix + ":" + inst.String()
}

// schedule returns the role's frozen schedule and its hash.
func (c *bookConfig) schedule(role FeeRole) (fees.Schedule, string) {
	if role == FeeMaker {
		return c.MakerSchedule, c.MakerScheduleHash
	}
	return c.TakerSchedule, c.TakerScheduleHash
}

// chargesFees reports whether a fill on the book can charge a fee at all:
// a schedule carries a percentage or a strategy rate (ValidateBookSchedule
// allows nothing else).
func (c *bookConfig) chargesFees() bool {
	for _, s := range []fees.Schedule{c.MakerSchedule, c.TakerSchedule} {
		if s.PercentageFeeBPS > 0 || s.StrategyFeeBPS > 0 {
			return true
		}
	}
	return false
}

// bookScheduleRequest is the publication of a book's schedule for a role.
func bookScheduleRequest(c *bookConfig, role FeeRole, actor econ.Actor) feeledger.PublishRequest {
	sched, _ := c.schedule(role)
	return feeledger.PublishRequest{
		Key: BookScheduleKey(c.InstrumentID, role), Schedule: sched, Actor: actor,
		Reason: "the " + string(role) + " fee schedule of internal book " + c.ID.String() + ", frozen when the book opened",
	}
}

// publishBookSchedules makes the book's maker and taker schedules the
// fee-ledger versions in force under their keys, or refuses.
func (s *Service) publishBookSchedules(ctx context.Context, tx pgx.Tx, c *bookConfig, actor econ.Actor) error {
	for _, role := range []FeeRole{FeeMaker, FeeTaker} {
		p, err := s.feeLedger.EnsureSchedule(ctx, tx, bookScheduleRequest(c, role, actor))
		if err != nil {
			return err
		}
		if _, hash := c.schedule(role); p.Hash != hash {
			return errs.New(errs.CodeInternal, "orders: the fee ledger published another schedule than the book's").
				WithField("schedule_key", p.Key)
		}
	}
	return nil
}

// requireCharging authorizes SPREAD_ENGINE in the book's mode when the book
// can charge a fee: the fee ledger records a charge only under it, so an order
// that could be charged is refused at its own stage rather than at a fill.
func (s *Service) requireCharging(ctx context.Context, q db.Querier, c *bookConfig, mode gates.Mode) error {
	if !c.chargesFees() {
		return nil
	}
	return s.auth.Require(ctx, q, gates.SpreadEngine, mode)
}

// tradeKey is the deterministic key of one trade of a book.
func tradeKey(book BookID, tradeSeq int64) string {
	return "orders:trade:" + book.String() + ":" + strconv.FormatInt(tradeSeq, 10)
}

// feeCharge is one customer party's disclosed fee on a fill.
type feeCharge struct {
	order orderRow
	quote feeledger.StoredQuote
}

// quoteFill discloses one paying party's fee on a fill in the fee ledger: the
// book's schedule for the party's role is ensured in force, the party's share
// of the fill is quoted under it, and the disclosure must charge exactly the
// fee the book computed -- the explicit fee and nothing else -- or the fill is
// refused.
func (s *Service) quoteFill(ctx context.Context, tx pgx.Tx, c *bookConfig, role FeeRole, o orderRow, tradeSeq, lots int64,
	price money.Price, fee money.Quantity) (feeledger.StoredQuote, error) {
	req := bookScheduleRequest(c, role, s.actor)
	pub, err := s.feeLedger.EnsureSchedule(ctx, tx, req)
	if err != nil {
		return feeledger.StoredQuote{}, err
	}
	base := c.Terms.BaseAsset
	q, err := s.feeLedger.RecordQuote(ctx, tx, feeledger.QuoteRequest{
		ScheduleKey: req.Key, At: feeledger.QuoteAt(pub, s.clk.Now().UTC()),
		Input: fillQuoteInput(o.Side, c.Terms, lots, o.strategy(), price), Subsystem: econ.SubsystemOrders,
		IdempotencyKey: tradeKey(c.ID, tradeSeq) + ":" + string(role), AccountID: o.AccountID, BaseAsset: &base, Mode: c.Mode,
		Actor: s.actor,
	})
	if err != nil {
		return feeledger.StoredQuote{}, err
	}
	_, hash := c.schedule(role)
	ch := q.Charges
	if q.ScheduleHash != hash || !ch.Spread.IsZero() || !ch.Venue.IsZero() || !ch.Network.IsZero() || !ch.Platform.Add(ch.Strategy).Equal(fee) {
		return feeledger.StoredQuote{}, errs.New(errs.CodeInternal, "orders: the fee ledger's disclosure of a fill is not the fee the book charges").
			WithField("disclosure_id", q.ID.String()).WithField("fee", fee.String())
	}
	return q, nil
}

// bookFills registers every disclosed fee of a trade against the trade's
// journal and the effect that applied it.
func (s *Service) bookFills(ctx context.Context, tx pgx.Tx, c *bookConfig, charges []feeCharge, journal ledger.TransactionID, effect econ.EffectID) error {
	for _, ch := range charges {
		if _, err := s.feeLedger.RecordEmbedded(ctx, tx, feeledger.EmbeddedRequest{
			DisclosureID: ch.quote.ID, AccountID: *ch.order.AccountID, JournalTransactionID: journal, EffectID: effect,
			IntentID: ch.order.IntentID, Mode: c.Mode, Actor: s.actor,
		}); err != nil {
			return err
		}
	}
	return nil
}
