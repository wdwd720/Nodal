package advancedjobs

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// ScopeOrderBook is the reconciliation scope type of an order-book journal
// record; the scope id is the book id.
const ScopeOrderBook = "order_book"

// OrdersBookProof proves, from the database alone, that every OPEN or HALTED
// order book's journal is a deterministic record of it
// (orders.Service.VerifyJournal: replaying every journalled command from the
// empty book, and the latest snapshot plus the tail, reproduces the recorded
// events, state hashes and orders). VerifyJournal changes nothing and runs in
// a REPEATABLE READ snapshot, so the cursor, the journal and the orders it
// compares are one view of a book that may still be trading.
//
// A journal that does not replay -- a replay hash or event that differs, a
// command that no longer decodes or applies, an order the replay does not
// hold -- opens a material reconciliation record (or refreshes the one still
// unresolved for the book) through internal/reconciliation's Repository.
// There is no reconciliation kind for an order book yet, so the record is
// LEDGER_INTERNAL -- internal books that disagree with themselves -- scoped
// to the book (ScopeOrderBook). A failure to read the book is not a finding
// and is reported as a failed unit instead.
type OrdersBookProof struct {
	db    *db.DB
	svc   *orders.Service
	recon *reconciliation.Repository
	spec  Spec
	pages pager
}

// NewOrdersBookProof wires the job.
func NewOrdersBookProof(d *db.DB, svc *orders.Service, recon *reconciliation.Repository, s Settings) *OrdersBookProof {
	return &OrdersBookProof{db: d, svc: svc, recon: recon, spec: spec(NameOrdersBookProof, s)}
}

// Spec implements Job.
func (j *OrdersBookProof) Spec() Spec { return j.spec }

type bookUnit struct {
	id         string
	instrument instruments.InstrumentID
}

func (u bookUnit) cursorKey() string { return u.id }

// provableBooksSQL lists the books that are not CLOSED -- OPEN, or HALTED and
// still taking cancels -- after a paging key, in id order. Read-only; orders
// exports no lister.
const provableBooksSQL = `SELECT id::text, instrument_id FROM book_states
	WHERE status IN ('OPEN','HALTED') AND id > $1::uuid ORDER BY id LIMIT $2`

// Run proves each book in its own snapshot and records each finding in its
// own transaction.
func (j *OrdersBookProof) Run(ctx context.Context) (Report, error) {
	units, err := readPage(ctx, &j.pages, j.db, j.spec.Batch, func(ctx context.Context, q db.Querier, after string, limit int) ([]bookUnit, error) {
		rows, err := q.Query(ctx, provableBooksSQL, after, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []bookUnit
		for rows.Next() {
			var u bookUnit
			if err := rows.Scan(&u.id, &u.instrument); err != nil {
				return nil, err
			}
			out = append(out, u)
		}
		return out, rows.Err()
	})
	if err != nil {
		return Report{}, err
	}
	var rep Report
	for _, u := range units {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Examined++
		verr := inSnapshot(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			_, err := j.svc.VerifyJournal(ctx, tx, u.instrument)
			return err
		})
		switch {
		case verr == nil:
			continue
		case !journalDiscrepancy(verr):
			rep.fail(u.id, verr)
			continue
		}
		if err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			return j.record(ctx, tx, u, verr)
		}); err != nil {
			rep.fail(u.id, err)
			continue
		}
		rep.Changed++
	}
	return rep, nil
}

// journalDiscrepancy reports whether a VerifyJournal error is a finding about
// the journal rather than a failure to read it. Everything the database or
// the connection said, the deadline, and a book that is not there are
// failures to read; what remains is the replay telling the stored journal
// apart from the book.
func journalDiscrepancy(err error) bool {
	var pgErr *pgconn.PgError
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.As(err, &pgErr), errors.As(err, &netErr), pgconn.Timeout(err), pgconn.SafeToRetry(err):
		return false
	case errors.Is(err, pgx.ErrTxClosed), errors.Is(err, db.ErrRetriesExhausted):
		return false
	case errs.CodeOf(err) == errs.CodeNotFound:
		return false
	}
	return true
}

// record opens the book's reconciliation record, or refreshes the one still
// unresolved, with the book's recorded cursor as what was expected and the
// redacted finding as what was observed.
func (j *OrdersBookProof) record(ctx context.Context, tx pgx.Tx, u bookUnit, finding error) error {
	code, reason := Redact(finding)
	if code == string(errs.CodeInternal) {
		// The replay's findings are INTERNAL by code, but they describe the
		// journal, not the process: keep the subsystem's own words.
		if e, ok := errs.As(finding); ok {
			reason = sanitize(e.Detail)
		}
	}
	expected := map[string]string{
		"claim": "replaying every journalled command from the empty book, and the latest snapshot plus the tail, " +
			"reproduces the recorded events, state hashes and orders",
		"book_id": u.id, "instrument_id": u.instrument.String(),
	}
	if p, err := j.svc.Proof(ctx, tx, u.instrument); err == nil {
		expected["last_command_seq"] = strconv.FormatInt(p.LastCommandSeq, 10)
		expected["state_hash"] = hex.EncodeToString(p.StateHash[:])
		expected["snapshot_seq"] = strconv.FormatInt(p.SnapshotSeq, 10)
	}
	observed := map[string]string{"verified": "false", "code": code, "finding": reason}
	difference := map[string]string{"journal": "does not replay to the recorded book"}
	prior, open, err := j.recon.FindUnresolvedByScopeForUpdate(ctx, tx, reconciliation.KindLedgerInternal, ScopeOrderBook, u.id)
	if err != nil {
		return err
	}
	if open {
		_, err = j.recon.UpdateObservation(ctx, tx, prior.ID, expected, observed, difference, true, false)
		return err
	}
	_, err = j.recon.Open(ctx, tx, reconciliation.OpenRequest{
		Kind: reconciliation.KindLedgerInternal, Mode: reconciliation.ModePeriodic, ScopeType: ScopeOrderBook, ScopeID: u.id,
		Expected: expected, Observed: observed, Difference: difference,
		Status: reconciliation.StatusMismatch, Material: true, Actor: workerReconActor(),
		Reason: "the order book's journal does not replay to the recorded book",
	})
	return err
}
