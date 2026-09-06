package stripe

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
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/funding"
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
	// Tolerance overrides the signature tolerance (tests only; default
	// SignatureTolerance).
	Tolerance time.Duration
}

// DefaultTimeout bounds every API call when Options.Timeout is zero.
const DefaultTimeout = 15 * time.Second

func (o Options) normalized() (Options, error) {
	if !o.Mode.IsValid() {
		return o, errs.Newf(errs.CodeValidationFailed, "stripe: unknown provider mode %q", o.Mode)
	}
	if !o.Env.IsValid() {
		return o, errs.Newf(errs.CodeValidationFailed, "stripe: unknown environment %q", o.Env)
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

// Client talks to the Stripe API in sandbox or live mode.
type Client struct {
	opts    Options
	base    *url.URL
	http    *http.Client
	auth    string
	release string
}

var _ funding.FundingProvider = (*Client)(nil)

// NewClient builds a sandbox or live client. Fake mode is refused here
// (use NewFake). Live mode requires a live key and https; sandbox requires
// a test key.
func NewClient(o Options) (*Client, error) {
	o, err := o.normalized()
	if err != nil {
		return nil, err
	}
	if o.Mode == config.ProviderModeFake {
		return nil, errs.New(errs.CodeValidationFailed, "stripe: NewClient does not serve fake mode; use NewFake")
	}
	if o.APIKey == "" || o.WebhookSecret == "" {
		return nil, errs.New(errs.CodeValidationFailed, "stripe: api key and webhook secret are required")
	}
	switch o.Mode {
	case config.ProviderModeLive:
		if !strings.HasPrefix(o.APIKey, "sk_live_") && !strings.HasPrefix(o.APIKey, "rk_live_") {
			return nil, errs.New(errs.CodeValidationFailed, "stripe: live mode requires a live secret key")
		}
	case config.ProviderModeSandbox:
		if !strings.HasPrefix(o.APIKey, "sk_test_") && !strings.HasPrefix(o.APIKey, "rk_test_") {
			return nil, errs.New(errs.CodeValidationFailed, "stripe: sandbox mode requires a test secret key")
		}
	}
	base, err := url.Parse(o.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, errs.New(errs.CodeValidationFailed, "stripe: base url is invalid")
	}
	if o.Mode == config.ProviderModeLive && base.Scheme != "https" {
		return nil, errs.New(errs.CodeValidationFailed, "stripe: live mode requires https")
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

// Name implements funding.FundingProvider.
func (c *Client) Name() string { return ProviderName }

// Verification implements funding.FundingProvider: CODE_COMPLETE until the
// application-gated sandbox is available (EB-003).
func (c *Client) Verification() provider.VerificationLabel { return provider.CodeComplete }

// Mode returns the configured mode.
func (c *Client) Mode() config.ProviderMode { return c.opts.Mode }

func (c *Client) livemode() bool { return c.opts.Mode == config.ProviderModeLive }

// CreateSession implements funding.FundingProvider with
// POST /v1/crypto/onramp_sessions (IDEMPOTENT_WRITE: the Idempotency-Key
// header carries req.IdempotencyKey, so a retry never mints a second
// session).
func (c *Client) CreateSession(ctx context.Context, req funding.CreateSessionRequest) (funding.Session, error) {
	form, err := sessionForm(req)
	if err != nil {
		return funding.Session{}, err
	}
	raw, err := c.do(ctx, http.MethodPost, "/v1/crypto/onramp_sessions", form, req.IdempotencyKey)
	if err != nil {
		return funding.Session{}, err
	}
	return c.session(raw)
}

// GetSession implements funding.FundingProvider with
// GET /v1/crypto/onramp_sessions/:id (SAFE_RETRY).
func (c *Client) GetSession(ctx context.Context, sessionID string) (funding.Session, error) {
	if sessionID == "" || !strings.HasPrefix(sessionID, SessionIDPrefix) || strings.ContainsAny(sessionID, "/?#") {
		return funding.Session{}, errs.New(errs.CodeValidationFailed, "stripe: session id is invalid")
	}
	raw, err := c.do(ctx, http.MethodGet, "/v1/crypto/onramp_sessions/"+url.PathEscape(sessionID), nil, "")
	if err != nil {
		return funding.Session{}, err
	}
	return c.session(raw)
}

// ParseWebhook implements funding.FundingProvider.
func (c *Client) ParseWebhook(_ context.Context, raw []byte, headers http.Header) (funding.WebhookEvent, error) {
	return parseWebhook(raw, headers, c.opts.WebhookSecret, c.opts.Clock.Now(), c.opts.Tolerance, c.livemode())
}

func (c *Client) session(raw []byte) (funding.Session, error) {
	s, err := decodeSession(raw)
	if err != nil {
		return funding.Session{}, errs.Wrap(err, errs.CodeProviderUnavailable, "stripe: invalid session response").
			WithField("provider_error", "malformed_response")
	}
	if s.Livemode != c.livemode() {
		return funding.Session{}, errs.New(errs.CodeInternal, "stripe: session livemode does not match the adapter mode").
			WithField("session_livemode", s.Livemode).WithField("adapter_livemode", c.livemode())
	}
	return s, nil
}

// sessionForm encodes the documented create parameters.
func sessionForm(req funding.CreateSessionRequest) (url.Values, error) {
	network, currency := strings.ToLower(req.DestinationNetwork), strings.ToLower(req.DestinationCurrency)
	switch {
	case req.IdempotencyKey == "":
		return nil, errs.New(errs.CodeValidationFailed, "stripe: idempotency key is required")
	case network == "" || currency == "":
		return nil, errs.New(errs.CodeValidationFailed, "stripe: destination network and currency are required")
	case req.WalletAddress == "":
		return nil, errs.New(errs.CodeValidationFailed, "stripe: wallet address is required")
	}
	if _, ok := CurrencyDecimals(network, currency); !ok {
		return nil, errs.Newf(errs.CodeUnsupported, "stripe: destination %s/%s is not supported by this deployment", network, currency)
	}
	form := url.Values{}
	form.Set("wallet_addresses["+network+"]", req.WalletAddress)
	form.Set("lock_wallet_address", strconv.FormatBool(req.LockWalletAddress))
	form.Set("destination_networks[]", network)
	form.Set("destination_currencies[]", currency)
	form.Set("destination_network", network)
	form.Set("destination_currency", currency)
	if req.SourceCurrency != "" {
		form.Set("source_currency", strings.ToLower(req.SourceCurrency))
	}
	if req.SourceAmount != "" {
		if err := ValidateAmount(req.SourceAmount); err != nil {
			return nil, err
		}
		form.Set("source_amount", req.SourceAmount)
	}
	if req.CustomerIPAddress != "" {
		form.Set("customer_ip_address", req.CustomerIPAddress)
	}
	if ci := req.Customer; ci != nil {
		if ci.Email != "" {
			form.Set("customer_information[email]", ci.Email)
		}
		if ci.FirstName != "" {
			form.Set("customer_information[first_name]", ci.FirstName)
		}
		if ci.LastName != "" {
			form.Set("customer_information[last_name]", ci.LastName)
		}
	}
	for k, v := range req.Metadata {
		form.Set("metadata["+k+"]", v)
	}
	return form, nil
}

// do performs one API call with timeout, auth and idempotency header,
// records a health sample and maps every failure to a stable code.
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
		return nil, errs.Wrap(err, errs.CodeInternal, "stripe: build request")
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
		observability.LoggerFrom(ctx).WarnContext(ctx, "stripe: transport failure", "method", method, "path", path, "error", err.Error())
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errs.Wrap(err, errs.CodeProviderUnavailable, "stripe: request timed out").WithField("provider_error", "timeout")
		}
		return nil, errs.Wrap(err, errs.CodeProviderUnavailable, "stripe: request failed").WithField("provider_error", "transport")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		c.observe(started, false)
		return nil, errs.Wrap(err, errs.CodeProviderUnavailable, "stripe: read response").WithField("provider_error", "transport")
	}
	if len(raw) > MaxResponseBytes {
		c.observe(started, false)
		return nil, errs.New(errs.CodeProviderUnavailable, "stripe: response too large").WithField("provider_error", "malformed_response")
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

// apiError maps a non-2xx response to a stable code. The provider's error
// code and type are carried in Fields; its message is never surfaced to
// customers (it may describe internal configuration).
func (c *Client) apiError(resp *http.Response, raw []byte) error {
	var eb errorBody
	_ = decodeStrict(raw, &eb)
	code, typ := eb.Error.Code, eb.Error.Type
	with := func(e *errs.Error) *errs.Error {
		return e.WithField("http_status", resp.StatusCode).WithField("provider_error_code", code).WithField("provider_error_type", typ)
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		e := with(errs.New(errs.CodeRateLimited, "stripe: rate limited"))
		if d, ok := retryAfter(resp.Header.Get("Retry-After"), c.opts.Clock.Now()); ok {
			e = e.WithRetryAfter(d)
		}
		return e
	case resp.StatusCode >= 500:
		return with(errs.New(errs.CodeProviderUnavailable, "stripe: provider error"))
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return with(errs.New(errs.CodeInternal, "stripe: provider rejected the credentials"))
	case resp.StatusCode == http.StatusNotFound:
		return with(errs.New(errs.CodeNotFound, "stripe: session not found"))
	}
	switch code {
	case "crypto_onramp_disabled", "crypto_onramp_merchant_not_properly_setup":
		return with(errs.New(errs.CodeProviderUnavailable, "stripe: onramp is disabled for this account"))
	case "crypto_onramp_unsupported_country", "crypto_onramp_unsupportable_customer":
		return with(errs.New(errs.CodeEligibilityJurisdiction, "stripe: customer is not eligible for the onramp"))
	}
	return with(errs.New(errs.CodeValidationFailed, "stripe: provider rejected the request"))
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
