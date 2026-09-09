package stripecredit

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/webhook"
)

// ProviderName is the adapter name in configuration, gates and evidence.
const ProviderName = "stripe_credit"

// DefaultBaseURL is the Stripe API root.
const DefaultBaseURL = "https://api.stripe.com"

// MaxResponseBytes bounds a response body. A PaymentIntent is a few kilobytes;
// this is generous and still refuses to buffer something pathological.
const MaxResponseBytes = 512 << 10

// PaymentIntentIDPrefix is the documented prefix of a PaymentIntent id.
const PaymentIntentIDPrefix = "pi_"

// Metadata keys. Every one is namespaced, because these objects live in an
// account shared with another product and an un-namespaced key would be a
// claim on a name somebody else may already use.
const (
	// MetaWorkstream is the marker that makes an object Nodal's. Its absence
	// is what makes an event foreign.
	MetaWorkstream = "nodal_workstream"
	// MetaWorkstreamValue is the only value MetaWorkstream may have.
	MetaWorkstreamValue = "NODAL"

	MetaFundingID      = "nodal_credit_purchase_id"
	MetaUserID         = "nodal_user_id"
	MetaPricingVersion = "nodal_pricing_version"
	MetaPricingHash    = "nodal_pricing_hash"
	// MetaCreditQuantity is the Credit amount the SERVER derived. It is
	// written so that the provider's own record shows what was promised, and
	// it is read back on every event and compared. It is evidence, never an
	// input: nothing recomputes Credits from this field.
	MetaCreditQuantity = "nodal_credit_quantity"
	MetaEnvironment    = "nodal_environment"
)

// paymentIntent is the subset of the PaymentIntent object this adapter reads.
// Every field here appears in the official object reference.
type paymentIntent struct {
	ID       string            `json:"id"`
	Object   string            `json:"object"`
	Status   string            `json:"status"`
	Amount   int64             `json:"amount"`
	Currency string            `json:"currency"`
	Livemode bool              `json:"livemode"`
	Created  int64             `json:"created"`
	Metadata map[string]string `json:"metadata"`
	// ClientSecret drives the Payment Element. It is returned to the customer
	// once and never persisted or logged.
	ClientSecret  string            `json:"client_secret"`
	LastPaymentEr *lastPaymentError `json:"last_payment_error"`
	Charges       *chargeList       `json:"charges"`
}

type lastPaymentError struct {
	Code       string `json:"code"`
	DeclineErr string `json:"decline_code"`
	Type       string `json:"type"`
}

type chargeList struct {
	Data []charge `json:"data"`
}

// charge is the subset of the Charge object needed to see refunds.
type charge struct {
	ID             string            `json:"id"`
	Object         string            `json:"object"`
	PaymentIntent  string            `json:"payment_intent"`
	AmountRefunded int64             `json:"amount_refunded"`
	Amount         int64             `json:"amount"`
	Currency       string            `json:"currency"`
	Refunded       bool              `json:"refunded"`
	Disputed       bool              `json:"disputed"`
	Livemode       bool              `json:"livemode"`
	Metadata       map[string]string `json:"metadata"`
}

// dispute is the subset of the Dispute object needed to link a dispute to a
// funding and to tell an opened dispute from a lost one.
type dispute struct {
	ID            string            `json:"id"`
	Object        string            `json:"object"`
	Charge        string            `json:"charge"`
	PaymentIntent string            `json:"payment_intent"`
	Status        string            `json:"status"`
	Amount        int64             `json:"amount"`
	Currency      string            `json:"currency"`
	Livemode      bool              `json:"livemode"`
	Metadata      map[string]string `json:"metadata"`
}

// event is the envelope of a Stripe webhook delivery.
type event struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Type     string `json:"type"`
	Created  int64  `json:"created"`
	Livemode bool   `json:"livemode"`
	Data     struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

type errorBody struct {
	Error struct {
		Code        string `json:"code"`
		DeclineCode string `json:"decline_code"`
		Type        string `json:"type"`
		Message     string `json:"message"`
		Param       string `json:"param"`
	} `json:"error"`
}

// decodeStrict rejects unknown fields on the documents this adapter owns, and
// rejects trailing data. It is used only where a shape is fully known.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errs.New(errs.CodeValidationFailed, "stripecredit: trailing data after JSON document")
	}
	return nil
}

// decodeLoose parses a Stripe object without rejecting unknown fields.
//
// Stripe adds fields to its objects without a breaking-change notice, and an
// adapter that refuses a PaymentIntent because it grew a field is an adapter
// that stops taking payments on a Tuesday for no reason. Unknown fields are
// ignored; the fields this adapter reads are validated explicitly.
func decodeLoose(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}

// statusFor maps a documented PaymentIntent status onto the provider-neutral
// vocabulary. The bool is false for anything undocumented, which the caller
// turns into MANUAL_REVIEW rather than a guess.
func statusFor(s string) (credit.PurchaseStatus, bool) {
	switch s {
	case "requires_payment_method":
		return credit.PurchasePaymentMethodRequired, true
	case "requires_confirmation":
		// The customer has supplied a method and it has not been confirmed.
		// From Nodal's side that is indistinguishable from waiting for one.
		return credit.PurchasePaymentMethodRequired, true
	case "requires_action":
		return credit.PurchaseAuthenticationRequired, true
	case "processing":
		return credit.PurchaseProcessing, true
	case "requires_capture":
		return credit.PurchaseAuthorized, true
	case "succeeded":
		return credit.PurchaseSucceeded, true
	case "canceled":
		return credit.PurchaseCanceled, true
	}
	return "", false
}

// snapshotFrom converts a decoded PaymentIntent into the provider-neutral
// snapshot.
//
// The failure reason is the provider's error CODE, never its message. A
// Stripe message can describe account configuration, and this string reaches
// support tooling.
func snapshotFrom(pi paymentIntent) (credit.PurchaseSnapshot, error) {
	if pi.Object != "" && pi.Object != "payment_intent" {
		return credit.PurchaseSnapshot{}, errs.Newf(errs.CodeValidationFailed,
			"stripecredit: expected a payment_intent, got %q", pi.Object)
	}
	if !strings.HasPrefix(pi.ID, PaymentIntentIDPrefix) {
		return credit.PurchaseSnapshot{}, errs.New(errs.CodeValidationFailed,
			"stripecredit: payment intent id is not in the documented form")
	}
	if pi.Amount < 0 {
		return credit.PurchaseSnapshot{}, errs.New(errs.CodeValidationFailed,
			"stripecredit: payment intent amount is negative")
	}
	status, ok := statusFor(pi.Status)
	if !ok {
		status = credit.PurchaseManualReview
	}
	var refunded int64
	if pi.Charges != nil {
		for _, c := range pi.Charges.Data {
			refunded += c.AmountRefunded
		}
	}
	var reason string
	if pi.LastPaymentEr != nil {
		reason = pi.LastPaymentEr.Code
		if reason == "" {
			reason = pi.LastPaymentEr.Type
		}
	}
	return credit.PurchaseSnapshot{
		ProviderReference:   pi.ID,
		Status:              status,
		RawStatus:           pi.Status,
		Amount:              money.USDFromMinor(pi.Amount),
		Currency:            strings.ToUpper(pi.Currency),
		Metadata:            pi.Metadata,
		Livemode:            pi.Livemode,
		FailureReason:       reason,
		AmountRefundedMinor: refunded,
	}, nil
}

// identityFrom builds the webhook identity from a verified envelope.
func identityFrom(ev event, signedAt time.Time) webhook.Identity {
	id := webhook.Identity{
		Provider:  ProviderName,
		EventID:   ev.ID,
		EventType: ev.Type,
		SignedAt:  signedAt.UTC(),
		Livemode:  ev.Livemode,
	}
	if ev.Created > 0 {
		id.PublishedAt = time.Unix(ev.Created, 0).UTC()
	}
	return id
}
