package bridge

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// RouteReconciliation is the BRIDGE reconciliation of one route (goal §18):
// the route's books against both chains, at one instant.
//
// The books (the ledger, which is authoritative) say:
//
//	Locked      = BRIDGE_LOCKED(source)        units whose lock is final and not refunded
//	Outstanding = BRIDGE_OUTSTANDING(destination) units released on the destination
//	InFlight    = Σ customers' BRIDGE_IN_FLIGHT(source)
//	BooksConserve: Locked = Outstanding + InFlight   (also enforced at COMMIT, BR003)
//
// The chains (a simulation, derived) say:
//
//	Escrow            = the source chain's escrow balance of the source mint
//	DestinationSupply = the destination mint's supply (only a release mints it)
//
// and the honest difference at an instant is what has landed on a chain and
// is not yet posted, plus the units a refund retired in the escrow:
//
//	Escrow            = Locked + Refunded + LandedUnpostedLocks
//	DestinationSupply = Outstanding + LandedUnpostedReleases
type RouteReconciliation struct {
	RouteID                RouteID        `json:"route_id"`
	Locked                 money.Quantity `json:"locked"`
	Outstanding            money.Quantity `json:"outstanding"`
	InFlight               money.Quantity `json:"in_flight"`
	Refunded               money.Quantity `json:"refunded"`
	Escrow                 money.Quantity `json:"escrow"`
	DestinationSupply      money.Quantity `json:"destination_supply"`
	LandedUnpostedLocks    money.Quantity `json:"landed_unposted_locks"`
	LandedUnpostedReleases money.Quantity `json:"landed_unposted_releases"`
	ExpectedEscrow         money.Quantity `json:"expected_escrow"`
	ExpectedSupply         money.Quantity `json:"expected_supply"`
	BooksConserve          bool           `json:"books_conserve"`
	EscrowMatches          bool           `json:"escrow_matches"`
	SupplyMatches          bool           `json:"supply_matches"`
	RecordID               string         `json:"record_id,omitempty"`
	BreakerTripped         bool           `json:"breaker_tripped"`
}

// Matched reports that the books conserve and both chains agree with them.
func (r RouteReconciliation) Matched() bool {
	return r.BooksConserve && r.EscrowMatches && r.SupplyMatches
}

// ReconcileRoute runs the BRIDGE reconciliation of a route in the caller's
// transaction. A mismatch opens (or refreshes) a material BRIDGE record that
// blocks new risk and trips the route's circuit breaker (ACTIVE -> PAUSED);
// legs already in flight still complete. A periodic or full run that matches
// records the match.
func (s *Service) ReconcileRoute(ctx context.Context, tx pgx.Tx, rid RouteID, mode reconciliation.Mode, actor econ.Actor) (RouteReconciliation, error) {
	if err := requireActor(actor); err != nil {
		return RouteReconciliation{}, err
	}
	r, err := s.getRoute(ctx, tx, rid, true)
	if err != nil {
		return RouteReconciliation{}, err
	}
	return s.reconcileRouteLocked(ctx, tx, r, mode, actor)
}

func (s *Service) facilityBalance(ctx context.Context, tx pgx.Tx, r Route, code ledger.Code, asset assets.AssetID) (money.Quantity, error) {
	var text string
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(b.balance), 0)::text FROM ledger_accounts la
		JOIN ledger_balances b ON b.ledger_account_id = la.id
		WHERE la.owner_type = 'FACILITY' AND la.owner_id = $1 AND la.code = $2 AND la.asset_id = $3`,
		r.FacilityID, string(code), asset).Scan(&text); err != nil {
		return money.Quantity{}, dbErr("read bridge facility balance", err)
	}
	return money.ScanQuantity(text)
}

func (s *Service) reconcileRouteLocked(ctx context.Context, tx pgx.Tx, r Route, mode reconciliation.Mode, actor econ.Actor) (RouteReconciliation, error) {
	out := RouteReconciliation{RouteID: r.ID}
	var err error
	if out.Locked, err = s.facilityBalance(ctx, tx, r, ledger.CodeBridgeLocked, r.SourceAssetID); err != nil {
		return RouteReconciliation{}, err
	}
	if out.Outstanding, err = s.facilityBalance(ctx, tx, r, ledger.CodeBridgeOutstanding, r.DestinationAssetID); err != nil {
		return RouteReconciliation{}, err
	}
	var inflight, refunded string
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(b.balance), 0)::text FROM ledger_accounts la
		JOIN ledger_balances b ON b.ledger_account_id = la.id
		WHERE la.owner_type = 'CUSTOMER' AND la.code = 'BRIDGE_IN_FLIGHT' AND la.asset_id = $1`, r.SourceAssetID).Scan(&inflight); err != nil {
		return RouteReconciliation{}, dbErr("sum bridge in flight", err)
	}
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(quantity), 0)::text FROM bridge_legs
		WHERE route_id = $1 AND refund_journal_transaction_id IS NOT NULL`, r.ID).Scan(&refunded); err != nil {
		return RouteReconciliation{}, dbErr("sum refunded legs", err)
	}
	if out.InFlight, err = money.ScanQuantity(inflight); err != nil {
		return RouteReconciliation{}, dbErr("sum bridge in flight", err)
	}
	if out.Refunded, err = money.ScanQuantity(refunded); err != nil {
		return RouteReconciliation{}, dbErr("sum refunded legs", err)
	}
	locks, err := listLegs(ctx, tx, `route_id = $1 AND status = 'BRIDGE_REQUESTED' ORDER BY id`, r.ID)
	if err != nil {
		return RouteReconciliation{}, err
	}
	relays, err := listLegs(ctx, tx, `route_id = $1 AND source_journal_transaction_id IS NOT NULL
		AND destination_journal_transaction_id IS NULL AND refund_journal_transaction_id IS NULL ORDER BY id`, r.ID)
	if err != nil {
		return RouteReconciliation{}, err
	}
	releases := make(map[LegID][]Attempt, len(relays))
	for _, l := range relays {
		if releases[l.ID], err = attemptsOf(ctx, tx, l.ID, AttemptRelease); err != nil {
			return RouteReconciliation{}, err
		}
	}
	// One View, read through this transaction: the balances and the statuses
	// that explain them, at one instant.
	err = s.host.ViewIn(ctx, tx, func(v devnethost.View) error {
		escrow, err := v.TokenBalance(r.SourceChain, devnet.BridgeEscrowAddress(r.SourceChain), r.SourceMint)
		if err != nil {
			return err
		}
		supply, err := v.Supply(r.DestinationChain, r.DestinationMint)
		if err != nil {
			return err
		}
		out.Escrow, out.DestinationSupply = devnet.QuantityOf(escrow), devnet.QuantityOf(supply)
		for _, l := range locks {
			st, err := v.Status(r.SourceChain, l.LockTxID)
			if err != nil {
				return err
			}
			if landed(st) {
				out.LandedUnpostedLocks = out.LandedUnpostedLocks.Add(l.Quantity)
			}
		}
		for _, l := range relays {
			for _, a := range releases[l.ID] {
				st, err := v.Status(a.Chain, a.TxID)
				if err != nil {
					return err
				}
				if landed(st) {
					// A lock is released at most once on the chain, so at most
					// one attempt of a leg has landed.
					out.LandedUnpostedReleases = out.LandedUnpostedReleases.Add(l.Quantity)
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return RouteReconciliation{}, err
	}
	out.ExpectedEscrow = out.Locked.Add(out.Refunded).Add(out.LandedUnpostedLocks)
	out.ExpectedSupply = out.Outstanding.Add(out.LandedUnpostedReleases)
	out.BooksConserve = out.Locked.Cmp(out.Outstanding.Add(out.InFlight)) == 0
	out.EscrowMatches = out.Escrow.Cmp(out.ExpectedEscrow) == 0
	out.SupplyMatches = out.DestinationSupply.Cmp(out.ExpectedSupply) == 0

	if out.Matched() {
		if mode == reconciliation.ModeEventDriven {
			return out, nil
		}
		if _, open, err := s.recon.FindUnresolvedByScope(ctx, tx, reconciliation.KindBridge, ScopeBridgeRoute, r.ID.String()); err != nil || open {
			return out, err
		}
		rec, err := s.recon.Open(ctx, tx, s.reconRequest(r, out, mode, actor, reconciliation.StatusMatched))
		if err != nil {
			return RouteReconciliation{}, err
		}
		out.RecordID = rec.ID.String()
		return out, nil
	}

	expected, observed, difference := routeReconDocuments(out)
	if existing, found, err := s.recon.FindUnresolvedByScopeForUpdate(ctx, tx, reconciliation.KindBridge, ScopeBridgeRoute, r.ID.String()); err != nil {
		return RouteReconciliation{}, err
	} else if found {
		if _, err := s.recon.UpdateObservation(ctx, tx, existing.ID, expected, observed, difference, true, true); err != nil {
			return RouteReconciliation{}, err
		}
		out.RecordID = existing.ID.String()
	} else {
		rec, err := s.recon.Open(ctx, tx, s.reconRequest(r, out, mode, actor, reconciliation.StatusMismatch))
		if err != nil {
			return RouteReconciliation{}, err
		}
		out.RecordID = rec.ID.String()
	}
	if r.Status == RouteActive {
		if _, err := s.transitionRouteLocked(ctx, tx, r, RoutePaused, breakerActor,
			"circuit breaker: the route's books disagree with the chains (BRIDGE reconciliation "+out.RecordID+")"); err != nil {
			return RouteReconciliation{}, err
		}
		out.BreakerTripped = true
	}
	return out, nil
}

func landed(st devnet.TxStatus) bool {
	return st.State == devnet.TxConfirmed || st.State == devnet.TxFinalized
}

func routeReconDocuments(r RouteReconciliation) (expected, observed, difference map[string]string) {
	expected = map[string]string{
		"locked": r.Locked.String(), "outstanding": r.Outstanding.String(), "in_flight": r.InFlight.String(),
		"refunded": r.Refunded.String(), "landed_unposted_locks": r.LandedUnpostedLocks.String(),
		"landed_unposted_releases": r.LandedUnpostedReleases.String(), "expected_escrow": r.ExpectedEscrow.String(),
		"expected_supply": r.ExpectedSupply.String(),
	}
	observed = map[string]string{"escrow": r.Escrow.String(), "destination_supply": r.DestinationSupply.String()}
	difference = map[string]string{
		"escrow_minus_expected":             r.Escrow.Sub(r.ExpectedEscrow).String(),
		"supply_minus_expected":             r.DestinationSupply.Sub(r.ExpectedSupply).String(),
		"locked_minus_outstanding_inflight": r.Locked.Sub(r.Outstanding).Sub(r.InFlight).String(),
	}
	return expected, observed, difference
}

func (s *Service) reconRequest(r Route, out RouteReconciliation, mode reconciliation.Mode, actor econ.Actor, status reconciliation.Status) reconciliation.OpenRequest {
	expected, observed, difference := routeReconDocuments(out)
	reason := "bridge route books, source escrow and destination supply agree"
	if status == reconciliation.StatusMismatch {
		reason = "bridge route books disagree with the source escrow or the destination supply"
	}
	return reconciliation.OpenRequest{
		Kind: reconciliation.KindBridge, Mode: mode, ScopeType: ScopeBridgeRoute, ScopeID: r.ID.String(),
		AssetID: r.SourceAssetID, Expected: expected, Observed: observed, Difference: difference, Status: status,
		Material: status == reconciliation.StatusMismatch, BlocksNewRisk: status == reconciliation.StatusMismatch,
		Actor: reconciliation.Actor{Type: actor.Type, ID: actor.ID}, Reason: reason,
	}
}
