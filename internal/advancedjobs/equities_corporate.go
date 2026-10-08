package advancedjobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/equities"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
)

// EquitiesCorporateActions applies announced corporate actions on the day
// they take effect (equities.Service.ApplyCorporateAction): a split or reverse
// split or a symbol change on its effective date, a cash dividend on its pay
// date, a delisting on its effective date (DELISTING) and again on its final
// date (DELISTED). Applying an applied action is a replay that posts nothing.
//
// The subsystem refuses an action whose preconditions have not cleared yet --
// orders on the instrument still live, holdings unsettled or reserved, an
// earlier split not yet applied -- with CONFLICT. Those clear by themselves
// (orders expire, settlements settle), so the action is reported Deferred and
// tried again next run, not counted as a failure.
type EquitiesCorporateActions struct {
	db   *db.DB
	svc  *equities.Service
	clk  clock.Clock
	spec Spec
}

// NewEquitiesCorporateActions wires the job.
func NewEquitiesCorporateActions(d *db.DB, svc *equities.Service, clk clock.Clock, s Settings) *EquitiesCorporateActions {
	return &EquitiesCorporateActions{db: d, svc: svc, clk: clk, spec: spec(NameEquitiesCorporate, s)}
}

// Spec implements Job.
func (j *EquitiesCorporateActions) Spec() Spec { return j.spec }

// openActionsSQL lists the corporate actions not yet fully applied -- the
// ones ApplyCorporateAction has anything to do for -- with the dates that
// decide when. Read-only; equities exports no lister.
const openActionsSQL = `SELECT id, instrument_id, action_type, status, effective_date, pay_date, final_date
	FROM equity_corporate_actions WHERE status IN ('ANNOUNCED','APPLYING') ORDER BY effective_date, id`

type corporateUnit struct {
	id         equities.CorporateActionID
	instrument instruments.InstrumentID
	kind       equities.CorporateActionType
	status     equities.CorporateActionStatus
	effective  time.Time
	pay, final *time.Time
}

// dueDate is the date the subsystem applies the action's next step on.
func (u corporateUnit) dueDate() (equities.Date, bool) {
	switch {
	case u.kind == equities.ActionCashDividend:
		if u.pay == nil {
			return equities.Date{}, false
		}
		return equities.DateOf(*u.pay), true
	case u.kind == equities.ActionDelisting && u.status == equities.ActionApplying:
		if u.final == nil {
			return equities.Date{}, false
		}
		return equities.DateOf(*u.final), true
	default:
		return equities.DateOf(u.effective), true
	}
}

// Run applies each due action in its own transaction, earliest first.
func (j *EquitiesCorporateActions) Run(ctx context.Context) (Report, error) {
	now := j.clk.Now().UTC()
	var due []corporateUnit
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, openActionsSQL)
		if err != nil {
			return err
		}
		var all []corporateUnit
		for rows.Next() {
			var u corporateUnit
			if err := rows.Scan(&u.id, &u.instrument, &u.kind, &u.status, &u.effective, &u.pay, &u.final); err != nil {
				rows.Close()
				return err
			}
			all = append(all, u)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// "Today" is the instrument's own trade date: the New York calendar
		// day of now, read through the subsystem's listing and calendar.
		today := map[instruments.InstrumentID]equities.Date{}
		for _, u := range all {
			if len(due) >= j.spec.Batch {
				break
			}
			d, ok := today[u.instrument]
			if !ok {
				l, err := j.svc.LoadListing(ctx, tx, u.instrument)
				if err != nil {
					return err
				}
				cal, err := j.svc.Calendar(ctx, tx, l.Terms.Terms.TradingCalendar)
				if err != nil {
					return err
				}
				d = cal.TradeDate(now)
				today[u.instrument] = d
			}
			if when, ok := u.dueDate(); ok && !d.Before(when) {
				due = append(due, u)
			}
		}
		return nil
	}); err != nil {
		return Report{}, err
	}
	var rep Report
	for _, u := range due {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Examined++
		var res equities.CorporateActionResult
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = j.svc.ApplyCorporateAction(ctx, tx, u.id, workerActor())
			return err
		})
		switch {
		case errs.CodeOf(err) == errs.CodeConflict:
			rep.Deferred++
		case err != nil:
			rep.fail(u.id.String(), err)
		case !res.Replayed:
			rep.Changed++
		}
	}
	return rep, nil
}
