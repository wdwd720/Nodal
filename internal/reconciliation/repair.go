package reconciliation

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/positions"
)

// Repair is the PART 195 financial repair: a canonical financial event that
// brings internal accounting truth back in line with external truth.
//
// It is a *new* balanced journal transaction referenced from the
// reconciliation record. There is no path in this system that edits a balance
// or overwrites a position: a repair either posts a compensating transaction,
// or it does nothing. Position changes ride along as ordinary lot
// acquisitions and disposals so the lot history stays complete.
type Repair struct {
	// Posting is the compensating journal transaction. Its Kind must be
	// RECONCILIATION_ADJUSTMENT, COMPENSATION or CORRECTION, and its
	// Reference must name the reconciliation record.
	Posting ledger.Posting
	// Position, when set, applies the same quantity change to the position
	// lots so positions.VerifyAgainstLedger keeps agreeing.
	Position *PositionRepair
}

// PositionRepair is the lot-level half of a repair. Exactly one of Acquire and
// Dispose is set.
type PositionRepair struct {
	Acquire *positions.AcquireLot
	Dispose *positions.Disposal
}

// Validate checks the structural rules a repair must satisfy before it can be
// attached to a resolution.
func (r Repair) Validate(recordID RecordID) error {
	problems := map[string]any{}
	switch r.Posting.Kind {
	case ledger.KindReconciliationAdjustment, ledger.KindCompensation, ledger.KindCorrection:
	default:
		problems["kind"] = "a repair posts RECONCILIATION_ADJUSTMENT, COMPENSATION or CORRECTION"
	}
	if r.Posting.Reference.Type != RepairReferenceType || r.Posting.Reference.ID != recordID.String() {
		problems["reference"] = "a repair must reference its reconciliation record"
	}
	if len(r.Posting.Entries) == 0 {
		problems["entries"] = "required"
	}
	if r.Posting.IdempotencyKey != RepairIdempotencyKey(recordID) {
		problems["idempotency_key"] = "must be " + RepairIdempotencyKey(recordID)
	}
	if r.Posting.ReasonCode() == "" {
		problems["reason_code"] = "required"
	}
	if p := r.Position; p != nil {
		if (p.Acquire == nil) == (p.Dispose == nil) {
			problems["position"] = "set exactly one of acquire and dispose"
		}
		if p.Acquire != nil {
			if err := p.Acquire.Validate(); err != nil {
				problems["position.acquire"] = err.Error()
			}
		}
		if p.Dispose != nil {
			if err := p.Dispose.Validate(); err != nil {
				problems["position.dispose"] = err.Error()
			}
		}
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "reconciliation: invalid repair").WithFields(problems)
	}
	return nil
}

// RepairReferenceType is the ledger FinancialEventReference type of a repair.
const RepairReferenceType = "reconciliation_record"

// RepairIdempotencyKey is the ledger idempotency key of a record's repair.
// One record can produce at most one compensating transaction, however many
// times a resolution is retried.
func RepairIdempotencyKey(recordID RecordID) string {
	return "reconciliation:" + recordID.String()
}

// BalanceRepairInputs describes a wallet-balance difference to compensate.
// Difference is observed − expected in exact base units: positive when the
// chain holds more than the ledger says, negative when it holds less.
type BalanceRepairInputs struct {
	RecordID    RecordID
	AccountID   accounts.AccountID
	AssetID     assets.AssetID
	Difference  money.Quantity
	ReasonCode  string
	Description string
	EffectiveAt time.Time
	// USDValue is the valuation metadata attached to the entries; optional.
	USDValue *money.USD
	PriceRef string
	// CorrelationID links the posting to the incident.
	CorrelationID string
}

// BalanceRepair builds the canonical compensating posting for a wallet
// balance difference (FINANCIAL_MODEL §2.1, RECONCILIATION_ADJUSTMENT):
//
//	chain holds more:  Dr WALLET:asset  d   Cr RECONCILIATION_ADJUSTMENT:asset  d
//	chain holds less:  Cr WALLET:asset  d   Dr RECONCILIATION_ADJUSTMENT:asset  d
//
// RECONCILIATION_ADJUSTMENT is credit-normal and may go negative in either
// direction, so both shapes are legal and the per-asset balance equation
// holds. WALLET can never go negative, which is what stops a repair from
// inventing units the customer does not hold: the ledger's negative-balance
// trigger refuses it.
func BalanceRepair(in BalanceRepairInputs) (Repair, error) {
	if in.RecordID.IsZero() {
		return Repair{}, errs.New(errs.CodeValidationFailed, "reconciliation: repair needs a record id")
	}
	if in.AccountID.IsZero() || in.AssetID.IsZero() {
		return Repair{}, errs.New(errs.CodeValidationFailed, "reconciliation: repair needs an account and an asset")
	}
	if in.Difference.IsZero() {
		return Repair{}, errs.New(errs.CodeValidationFailed, "reconciliation: nothing to repair").
			WithField("difference", in.Difference.String())
	}
	if in.ReasonCode == "" {
		return Repair{}, errs.New(errs.CodeValidationFailed, "reconciliation: repair needs a reason code")
	}
	qty := in.Difference.Abs()
	walletSide, adjustmentSide := ledger.Debit, ledger.Credit
	if in.Difference.IsNegative() {
		walletSide, adjustmentSide = ledger.Credit, ledger.Debit
	}
	var usdMinor *int64
	if in.USDValue != nil {
		m := in.USDValue.Minor()
		usdMinor = &m
	}
	var priceRef *string
	if in.PriceRef != "" {
		priceRef = &in.PriceRef
	}
	at := in.EffectiveAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	desc := in.Description
	if desc == "" {
		desc = "reconciliation adjustment"
	}
	return Repair{
		Posting: ledger.Posting{
			Kind:           ledger.KindReconciliationAdjustment,
			IdempotencyKey: RepairIdempotencyKey(in.RecordID),
			Reference:      ledger.FinancialEventReference{Type: RepairReferenceType, ID: in.RecordID.String()},
			EffectiveAt:    at.UTC(),
			Description:    desc,
			CorrelationID:  in.CorrelationID,
			Entries: []ledger.Entry{
				{
					Account:       ledger.CustomerAccount(in.AccountID, ledger.CodeWallet, in.AssetID),
					Side:          walletSide,
					Quantity:      qty,
					USDValueMinor: usdMinor,
					PriceRef:      priceRef,
				},
				{
					Account:       ledger.CustomerAccount(in.AccountID, ledger.CodeReconciliationAdjustment, in.AssetID),
					Side:          adjustmentSide,
					Quantity:      qty,
					USDValueMinor: usdMinor,
					PriceRef:      priceRef,
				},
			},
			Metadata: map[string]any{
				ledger.MetadataReasonCode:  in.ReasonCode,
				"reconciliation_record_id": in.RecordID.String(),
				"difference":               in.Difference.String(),
			},
		},
	}, nil
}

// WithPositionRepair attaches the lot-level half of a balance repair so that
// Σ open lots keeps matching the WALLET balance. A positive difference
// acquires a lot with the supplied basis; a negative one disposes.
func (r Repair) WithPositionRepair(in BalanceRepairInputs, valuationSource string) (Repair, error) {
	if valuationSource == "" {
		return Repair{}, errs.New(errs.CodeValidationFailed, "reconciliation: position repair needs a valuation source")
	}
	ref := positions.Ref{Type: RepairReferenceType, ID: in.RecordID.String()}
	usd := money.USDFromMinor(0)
	if in.USDValue != nil {
		abs, err := in.USDValue.Abs()
		if err != nil {
			return Repair{}, errs.Wrap(err, errs.CodeOverflow, "reconciliation: repair valuation")
		}
		usd = abs
	}
	at := in.EffectiveAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if in.Difference.IsPositive() {
		r.Position = &PositionRepair{Acquire: &positions.AcquireLot{
			AccountID: in.AccountID, AssetID: in.AssetID, Quantity: in.Difference.Abs(), AcquiredAt: at.UTC(),
			Cost: usd, BasisSource: "reconciliation_adjustment", ValuationSource: valuationSource, AcquisitionRef: ref,
		}}
		return r, nil
	}
	r.Position = &PositionRepair{Dispose: &positions.Disposal{
		AccountID: in.AccountID, AssetID: in.AssetID, Quantity: in.Difference.Abs(), DisposedAt: at.UTC(),
		Proceeds: usd, ValuationSource: valuationSource, DispositionRef: ref,
	}}
	return r, nil
}

// applyRepair posts the compensating transaction and, when present, the lot
// change, inside tx. It is idempotent on the ledger idempotency key: a repeat
// returns the transaction already posted and touches nothing.
func (e *Engine) applyRepair(ctx context.Context, tx pgx.Tx, rec Record, rep Repair) (string, error) {
	if err := rep.Validate(rec.ID); err != nil {
		return "", err
	}
	if e.ledger == nil {
		return "", errs.New(errs.CodeInternal, "reconciliation: no ledger is configured; a repair cannot be posted")
	}
	res, err := e.ledger.Post(ctx, tx, rep.Posting)
	if err != nil {
		return "", err
	}
	if res.Existing {
		return res.TransactionID.String(), nil
	}
	if p := rep.Position; p != nil && e.positions != nil {
		switch {
		case p.Acquire != nil:
			lot := *p.Acquire
			lot.JournalTxID = res.TransactionID.Untyped()
			if _, err := e.positions.Acquire(ctx, tx, lot); err != nil {
				return "", err
			}
		case p.Dispose != nil:
			d := *p.Dispose
			d.JournalTxID = res.TransactionID.Untyped()
			if _, _, err := e.positions.Dispose(ctx, tx, d); err != nil {
				return "", err
			}
		}
	}
	return res.TransactionID.String(), nil
}
