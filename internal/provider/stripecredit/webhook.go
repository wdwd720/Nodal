package stripecredit

import (
	"net/http"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider/stripesig"
	"github.com/nodal/controlplane/internal/webhook"
)

// SignatureTolerance is Stripe's documented replay window.
const SignatureTolerance = stripesig.DefaultTolerance

// Event types this adapter models. Anything else is signed, retained as
// evidence and recorded as IGNORED.
const (
	EventPaymentIntentCreated          = "payment_intent.created"
	EventPaymentIntentRequiresAction   = "payment_intent.requires_action"
	EventPaymentIntentProcessing       = "payment_intent.processing"
	EventPaymentIntentAmountCapturable = "payment_intent.amount_capturable_updated"
	EventPaymentIntentSucceeded        = "payment_intent.succeeded"
	EventPaymentIntentFailed           = "payment_intent.payment_failed"
	EventPaymentIntentCanceled         = "payment_intent.canceled"

	EventChargeRefunded         = "charge.refunded"
	EventDisputeCreated         = "charge.dispute.created"
	EventDisputeClosed          = "charge.dispute.closed"
	EventDisputeFundsWithdrawn  = "charge.dispute.funds_withdrawn"
	EventDisputeFundsReinstated = "charge.dispute.funds_reinstated"
)

// ModelledEventTypes returns every event type this adapter acts on. It is what
// the webhook endpoint should be subscribed to, and a test asserts the two
// agree so that a new handler cannot ship without its subscription.
func ModelledEventTypes() []string {
	return []string{
		EventPaymentIntentCreated,
		EventPaymentIntentRequiresAction,
		EventPaymentIntentProcessing,
		EventPaymentIntentAmountCapturable,
		EventPaymentIntentSucceeded,
		EventPaymentIntentFailed,
		EventPaymentIntentCanceled,
		EventChargeRefunded,
		EventDisputeCreated,
		EventDisputeClosed,
		EventDisputeFundsWithdrawn,
		EventDisputeFundsReinstated,
	}
}

// parseWebhook is the mode-independent webhook path: verify the signature,
// decode the envelope, check livemode, then classify.
func parseWebhook(raw []byte, headers http.Header, secret string, now time.Time, tolerance time.Duration, livemode bool, environment string) (credit.PurchaseEvent, error) {
	signedAt, err := stripesig.Verify(headers, raw, secret, now, tolerance)
	if err != nil {
		return credit.PurchaseEvent{}, err
	}
	var ev event
	if err := decodeLoose(raw, &ev); err != nil {
		return credit.PurchaseEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed,
			"stripecredit: event body is not JSON")
	}
	if ev.ID == "" || ev.Type == "" {
		return credit.PurchaseEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed,
			"stripecredit: event is missing an id or a type")
	}
	// A live event arriving at a test-mode adapter, or the reverse, is the
	// environment-isolation failure the goal document's Section 32 is about.
	// It is refused here rather than at the domain, because by the domain it
	// would already be a fact in the inbox.
	if ev.Livemode != livemode {
		return credit.PurchaseEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed,
			"stripecredit: event livemode does not match the adapter mode").
			WithField("event_livemode", ev.Livemode).WithField("adapter_livemode", livemode)
	}

	out := credit.PurchaseEvent{Identity: identityFrom(ev, signedAt), Raw: raw}

	switch ev.Type {
	case EventPaymentIntentCreated, EventPaymentIntentRequiresAction, EventPaymentIntentProcessing,
		EventPaymentIntentAmountCapturable, EventPaymentIntentSucceeded, EventPaymentIntentFailed,
		EventPaymentIntentCanceled:
		return paymentIntentEvent(out, ev, environment)

	case EventChargeRefunded:
		return chargeRefundEvent(out, ev)

	case EventDisputeCreated, EventDisputeClosed, EventDisputeFundsWithdrawn, EventDisputeFundsReinstated:
		return disputeEvent(out, ev)
	}

	// Signed, genuine, and not something this adapter acts on. Recognized
	// stays false and the pipeline records IGNORED.
	return out, nil
}

// paymentIntentEvent handles the event family that carries Nodal's own
// metadata, and is therefore the family where foreignness can be decided here
// rather than by a database lookup.
func paymentIntentEvent(out credit.PurchaseEvent, ev event, environment string) (credit.PurchaseEvent, error) {
	var pi paymentIntent
	if err := decodeLoose(ev.Data.Object, &pi); err != nil {
		return credit.PurchaseEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed,
			"stripecredit: event data is not a payment intent")
	}
	snap, err := snapshotFrom(pi)
	if err != nil {
		return credit.PurchaseEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed, err.Error())
	}
	out.Snapshot = snap
	out.Recognized = true

	// A payment_intent.payment_failed carries a PaymentIntent whose status is
	// usually back to requires_payment_method, because the customer may try
	// again. The event type is the fact, not the status.
	if ev.Type == EventPaymentIntentFailed {
		out.Snapshot.Status = credit.PurchaseFailed
	}

	if foreign, reason := classify(pi.Metadata, environment); foreign {
		out.Foreign, out.ForeignReason = true, reason
		return out, nil
	}
	// The metadata funding id is a cross-check, not the link. The link is the
	// provider reference, which the database holds under a unique constraint.
	// Reading the id here lets the service refuse an object whose two
	// identities disagree, which is the shape a tampered or mis-copied
	// PaymentIntent would take.
	if raw := strings.TrimSpace(pi.Metadata[MetaFundingID]); raw != "" {
		id, err := credit.ParseFundingID(raw)
		if err != nil {
			out.Foreign, out.ForeignReason = true,
				"carries the Nodal workstream marker but its "+MetaFundingID+" is not a Nodal id"
			return out, nil
		}
		out.FundingID = id
	}
	return out, nil
}

// chargeRefundEvent handles charge.refunded.
//
// A Charge does not carry the PaymentIntent's metadata, so foreignness cannot
// be decided here: the event names a payment intent and the service resolves
// it. An unresolvable reference is a foreign event, discovered one step later.
func chargeRefundEvent(out credit.PurchaseEvent, ev event) (credit.PurchaseEvent, error) {
	var ch charge
	if err := decodeLoose(ev.Data.Object, &ch); err != nil {
		return credit.PurchaseEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed,
			"stripecredit: event data is not a charge")
	}
	if !strings.HasPrefix(ch.PaymentIntent, PaymentIntentIDPrefix) {
		// A charge with no payment intent was not created by this adapter.
		out.Foreign, out.ForeignReason = true, "charge names no payment intent"
		return out, nil
	}
	out.Recognized = true
	out.Snapshot = credit.PurchaseSnapshot{
		ProviderReference:   ch.PaymentIntent,
		RawStatus:           ev.Type,
		Amount:              money.USDFromMinor(ch.Amount),
		Currency:            strings.ToUpper(ch.Currency),
		Livemode:            ch.Livemode,
		AmountRefundedMinor: ch.AmountRefunded,
	}
	// A partial refund has no representation in the funding lifecycle. REFUNDED
	// is terminal and would destroy every Credit the purchase issued; ignoring
	// it would leave the platform out of pocket with the Credits still spendable.
	// Neither is defensible, so a partial refund stops and a person decides.
	// Making that visible is better than choosing the wrong one silently, and
	// it is recorded as a known gap rather than hidden as an edge case.
	if ch.AmountRefunded > 0 && ch.AmountRefunded < ch.Amount {
		out.Snapshot.Status = credit.PurchaseManualReview
		return out, nil
	}
	out.Snapshot.Status = credit.PurchaseRefunded
	return out, nil
}

// disputeEvent handles the four dispute events.
//
// A Dispute names its PaymentIntent, which is the link. Its own metadata is
// empty by default and is not used.
func disputeEvent(out credit.PurchaseEvent, ev event) (credit.PurchaseEvent, error) {
	var d dispute
	if err := decodeLoose(ev.Data.Object, &d); err != nil {
		return credit.PurchaseEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed,
			"stripecredit: event data is not a dispute")
	}
	if !strings.HasPrefix(d.PaymentIntent, PaymentIntentIDPrefix) {
		out.Foreign, out.ForeignReason = true, "dispute names no payment intent"
		return out, nil
	}
	status, ok := disputeStatus(ev.Type, d.Status)
	if !ok {
		// A dispute outcome this binary does not understand is exactly the
		// case MANUAL_REVIEW exists for: the money may or may not be ours and
		// guessing decides whether a user keeps Credits they may not have paid
		// for.
		status = credit.PurchaseManualReview
	}
	out.Recognized = true
	out.Snapshot = credit.PurchaseSnapshot{
		ProviderReference: d.PaymentIntent,
		Status:            status,
		RawStatus:         ev.Type + ":" + d.Status,
		Amount:            money.USDFromMinor(d.Amount),
		Currency:          strings.ToUpper(d.Currency),
		Livemode:          d.Livemode,
	}
	return out, nil
}

// disputeStatus maps an event type and the dispute's own status onto the
// provider-neutral vocabulary.
//
// funds_withdrawn and funds_reinstated are the events that actually move
// money, and they are treated as authoritative over the dispute's status
// string: Stripe can withdraw funds while a dispute is still under review, and
// the funding must reflect where the money is rather than where the
// paperwork is.
func disputeStatus(eventType, status string) (credit.PurchaseStatus, bool) {
	switch eventType {
	case EventDisputeCreated:
		return credit.PurchaseDisputed, true
	case EventDisputeFundsWithdrawn:
		return credit.PurchaseChargeback, true
	case EventDisputeFundsReinstated:
		return credit.PurchaseDisputeWon, true
	case EventDisputeClosed:
		switch status {
		case "lost":
			return credit.PurchaseChargeback, true
		case "won":
			return credit.PurchaseDisputeWon, true
		case "warning_closed":
			// An early-warning notice that closed without becoming a dispute.
			// Nothing moved and nothing is owed.
			return credit.PurchaseDisputeWon, true
		}
	}
	return "", false
}

// classify decides whether an object carrying metadata is Nodal's.
//
// Two independent checks, and both must pass. The workstream marker separates
// Nodal's objects from the other product's on a shared account. The
// environment marker separates deployments from each other: a staging
// deployment pointed at the same live account would otherwise act on
// production purchases, which is the worst possible version of this bug
// because everything about it looks like it is working.
func classify(md map[string]string, environment string) (foreign bool, reason string) {
	if md == nil {
		return true, "object carries no metadata, so it was not created by Nodal"
	}
	if md[MetaWorkstream] != MetaWorkstreamValue {
		return true, "object does not carry " + MetaWorkstream + "=" + MetaWorkstreamValue
	}
	if got := md[MetaEnvironment]; !strings.EqualFold(got, environment) {
		return true, "object belongs to environment " + got + ", not " + environment
	}
	return false, ""
}
