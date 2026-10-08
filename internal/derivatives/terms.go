package derivatives

import (
	"context"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
)

// A derivative contract's terms are frozen while it has open positions.
//
// Every computation of this package -- a fill's value, margin, variation
// margin, funding, exercise, final settlement, valuation -- reads one Spec per
// instrument: its current terms. A position does not carry its own copy. If
// the terms could change under open positions, the next run would move value
// at an unchanged price (a doubled multiplier doubles Q × V(mark) −
// settled_value), and two positions of one contract could no longer be valued
// the same way, which funding conservation and exercise/assignment depend on.
// So an amendment is refused while any position on the instrument is OPEN
// (the database refuses it too: a new terms version or a change of the
// current terms row raises DV007), and every operation that trades, runs,
// settles or exercises share-locks the terms row first (lockSpec), so an
// amendment and a first open cannot pass each other.

// RequireTermsAmendable is the pre-check for amending a derivative contract's
// terms (instruments.Repository.AmendTerms): it refuses, with CONFLICT and the
// number of open positions, while the contract has any. Close or settle them
// first, or list a new contract with the new terms.
func RequireTermsAmendable(ctx context.Context, q db.Querier, iid instruments.InstrumentID) error {
	var open int64
	if err := q.QueryRow(ctx, `SELECT count(*) FROM derivative_positions WHERE instrument_id = $1 AND status = 'OPEN'`, iid).
		Scan(&open); err != nil {
		return dbErr("count open positions", err)
	}
	if open > 0 {
		return errs.Newf(errs.CodeConflict,
			"derivatives: contract %s has %d open position(s); its terms are frozen until they are closed or settled", iid, open).
			WithField("instrument_id", iid.String()).WithField("open_positions", open)
	}
	return nil
}
