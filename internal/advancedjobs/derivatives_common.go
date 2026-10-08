package advancedjobs

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/instruments"
)

// maxListedInstruments is the most instruments of one type
// instruments.Repository.ListWithTerms returns.
const maxListedInstruments = 1000

// listInstruments reads the instruments of the given types with their current
// terms through instruments.Repository.ListWithTerms -- the exported reader,
// in the REPEATABLE READ snapshot its documentation asks for -- in instrument
// id order.
func listInstruments(ctx context.Context, d *db.DB, repo *instruments.Repository, types ...instruments.InstrumentType) ([]instruments.Listed, error) {
	var out []instruments.Listed
	err := inSnapshot(ctx, d, func(ctx context.Context, tx pgx.Tx) error {
		out = out[:0]
		for _, t := range types {
			l, err := repo.ListWithTerms(ctx, tx, t, maxListedInstruments)
			if err != nil {
				return err
			}
			out = append(out, l...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Instrument.ID.String() < out[k].Instrument.ID.String() })
	return out, nil
}

// instrumentsWithOpenPositionsSQL lists the instruments that have at least one
// OPEN derivative position. Read-only; derivatives exports no lister.
const instrumentsWithOpenPositionsSQL = `SELECT DISTINCT instrument_id FROM derivative_positions WHERE status = 'OPEN'`

// instrumentSet reads a set of instrument ids.
func instrumentSet(ctx context.Context, q db.Querier, sql string, args ...any) (map[instruments.InstrumentID]bool, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[instruments.InstrumentID]bool{}
	for rows.Next() {
		var id instruments.InstrumentID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// floorDiv is integer division rounding toward negative infinity.
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// gridStart is the start of the interval of length ms containing t, on the
// grid of whole intervals since the Unix epoch -- the grid the derivatives
// runs require of an interval start.
func gridStart(t time.Time, ms int64) time.Time {
	return time.UnixMilli(floorDiv(t.UnixMilli(), ms) * ms).UTC()
}

// latestCompleted is the start of the latest interval of length ms that has
// ended at or before now.
func latestCompleted(now time.Time, ms int64) time.Time {
	return gridStart(now, ms).Add(-time.Duration(ms) * time.Millisecond)
}

// keyedInstrument adapts an instrument for rotation.
type keyedInstrument struct{ instruments.Listed }

func (k keyedInstrument) cursorKey() string { return k.Instrument.ID.String() }

// rotate returns items (in key order) starting after the key `after`, then
// wrapping round, so a batch smaller than the population reaches every item
// in turn.
func rotate[T keyed](items []T, after string) []T {
	for i, it := range items {
		if it.cursorKey() > after {
			return append(append([]T(nil), items[i:]...), items[:i]...)
		}
	}
	return items
}
