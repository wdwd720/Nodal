package activity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// Feed reads the timeline.
type Feed struct {
	// simulated marks every item on a deployment where no value is real. It
	// is a deployment fact (ADR-0023's sandbox tier), resolved once at wiring
	// rather than per request, because a request cannot choose it.
	simulated bool
}

// NewFeed returns a Feed. simulated is cfg.SandboxTier(): on a sandbox tier no
// value can move anywhere, so every amount in the feed is SIMULATED and says so.
func NewFeed(simulated bool) *Feed { return &Feed{simulated: simulated} }

// cursor is an opaque "(occurred_at, id)" position.
type cursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

func encodeCursor(c cursor) string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursor, bool, error) {
	if strings.TrimSpace(s) == "" {
		return cursor{}, false, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, false, errs.New(errs.CodeValidationFailed, "cursor is not a valid pagination cursor")
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil || c.ID == "" || c.At.IsZero() {
		return cursor{}, false, errs.New(errs.CodeValidationFailed, "cursor is not a valid pagination cursor")
	}
	return c, true, nil
}

// farFuture bounds the first page, so one statement serves both the first page
// and every later one. It is the same device internal/httpapi's read model uses.
var farFuture = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

const farFutureID = "ffffffff-ffff-ffff-ffff-ffffffffffff"

// Activity returns one page of an account's timeline, newest first.
//
// The kind filter narrows which SOURCES run rather than filtering their output,
// so asking for one kind reads one table instead of seven and then discarding
// six. That is the difference between a feed a page can poll and one it cannot.
func (f *Feed) Activity(ctx context.Context, q db.Querier, r Request) (Page, error) {
	if err := r.Validate(); err != nil {
		return Page{}, err
	}
	limit := r.Limit
	if limit <= 0 || limit > MaxLimit {
		limit = 25
	}
	c, hasCursor, err := decodeCursor(r.Cursor)
	if err != nil {
		return Page{}, err
	}
	at, id := farFuture, farFutureID
	if hasCursor {
		at, id = c.At, c.ID
	}

	// The kind filter is a bound parameter, not a rebuilt statement: see
	// sources.go for why the union is one compiled-in constant. NULL means
	// every kind, which is what an empty filter is.
	var kinds []string
	for _, k := range r.Kinds {
		kinds = append(kinds, string(k))
	}

	rows, err := q.Query(ctx, feedQuery, r.AccountID, kinds, at, id, limit+1)
	if err != nil {
		return Page{}, errs.Wrap(err, errs.CodeInternal, "activity: the timeline could not be read")
	}
	defer rows.Close()

	out := make([]Item, 0, limit+1)
	for rows.Next() {
		var (
			it                          Item
			kind, refType, refID        string
			credits, assetUnits         string
			moneyMinor                  int64
			currency, origin, side, sym string
			demo                        bool
		)
		if err := rows.Scan(&kind, &it.OccurredAt, &it.ID, &refType, &refID, &it.Status,
			&credits, &moneyMinor, &currency, &origin, &side, &sym, &assetUnits, &demo); err != nil {
			return Page{}, errs.Wrap(err, errs.CodeInternal, "activity: the timeline could not be read")
		}
		it.Kind = Kind(kind)
		it.OccurredAt = it.OccurredAt.UTC()
		it.Reference = Reference{Type: refType, ID: refID}
		it.Simulated = demo || f.simulated
		it.Summary = summaryFor(it.Kind, it.Status, side, sym)
		it.Amounts = f.amountsOf(it.Simulated, credits, moneyMinor, currency, origin, sym, assetUnits)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return Page{}, errs.Wrap(err, errs.CodeInternal, "activity: the timeline could not be read")
	}

	page := Page{Items: out}
	if len(out) > limit {
		page.Items = out[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(cursor{At: last.OccurredAt, ID: last.ID})
	}
	return page, nil
}

// amountsOf turns the source's columns into the amounts the item carries.
//
// A zero amount is omitted rather than reported as "0": an item with no money
// on it and an item with none left are different facts, and a column that is
// zero because this kind has no money for it is the first.
func (f *Feed) amountsOf(simulated bool, credits string, moneyMinor int64, currency, origin, symbol, assetUnits string) []Amount {
	// The deployment's own flag is ORed in here rather than only at the call
	// site: on a sandbox tier nothing can move anywhere, and an amount that
	// could be rendered REAL by passing false would be a way to lose that.
	simulated = simulated || f.simulated
	temp := func(real bool) Temperature {
		switch {
		case simulated:
			return TemperatureSimulated
		case real:
			return TemperatureReal
		default:
			return TemperatureEconomy
		}
	}
	var out []Amount
	if !isZeroDigits(credits) {
		out = append(out, Amount{
			Unit: UnitCredits, Value: credits, Origin: origin, Temperature: temp(false),
		})
	}
	if moneyMinor != 0 {
		out = append(out, Amount{
			Unit: UnitMoneyMinor, Value: strconv.FormatInt(moneyMinor, 10),
			Currency: currency, Temperature: temp(true),
		})
	}
	if !isZeroDigits(assetUnits) {
		out = append(out, Amount{
			Unit: UnitAssetUnits, Value: assetUnits, Symbol: sanitizeSymbol(symbol),
			// A native asset is internal, closed-loop platform value like a
			// Credit: it is never real money, whatever the deployment is.
			Temperature: temp(false),
		})
	}
	return out
}

// isZeroDigits reports whether a numeric's text form is zero, without parsing
// it: numeric(38,0) renders as "0", and a leading sign or padding would be a
// bug in a cast rather than something to be tolerant of.
func isZeroDigits(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || s == "0"
}
