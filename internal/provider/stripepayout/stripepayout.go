package stripepayout

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/provider"
)

// ProviderName is the adapter name in configuration, gates and evidence.
const ProviderName = "stripe_stablecoin_payout"

// DefaultBaseURL is the Stripe API root.
const DefaultBaseURL = "https://api.stripe.com"

// MaxResponseBytes bounds a response body.
const MaxResponseBytes = 512 << 10

// Metadata keys written onto every transfer. They are namespaced because this
// Stripe account also serves another product, and MetaIdempotencyKey exists
// because Lookup has to be able to find a transfer by the key we submitted it
// under.
const (
	MetaWorkstream      = "nodal_workstream"
	MetaWorkstreamValue = "NODAL"
	MetaRequestID       = "nodal_payout_request_id"
	MetaEnvironment     = "nodal_environment"
	MetaIdempotencyKey  = "nodal_idempotency_key"
)

// PayoutAssetUSDC is the only asset Stripe's stablecoin payout supports.
//
// From https://docs.stripe.com/connect/stablecoin-payouts: "Stripe supports
// only USDC for stablecoin balances".
const PayoutAssetUSDC = "USDC"

// The networks Stripe Express actually processes USDC payouts over.
//
// From https://support.stripe.com/express/questions/stablecoin-payouts:
// "Stripe Express currently processes USDC payouts over the Base and Polygon
// Networks". Solana is deliberately absent. If it is ever added, it is added
// here with a source, and not because a wallet supports it.
const (
	NetworkBase    = "base"
	NetworkPolygon = "polygon"
)

// SupportedNetworks returns the payout networks, in the order Stripe lists
// them.
func SupportedNetworks() []string { return []string{NetworkBase, NetworkPolygon} }

// Recipient kinds Stripe will pay. From the product's own limitations:
// "Stablecoin payouts can currently be sent only to individuals or sole
// proprietors ... Payouts to companies and non-profits aren't supported yet."
const (
	RecipientIndividual     = "individual"
	RecipientSoleProprietor = "sole_proprietor"
)

// supportedCountries is the recipient list the product documentation
// publishes, verbatim, in the order it publishes them.
//
// It is here rather than in a config file because it is a fact about Stripe,
// not a choice this deployment makes. A jurisdiction absent from it cannot be
// paid however permissive Nodal's own policy becomes.
var supportedCountries = []string{
	"AE", "AM", "AR", "AT", "AU", "AZ", "BE", "BG", "BH", "BJ", "CA", "CH", "CL", "CO", "CR",
	"CY", "CZ", "DK", "DO", "EC", "EE", "FI", "FR", "GH", "GR", "HR", "HU", "IE", "IL", "JM",
	"JO", "KE", "KR", "KW", "KZ", "LI", "LK", "LT", "LU", "LV", "MN", "MT", "MU", "MX", "MY",
	"NL", "NO", "NZ", "PA", "PE", "PH", "PL", "PT", "PY", "RO", "SA", "SE", "SG", "SI", "SK",
	"SV", "TH", "TN", "US", "UY", "UZ", "ZA",
}

// excludedUSStates are the US states the product excludes: "except the US
// states of New York and Hawaii".
var excludedUSStates = []string{"NY", "HI"}

// SupportedCountries returns the recipient countries, as published.
func SupportedCountries() []string { return append([]string(nil), supportedCountries...) }

// ExcludedUSStates returns the US states excluded from the product.
func ExcludedUSStates() []string { return append([]string(nil), excludedUSStates...) }

// SupportsCountry reports whether a two-letter country code is on Stripe's
// published list.
func SupportsCountry(code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	for _, c := range supportedCountries {
		if c == code {
			return true
		}
	}
	return false
}

// SupportsUSState reports whether a US state may receive a stablecoin payout.
// An empty state is refused: "we do not know which state" is not "any state".
func SupportsUSState(state string) bool {
	state = strings.ToUpper(strings.TrimSpace(state))
	if state == "" {
		return false
	}
	for _, s := range excludedUSStates {
		if s == state {
			return false
		}
	}
	return true
}

// Options configure a Client.
type Options struct {
	Mode       config.ProviderMode
	Env        config.Environment
	BaseURL    string
	APIKey     string
	Timeout    time.Duration
	HTTPClient *http.Client
	Clock      clock.Clock
	Health     *provider.Tracker

	// AccountID is the Stripe platform account, asserted at startup.
	AccountID string

	// Availability is how far this account has actually been granted the
	// product. It is configuration rather than a constant because it changes
	// when Stripe answers an application, and the code should not need a
	// release to reflect that.
	//
	// The default is the zero value, which is not usable. An operator who
	// forgets to set it gets a refusal, not a payout.
	Availability payout.Availability

	// ContractReference names the account and agreement behind this adapter.
	ContractReference string
}

// DefaultTimeout bounds every API call when Options.Timeout is zero.
const DefaultTimeout = 20 * time.Second

// Client is the Stripe stablecoin payout adapter.
type Client struct {
	opts    Options
	base    *url.URL
	http    *http.Client
	auth    string
	release string
}

var _ payout.Provider = (*Client)(nil)

// NewClient builds the adapter.
func NewClient(o Options) (*Client, error) {
	if !o.Mode.IsValid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "stripepayout: unknown provider mode %q", o.Mode)
	}
	if !o.Env.IsValid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "stripepayout: unknown environment %q", o.Env)
	}
	if o.Mode == config.ProviderModeFake {
		return nil, errs.New(errs.CodeValidationFailed, "stripepayout: fake mode is not served here")
	}
	if o.APIKey == "" {
		return nil, errs.New(errs.CodeValidationFailed, "stripepayout: api key is required")
	}
	switch o.Mode {
	case config.ProviderModeLive:
		if !strings.HasPrefix(o.APIKey, "sk_live_") && !strings.HasPrefix(o.APIKey, "rk_live_") {
			return nil, errs.New(errs.CodeValidationFailed, "stripepayout: live mode requires a live secret key")
		}
	case config.ProviderModeSandbox:
		if !strings.HasPrefix(o.APIKey, "sk_test_") && !strings.HasPrefix(o.APIKey, "rk_test_") {
			return nil, errs.New(errs.CodeValidationFailed, "stripepayout: sandbox mode requires a test secret key")
		}
	}
	// The same pairing stripecredit enforces, for the same reasons: PROD is
	// live only, and STAGING is sandbox only so a rehearsal cannot pay real
	// money out. See the comment there.
	switch o.Env {
	case config.EnvProd:
		if o.Mode != config.ProviderModeLive {
			return nil, errs.Newf(errs.CodeValidationFailed,
				"stripepayout: %s cannot run in %s mode", o.Env, o.Mode)
		}
	case config.EnvStaging:
		if o.Mode == config.ProviderModeLive {
			return nil, errs.Newf(errs.CodeValidationFailed,
				"stripepayout: %s must run against the Stripe sandbox, not live mode; a rehearsal that pays out real money is not a rehearsal",
				o.Env)
		}
	}
	// A sandbox-mode adapter claiming LIVE availability would let a test-mode
	// key be the thing that moves real value.
	if o.Availability == payout.AvailabilityLive && o.Mode != config.ProviderModeLive {
		return nil, errs.New(errs.CodeValidationFailed,
			"stripepayout: availability LIVE requires live mode; a test key cannot move real value")
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	base, err := url.Parse(o.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, errs.New(errs.CodeValidationFailed, "stripepayout: base url is invalid")
	}
	if o.Mode == config.ProviderModeLive && base.Scheme != "https" {
		return nil, errs.New(errs.CodeValidationFailed, "stripepayout: live mode requires https")
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

// Name implements payout.Provider.
func (c *Client) Name() string { return ProviderName }

// Verification reports how far this integration has been proven.
func (c *Client) Verification() provider.VerificationLabel { return provider.CodeComplete }

// Capabilities implements payout.Provider.
//
// Every value here is a quotation from Stripe's current documentation, and
// nothing is inferred. In particular SupportedNetworks does not contain
// "solana", however much the rest of this system is built around Solana
// wallets, because Stripe does not send USDC there.
func (c *Client) Capabilities() payout.Capabilities {
	return payout.Capabilities{
		// The only destination kind this product has.
		SupportsCryptoPayout: true,
		SupportsBankPayout:   false,
		SupportsCardPush:     false,
		SupportsFiatWallet:   false,

		SupportsKYCAtExit: true,
		SupportsWebhooks:  true,
		SupportsLookup:    true,
		Currencies:        []string{"USD"},

		SupportedAssets:   []string{PayoutAssetUSDC},
		SupportedNetworks: SupportedNetworks(),

		// The user links their own wallet in the Express Dashboard.
		SupportsExternalWallet:    true,
		DestinationHeldByProvider: true,

		RequiresConnect:          true,
		RequiresRecipientAccount: true,
		RequiresKYC:              true,
		KYCPerformedByProvider:   true,
		RequiresTaxInfo:          true,
		RecipientKinds:           []string{RecipientIndividual, RecipientSoleProprietor},
		SupportedCountries:       SupportedCountries(),
		// "except the US states of New York and Hawaii".
		ExcludedRegions: map[string][]string{"US": ExcludedUSStates()},

		// Stripe publishes neither a minimum nor a maximum for this product.
		// Both are left zero, which the payout engine reads as unknown. A
		// guessed bound would be a promise nobody made.
		MinimumAmount: money.USDFromMinor(0),
		MaximumAmount: money.USDFromMinor(0),

		Availability:      c.opts.Availability,
		ContractReference: c.opts.ContractReference,
	}
}

// ErrNotApproved is returned when the account has not been granted the
// product. It is deliberately distinct from an outage: waiting will not fix
// it, and a retry loop should not treat it as transient.
var ErrNotApproved = errors.New("stripepayout: this account has not been granted stablecoin payouts")

// Submit implements payout.Provider.
//
// It refuses before touching the network unless the product is actually
// available to this account. That refusal is the whole point of Availability:
// an adapter that would attempt a payout it has no permission to make is
// indistinguishable from a working one until the first real user is waiting
// for money.
func (c *Client) Submit(ctx context.Context, req payout.SubmitRequest) (payout.SubmitResult, error) {
	if err := c.guard(req); err != nil {
		return payout.SubmitResult{}, err
	}
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(req.Amount.Minor(), 10))
	form.Set("currency", strings.ToLower(req.Currency))
	// The destination is the connected account, never a wallet address. The
	// address lives in the recipient's Express Dashboard and Stripe resolves
	// it; there is no parameter here that could carry one.
	form.Set("destination", req.DestinationReference)
	form.Set("transfer_group", req.Reference)
	form.Set("metadata["+MetaWorkstream+"]", MetaWorkstreamValue)
	form.Set("metadata["+MetaRequestID+"]", req.Reference)
	form.Set("metadata["+MetaEnvironment+"]", string(c.opts.Env))
	// The idempotency key is written into metadata because that is the only
	// way Lookup can find the object later. Stripe's Idempotency-Key header is
	// not a queryable property of the transfer it created, so a key that lives
	// only in the header is a key nothing can be searched by -- and Lookup is
	// what resolves PAYOUT_STATUS_UNKNOWN. Without this line the search below
	// matches nothing and every timed-out submission stays ambiguous forever.
	form.Set("metadata["+MetaIdempotencyKey+"]", req.IdempotencyKey)

	raw, err := c.do(ctx, http.MethodPost, "/v1/transfers", form, req.IdempotencyKey)
	if err != nil {
		return payout.SubmitResult{}, err
	}
	return c.result(raw)
}

// Lookup implements payout.Provider.
//
// Stripe's idempotency is keyed per request, and the way to ask "what happened
// to this key" is to replay the create with it: a repeated key returns the
// original object rather than making a second one. That is why this method can
// exist at all, and why an adapter without it could not be used for payouts.
func (c *Client) Lookup(ctx context.Context, idempotencyKey string) (payout.SubmitResult, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return payout.SubmitResult{}, errs.New(errs.CodeValidationFailed, "stripepayout: an idempotency key is required")
	}
	if !c.opts.Availability.Usable() {
		return payout.SubmitResult{}, errs.Newf(errs.CodeCapabilityNotApproved,
			"stripepayout: cannot look up a payout on an account that has not been granted the product (availability %s)",
			c.availability())
	}
	// Search rather than replay. Replaying a create to discover its outcome
	// means sending a write to find out whether a write happened, and a
	// mistake in that call is a second payout.
	q := url.Values{}
	q.Set("query", "metadata['"+MetaIdempotencyKey+"']:'"+escapeSearch(idempotencyKey)+"'")
	raw, err := c.do(ctx, http.MethodGet, "/v1/transfers/search?"+q.Encode(), nil, "")
	if err != nil {
		return payout.SubmitResult{}, err
	}
	var list struct {
		Data []transferObject `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return payout.SubmitResult{}, errs.Wrap(err, errs.CodeProviderUnavailable,
			"stripepayout: invalid search response").WithField("provider_error", "malformed_response")
	}
	if len(list.Data) == 0 {
		// Nothing under that key. The provider never took the request, so the
		// caller may safely submit. This is the ONLY answer that licenses a
		// resubmission.
		return payout.SubmitResult{Status: payout.ProviderFailed, RawStatus: "not_found",
			FailureReason: "no transfer exists under this idempotency key"}, nil
	}
	if len(list.Data) > 1 {
		// Two objects under one key is a broken idempotency guarantee and the
		// worst possible thing to resolve automatically.
		return payout.SubmitResult{Status: payout.ProviderUnknown, RawStatus: "ambiguous",
			FailureReason: "more than one transfer carries this idempotency key"}, nil
	}
	return resultFrom(list.Data[0])
}

func (c *Client) guard(req payout.SubmitRequest) error {
	if !c.opts.Availability.Usable() {
		return errs.Newf(errs.CodeCapabilityNotApproved,
			"stripepayout: stablecoin payouts are not available on this account (availability %s); the product is in private preview and requires a granted application",
			c.availability()).
			WithField("availability", string(c.availability())).
			WithField("provider", ProviderName)
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return errs.New(errs.CodeValidationFailed, "stripepayout: an idempotency key is required")
	}
	if req.DestinationKind != payout.DestinationCryptoWallet {
		return errs.Newf(errs.CodeUnsupported,
			"stripepayout: this provider pays crypto wallets only, not %s", req.DestinationKind)
	}
	if !strings.HasPrefix(req.DestinationReference, "acct_") {
		// The destination is a connected account. An address here means the
		// caller believes it is choosing the wallet, which it is not.
		return errs.New(errs.CodeValidationFailed,
			"stripepayout: the destination is a Stripe connected account, not a wallet address; the recipient links their own wallet in the Express Dashboard")
	}
	if !strings.EqualFold(req.Currency, "USD") {
		return errs.Newf(errs.CodeUnsupported,
			"stripepayout: transfers are created in USD and converted by Stripe, not in %s", req.Currency)
	}
	if !req.Amount.IsPositive() {
		return errs.New(errs.CodeValidationFailed, "stripepayout: a payout amount must be positive")
	}
	return nil
}

func (c *Client) availability() payout.Availability {
	if c.opts.Availability == "" {
		return payout.AvailabilityUnknown
	}
	return c.opts.Availability
}

// transferObject is the subset of the Transfer object this adapter reads.
type transferObject struct {
	ID       string            `json:"id"`
	Object   string            `json:"object"`
	Amount   int64             `json:"amount"`
	Currency string            `json:"currency"`
	Created  int64             `json:"created"`
	Livemode bool              `json:"livemode"`
	Reversed bool              `json:"reversed"`
	Metadata map[string]string `json:"metadata"`
}

func (c *Client) result(raw []byte) (payout.SubmitResult, error) {
	var t transferObject
	if err := json.Unmarshal(raw, &t); err != nil {
		return payout.SubmitResult{}, errs.Wrap(err, errs.CodeProviderUnavailable,
			"stripepayout: invalid transfer response").WithField("provider_error", "malformed_response")
	}
	return resultFrom(t)
}

func resultFrom(t transferObject) (payout.SubmitResult, error) {
	if t.ID == "" {
		return payout.SubmitResult{}, errs.New(errs.CodeProviderUnavailable,
			"stripepayout: transfer response carries no id").WithField("provider_error", "malformed_response")
	}
	if t.Reversed {
		return payout.SubmitResult{
			Status: payout.ProviderFailed, ProviderReference: t.ID,
			RawStatus: "reversed", FailureReason: "the transfer was reversed",
		}, nil
	}
	// ACCEPTED, not SETTLED. A transfer moves money into the connected
	// account's balance; the payout from that balance to the user's wallet is
	// a later event with its own lifecycle. Reporting SETTLED here would tell
	// the ledger that money reached a wallet when it has reached a balance,
	// and the difference is days and a possible failure.
	return payout.SubmitResult{
		Status: payout.ProviderAccepted, ProviderReference: t.ID, RawStatus: "created",
	}, nil
}

// escapeSearch escapes a value for Stripe's search query language, where a
// single quote terminates a literal.
func escapeSearch(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `'`, `\'`)
}

func (c *Client) do(ctx context.Context, method, path string, form url.Values, idempotencyKey string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	u := *c.base
	if i := strings.IndexByte(path, '?'); i >= 0 {
		u.Path = strings.TrimRight(c.base.Path, "/") + path[:i]
		u.RawQuery = path[i+1:]
	} else {
		u.Path = strings.TrimRight(c.base.Path, "/") + path
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "stripepayout: build request")
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
		observability.LoggerFrom(ctx).WarnContext(ctx, "stripepayout: transport failure",
			"method", method, "path", path, "error", err.Error())
		// A timeout on a submission is the case PAYOUT_STATUS_UNKNOWN exists
		// for. This error must never be read as "the payout did not happen".
		return nil, errs.Wrap(payout.ErrProviderUnavailable, errs.CodeSubmissionStateUnknown,
			"stripepayout: the provider did not answer; whether the payout was accepted is unknown").
			WithField("provider_error", "timeout")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil || len(raw) > MaxResponseBytes {
		c.observe(started, false)
		return nil, errs.New(errs.CodeSubmissionStateUnknown,
			"stripepayout: the provider's answer could not be read; whether the payout was accepted is unknown").
			WithField("provider_error", "malformed_response")
	}
	ok := resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests
	c.observe(started, ok)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	return nil, c.apiError(resp, raw, method)
}

func (c *Client) observe(started time.Time, ok bool) {
	if c.opts.Health != nil {
		now := c.opts.Clock.Now()
		c.opts.Health.Observe(now, ok, now.Sub(started))
	}
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) apiError(resp *http.Response, raw []byte, method string) error {
	var eb errorBody
	_ = json.Unmarshal(raw, &eb)
	with := func(e *errs.Error) *errs.Error {
		return e.WithField("http_status", resp.StatusCode).
			WithField("provider_error_code", eb.Error.Code).
			WithField("provider_error_type", eb.Error.Type)
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return with(errs.New(errs.CodeRateLimited, "stripepayout: rate limited"))
	case resp.StatusCode >= 500:
		// A 5xx on a POST leaves the outcome genuinely unknown. Only a read
		// can be safely called a failure.
		if method == http.MethodPost {
			return with(errs.New(errs.CodeSubmissionStateUnknown,
				"stripepayout: the provider errored on a submission; whether it was accepted is unknown"))
		}
		return with(errs.New(errs.CodeProviderUnavailable, "stripepayout: provider error"))
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return with(errs.New(errs.CodeInternal, "stripepayout: provider rejected the credentials"))
	case resp.StatusCode == http.StatusNotFound:
		return with(errs.New(errs.CodeNotFound, "stripepayout: transfer not found"))
	}
	return with(errs.New(errs.CodeValidationFailed, "stripepayout: provider rejected the request"))
}
