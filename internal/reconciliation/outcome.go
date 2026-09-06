package reconciliation

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/execution"
)

// recordOutcome writes the reconciliation record for one recovery pass and,
// when wallet activity turned up transactions the platform did not initiate,
// opens a SUBMISSION_UNKNOWN record for each of them.
//
// It is the single place where a disposition becomes a status, so the mapping
// is auditable in one screen:
//
//	ADOPTED, economics within the signed bounds  → MATCHED
//	ADOPTED, economics outside them              → MISMATCH, material, blocks new risk, SEV1
//	FAILED_ON_CHAIN                              → MATCHED (a definite negative answer)
//	PROVEN_ABSENT                                → MATCHED (a definite negative answer)
//	UNCERTAIN                                    → MISMATCH, material, blocks new risk
//	ALREADY_SETTLED                              → MATCHED
func (e *Engine) recordOutcome(ctx context.Context, att execution.Attempt, ord execution.Order, out RecoveryOutcome, ev evidence) (Record, error) {
	var rec Record
	err := e.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = e.recordOutcomeTx(ctx, tx, att, ord, out, ev)
		return err
	})
	return rec, err
}

func (e *Engine) recordOutcomeTx(ctx context.Context, tx pgx.Tx, att execution.Attempt, ord execution.Order, out RecoveryOutcome, ev evidence) (Record, error) {
	kind := KindExecution
	if out.Disposition == DispositionUncertain {
		kind = KindSubmissionUnknown
	}
	expected := map[string]any{
		"order_id": ord.ID.String(), "attempt_id": att.ID.String(), "attempt_no": att.AttemptNo,
		"order_status": string(ord.Status), "attempt_status": string(att.Status),
		"input_asset": ord.InputAssetID.String(), "input_quantity": ord.InputQuantity.String(),
		"output_asset": ord.OutputAssetID.String(), "min_output_quantity": ord.MinOutputQuantity.String(),
		"filled_input_quantity": ord.FilledInputQuantity.String(),
		"tx_signature":          att.TxSignature, "provider": att.Provider,
		"last_valid_block_height": att.LastValidBlockHeight,
		"reservation_id":          ord.ReservationID,
	}
	observed := map[string]any{
		"disposition": string(out.Disposition), "signature": out.Signature,
		"provider_state": string(ev.ProviderState), "provider_raw_ref": ev.ProviderRaw,
		"provider_error": ev.ProviderErr, "detail": ev.Detail,
		"wallet_address": ev.WalletAddr, "candidates": ev.Candidates,
		"agreement_state": string(ev.Resolution.State), "agreement_detail": ev.Resolution.Detail,
		"agreement_finality": string(ev.Resolution.Finality), "agreement_degraded": ev.Resolution.Degraded,
		"agreement_differences": ev.Resolution.Differences,
	}
	if ev.Observation != nil {
		observed["chain"] = map[string]any{
			"signature": ev.Observation.Signature, "slot": ev.Observation.Slot,
			"err": ev.Observation.Err, "fee": ev.Observation.Fee.String(),
			"commitment": string(ev.Observation.Commitment), "raw_ref": ev.Observation.RawRef,
		}
	}
	difference := map[string]any{"retry_allowed": out.RetryAllowed, "reservation_kept": out.ReservationKept}
	if out.Fill != nil {
		observed["fill"] = map[string]any{
			"fill_id": out.Fill.ID.String(), "external_fill_id": out.Fill.ExternalFillID,
			"input_quantity": out.Fill.InputQuantity.String(), "output_quantity": out.Fill.OutputQuantity.String(),
			"network_fee_quantity":   out.Fill.NetworkFeeQuantity.String(),
			"journal_transaction_id": out.Fill.JournalTransactionID, "finality": string(out.Fill.Finality),
		}
		difference["output_vs_minimum"] = out.Fill.OutputQuantity.Sub(ord.MinOutputQuantity).String()
		difference["input_vs_order"] = out.Fill.InputQuantity.Sub(ord.InputQuantity).String()
	}
	if ev.Existing != nil {
		observed["already_recorded_fill_id"] = ev.Existing.ID.String()
	}
	if ev.OutsideExpectations != "" {
		difference["outside_expectations"] = ev.OutsideExpectations
	}
	if len(ev.UnknownActivity) > 0 {
		observed["unknown_activity"] = ev.UnknownActivity
	}

	req := OpenRequest{
		Kind: kind, Mode: ModeEventDriven, ScopeType: ScopeAttempt, ScopeID: att.ID.String(),
		AccountID: ord.AccountID, AssetID: ord.InputAssetID,
		Expected: expected, Observed: observed, Difference: difference,
		CorrelationID: ord.CorrelationID, Actor: SystemActor(),
		Reason:      "PART 48 recovery: " + string(out.Disposition),
		EvidenceRef: firstNonEmpty(ev.ProviderRaw, observationRawRef(ev.Observation)),
	}
	switch out.Disposition {
	case DispositionAdopted:
		if ev.OutsideExpectations != "" {
			req.Status, req.Material, req.BlocksNewRisk = StatusMismatch, true, true
			e.metrics.Raise(ctx, Alert{
				Name: AlertUnauthorizedSigning, Severity: SEV1,
				Detail: "adopted fill falls outside the order's signed bounds: " + ev.OutsideExpectations,
				Fields: map[string]any{"order_id": ord.ID.String(), "attempt_id": att.ID.String()},
			})
		} else {
			req.Status = StatusMatched
		}
	case DispositionFailedOnChain, DispositionProvenAbsent, DispositionAlreadySettled:
		req.Status = StatusMatched
	default:
		req.Status, req.Material, req.BlocksNewRisk = StatusMismatch, true, true
	}
	if ev.Resolution.State == chain.Disagreed {
		req.Status, req.Material, req.BlocksNewRisk = StatusMismatch, true, true
		e.metrics.Raise(ctx, Alert{
			Name: AlertObserverDisagreement, Severity: SEV1,
			Detail: ev.Resolution.Detail,
			Fields: map[string]any{"attempt_id": att.ID.String(), "signature": out.Signature},
		})
	}

	rec, err := e.upsert(ctx, tx, req)
	if err != nil {
		return Record{}, err
	}

	// PART 48 "inspect wallet activity": a transaction touching the wallet
	// that belongs to no attempt is an unknown transaction. It gets its own
	// record, blocks new risk for the account, and needs a human to classify
	// it (adopted-as-fill, external-deposit, unauthorized).
	for _, sig := range ev.UnknownActivity {
		unknown := OpenRequest{
			Kind: KindSubmissionUnknown, Mode: ModeEventDriven, ScopeType: ScopeSignature, ScopeID: sig,
			AccountID: ord.AccountID,
			Expected:  map[string]any{"attributed_attempt": nil, "wallet_address": ev.WalletAddr},
			Observed:  map[string]any{"signature": sig, "source": "wallet_activity"},
			Difference: map[string]any{
				"classification_required": []string{"adopted-as-fill", "external-deposit", "unauthorized"},
			},
			Status: StatusMismatch, Material: true, BlocksNewRisk: true,
			CorrelationID: ord.CorrelationID, Actor: SystemActor(),
			Reason: "wallet activity matches no execution attempt",
		}
		if _, err := e.upsert(ctx, tx, unknown); err != nil {
			return Record{}, err
		}
		e.metrics.Raise(ctx, Alert{
			Name: AlertUnknownTransaction, Severity: SEV1,
			Detail: "wallet activity matches no execution attempt",
			Fields: map[string]any{"signature": sig, "account_id": ord.AccountID.String()},
		})
	}

	// The recovery trail itself is evidence (PART 49 step 13): what was
	// queried, what each observer said, and what was decided.
	if err := e.records.AppendEvidence(ctx, tx, rec, SystemActor(), recoveryAuditAction(out.Disposition),
		"PART 48 recovery pass", req.EvidenceRef, map[string]any{
			"attempt_id": att.ID.String(), "order_id": ord.ID.String(),
			"disposition": string(out.Disposition), "signature": out.Signature,
			"provider_state": string(ev.ProviderState), "agreement_state": string(ev.Resolution.State),
			"agreement_detail": ev.Resolution.Detail, "retry_allowed": out.RetryAllowed,
			"reservation_kept": out.ReservationKept, "candidates": ev.Candidates,
			"duplicate_submission_sent": false,
		}); err != nil {
		return Record{}, err
	}
	return rec, nil
}

func recoveryAuditAction(d Disposition) string {
	switch d {
	case DispositionAdopted:
		return AuditRecoveryAdopted
	case DispositionProvenAbsent:
		return AuditRecoveryAbsent
	default:
		return AuditRecordTransitioned
	}
}

func observationRawRef(o *chain.TxObservation) string {
	if o == nil {
		return ""
	}
	return o.RawRef
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
