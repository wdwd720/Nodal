package advancedjobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/equities"
	"github.com/nodal/controlplane/internal/errs"
)

// EquitiesVenue is the venue worker of paper equities: every order the venue
// still owes an answer gets one, the way the operator's venue run gives it
// (httpapi's equities adapter, RunVenue), on a schedule.
//
//   - An ACCEPTED order is placed (equities.Service.PlaceAtVenue, outside any
//     transaction). The service sends only a submission still PENDING; one
//     whose outcome is UNKNOWN answers RECONCILIATION_REQUIRED and is polled
//     instead (PollVenue) -- an order whose submission is unknown is never
//     placed again. When a placement itself answers SUBMISSION_STATE_UNKNOWN
//     the job records exactly that (MarkSubmissionUnknown, in its own
//     transaction) and leaves the order for the next run's poll to resolve.
//     Any other placement error leaves the submission PENDING, which the
//     service lets the next run send again.
//   - An OPEN order is polled: a resting limit order fills when the mark
//     crosses, and every order expires at the end of its day.
//
// Each answer is applied with ApplyOrderReport in its own transaction, even
// when the run's deadline has passed meanwhile: the venue has answered, and
// the answer is recorded exactly once (a second answer for a terminal order
// is a replay, or a CONFLICT that changes nothing). An order that moved on
// between the read and the call (INVALID_STATE_TRANSITION) is not a failure.
//
// The API places an order the moment it accepts it, so an ACCEPTED order is
// only this job's once it is older than PlacementGrace: the job never races a
// request that is about to place the same order. The ACCEPTED orders come
// first, oldest first; the rest of the batch pages through the OPEN orders by
// id, so a book of resting orders larger than a batch is still polled in
// turn. equities.Service.OpenOrders is not used for the worklist: it returns
// the oldest ACCEPTED and OPEN orders together with no cursor, so more
// resting orders than a batch would keep newer ones -- and new placements --
// from ever being reached.
//
// Kill switches: the job consults none itself. The service withholds the mark
// from an order a kill switch or its instrument's status now blocks (a buy is
// NEW_RISK, a sell REDUCE_RISK), so a blocked order never executes -- a
// resting one keeps resting and a blocked market order is refused -- while
// an unblocked sell still can.
type EquitiesVenue struct {
	db   *db.DB
	svc  *equities.Service
	clk  clock.Clock
	spec Spec
	open pager
}

// PlacementGrace is how old an ACCEPTED order must be before the job places
// it.
const PlacementGrace = 30 * time.Second

// NewEquitiesVenue wires the job.
func NewEquitiesVenue(d *db.DB, svc *equities.Service, clk clock.Clock, s Settings) *EquitiesVenue {
	return &EquitiesVenue{db: d, svc: svc, clk: clk, spec: spec(NameEquitiesVenue, s)}
}

// Spec implements Job.
func (j *EquitiesVenue) Spec() Spec { return j.spec }

// The worklist reads. Read-only. created_at is written by the service from
// its clock, so the grace is measured on the same clock.
const (
	// acceptedOrdersSQL: ACCEPTED orders past the grace, oldest first.
	acceptedOrdersSQL = `SELECT id, status FROM equity_orders
		WHERE status = 'ACCEPTED' AND created_at <= $1 ORDER BY created_at, id LIMIT $2`
	// restingOrdersSQL: OPEN orders after a paging key, in id order.
	restingOrdersSQL = `SELECT id, status FROM equity_orders
		WHERE status = 'OPEN' AND id > $1::uuid ORDER BY id LIMIT $2`
)

type venueUnit struct {
	id     equities.OrderID
	status equities.OrderStatus
}

func (u venueUnit) cursorKey() string { return u.id.String() }

// Run answers each order on its own.
func (j *EquitiesVenue) Run(ctx context.Context) (Report, error) {
	var accepted []venueUnit
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		accepted, err = scanVenueUnits(ctx, tx, acceptedOrdersSQL, j.clk.Now().UTC().Add(-PlacementGrace), j.spec.Batch)
		return err
	}); err != nil {
		return Report{}, err
	}
	units := accepted
	if left := j.spec.Batch - len(accepted); left > 0 {
		resting, err := readPage(ctx, &j.open, j.db, left, func(ctx context.Context, q db.Querier, after string, limit int) ([]venueUnit, error) {
			return scanVenueUnits(ctx, q, restingOrdersSQL, after, limit)
		})
		if err != nil {
			return Report{}, err
		}
		units = append(units, resting...)
	}
	var rep Report
	for _, u := range units {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Examined++
		j.answer(ctx, u, &rep)
	}
	return rep, nil
}

// answer gets the venue's answer for one order and applies it.
func (j *EquitiesVenue) answer(ctx context.Context, u venueUnit, rep *Report) {
	var (
		report equities.OrderReport
		err    error
	)
	if u.status == equities.OrderAccepted {
		report, err = j.svc.PlaceAtVenue(ctx, j.db, u.id)
		switch errs.CodeOf(err) {
		case errs.CodeReconciliationRequired:
			// Its submission's outcome is unknown: resolved by polling,
			// never sent again.
			report, err = j.svc.PollVenue(ctx, j.db, u.id)
		case errs.CodeSubmissionStateUnknown:
			// The venue may or may not have it. Record that, so nothing ever
			// sends it again, and let the next run's poll resolve it.
			cause := err
			err = inUnit(context.WithoutCancel(ctx), j.db, func(ctx context.Context, tx pgx.Tx) error {
				return j.svc.MarkSubmissionUnknown(ctx, tx, u.id, "the venue's answer to the placement is unknown: "+boundedReason(cause))
			})
			if err != nil {
				rep.fail(u.id.String(), err)
				return
			}
			rep.Deferred++
			return
		}
	} else {
		report, err = j.svc.PollVenue(ctx, j.db, u.id)
	}
	if err != nil {
		if errs.CodeOf(err) == errs.CodeInvalidStateTransition {
			return // it moved on since the worklist was read
		}
		rep.fail(u.id.String(), err)
		return
	}
	var after equities.Order
	err = inUnit(context.WithoutCancel(ctx), j.db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		after, err = j.svc.ApplyOrderReport(ctx, tx, u.id, report)
		return err
	})
	if err != nil {
		rep.fail(u.id.String(), err)
		return
	}
	if after.Status != u.status {
		rep.Changed++
	}
}

// boundedReason is an error's redacted reason, for a record that keeps it.
func boundedReason(err error) string {
	_, reason := Redact(err)
	return reason
}

func scanVenueUnits(ctx context.Context, q db.Querier, sql string, args ...any) ([]venueUnit, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []venueUnit
	for rows.Next() {
		var u venueUnit
		if err := rows.Scan(&u.id, &u.status); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
