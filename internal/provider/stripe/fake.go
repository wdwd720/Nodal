package stripe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/provider"
)

// FakeWebhookSecret is the signing secret the fake uses when none is
// configured. It exists so LOCAL/TEST/DEV deployments can exercise the
// full signature path.
const FakeWebhookSecret = "whsec_fake_local_only" // #nosec G101 -- default signing secret of the in-process fake; NewFake refuses any environment outside LOCAL/TEST/DEV

// Fake is the in-process double of the onramp: Stripe-shaped sessions and
// signed webhook deliveries, no network. It is refused outside
// LOCAL/TEST/DEV.
type Fake struct {
	opts      Options
	mu        sync.Mutex
	seq       int
	byID      map[string]sessionObject
	byKey     map[string]string // idempotency key → session id
	created   []funding.CreateSessionRequest
	CreateErr error
}

var _ funding.FundingProvider = (*Fake)(nil)

// NewFake builds the fake. Options.Mode must be "fake" and Options.Env must
// allow fake providers.
func NewFake(o Options) (*Fake, error) {
	o, err := o.normalized()
	if err != nil {
		return nil, err
	}
	if o.Mode != config.ProviderModeFake {
		return nil, errs.New(errs.CodeValidationFailed, "stripe: NewFake serves fake mode only")
	}
	if !o.Env.AllowsFakeProviders() {
		return nil, errs.Newf(errs.CodeValidationFailed, "stripe: fake provider is not allowed in %s", o.Env)
	}
	if o.WebhookSecret == "" {
		o.WebhookSecret = FakeWebhookSecret
	}
	return &Fake{opts: o, byID: map[string]sessionObject{}, byKey: map[string]string{}}, nil
}

// Name implements funding.FundingProvider (the fake keeps the provider
// name so deposits and webhooks bind identically).
func (f *Fake) Name() string { return ProviderName }

// Verification implements funding.FundingProvider.
func (f *Fake) Verification() provider.VerificationLabel { return provider.CodeComplete }

// WebhookSecret returns the signing secret (tests build deliveries with it).
func (f *Fake) WebhookSecret() string { return f.opts.WebhookSecret }

// Created returns every create request received.
func (f *Fake) Created() []funding.CreateSessionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]funding.CreateSessionRequest(nil), f.created...)
}

// CreateSession mints an initialized session, idempotent on the key.
func (f *Fake) CreateSession(_ context.Context, req funding.CreateSessionRequest) (funding.Session, error) {
	if f.CreateErr != nil {
		return funding.Session{}, f.CreateErr
	}
	if _, err := sessionForm(req); err != nil {
		return funding.Session{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	if id, ok := f.byKey[req.IdempotencyKey]; ok {
		return toSession(f.byID[id])
	}
	f.seq++
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	obj := sessionObject{
		ID: SessionIDPrefix + "test_" + strconv.Itoa(f.seq), Object: ObjectSession,
		ClientSecret: SessionIDPrefix + "test_" + strconv.Itoa(f.seq) + "_secret_" + hex.EncodeToString(nonce[:]),
		Created:      f.opts.Clock.Now().Unix(), Livemode: false, Metadata: req.Metadata,
		RedirectURL: "https://fake.stripe.invalid/onramp?session=" + SessionIDPrefix + "test_" + strconv.Itoa(f.seq),
		Status:      StatusInitialized,
		TransactionDetails: &transactionDetails{
			DestinationCurrency: req.DestinationCurrency, DestinationNetwork: req.DestinationNetwork,
			DestinationCurrencies: []string{req.DestinationCurrency}, DestinationNetworks: []string{req.DestinationNetwork},
			LockWalletAddress: req.LockWalletAddress, SourceAmount: req.SourceAmount, SourceCurrency: req.SourceCurrency,
			WalletAddress: req.WalletAddress, WalletAddresses: map[string]string{req.DestinationNetwork: req.WalletAddress},
		},
	}
	f.byID[obj.ID] = obj
	f.byKey[req.IdempotencyKey] = obj.ID
	return toSession(obj)
}

// GetSession returns a session (NOT_FOUND when unknown).
func (f *Fake) GetSession(_ context.Context, sessionID string) (funding.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.byID[sessionID]
	if !ok {
		return funding.Session{}, errs.New(errs.CodeNotFound, "stripe: session not found").WithField("session_id", sessionID)
	}
	return toSession(obj)
}

// ParseWebhook verifies and decodes exactly as the real client does.
func (f *Fake) ParseWebhook(_ context.Context, raw []byte, headers http.Header) (funding.WebhookEvent, error) {
	return parseWebhook(raw, headers, f.opts.WebhookSecret, f.opts.Clock.Now(), f.opts.Tolerance, false)
}

// Fulfillment describes what SetStatus records for a fulfilled session.
type Fulfillment struct {
	DestinationAmount string
	TransactionID     string
	NetworkFee        string
	TransactionFee    string
	WalletAddress     string // overrides the locked address (tests of mismatch)
}

// SetStatus moves a session to a Stripe status string (any string; unknown
// values exercise the open-enum path) with optional fulfillment details.
func (f *Fake) SetStatus(sessionID, status string, ful *Fulfillment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.byID[sessionID]
	if !ok {
		return errs.New(errs.CodeNotFound, "stripe: session not found").WithField("session_id", sessionID)
	}
	obj.Status = status
	if ful != nil {
		td := obj.TransactionDetails
		if td == nil {
			td = &transactionDetails{}
		}
		if ful.DestinationAmount != "" {
			td.DestinationAmount = ful.DestinationAmount
		}
		if ful.TransactionID != "" {
			td.TransactionID = ful.TransactionID
		}
		if ful.WalletAddress != "" {
			td.WalletAddress = ful.WalletAddress
		}
		if ful.NetworkFee != "" || ful.TransactionFee != "" {
			td.Fees = &feesObject{NetworkFeeMonetary: ful.NetworkFee, TransactionFeeMonetary: ful.TransactionFee}
		}
		obj.TransactionDetails = td
	}
	f.byID[sessionID] = obj
	return nil
}

// Webhook renders a signed crypto.onramp_session.updated delivery for the
// session: the raw body and the Stripe-Signature header. eventType may be
// overridden to produce a signed but unmodelled event.
func (f *Fake) Webhook(eventID, sessionID string) ([]byte, http.Header, error) {
	return f.WebhookOfType(eventID, EventOnrampSessionUpdated, sessionID, f.opts.Clock.Now())
}

// WebhookOfType is Webhook with an explicit event type and signing time.
func (f *Fake) WebhookOfType(eventID, eventType, sessionID string, signedAt time.Time) ([]byte, http.Header, error) {
	f.mu.Lock()
	obj, ok := f.byID[sessionID]
	f.mu.Unlock()
	if !ok {
		return nil, nil, errs.New(errs.CodeNotFound, "stripe: session not found").WithField("session_id", sessionID)
	}
	obj.ClientSecret = ""
	raw, err := json.Marshal(map[string]any{
		"id": eventID, "object": ObjectEvent, "type": eventType, "created": f.opts.Clock.Now().Unix(), "livemode": false,
		"data": map[string]any{"object": obj},
	})
	if err != nil {
		return nil, nil, errs.Wrap(err, errs.CodeInternal, "stripe: encode fake webhook")
	}
	h := http.Header{}
	h.Set(SignatureHeader, SignatureHeaderValue(f.opts.WebhookSecret, raw, signedAt))
	h.Set("Content-Type", "application/json")
	return raw, h, nil
}
