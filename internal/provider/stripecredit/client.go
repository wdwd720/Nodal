package stripecredit

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/provider"
)

// Options configure a Client or a Fake.
type Options struct {
	Mode          config.ProviderMode
	Env           config.Environment
	BaseURL       string
	APIKey        string
	WebhookSecret string
	Timeout       time.Duration
	HTTPClient    *http.Client
	Clock         clock.Clock
	// Health, when set, receives one sample per API call.
	Health *provider.Tracker
	// Tolerance overrides the signature tolerance (tests only).
	Tolerance time.Duration

	// AccountID is the Stripe account this adapter is configured for, e.g.
	// "acct_...". It is asserted against the account the API key actually
	// belongs to at startup, so a key rotated to the wrong account is caught
	// before it takes a payment rather than after.
	AccountID string

	// SharedAccount declares that the Stripe account also serves another
	// product. It makes foreign-event rejection mandatory rather than
	// incidental, and it is surfaced in Capabilities so that operators can see
	// it without reading configuration.
	SharedAccount bool

	// ContractReference names the account or agreement behind this adapter.
	// Without one, credit.PurchaseRegistry refuses to load it in production.
	ContractReference string

	// StatementDescriptorSuffix is what the cardholder sees. On a shared
	// account this is the difference between a recognised charge and a
	// dispute, so it is configuration rather than a constant.
	StatementDescriptorSuffix string
}

// DefaultTimeout bounds every API call when Options.Timeout is zero.
const DefaultTimeout = 15 * time.Second

func (o Options) normalized() (Options, error) {
	if !o.Mode.IsValid() {
		return o, errs.Newf(errs.CodeValidationFailed, "stripecredit: unknown provider mode %q", o.Mode)
	}
	if !o.Env.IsValid() {
		return o, errs.Newf(errs.CodeValidationFailed, "stripecredit: unknown environment %q", o.Env)
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Tolerance <= 0 {
		o.Tolerance = SignatureTolerance
	}
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	return o, nil
}

// Client talks to the Stripe PaymentIntents API in sandbox or live mode.
type Client struct {
	opts    Options
	base    *url.URL
	http    *http.Client
	auth    string
	release string
}

var _ credit.PurchaseProvider = (*Client)(nil)

// NewClient builds a sandbox or live client.
//
// The key-prefix checks are the programmatic half of environment isolation.
// They exist because the expensive version of this mistake is silent: a test
// key in production takes payments that never settle, and a live key in
// staging takes payments from people who were testing.
func NewClient(o Options) (*Client, error) {
	o, err := o.normalized()
	if err != nil {
		return nil, err
	}
	if o.Mode == config.ProviderModeFake {
		return nil, errs.New(errs.CodeValidationFailed, "stripecredit: NewClient does not serve fake mode; use NewFake")
	}
	if o.APIKey == "" || o.WebhookSecret == "" {
		return nil, errs.New(errs.CodeValidationFailed, "stripecredit: api key and webhook secret are required")
	}
	switch o.Mode {
	case config.ProviderModeLive:
		if !strings.HasPrefix(o.APIKey, "sk_live_") && !strings.HasPrefix(o.APIKey, "rk_live_") {
			return nil, errs.New(errs.CodeValidationFailed, "stripecredit: live mode requires a live secret key")
		}
	case config.ProviderModeSandbox:
		if !strings.HasPrefix(o.APIKey, "sk_test_") && !strings.HasPrefix(o.APIKey, "rk_test_") {
			return nil, errs.New(errs.CodeValidationFailed, "stripecredit: sandbox mode requires a test secret key")
		}
	}
	if o.Env.IsProductionLike() && o.Mode != config.ProviderModeLive {
		return nil, errs.Newf(errs.CodeValidationFailed,
			"stripecredit: %s cannot run the credit purchase adapter in %s mode; test Stripe objects must never reach production",
			o.Env, o.Mode)
	}
	base, err := url.Parse(o.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, errs.New(errs.CodeValidationFailed, "stripecredit: base url is invalid")
	}
	if o.Mode == config.ProviderModeLive && base.Scheme != "https" {
		return nil, errs.New(errs.CodeValidationFailed, "stripecredit: live mode requires https")
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: o.Timeout}
	}
	return &Client{
		opts: o, base: base, http: hc,
		auth:    "Basic " + base64.StdEncoding.EncodeToString([]byte(o.APIKey+":")),
		release: "nodal-controlplane/" + config.BuildVersion,
	}, nil
}

// Name implements credit.PurchaseProvider.
func (c *Client) Name() string { return ProviderName }

// Mode returns the configured mode.
func (c *Client) Mode() config.ProviderMode { return c.opts.Mode }

// Verification reports how far this integration has actually been proven.
//
// It is CODE_COMPLETE and not SANDBOX_VERIFIED. The adapter is written against
// the documented PaymentIntents API and covered by contract tests over
// recorded fixtures, but no Stripe sandbox run has exercised it end to end on
// this account, and claiming otherwise would be the exact failure the goal
// document's Section 70 is about.
func (c *Client) Verification() provider.VerificationLabel { return provider.CodeComplete }

func (c *Client) livemode() bool { return c.opts.Mode == config.ProviderModeLive }

// Capabilities implements credit.PurchaseProvider.
//
// Every true below is a statement about Stripe's documented behaviour for the
// PaymentIntents product, not about what would be convenient.
func (c *Client) Capabilities() credit.PurchaseCapabilities {
	return credit.PurchaseCapabilities{
		SupportsHostedPaymentUI:  true, // Payment Element; no PAN reaches Nodal
		SupportsSCA:              true, // 3-D Secure is handled by the Element
		SupportsIdempotentCreate: true, // Idempotency-Key on POST /v1/payment_intents
		SupportsLookup:           true, // GET /v1/payment_intents/:id
		SupportsRefund:           true,
		SupportsDisputeEvents:    true, // charge.dispute.*
		SupportsFraudScreening:   true, // Radar
		Currencies:               []string{"USD"},
		SharedProviderAccount:    c.opts.SharedAccount,
		ContractReference:        c.opts.ContractReference,
	}
}

// AccountID is the Stripe account this adapter is configured for.
func (c *Client) AccountID() string { return c.opts.AccountID }

// CreatePurchase implements credit.PurchaseProvider with
// POST /v1/payment_intents.
//
// It is an IDEMPOTENT_WRITE: the Idempotency-Key header carries the caller's
// key, which was persisted before this call, so a retry after a lost response
// returns the same PaymentIntent rather than charging a second time.
func (c *Client) CreatePurchase(ctx context.Context, req credit.CreatePurchaseRequest) (credit.PurchaseSession, error) {
	if err := req.Validate(); err != nil {
		return credit.PurchaseSession{}, err
	}
	if !c.Capabilities().SupportsCurrency(req.Currency) {
		return credit.PurchaseSession{}, errs.Newf(errs.CodeUnsupported,
			"stripecredit: this deployment does not sell Credits in %s", req.Currency)
	}
	form, err := c.purchaseForm(req)
	if err != nil {
		return credit.PurchaseSession{}, err
	}
	raw, err := c.do(ctx, http.MethodPost, "/v1/payment_intents", form, req.IdempotencyKey)
	if err != nil {
		return credit.PurchaseSession{}, err
	}
	var pi paymentIntent
	if err := decodeLoose(raw, &pi); err != nil {
		return credit.PurchaseSession{}, errs.Wrap(err, errs.CodeProviderUnavailable,
			"stripecredit: invalid payment intent response").WithField("provider_error", "malformed_response")
	}
	snap, err := snapshotFrom(pi)
	if err != nil {
		return credit.PurchaseSession{}, errs.Wrap(err, errs.CodeProviderUnavailable,
			"stripecredit: invalid payment intent response").WithField("provider_error", "malformed_response")
	}
	if snap.Livemode != c.livemode() {
		return credit.PurchaseSession{}, errs.New(errs.CodeInternal,
			"stripecredit: payment intent livemode does not match the adapter mode").
			WithField("object_livemode", snap.Livemode).WithField("adapter_livemode", c.livemode())
	}
	// The provider is authoritative for what it charged. If it did not charge
	// what we asked, the safe move is to refuse the session rather than show
	// the customer a payment form for an amount nobody approved.
	if snap.Amount.Minor() != req.Amount.Minor() {
		return credit.PurchaseSession{}, errs.Newf(errs.CodeInternal,
			"stripecredit: asked for %s and the provider created %s", req.Amount, snap.Amount)
	}
	created := time.Unix(pi.Created, 0).UTC()
	if pi.Created <= 0 {
		created = c.opts.Clock.Now().UTC()
	}
	return credit.PurchaseSession{
		ProviderReference: snap.ProviderReference,
		Status:            snap.Status,
		RawStatus:         snap.RawStatus,
		ClientSecret:      pi.ClientSecret,
		Livemode:          snap.Livemode,
		CreatedAt:         created,
	}, nil
}

// GetPurchase implements credit.PurchaseProvider with
// GET /v1/payment_intents/:id (SAFE_RETRY).
func (c *Client) GetPurchase(ctx context.Context, ref string) (credit.PurchaseSnapshot, error) {
	if ref == "" || !strings.HasPrefix(ref, PaymentIntentIDPrefix) || strings.ContainsAny(ref, "/?#") {
		return credit.PurchaseSnapshot{}, errs.New(errs.CodeValidationFailed,
			"stripecredit: payment intent id is invalid")
	}
	raw, err := c.do(ctx, http.MethodGet, "/v1/payment_intents/"+url.PathEscape(ref), nil, "")
	if err != nil {
		return credit.PurchaseSnapshot{}, err
	}
	var pi paymentIntent
	if err := decodeLoose(raw, &pi); err != nil {
		return credit.PurchaseSnapshot{}, errs.Wrap(err, errs.CodeProviderUnavailable,
			"stripecredit: invalid payment intent response").WithField("provider_error", "malformed_response")
	}
	snap, err := snapshotFrom(pi)
	if err != nil {
		return credit.PurchaseSnapshot{}, errs.Wrap(err, errs.CodeProviderUnavailable,
			"stripecredit: invalid payment intent response").WithField("provider_error", "malformed_response")
	}
	if snap.Livemode != c.livemode() {
		return credit.PurchaseSnapshot{}, errs.New(errs.CodeInternal,
			"stripecredit: payment intent livemode does not match the adapter mode")
	}
	return snap, nil
}

// ParseWebhook implements credit.PurchaseProvider.
func (c *Client) ParseWebhook(_ context.Context, raw []byte, headers http.Header) (credit.PurchaseEvent, error) {
	return parseWebhook(raw, headers, c.opts.WebhookSecret, c.opts.Clock.Now(),
		c.opts.Tolerance, c.livemode(), string(c.opts.Env))
}

// purchaseForm encodes the documented create parameters.
//
// Note what is not here: no confirm, no payment_method, no off_session. This
// adapter creates an intent and hands its client secret to the browser, where
// Stripe's own Element collects the card. Nothing server-side ever sees a
// payment method.
func (c *Client) purchaseForm(req credit.CreatePurchaseRequest) (url.Values, error) {
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(req.Amount.Minor(), 10))
	form.Set("currency", strings.ToLower(req.Currency))
	form.Set("automatic_payment_methods[enabled]", "true")
	// Capture immediately. A separate authorize-then-capture would hold the
	// customer's funds while Nodal decided something, and Nodal has nothing to
	// decide: the Credits are minted from a captured payment or not at all.
	form.Set("capture_method", "automatic")
	if req.Description != "" {
		form.Set("description", req.Description)
	}
	suffix := req.StatementDescriptorSuffix
	if suffix == "" {
		suffix = c.opts.StatementDescriptorSuffix
	}
	if suffix != "" {
		form.Set("statement_descriptor_suffix", suffix)
	}
	for k, v := range MetadataFor(req, string(c.opts.Env)) {
		form.Set("metadata["+k+"]", v)
	}
	return form, nil
}

// MetadataFor is the immutable metadata written onto every Nodal PaymentIntent.
//
// It is a package-level function rather than a method because it is also what
// the fake writes and what the contract tests assert, and three copies of this
// map would eventually disagree about a key name -- at which point events stop
// being recognised as Nodal's and Credits stop being issued.
func MetadataFor(req credit.CreatePurchaseRequest, environment string) map[string]string {
	return map[string]string{
		MetaWorkstream:     MetaWorkstreamValue,
		MetaFundingID:      req.FundingID.String(),
		MetaUserID:         req.AccountID.String(),
		MetaPricingVersion: req.PricingVersion,
		MetaPricingHash:    req.PricingHash,
		MetaCreditQuantity: req.CreditQuantity.String(),
		MetaEnvironment:    environment,
	}
}

// do performs one API call with timeout, auth and idempotency header, records
// a health sample and maps every failure to a stable code.
func (c *Client) do(ctx context.Context, method, path string, form url.Values, idempotencyKey string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + path
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "stripecredit: build request")
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.release)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	started := c.opts.Clock.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.observe(started, false)
		observability.LoggerFrom(ctx).WarnContext(ctx, "stripecredit: transport failure",
			"method", method, "path", path, "error", err.Error())
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errs.Wrap(err, errs.CodeProviderUnavailable, "stripecredit: request timed out").
				WithField("provider_error", "timeout")
		}
		return nil, errs.Wrap(err, errs.CodeProviderUnavailable, "stripecredit: request failed").
			WithField("provider_error", "transport")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		c.observe(started, false)
		return nil, errs.Wrap(err, errs.CodeProviderUnavailable, "stripecredit: read response").
			WithField("provider_error", "transport")
	}
	if len(raw) > MaxResponseBytes {
		c.observe(started, false)
		return nil, errs.New(errs.CodeProviderUnavailable, "stripecredit: response too large").
			WithField("provider_error", "malformed_response")
	}
	ok := resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests
	c.observe(started, ok)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	return nil, c.apiError(resp, raw)
}

func (c *Client) observe(started time.Time, ok bool) {
	if c.opts.Health != nil {
		now := c.opts.Clock.Now()
		c.opts.Health.Observe(now, ok, now.Sub(started))
	}
}

// apiError maps a non-2xx response to a stable code.
//
// The provider's message is never surfaced: it can describe account
// configuration. Its code and type travel in Fields for operators.
func (c *Client) apiError(resp *http.Response, raw []byte) error {
	var eb errorBody
	_ = decodeStrict(raw, &eb)
	code, typ, decline := eb.Error.Code, eb.Error.Type, eb.Error.DeclineCode
	with := func(e *errs.Error) *errs.Error {
		e = e.WithField("http_status", resp.StatusCode).
			WithField("provider_error_code", code).
			WithField("provider_error_type", typ)
		if decline != "" {
			e = e.WithField("provider_decline_code", decline)
		}
		return e
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		e := with(errs.New(errs.CodeRateLimited, "stripecredit: rate limited"))
		if d, ok := retryAfter(resp.Header.Get("Retry-After"), c.opts.Clock.Now()); ok {
			e = e.WithRetryAfter(d)
		}
		return e
	case resp.StatusCode >= 500:
		// A 5xx on a POST is the dangerous one: the payment intent may exist.
		// It is never retried without the same idempotency key, which is why
		// the key is persisted before the call rather than generated in it.
		return with(errs.New(errs.CodeProviderUnavailable, "stripecredit: provider error"))
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return with(errs.New(errs.CodeInternal, "stripecredit: provider rejected the credentials"))
	case resp.StatusCode == http.StatusNotFound:
		return with(errs.New(errs.CodeNotFound, "stripecredit: payment intent not found"))
	}
	if typ == "card_error" {
		// A decline is the customer's card issuer saying no. It is a 4xx and
		// not a provider outage, and the decline code travels in Fields so
		// support can tell "insufficient funds" from "do not honour" without
		// the customer-facing message repeating either.
		return with(errs.New(errs.CodeValidationFailed, "stripecredit: the card was declined"))
	}
	return with(errs.New(errs.CodeValidationFailed, "stripecredit: provider rejected the request"))
}

// retryAfter parses a Retry-After header (delta-seconds or HTTP-date).
func retryAfter(v string, now time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}
