package predmarket

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// ScopePredictionMarket is the reconciliation scope type of a market's
// PREDICTION_ESCROW records; the scope id is the market id.
const ScopePredictionMarket = "prediction_market"

// ReconcileRequest asks for one market's escrow to be reconciled.
type ReconcileRequest struct {
	MarketID MarketID
	Mode     reconciliation.Mode
	// Actor defaults to the reconciliation engine's system actor.
	Actor  reconciliation.Actor
	Reason string
}

// ReconcileEscrow compares a market's escrow with the outcome shares it has
// outstanding -- the PREDICTION_ESCROW reconciliation kind -- and records the
// result through the reconciliation repository:
//
//	expected (internal truth): escrow = collateral = shares outstanding(i) + disclosed residual,
//	                           shares issued(i) = shares outstanding(i) = held by customers(i)
//	observed:                  the facility's balances and the customer holdings, one snapshot
//
// A market whose books agree gets a MATCHED record. One that does not gets a
// material MISMATCH naming every violation and the escrow's difference from
// what its shares imply; while it stays unresolved, re-running the comparison
// refreshes that record instead of opening another. Issuance, redemption,
// settlement and voids all re-check the same identity themselves and refuse
// while it is broken.
func (s *Service) ReconcileEscrow(ctx context.Context, tx pgx.Tx, req ReconcileRequest) (reconciliation.Record, error) {
	m, err := s.Get(ctx, tx, req.MarketID)
	if err != nil {
		return reconciliation.Record{}, err
	}
	residual, err := s.disclosedResidual(ctx, tx, m.ID)
	if err != nil {
		return reconciliation.Record{}, err
	}
	books, err := s.Books(ctx, tx, m)
	if err != nil {
		return reconciliation.Record{}, err
	}
	violations := books.Violations(residual)

	expectedEscrow := make([]string, 0, len(m.Outcomes))
	escrowDiff := make([]string, 0, len(m.Outcomes))
	for _, o := range books.SharesOutstanding {
		implied := o.Add(residual).Add(books.Held)
		expectedEscrow = append(expectedEscrow, implied.String())
		escrowDiff = append(escrowDiff, books.Escrow.Sub(implied).String())
	}
	expected := map[string]any{
		"identity":                  "escrow = collateral = shares_outstanding(i) + residual + held_unclaimed; shares_issued(i) = shares_outstanding(i) = held_by_customers(i)",
		"escrow_implied_by_outcome": expectedEscrow,
		"residual":                  residual.String(),
		"held_unclaimed":            books.Held.String(),
		"market_status":             string(m.Status),
	}
	observed := map[string]any{
		"escrow":             books.Escrow.String(),
		"collateral":         books.Collateral.String(),
		"shares_issued":      quantities(books.SharesIssued),
		"shares_outstanding": quantities(books.SharesOutstanding),
		"held_by_customers":  quantities(books.HeldByCustomers),
	}
	if violations == nil {
		violations = []string{}
	}
	difference := map[string]any{
		"escrow_minus_implied": escrowDiff,
		"violations":           violations,
	}
	status := reconciliation.StatusMatched
	if len(violations) > 0 {
		status = reconciliation.StatusMismatch
	}
	mode := req.Mode
	if mode == "" {
		mode = reconciliation.ModePeriodic
	}
	actor := req.Actor
	if actor.Type == "" {
		actor = reconciliation.SystemActor()
	}
	reason := req.Reason
	if reason == "" {
		reason = "prediction market escrow reconciliation"
	}

	prior, found, err := s.recon.FindUnresolvedByScopeForUpdate(ctx, tx, reconciliation.KindPredictionEscrow, ScopePredictionMarket, m.ID.String())
	if err != nil {
		return reconciliation.Record{}, err
	}
	if found {
		return s.recon.UpdateObservation(ctx, tx, prior.ID, expected, observed, difference, len(violations) > 0, false)
	}
	rec, err := s.recon.Open(ctx, tx, reconciliation.OpenRequest{
		Kind: reconciliation.KindPredictionEscrow, Mode: mode, ScopeType: ScopePredictionMarket, ScopeID: m.ID.String(),
		AssetID: m.CollateralAssetID, Expected: expected, Observed: observed, Difference: difference,
		Status: status, Material: len(violations) > 0, Actor: actor, Reason: reason,
	})
	if err != nil {
		return reconciliation.Record{}, err
	}
	if rec.Kind != reconciliation.KindPredictionEscrow {
		return reconciliation.Record{}, errs.New(errs.CodeInternal, "predmarket: the reconciliation record has the wrong kind")
	}
	return rec, nil
}

func quantities(qs []money.Quantity) []string {
	out := make([]string, len(qs))
	for i, q := range qs {
		out[i] = q.String()
	}
	return out
}
