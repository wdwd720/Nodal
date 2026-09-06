package reconciliation

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
)

// VerifyInternal runs the PART 21 internal consistency checks
// (RECONCILIATION.md §6) and records every drift as a LEDGER_INTERNAL
// mismatch: material, blocking new risk, and SEV1
// `ledger_integrity_violation`.
//
//	Σ journal entries      == ledger_balances          (ledger.VerifyBalances)
//	Σ active reservations  == asset_reservation_totals (capital.VerifyReservationTotals)
//	envelope allocation    == available + reserved + deployed
//
// Drift here means a write reached a projection outside a posting: a
// privileged role, a restored backup, or a bug. It is never "expected noise".
func (e *Engine) VerifyInternal(ctx context.Context) ([]Record, error) {
	out := []Record{}

	drifts, err := ledger.VerifyBalances(ctx, e.db)
	if err != nil {
		return out, err
	}
	for _, d := range drifts {
		rec, err := e.recordLedgerDrift(ctx, d)
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}

	totals, err := capital.VerifyReservationTotals(ctx, e.db)
	if err != nil {
		return out, err
	}
	for _, d := range totals {
		rec, err := e.recordReservationDrift(ctx, d)
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}

	envelopes, err := capital.VerifyEnvelopeBudgets(ctx, e.db)
	if err != nil {
		return out, err
	}
	for _, d := range envelopes {
		rec, err := e.recordEnvelopeDrift(ctx, d)
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func (e *Engine) recordLedgerDrift(ctx context.Context, d ledger.Drift) (Record, error) {
	acct := accounts.AccountID{}
	if d.Account.OwnerType == ledger.OwnerCustomer {
		if parsed, err := accounts.ParseAccountID(d.Account.OwnerID); err == nil {
			acct = parsed
		}
	}
	diff := d.StoredBalance.Sub(d.ComputedBalance)
	req := OpenRequest{
		Kind: KindLedgerInternal, Mode: ModeFull, ScopeType: ScopeLedger, ScopeID: d.LedgerAccountID.String(),
		AccountID: acct, AssetID: d.Account.AssetID,
		Expected: map[string]any{
			"source": "sum(journal_entries)", "balance": d.ComputedBalance.String(), "entry_count": d.ComputedEntryCount,
			"ledger_account": d.Account.String(),
		},
		Observed: map[string]any{
			"source": "ledger_balances", "balance": d.StoredBalance.String(), "entry_count": d.StoredEntryCount,
		},
		Difference: map[string]any{
			"balance": diff.String(), "entry_count": d.StoredEntryCount - d.ComputedEntryCount,
		},
		Status: StatusMismatch, Material: true, BlocksNewRisk: true,
		Actor: SystemActor(), Reason: "ledger projection drift",
	}
	return e.upsertTx(ctx, req)
}

func (e *Engine) recordReservationDrift(ctx context.Context, d capital.Drift) (Record, error) {
	req := OpenRequest{
		Kind: KindLedgerInternal, Mode: ModeFull, ScopeType: ScopeAccount,
		ScopeID:   "reservations:" + d.AccountID.String() + ":" + d.AssetID.String(),
		AccountID: d.AccountID, AssetID: d.AssetID,
		Expected:   map[string]any{"source": "sum(active asset_reservations)", "reserved": d.Computed.String()},
		Observed:   map[string]any{"source": "asset_reservation_totals", "reserved": d.Recorded.String()},
		Difference: map[string]any{"reserved": d.Delta().String()},
		Status:     StatusMismatch, Material: true, BlocksNewRisk: true,
		Actor: SystemActor(), Reason: "reservation totals drift",
	}
	return e.upsertTx(ctx, req)
}

func (e *Engine) recordEnvelopeDrift(ctx context.Context, d capital.EnvelopeDrift) (Record, error) {
	req := OpenRequest{
		Kind: KindLedgerInternal, Mode: ModeFull, ScopeType: ScopeSystem, ScopeID: "envelope:" + d.EnvelopeID.String(),
		Expected: map[string]any{
			"source": "allocation", "allocation_usd_minor": d.Allocation.Minor(),
			"active_reserved_usd_minor": d.ActiveReserved.Minor(),
		},
		Observed: map[string]any{
			"available_usd_minor": d.Available.Minor(), "reserved_usd_minor": d.Reserved.Minor(),
			"deployed_usd_minor": d.Deployed.Minor(),
		},
		Difference: map[string]any{
			"allocation_minus_flow_usd_minor": d.Allocation.Minor() - d.Available.Minor() - d.Reserved.Minor() - d.Deployed.Minor(),
			"reserved_minus_active_usd_minor": d.Reserved.Minor() - d.ActiveReserved.Minor(),
		},
		Status: StatusMismatch, Material: true, BlocksNewRisk: false,
		Actor: SystemActor(), Reason: "envelope budget drift",
	}
	return e.upsertTx(ctx, req)
}

// verifyPositions compares Σ open lots with the WALLET ledger balance per
// asset for one account (positions.VerifyAgainstLedger) and records every
// difference as a POSITION_LEDGER mismatch.
func (e *Engine) verifyPositions(ctx context.Context, accountID accounts.AccountID) ([]Record, error) {
	if e.positions == nil {
		return []Record{}, nil
	}
	drifts, err := e.positions.VerifyAgainstLedger(ctx, e.db, accountID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "reconciliation: verify positions against ledger")
	}
	out := []Record{}
	for _, d := range drifts {
		a, err := e.asset(ctx, d.AssetID)
		if err != nil {
			return out, err
		}
		req := OpenRequest{
			Kind: KindPositionLedger, Mode: ModeFull, ScopeType: ScopeAccount,
			ScopeID:   "positions:" + accountID.String() + ":" + d.AssetID.String(),
			AccountID: accountID, AssetID: d.AssetID,
			Expected: map[string]any{
				"source": "ledger.WALLET", "quantity": d.LedgerQuantity.String(),
				"decimal": d.LedgerQuantity.ToDecimalString(a.Decimals),
			},
			Observed: map[string]any{
				"source": "sum(open position_lots)", "quantity": d.LotQuantity.String(),
				"decimal": d.LotQuantity.ToDecimalString(a.Decimals),
			},
			Difference: differenceDoc(d.Difference, a.Decimals, e.valueUSD(ctx, a, d.Difference, e.clk.Now()), nil),
			Status:     StatusMismatch, Material: true, BlocksNewRisk: true,
			Actor: SystemActor(), Reason: "position/ledger drift",
		}
		rec, err := e.upsertTx(ctx, req)
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// upsertTx is upsert in its own transaction.
func (e *Engine) upsertTx(ctx context.Context, req OpenRequest) (Record, error) {
	var rec Record
	err := e.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = e.upsert(ctx, tx, req)
		return err
	})
	return rec, err
}
