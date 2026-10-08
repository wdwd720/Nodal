package advancedjobs

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/instruments"
)

// DerivativesFunding charges perpetual funding (derivatives.Service.RunFunding)
// for every completed funding interval in which some position of the
// perpetual was open at the interval's end and which has not been funded yet.
//
// Each (instrument, interval) is one unit, keyed by the interval's start on
// the instrument's funding grid; funding_rates is UNIQUE per (instrument,
// interval start), so a re-run of a funded interval is a replay that posts
// nothing. Unlike variation margin, funding is owed per interval: an interval
// the worker missed is not subsumed by a later one, so unfunded intervals are
// found by difference against funding_rates rather than from the latest one,
// and an interval that fails stays a candidate however many later ones
// succeed. Candidates run newest first, so the interval that just ended is
// never held up behind an old one that keeps failing (an interval without a
// single mark with an index cannot be funded, and says so every run).
type DerivativesFunding struct {
	db    *db.DB
	svc   *derivatives.Service
	instr *instruments.Repository
	clk   clock.Clock
	spec  Spec
}

// NewDerivativesFunding wires the job.
func NewDerivativesFunding(d *db.DB, svc *derivatives.Service, instr *instruments.Repository, clk clock.Clock, s Settings) *DerivativesFunding {
	return &DerivativesFunding{db: d, svc: svc, instr: instr, clk: clk, spec: spec(NameDerivativesFunding, s)}
}

// Spec implements Job.
func (j *DerivativesFunding) Spec() Spec { return j.spec }

// fundingUnit is one interval of one perpetual.
type fundingUnit struct {
	instrument instruments.InstrumentID
	start      time.Time
}

// positionSpansSQL lists when each position of an instrument was open.
// Read-only; derivatives exports no lister. A position is open at t when
// opened_at < t and it is not closed before t -- RunFunding's own predicate.
const positionSpansSQL = `SELECT opened_at, closed_at FROM derivative_positions WHERE instrument_id = $1`

// fundedIntervalsSQL lists the interval starts already funded.
const fundedIntervalsSQL = `SELECT interval_start FROM funding_rates WHERE instrument_id = $1 AND interval_start >= $2`

type span struct {
	opened time.Time
	closed *time.Time
}

func (s span) openAt(t time.Time) bool {
	return s.opened.Before(t) && (s.closed == nil || !s.closed.Before(t))
}

// Run funds each due interval in its own transaction.
func (j *DerivativesFunding) Run(ctx context.Context) (Report, error) {
	now := j.clk.Now().UTC()
	perps, err := listInstruments(ctx, j.db, j.instr, instruments.TypePerpetual)
	if err != nil {
		return Report{}, err
	}
	var due []fundingUnit
	var rep Report
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		for _, p := range perps {
			ms := p.Current.Terms.FundingIntervalMS
			if ms <= 0 {
				continue
			}
			units, err := unfundedIntervals(ctx, tx, p.Instrument.ID, ms, now)
			if err != nil {
				return err
			}
			due = append(due, units...)
		}
		return nil
	}); err != nil {
		return Report{}, err
	}
	sort.Slice(due, func(a, b int) bool {
		if !due[a].start.Equal(due[b].start) {
			return due[a].start.After(due[b].start)
		}
		return due[a].instrument.String() < due[b].instrument.String()
	})
	if len(due) > j.spec.Batch {
		due = due[:j.spec.Batch]
	}
	for _, u := range due {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Examined++
		corr := "advanced-worker/" + NameDerivativesFunding + "/" + strconv.FormatInt(u.start.UnixMilli(), 10)
		var res derivatives.RunResult
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = j.svc.RunFunding(ctx, tx, derivatives.RunRequest{InstrumentID: u.instrument, IntervalStart: u.start, CorrelationID: corr})
			return err
		})
		if err != nil {
			rep.fail(u.instrument.String()+"@"+u.start.Format(time.RFC3339), err)
			continue
		}
		if !res.Replay {
			rep.Changed++
		}
	}
	return rep, nil
}

// unfundedIntervals lists a perpetual's completed, unfunded intervals at
// whose end some position was open, from the interval in which its first
// position opened to the latest completed one.
func unfundedIntervals(ctx context.Context, q db.Querier, id instruments.InstrumentID, ms int64, now time.Time) ([]fundingUnit, error) {
	rows, err := q.Query(ctx, positionSpansSQL, id)
	if err != nil {
		return nil, err
	}
	var spans []span
	for rows.Next() {
		var s span
		if err := rows.Scan(&s.opened, &s.closed); err != nil {
			rows.Close()
			return nil, err
		}
		spans = append(spans, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(spans) == 0 {
		return nil, nil
	}
	first := spans[0].opened
	for _, s := range spans[1:] {
		if s.opened.Before(first) {
			first = s.opened
		}
	}
	from := gridStart(first, ms)
	latest := latestCompleted(now, ms)
	if latest.Before(from) {
		return nil, nil
	}
	funded := map[int64]bool{}
	frows, err := q.Query(ctx, fundedIntervalsSQL, id, from)
	if err != nil {
		return nil, err
	}
	for frows.Next() {
		var s time.Time
		if err := frows.Scan(&s); err != nil {
			frows.Close()
			return nil, err
		}
		funded[s.UnixMilli()] = true
	}
	frows.Close()
	if err := frows.Err(); err != nil {
		return nil, err
	}
	step := time.Duration(ms) * time.Millisecond
	var out []fundingUnit
	for s := from; !s.After(latest); s = s.Add(step) {
		if funded[s.UnixMilli()] {
			continue
		}
		end := s.Add(step)
		for _, sp := range spans {
			if sp.openAt(end) {
				out = append(out, fundingUnit{instrument: id, start: s})
				break
			}
		}
	}
	return out, nil
}
