package stripe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/webhook"
)

// Provider identity and constants verified from the official docs.
const (
	ProviderName = "stripe"
	// DefaultBaseURL is the API host for sandbox and live (keys select the
	// mode).
	DefaultBaseURL = "https://api.stripe.com"
	// ObjectSession is the object name of an onramp session.
	ObjectSession = "crypto.onramp_session"
	// ObjectEvent is the object name of a webhook event.
	ObjectEvent = "event"
	// EventOnrampSessionUpdated is the only onramp webhook event type.
	EventOnrampSessionUpdated = "crypto.onramp_session.updated"
	// SessionIDPrefix and EventIDPrefix are the documented id prefixes.
	SessionIDPrefix = "cos_"
	EventIDPrefix   = "evt_"
	// SignatureTolerance is Stripe's documented default (5 minutes).
	SignatureTolerance = 300 * time.Second
	// MaxResponseBytes caps any body read from the provider.
	MaxResponseBytes = 1 << 20
)

// Documented session statuses (embedded guide) plus the quote_ready value
// observed in the /quote example response.
const (
	StatusInitialized           = "initialized"
	StatusRejected              = "rejected"
	StatusRequiresPayment       = "requires_payment"
	StatusQuoteReady            = "quote_ready"
	StatusFulfillmentProcessing = "fulfillment_processing"
	StatusFulfillmentComplete   = "fulfillment_complete"
)

// MapStatus maps a Stripe session status to the provider-neutral status.
// Unknown values map to funding.ProviderStatusUnknown (open enum).
func MapStatus(s string) funding.ProviderStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case StatusInitialized:
		return funding.ProviderStatusInitialized
	case StatusRequiresPayment, StatusQuoteReady:
		return funding.ProviderStatusCustomerActionRequired
	case StatusFulfillmentProcessing:
		return funding.ProviderStatusProcessing
	case StatusFulfillmentComplete:
		return funding.ProviderStatusConfirmed
	case StatusRejected:
		return funding.ProviderStatusRejected
	}
	return funding.ProviderStatusUnknown
}

// sessionObject mirrors the documented crypto.onramp_session object. Only
// documented fields are declared; unknown fields are ignored on decode.
// Amounts are strings: a JSON number here fails decoding by design.
type sessionObject struct {
	ID                 string              `json:"id"`
	Object             string              `json:"object"`
	ClientSecret       string              `json:"client_secret"`
	Created            int64               `json:"created"`
	KYCDetailsProvided bool                `json:"kyc_details_provided"`
	Livemode           bool                `json:"livemode"`
	Metadata           map[string]string   `json:"metadata"`
	RedirectURL        string              `json:"redirect_url"`
	Status             string              `json:"status"`
	TransactionDetails *transactionDetails `json:"transaction_details"`
}

type transactionDetails struct {
	DestinationAmount     string            `json:"destination_amount"`
	DestinationCurrency   string            `json:"destination_currency"`
	DestinationNetwork    string            `json:"destination_network"`
	DestinationCurrencies []string          `json:"destination_currencies"`
	DestinationNetworks   []string          `json:"destination_networks"`
	Fees                  *feesObject       `json:"fees"`
	LockWalletAddress     bool              `json:"lock_wallet_address"`
	SourceAmount          string            `json:"source_amount"`
	SourceCurrency        string            `json:"source_currency"`
	TransactionID         string            `json:"transaction_id"`
	WalletAddress         string            `json:"wallet_address"`
	WalletAddresses       map[string]string `json:"wallet_addresses"`
}

type feesObject struct {
	NetworkFeeMonetary     string `json:"network_fee_monetary"`
	TransactionFeeMonetary string `json:"transaction_fee_monetary"`
}

// eventObject mirrors the documented webhook event envelope.
type eventObject struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Type     string `json:"type"`
	Created  int64  `json:"created"`
	Livemode bool   `json:"livemode"`
	Data     struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// errorBody mirrors the documented error envelope {error:{type,code,message}}.
type errorBody struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Param   string `json:"param"`
	} `json:"error"`
}

// decodeStrict decodes a JSON document into v, refusing trailing data.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data after json document")
	}
	return nil
}

// decodeSession parses and validates a session object. Missing required
// fields (id with the cos_ prefix, object name, status) and malformed
// amounts are errors; the caller decides whether that is a malformed
// webhook or an invalid API response.
func decodeSession(raw []byte) (funding.Session, error) {
	var obj sessionObject
	if err := decodeStrict(raw, &obj); err != nil {
		return funding.Session{}, fmt.Errorf("stripe: session json: %w", err)
	}
	return toSession(obj)
}

func toSession(obj sessionObject) (funding.Session, error) {
	switch {
	case obj.ID == "" || !strings.HasPrefix(obj.ID, SessionIDPrefix):
		return funding.Session{}, fmt.Errorf("stripe: session id missing or not %q-prefixed", SessionIDPrefix)
	case obj.Object != "" && obj.Object != ObjectSession:
		return funding.Session{}, fmt.Errorf("stripe: object %q is not %q", obj.Object, ObjectSession)
	case strings.TrimSpace(obj.Status) == "":
		return funding.Session{}, fmt.Errorf("stripe: session %s has no status", obj.ID)
	}
	s := funding.Session{
		ID: obj.ID, Status: MapStatus(obj.Status), RawStatus: obj.Status, ClientSecret: obj.ClientSecret,
		RedirectURL: obj.RedirectURL, Livemode: obj.Livemode, KYCDetailsProvided: obj.KYCDetailsProvided, Metadata: obj.Metadata,
	}
	if obj.Created > 0 {
		s.CreatedAt = time.Unix(obj.Created, 0).UTC()
	}
	if td := obj.TransactionDetails; td != nil {
		for name, amount := range map[string]string{
			"destination_amount": td.DestinationAmount, "source_amount": td.SourceAmount,
		} {
			if amount != "" {
				if err := ValidateAmount(amount); err != nil {
					return funding.Session{}, fmt.Errorf("stripe: session %s %s: %w", obj.ID, name, err)
				}
			}
		}
		if td.Fees != nil {
			for name, amount := range map[string]string{
				"network_fee_monetary": td.Fees.NetworkFeeMonetary, "transaction_fee_monetary": td.Fees.TransactionFeeMonetary,
			} {
				if amount != "" {
					if err := ValidateAmount(amount); err != nil {
						return funding.Session{}, fmt.Errorf("stripe: session %s fees.%s: %w", obj.ID, name, err)
					}
				}
			}
			s.NetworkFee, s.TransactionFee = td.Fees.NetworkFeeMonetary, td.Fees.TransactionFeeMonetary
		}
		s.DestinationNetwork, s.DestinationCurrency, s.DestinationAmount = td.DestinationNetwork, td.DestinationCurrency, td.DestinationAmount
		s.SourceCurrency, s.SourceAmount, s.TransactionID = td.SourceCurrency, td.SourceAmount, td.TransactionID
		s.WalletAddress = td.WalletAddress
		if s.WalletAddress == "" && td.DestinationNetwork != "" {
			s.WalletAddress = td.WalletAddresses[td.DestinationNetwork]
		}
	}
	return s, nil
}

// decodeEvent parses a webhook envelope into a funding.WebhookEvent. The
// session is decoded only for the modeled event type.
func decodeEvent(raw []byte, signedAt time.Time) (funding.WebhookEvent, error) {
	var ev eventObject
	if err := decodeStrict(raw, &ev); err != nil {
		return funding.WebhookEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed, "stripe: event json is malformed")
	}
	switch {
	case ev.ID == "" || !strings.HasPrefix(ev.ID, EventIDPrefix):
		return funding.WebhookEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed, "stripe: event id missing or not evt_-prefixed")
	case ev.Object != "" && ev.Object != ObjectEvent:
		return funding.WebhookEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed, "stripe: object is not an event")
	case strings.TrimSpace(ev.Type) == "":
		return funding.WebhookEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed, "stripe: event type missing")
	}
	out := funding.WebhookEvent{
		Identity: webhook.Identity{Provider: ProviderName, EventID: ev.ID, EventType: ev.Type, SignedAt: signedAt, Livemode: ev.Livemode},
		Raw:      raw,
	}
	if ev.Created > 0 {
		out.Identity.PublishedAt = time.Unix(ev.Created, 0).UTC()
	}
	if ev.Type != EventOnrampSessionUpdated {
		return out, nil
	}
	if len(ev.Data.Object) == 0 {
		return funding.WebhookEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed, "stripe: session event without data.object")
	}
	session, err := decodeSession(ev.Data.Object)
	if err != nil {
		return funding.WebhookEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed, "stripe: "+err.Error())
	}
	session.ClientSecret = "" // never carried beyond the API response
	out.Session, out.SessionKnown = session, true
	return out, nil
}
