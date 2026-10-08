package marketplace

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/sandboxrt"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
	"github.com/nodal/controlplane/internal/valuation"
)

// A follow's input and its paper performance come from what the service reads
// itself, never from a caller (review-2 F10):
//
//   - the install's PAPER BOOK: the paper capital recorded on the install,
//     moved by the fills of the install's own dispatched intents
//     (marketplace_paper_fills) -- each the dispatched quantity at the
//     reference price the run valued it at;
//   - REFERENCE PRICES: for every instrument the version names, the freshest
//     observation of its pair in the price store (asset_prices) no older than
//     MaxPriceAge at the run's as-of time, and up to MaxCloses observations
//     before it as the instrument's closes.
//
// The run's equity is the book's cash plus every position at its reference
// price, exactly; the database recomputes it from the recorded fills and marks
// at COMMIT (MK013). Deposits, withdrawals and anything else the account does
// never enter a book.

// paperBook is an install's paper portfolio.
type paperBook struct {
	cash nodalsdk.Decimal
	// positions maps an instrument to its signed quantity in base units.
	positions map[string]nodalsdk.Decimal
}

// paperFill is one dispatched intent applied to a paper book.
type paperFill struct {
	IntentID     econ.IntentID
	InstrumentID string
	Side         sandboxrt.Side
	Quantity     money.Quantity
	Mark         nodalsdk.Decimal
	Notional     nodalsdk.Decimal
}

// apply moves the book by a fill: a buy spends its notional and adds its
// quantity; a sell does the opposite.
func (b *paperBook) apply(f paperFill) error {
	qty, err := nodalsdk.ParseUnits(f.Quantity.String())
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "marketplace: fill quantity unreadable")
	}
	pos := b.positions[f.InstrumentID]
	switch f.Side {
	case sandboxrt.SideBuy:
		b.cash, pos = b.cash.Sub(f.Notional), pos.Add(qty)
	case sandboxrt.SideSell:
		b.cash, pos = b.cash.Add(f.Notional), pos.Sub(qty)
	default:
		return errs.Newf(errs.CodeInternal, "marketplace: fill side %q", f.Side)
	}
	b.positions[f.InstrumentID] = pos
	return nil
}

// held returns the instruments the book holds a non-zero position in, sorted.
func (b *paperBook) held() []string {
	var out []string
	for id, q := range b.positions {
		if q.Sign() != 0 {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// loadBook reads an install's paper book: its paper capital and every fill
// recorded for it.
func (s *Service) loadBook(ctx context.Context, q db.Querier, iid InstallID) (paperBook, error) {
	var capitalText string
	if err := q.QueryRow(ctx, `SELECT paper_capital::text FROM marketplace_installs WHERE id = $1`, iid).Scan(&capitalText); err != nil {
		return paperBook{}, dbErr("load paper capital", err)
	}
	capital, err := nodalsdk.ParseDecimal(capitalText)
	if err != nil {
		return paperBook{}, errs.Wrap(err, errs.CodeInternal, "marketplace: stored paper capital unreadable")
	}
	book := paperBook{cash: capital, positions: map[string]nodalsdk.Decimal{}}
	rows, err := q.Query(ctx, `SELECT intent_id, instrument_id::text, side, quantity::text, mark::text, notional::text
		FROM marketplace_paper_fills WHERE install_id = $1 ORDER BY created_at, id`, iid)
	if err != nil {
		return paperBook{}, dbErr("load paper fills", err)
	}
	defer rows.Close()
	for rows.Next() {
		var f paperFill
		var qty, mark, notional string
		if err := rows.Scan(&f.IntentID, &f.InstrumentID, &f.Side, &qty, &mark, &notional); err != nil {
			return paperBook{}, dbErr("scan paper fill", err)
		}
		if f.Quantity, err = money.ParseQuantity(qty); err != nil {
			return paperBook{}, errs.Wrap(err, errs.CodeInternal, "marketplace: stored fill quantity unreadable")
		}
		if f.Mark, err = nodalsdk.ParseDecimal(mark); err != nil {
			return paperBook{}, errs.Wrap(err, errs.CodeInternal, "marketplace: stored fill mark unreadable")
		}
		if f.Notional, err = nodalsdk.ParseDecimal(notional); err != nil {
			return paperBook{}, errs.Wrap(err, errs.CodeInternal, "marketplace: stored fill notional unreadable")
		}
		if err := book.apply(f); err != nil {
			return paperBook{}, err
		}
	}
	return book, rows.Err()
}

// reference is an instrument's reference price as the service read it.
type reference struct {
	InstrumentID  string
	Kind          string
	BaseDecimals  uint8
	QuoteDecimals uint8
	PriceID       valuation.PriceID
	Mark          nodalsdk.Decimal
	ObservedAt    time.Time
	// Closes are earlier observations of the pair, oldest first, ending with
	// the mark.
	Closes []nodalsdk.Decimal
}

// FollowMark is a reference price a follow run valued its book and its
// proposals at: the price store's observation, by id.
type FollowMark struct {
	InstrumentID string            `json:"instrument_id"`
	PriceID      valuation.PriceID `json:"price_id"`
	Mark         string            `json:"mark"`
	ObservedAt   time.Time         `json:"observed_at"`
}

// references reads the reference price of each instrument at asOf. An
// instrument without a pair of assets, or without an observation of its pair
// within MaxPriceAge before asOf, has none and is left out: a strategy is
// never shown, and a book is never valued at, a price the service did not
// read itself.
//
// The freshness rule is valuation.PriceStore.Latest's, read here by id so the
// run can record which observation it used.
func (s *Service) references(ctx context.Context, q db.Querier, ids []string, asOf time.Time) (map[string]reference, error) {
	out := map[string]reference{}
	if len(ids) == 0 {
		return out, nil
	}
	type pair struct {
		id, kind      string
		base, quote   string
		baseD, quoteD int16
	}
	rows, err := q.Query(ctx, `SELECT i.id::text, i.type, i.base_asset_id::text, i.quote_asset_id::text, b.decimals, qa.decimals
		FROM instruments i JOIN assets b ON b.id = i.base_asset_id JOIN assets qa ON qa.id = i.quote_asset_id
		WHERE i.id = ANY($1::uuid[]) ORDER BY i.id`, ids)
	if err != nil {
		return nil, dbErr("load instruments", err)
	}
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.kind, &p.base, &p.quote, &p.baseD, &p.quoteD); err != nil {
			rows.Close()
			return nil, dbErr("scan instrument", err)
		}
		pairs = append(pairs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, dbErr("load instruments", err)
	}
	asOf = asOf.UTC()
	for _, p := range pairs {
		if !nodalsdk.ValidKind(p.kind) || p.baseD < 0 || p.baseD > 18 || p.quoteD < 0 || p.quoteD > 18 {
			continue
		}
		obs, err := q.Query(ctx, `SELECT id, mantissa::text, scale, observed_at FROM asset_prices
			WHERE asset_id = $1 AND quote_asset_id = $2 AND observed_at <= $3
			ORDER BY observed_at DESC, received_at DESC, id DESC LIMIT $4`, p.base, p.quote, asOf, s.cfg.MaxCloses)
		if err != nil {
			return nil, dbErr("load reference prices", err)
		}
		var latest []observation
		for obs.Next() {
			var o observation
			if err := obs.Scan(&o.id, &o.mantissa, &o.scale, &o.at); err != nil {
				obs.Close()
				return nil, dbErr("scan reference price", err)
			}
			latest = append(latest, o)
		}
		obs.Close()
		if err := obs.Err(); err != nil {
			return nil, dbErr("load reference prices", err)
		}
		if len(latest) == 0 || latest[0].at.UTC().Before(asOf.Add(-s.cfg.MaxPriceAge)) {
			continue
		}
		ref := reference{InstrumentID: p.id, Kind: p.kind, BaseDecimals: uint8(p.baseD), QuoteDecimals: uint8(p.quoteD), // #nosec G115 -- 0..18 by the check above
			PriceID: latest[0].id, ObservedAt: latest[0].at.UTC()}
		for i := len(latest) - 1; i >= 0; i-- {
			d, err := latest[i].decimal()
			if err != nil {
				return nil, err
			}
			ref.Closes = append(ref.Closes, d)
		}
		ref.Mark = ref.Closes[len(ref.Closes)-1]
		if ref.Mark.Sign() <= 0 {
			continue
		}
		out[p.id] = ref
	}
	return out, nil
}

type observation struct {
	id       valuation.PriceID
	mantissa string
	scale    int32
	at       time.Time
}

// decimal renders mantissa x 10^-scale as an exact decimal.
func (o observation) decimal() (nodalsdk.Decimal, error) {
	return decimalOf(o.mantissa, o.scale)
}

// decimalOf builds mantissa x 10^-scale from a non-negative integer string.
func decimalOf(mantissa string, scale int32) (nodalsdk.Decimal, error) {
	if mantissa == "" || strings.TrimLeft(mantissa, "0123456789") != "" || scale < 0 || scale > nodalsdk.MaxScale {
		return nodalsdk.Decimal{}, errs.New(errs.CodeInternal, "marketplace: a stored price is not a non-negative decimal")
	}
	digits := strings.TrimLeft(mantissa, "0")
	if digits == "" {
		digits = "0"
	}
	if scale == 0 {
		d, err := nodalsdk.ParseDecimal(digits)
		if err != nil {
			return nodalsdk.Decimal{}, errs.Wrap(err, errs.CodeInternal, "marketplace: stored price unreadable")
		}
		return d, nil
	}
	if pad := int(scale) + 1 - len(digits); pad > 0 {
		digits = strings.Repeat("0", pad) + digits
	}
	cut := len(digits) - int(scale)
	d, err := nodalsdk.ParseDecimal(digits[:cut] + "." + digits[cut:])
	if err != nil {
		return nodalsdk.Decimal{}, errs.Wrap(err, errs.CodeInternal, "marketplace: stored price unreadable")
	}
	return d, nil
}

// universe is every instrument a version's runs may see: the instruments its
// book holds, its manifest allows by id and it declares it supports, in that
// priority, at most nodalsdk.MaxInstruments.
func universe(v Version, book paperBook) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ids []string) {
		sorted := append([]string(nil), ids...)
		sort.Strings(sorted)
		for _, id := range sorted {
			if !seen[id] && len(out) < nodalsdk.MaxInstruments {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	add(book.held())
	add(v.Manifest.AllowedInstrumentIDs)
	add(v.SupportedInstrumentIDs)
	sort.Strings(out)
	return out
}

// followInput is the input document of a follow run: the instruments with a
// reference price, the paper book, and the version's approved configuration.
func followInput(asOf time.Time, refs map[string]reference, book paperBook, config map[string]string) sandboxrt.Input {
	in := sandboxrt.Input{Schema: sandboxrt.InputSchema, AsOf: asOf.UTC(), Config: make(map[string]string, len(config)),
		Portfolio: sandboxrt.InputPortfolio{Cash: book.cash.String(), Positions: []sandboxrt.InputPosition{}}}
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := refs[id]
		closes := make([]string, len(r.Closes))
		for i, c := range r.Closes {
			closes[i] = c.String()
		}
		in.Instruments = append(in.Instruments, sandboxrt.InputInstrument{InstrumentID: id, Kind: r.Kind, BaseDecimals: r.BaseDecimals,
			QuoteDecimals: r.QuoteDecimals, Mark: r.Mark.String(), Closes: closes})
	}
	for _, id := range book.held() {
		in.Portfolio.Positions = append(in.Portfolio.Positions, sandboxrt.InputPosition{InstrumentID: id, Quantity: book.positions[id].String()})
	}
	for k, v := range config {
		in.Config[k] = v
	}
	return in
}

// referencePrices are the marks a run's proposals are valued at.
func referencePrices(refs map[string]reference) sandboxrt.ReferencePrices {
	out := make(sandboxrt.ReferencePrices, len(refs))
	for id, r := range refs {
		out[id] = r.Mark
	}
	return out
}

// usdFloor and usdCeil render an exact decimal in cents, toward zero's floor
// and ceiling: a paper book's spendable cash is never overstated and what an
// intent spends is never understated.
func usdFloor(d nodalsdk.Decimal) (money.USD, error) { return usdAt(d, nodalsdk.RoundFloor) }

func usdCeil(d nodalsdk.Decimal) (money.USD, error) { return usdAt(d, nodalsdk.RoundCeil) }

func usdAt(d nodalsdk.Decimal, mode nodalsdk.Rounding) (money.USD, error) {
	cents, err := d.Rescale(2, mode)
	if err != nil {
		return money.USD{}, errs.Wrap(err, errs.CodeOverflow, "marketplace: amount out of range")
	}
	u := cents.Unscaled()
	if !u.IsInt64() {
		return money.USD{}, errs.New(errs.CodeOverflow, "marketplace: amount does not fit in USD minor units")
	}
	return money.USDFromMinor(u.Int64()), nil
}
