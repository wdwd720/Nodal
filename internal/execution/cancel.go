package execution

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/errs"
)

// CancelConfirmation is the external evidence that an order will not (or
// did not) execute: the venue's or the chain's word, never the platform's
// wish (PART 227).
type CancelConfirmation struct {
	// Source says who confirmed: PROVIDER, CHAIN_OBSERVER or RECONCILIATION.
	Source FillSource
	// ExternalRef is the venue reference, transaction signature or
	// reconciliation record that proves non-execution.
	ExternalRef string
	// EvidenceRef is the archived evidence body.
	EvidenceRef string
	ConfirmedAt time.Time
	ActorType   string
	ActorID     string
	Reason      string
}

// RequestCancel records the user's or operator's cancel request. It moves
// the order to CANCEL_REQUESTED (from PLANNED, SUBMITTING, SUBMITTED,
// ACKNOWLEDGED or PARTIALLY_FILLED); it never cancels anything by itself.
// Canceling is CANCEL class for kill switches: it is never blocked, so no
// switch is consulted here.
func (r *Repository) RequestCancel(ctx context.Context, tx pgx.Tx, orderID OrderID, ev TransitionEvidence) (Order, error) {
	if strings.TrimSpace(ev.Reason) == "" {
		ev.Reason = "cancel requested"
	}
	return r.Transition(ctx, tx, orderID, OrderCancelRequested, ev)
}

// ConfirmCancelled moves a CANCEL_REQUESTED (or RECONCILIATION_REQUIRED)
// order to CANCELLED on external confirmation. Without an external reference
// the call fails with VALIDATION_FAILED: a cancel request alone never
// produces CANCELLED. A fill that arrived in the meantime has already moved
// the order to PARTIALLY_FILLED or FILLED, so the transition table refuses
// the cancellation with INVALID_STATE_TRANSITION: the fill wins.
func (r *Repository) ConfirmCancelled(ctx context.Context, tx pgx.Tx, orderID OrderID, c CancelConfirmation) (Order, error) {
	if strings.TrimSpace(c.ExternalRef) == "" || !c.Source.Valid() {
		return Order{}, errs.Wrap(ErrRequiresExternalConfirmation, errs.CodeValidationFailed,
			"execution: CANCELLED requires external confirmation (source and external reference)").WithField("order_id", orderID.String())
	}
	if c.ConfirmedAt.IsZero() {
		return Order{}, errs.New(errs.CodeValidationFailed, "execution: confirmation time is required").WithField("order_id", orderID.String())
	}
	reason := c.Reason
	if reason == "" {
		reason = "cancellation confirmed by " + string(c.Source)
	}
	ev := TransitionEvidence{ActorType: c.ActorType, ActorID: c.ActorID, Reason: reason, EvidenceRef: c.EvidenceRef, CausationID: c.ExternalRef}
	if ev.EvidenceRef == "" {
		ev.EvidenceRef = c.ExternalRef
	}
	o, err := lockOrder(ctx, tx, orderID)
	if err != nil {
		return Order{}, err
	}
	if o.Status != OrderCancelRequested && o.Status != OrderReconciliationRequired {
		return Order{}, checkTransition(o.ID, o.Status, OrderCancelled)
	}
	return r.transitionLocked(ctx, tx, o, OrderCancelled, ev, c.ExternalRef)
}
