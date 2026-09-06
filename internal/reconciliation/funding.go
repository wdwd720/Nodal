package reconciliation

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// depositView is the read-only projection of a deposit this package needs.
// internal/funding owns the deposit state machine; reconciliation only
// compares its recorded expectation with what the chain shows and records the
// difference. It never transitions a deposit: only funding may do that, from
// the evidence this engine produces.
type depositView struct {
	ID              string
	AccountID       accounts.AccountID
	Provider        string
	Status          string
	ExpectedAsset   assets.AssetID
	ExpectedQty     *money.Quantity
	ObservedQty     *money.Quantity
	DestinationAddr string
	TxSignature     string
	JournalTxID     string
	CorrelationID   string
	CreatedAt       time.Time
}

const depositViewColumns = `d.id::text, d.account_id, d.provider, d.status, d.expected_asset_id,
	d.expected_quantity::text, d.observed_quantity::text,
	coalesce(d.destination_address, coalesce(w.address,'')), coalesce(d.tx_signature,''),
	coalesce(d.journal_transaction_id::text,''), coalesce(d.correlation_id,''), d.created_at`

func (e *Engine) loadDeposit(ctx context.Context, depositID string) (depositView, error) {
	var d depositView
	var expected, observed *string
	err := e.db.QueryRow(ctx, `SELECT `+depositViewColumns+` FROM deposits d
		LEFT JOIN wallets w ON w.id = d.destination_wallet_id WHERE d.id = $1::uuid`, depositID).
		Scan(&d.ID, &d.AccountID, &d.Provider, &d.Status, &d.ExpectedAsset, &expected, &observed,
			&d.DestinationAddr, &d.TxSignature, &d.JournalTxID, &d.CorrelationID, &d.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return depositView{}, errs.New(errs.CodeNotFound, "deposit not found").WithField("deposit_id", depositID)
		}
		return depositView{}, dbErr("load deposit", err)
	}
	if expected != nil {
		q, perr := money.ParseQuantity(*expected)
		if perr != nil {
			return depositView{}, errs.Wrap(perr, errs.CodeInternal, "reconciliation: decode expected deposit quantity")
		}
		d.ExpectedQty = &q
	}
	if observed != nil {
		q, perr := money.ParseQuantity(*observed)
		if perr != nil {
			return depositView{}, errs.Wrap(perr, errs.CodeInternal, "reconciliation: decode observed deposit quantity")
		}
		d.ObservedQty = &q
	}
	return d, nil
}

// ReconcileDeposit is the EVENT_DRIVEN funding path (PARTS 27, 50, 162,
// RECONCILIATION.md §5). It compares a deposit's expected credit with the
// chain receipt both observers agree on and records the difference.
//
// It never advances the deposit: only a chain receipt that this comparison
// found and agreed on lets internal/funding move a deposit to
// SETTLEMENT_OBSERVED, and that decision is funding's to make from this
// record.
func (e *Engine) ReconcileDeposit(ctx context.Context, depositID string) (Record, error) {
	dep, err := e.loadDeposit(ctx, depositID)
	if err != nil {
		return Record{}, err
	}
	a, err := e.asset(ctx, dep.ExpectedAsset)
	if err != nil {
		return Record{}, err
	}
	expected := money.QuantityFromInt64(0)
	if dep.ExpectedQty != nil {
		expected = *dep.ExpectedQty
	}

	var (
		resolution chain.Resolution
		credited   = money.QuantityFromInt64(0)
		detail     string
		haveChain  bool
	)
	switch {
	case dep.TxSignature == "":
		detail = "no chain receipt recorded yet"
	case e.observers.Primary == nil:
		detail = "no chain observer configured"
	default:
		resolution = e.resolveSignature(ctx, dep.TxSignature)
		if obs := resolution.Observation; obs != nil && obs.Succeeded() {
			haveChain = true
			for _, d := range obs.DeltasFor(dep.DestinationAddr) {
				if d.Mint == a.MintAddress {
					credited = credited.Add(d.Delta())
				}
			}
		} else {
			detail = resolution.Detail
		}
	}

	diff := credited.Sub(expected)
	usd := e.valueUSD(ctx, a, diff, e.clk.Now())
	req := OpenRequest{
		Kind: KindFunding, Mode: ModeEventDriven, ScopeType: ScopeDeposit, ScopeID: dep.ID,
		AccountID: dep.AccountID, AssetID: dep.ExpectedAsset,
		Expected: map[string]any{
			"source": "deposits", "status": dep.Status, "provider": dep.Provider,
			"quantity": expected.String(), "decimal": expected.ToDecimalString(a.Decimals),
			"destination_address": dep.DestinationAddr, "tx_signature": dep.TxSignature,
			"journal_transaction_id": dep.JournalTxID,
		},
		Observed: map[string]any{
			"source": "chain", "quantity": credited.String(), "decimal": credited.ToDecimalString(a.Decimals),
			"agreement_state": string(resolution.State), "agreement_detail": resolution.Detail,
			"agreement_degraded": resolution.Degraded, "detail": detail, "receipt_found": haveChain,
		},
		Difference:    differenceDoc(diff, a.Decimals, usd, resolution.Differences),
		CorrelationID: dep.CorrelationID, Actor: SystemActor(),
		Reason:      "funding reconciliation",
		EvidenceRef: observationRawRef(resolution.Observation),
	}
	switch {
	case resolution.State == chain.Disagreed:
		req.Status, req.Material, req.BlocksNewRisk = StatusMismatch, true, true
		e.metrics.Raise(ctx, Alert{
			Name: AlertObserverDisagreement, Severity: SEV1, Detail: resolution.Detail,
			Fields: map[string]any{"deposit_id": dep.ID, "signature": dep.TxSignature},
		})
	case !haveChain:
		// A provider "success" with no chain receipt is not settled money.
		// The record stays OPEN until a receipt appears or the funding
		// settlement timeout turns it into a mismatch.
		req.Status = StatusOpen
		if e.fundingSettlementOverdue(dep) {
			req.Status, req.Material, req.BlocksNewRisk = StatusMismatch, true, true
			req.Reason = "provider reported success but no chain receipt was observed"
		}
	case diff.IsZero():
		req.Status = StatusMatched
	default:
		req.Status = StatusMismatch
		req.Material = e.policy.Material(KindFunding, dep.ExpectedAsset, diff, usd)
		req.BlocksNewRisk = req.Material
	}
	return e.upsertTx(ctx, req)
}

// fundingSettlementOverdue reports whether a provider-confirmed deposit has
// gone past the settlement timeout without a chain receipt
// (RECONCILIATION.md §5).
func (e *Engine) fundingSettlementOverdue(dep depositView) bool {
	switch dep.Status {
	case "PROVIDER_CONFIRMED", "SETTLEMENT_OBSERVED":
	default:
		return false
	}
	if e.policy.FundingSettlementTimeout <= 0 {
		return false
	}
	return e.clk.Now().Sub(dep.CreatedAt) > e.policy.FundingSettlementTimeout
}

// RunPeriodicFunding sweeps deposits that a provider has confirmed but whose
// chain receipt is not yet reconciled, oldest first.
func (e *Engine) RunPeriodicFunding(ctx context.Context, limit int) ([]Record, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := e.db.Query(ctx, `SELECT id::text FROM deposits
		WHERE status IN ('PROVIDER_CONFIRMED','SETTLEMENT_OBSERVED','RECONCILED')
		ORDER BY created_at, id LIMIT $1`, limit)
	if err != nil {
		return nil, dbErr("list deposits to reconcile", err)
	}
	ids := []string{}
	for rows.Next() {
		var idText string
		if err := rows.Scan(&idText); err != nil {
			rows.Close()
			return nil, dbErr("scan deposit id", err)
		}
		ids = append(ids, idText)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, dbErr("list deposits to reconcile", err)
	}
	out := []Record{}
	for _, depositID := range ids {
		rec, err := e.ReconcileDeposit(ctx, depositID)
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}
