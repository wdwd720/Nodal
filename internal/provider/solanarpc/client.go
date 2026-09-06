package solanarpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/provider"
)

// Defaults.
const (
	// DefaultName is the observer name when the config carries none; it is
	// the source label persisted in wallet_balance_observations.
	DefaultName = "rpc-fallback"
	// DefaultDevnetURL is the public devnet endpoint used in sandbox mode.
	DefaultDevnetURL = "https://api.devnet.solana.com"
	// DefaultTimeout bounds one HTTP attempt.
	DefaultTimeout = 10 * time.Second
	// DefaultMaxAttempts is the SAFE_RETRY budget per call.
	DefaultMaxAttempts = 3
	// DefaultRetryBaseDelay / DefaultRetryMaxDelay bound the jittered backoff.
	DefaultRetryBaseDelay = 100 * time.Millisecond
	DefaultRetryMaxDelay  = 2 * time.Second
	// DefaultMaxRetryAfterWait caps how long a Retry-After header is honored.
	DefaultMaxRetryAfterWait = 5 * time.Second
	// DefaultMaxActivityTransactions bounds SearchWalletActivity fetches.
	DefaultMaxActivityTransactions = 100
	// DefaultActivityConcurrency bounds concurrent getTransaction fetches.
	DefaultActivityConcurrency = 4
	// MaxSignatureStatuses is the documented per-call limit.
	MaxSignatureStatuses = 256
	// MaxSignaturesPage is the documented getSignaturesForAddress limit.
	MaxSignaturesPage = 1000
	// maxActivityPages bounds paging per address.
	maxActivityPages = 10
	// schemaVersion of the archived raw payloads.
	schemaVersion = 1
)

// RetryPolicy configures SAFE_RETRY reads.
type RetryPolicy struct {
	MaxAttempts       int
	BaseDelay         time.Duration
	MaxDelay          time.Duration
	MaxRetryAfterWait time.Duration
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = DefaultMaxAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = DefaultRetryBaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = DefaultRetryMaxDelay
	}
	if p.MaxRetryAfterWait <= 0 {
		p.MaxRetryAfterWait = DefaultMaxRetryAfterWait
	}
	return p
}

// Options are adapter tunables beyond config.ProviderConfig.
type Options struct {
	// Name overrides cfg.Name (default DefaultName).
	Name string
	// Commitment for reads (default confirmed; processed is rejected because
	// getTransaction and getSignaturesForAddress do not accept it).
	Commitment chain.Commitment
	Retry      RetryPolicy
	// AuthHeader / AuthQueryParam say where the resolved API key goes. When
	// cfg.APIKeyRef is set exactly one must be given; the adapter never
	// guesses a vendor's auth scheme.
	AuthHeader     string
	AuthQueryParam string
	// MaxActivityTransactions / ActivityConcurrency bound SearchWalletActivity.
	MaxActivityTransactions int
	ActivityConcurrency     int
	// SignaturesPageSize is the getSignaturesForAddress page size (default
	// and maximum MaxSignaturesPage); smaller pages exercise paging.
	SignaturesPageSize int
}

// Deps are the injected collaborators.
type Deps struct {
	HTTPClient *http.Client
	Resolver   config.Resolver
	Archive    chain.RawArchive // required
	Tracker    *provider.Tracker
	Thresholds *provider.Thresholds
	Clock      clock.Clock
	Logger     *slog.Logger
}

// Client is a Solana JSON-RPC ChainObserver.
type Client struct {
	name       string
	endpoint   observability.Secret // full URL, may embed a key
	redacted   string               // scheme://host/path for logs
	authHeader string
	authValue  observability.Secret
	commitment chain.Commitment
	timeout    time.Duration
	retry      RetryPolicy
	maxTx      int
	conc       int
	pageSize   int

	http    *http.Client
	archive chain.RawArchive
	tracker *provider.Tracker
	clk     clock.Clock
	log     *slog.Logger
	nextID  atomic.Uint64
}

var _ chain.ChainObserver = (*Client)(nil)

// New builds a client from the provider config. Fake mode is refused: the
// composition root wires chaintest fakes itself so no production path can
// construct one. Sandbox mode defaults to devnet; live mode requires an
// explicit BaseURL (the public mainnet endpoint is not for production).
func New(ctx context.Context, cfg config.ProviderConfig, opts Options, deps Deps) (*Client, error) {
	if deps.Archive == nil {
		return nil, errors.New("solanarpc: raw archive is required")
	}
	var base string
	switch cfg.Mode {
	case config.ProviderModeFake:
		return nil, errs.New(errs.CodeUnsupported, "solanarpc: fake mode is wired through chain/chaintest by the composition root")
	case config.ProviderModeSandbox:
		base = cfg.BaseURL
		if base == "" {
			base = DefaultDevnetURL
		}
	case config.ProviderModeLive:
		if cfg.BaseURL == "" {
			return nil, errors.New("solanarpc: live mode requires an explicit BaseURL")
		}
		base = cfg.BaseURL
	default:
		return nil, fmt.Errorf("solanarpc: unknown provider mode %q", cfg.Mode)
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("solanarpc: BaseURL must be an absolute http(s) URL")
	}
	if cfg.Mode == config.ProviderModeLive && u.Scheme != "https" {
		return nil, errors.New("solanarpc: live mode requires https")
	}
	name := opts.Name
	if name == "" {
		name = cfg.Name
	}
	if name == "" {
		name = DefaultName
	}
	c := &Client{
		name:       name,
		commitment: opts.Commitment,
		timeout:    cfg.Timeout,
		retry:      opts.Retry.withDefaults(),
		maxTx:      opts.MaxActivityTransactions,
		conc:       opts.ActivityConcurrency,
		pageSize:   opts.SignaturesPageSize,
		http:       deps.HTTPClient,
		archive:    deps.Archive,
		tracker:    deps.Tracker,
		clk:        deps.Clock,
		log:        deps.Logger,
	}
	if c.commitment == "" {
		c.commitment = chain.CommitmentConfirmed
	}
	if !c.commitment.Valid() || c.commitment == chain.CommitmentProcessed {
		return nil, errors.New("solanarpc: commitment must be confirmed or finalized")
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	if c.maxTx <= 0 {
		c.maxTx = DefaultMaxActivityTransactions
	}
	if c.conc <= 0 {
		c.conc = DefaultActivityConcurrency
	}
	if c.pageSize <= 0 || c.pageSize > MaxSignaturesPage {
		c.pageSize = MaxSignaturesPage
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	if c.clk == nil {
		c.clk = clock.System()
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.tracker == nil {
		th := provider.DefaultThresholds()
		if deps.Thresholds != nil {
			th = *deps.Thresholds
		}
		c.tracker, err = provider.NewTracker(name, th, c.clk.Now())
		if err != nil {
			return nil, err
		}
	}
	if !cfg.APIKeyRef.IsZero() {
		if deps.Resolver == nil {
			return nil, errors.New("solanarpc: APIKeyRef set but no secret resolver")
		}
		if (opts.AuthHeader == "") == (opts.AuthQueryParam == "") {
			return nil, errors.New("solanarpc: exactly one of AuthHeader or AuthQueryParam is required with APIKeyRef")
		}
		key, err := deps.Resolver.Resolve(ctx, cfg.APIKeyRef)
		if err != nil {
			return nil, fmt.Errorf("solanarpc: resolve api key: %w", err)
		}
		if strings.TrimSpace(key) == "" {
			return nil, errors.New("solanarpc: resolved api key is empty")
		}
		if opts.AuthQueryParam != "" {
			q := u.Query()
			q.Set(opts.AuthQueryParam, key)
			u.RawQuery = q.Encode()
		} else {
			c.authHeader = opts.AuthHeader
			c.authValue = observability.Secret(key)
		}
	}
	c.endpoint = observability.Secret(u.String())
	c.redacted = RedactURL(u.String())
	c.log = c.log.With(slog.String("provider", name), slog.String("endpoint", c.redacted))
	return c, nil
}

// RedactURL strips credentials (userinfo and the whole query) from a URL for
// logs and errors; unparsable input yields "<redacted>".
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<redacted>"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// Name implements chain.ChainObserver.
func (c *Client) Name() string { return c.name }

// Health implements chain.ChainObserver.
func (c *Client) Health() provider.Health { return c.tracker.Health() }

// Tracker exposes the health tracker for registries and periodic ticks.
func (c *Client) Tracker() *provider.Tracker { return c.tracker }

// Commitment is the read commitment.
func (c *Client) Commitment() chain.Commitment { return c.commitment }

// VerificationLabel is the honest integration status (PART 208).
func (c *Client) VerificationLabel() provider.VerificationLabel { return provider.CodeComplete }

// RedactedEndpoint is the endpoint without credentials.
func (c *Client) RedactedEndpoint() string { return c.redacted }

// CallResult is the provenance of one successful RPC call.
type CallResult struct {
	Method     string
	Raw        []byte // the JSON "result" value
	RawRef     string // archive reference of the full response body
	Attempts   int
	ObservedAt time.Time
	ReceivedAt time.Time
}

// Call performs one JSON-RPC read with SAFE_RETRY semantics: transport
// failures, HTTP 429/5xx and node-unavailable RPC errors are retried with
// full jitter until the retry budget or ctx is exhausted. Every attempt is a
// health sample; every response body is archived before parsing. eventID is
// the source event id recorded with the archive object (signature, wallet…).
func (c *Client) Call(ctx context.Context, method string, params any, eventID string) (CallResult, error) {
	if ctx == nil {
		return CallResult{}, errs.New(errs.CodeValidationFailed, "nil context")
	}
	res := CallResult{Method: method, ObservedAt: c.clk.Now()}
	var last error
	for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
		res.Attempts = attempt
		raw, ref, retryAfter, err := c.attempt(ctx, method, params, eventID)
		if err == nil {
			res.Raw, res.RawRef, res.ReceivedAt = raw, ref, c.clk.Now()
			return res, nil
		}
		last = err
		if ctx.Err() != nil || !retryable(err) || attempt == c.retry.MaxAttempts {
			break
		}
		delay := backoffDelay(attempt, c.retry.BaseDelay, c.retry.MaxDelay)
		if retryAfter > 0 {
			if retryAfter > c.retry.MaxRetryAfterWait {
				break
			}
			delay = retryAfter
		}
		c.log.DebugContext(ctx, "rpc retry", slog.String("method", method), slog.Int("attempt", attempt), slog.Duration("delay", delay), slog.String("code", string(errs.CodeOf(err))))
		if err := sleep(ctx, delay); err != nil {
			last = errs.Wrap(err, errs.CodeProviderUnavailable, "context done while waiting to retry "+method)
			break
		}
	}
	if e, ok := errs.As(last); ok {
		return res, e.WithField("attempts", res.Attempts).WithField("method", method)
	}
	return res, errs.Wrap(last, errs.CodeProviderUnavailable, method+" failed").WithField("attempts", res.Attempts)
}

// attempt runs one HTTP round trip. It returns the result bytes, the archive
// ref, a Retry-After hint (429 only) and a classified error.
func (c *Client) attempt(ctx context.Context, method string, params any, eventID string) (raw []byte, ref string, retryAfter time.Duration, err error) {
	id := c.nextID.Add(1)
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, "", 0, errs.Wrap(err, errs.CodeValidationFailed, "encode rpc request")
	}
	actx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodPost, c.endpoint.Reveal(), bytes.NewReader(body))
	if err != nil {
		return nil, "", 0, errs.New(errs.CodeValidationFailed, "build rpc request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.authHeader != "" {
		req.Header.Set(c.authHeader, c.authValue.Reveal())
	}
	sw := clock.Start()
	resp, err := c.http.Do(req)
	if err != nil {
		lat := sw.Elapsed()
		c.sample(false, lat)
		return nil, "", 0, c.transportError(ctx, method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	lat := sw.Elapsed()
	if err != nil {
		c.sample(false, lat)
		return nil, "", 0, c.transportError(ctx, method, err)
	}
	if len(respBody) > maxBodyBytes {
		c.sample(false, lat)
		return nil, "", 0, errs.New(errs.CodeValidationFailed, method+": response exceeds size limit")
	}
	received := c.clk.Now()
	ref, err = c.archive.Store(ctx, chain.RawObject{
		Provider: c.name, EventType: method, SourceEventID: eventID,
		DedupKey: dedupKey(method, body, respBody), ContentType: contentTypeOf(resp), Body: respBody,
		SchemaVersion: schemaVersion, PlatformReceivedAt: received,
	})
	if err != nil {
		c.sample(false, lat)
		return nil, "", 0, errs.Wrap(err, errs.CodeProviderUnavailable, method+": raw archive unavailable; observation refused")
	}
	if resp.StatusCode != http.StatusOK {
		c.sample(false, lat)
		return nil, ref, retryAfterOf(resp.Header, c.clk.Now()), c.statusError(method, resp.StatusCode)
	}
	var env rpcResponse
	if err := DecodeStrict(respBody, &env); err != nil {
		c.sample(false, lat)
		return nil, ref, 0, err
	}
	if env.JSONRPC != "2.0" {
		c.sample(false, lat)
		return nil, ref, 0, Malformed("jsonrpc version %q", env.JSONRPC)
	}
	if string(bytes.TrimSpace(env.ID)) != strconv.FormatUint(id, 10) {
		c.sample(false, lat)
		return nil, ref, 0, Malformed("response id does not match request")
	}
	if env.Error != nil {
		c.sample(false, lat)
		return nil, ref, 0, rpcErrorToErrs(method, env.Error)
	}
	c.sample(true, lat)
	return env.Result, ref, 0, nil
}

func (c *Client) sample(ok bool, lat time.Duration) {
	c.tracker.Observe(c.clk.Now(), ok, lat)
}

// transportError classifies a client-side failure without ever including
// the request URL (url.Error embeds it, query string and all).
func (c *Client) transportError(ctx context.Context, method string, err error) error {
	cause := err
	var uerr *url.Error
	if errors.As(err, &uerr) {
		cause = uerr.Err
	}
	detail := "transport failure"
	switch {
	case ctx.Err() != nil:
		detail = "request cancelled"
	case errors.Is(cause, context.DeadlineExceeded):
		detail = "request timed out after " + c.timeout.String()
	}
	c.log.Warn("rpc transport failure", slog.String("method", method), slog.String("detail", detail))
	return errs.Wrap(sanitizedCause(cause), errs.CodeProviderUnavailable, method+": "+detail).WithField("retryable", ctx.Err() == nil)
}

// sanitizedCause keeps the sentinel identity (context errors, net errors)
// while making sure the rendered text carries no URL.
func sanitizedCause(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	msg := err.Error()
	if strings.Contains(msg, "://") || strings.Contains(msg, "api-key") || strings.Contains(msg, "?") {
		return errors.New("transport error (details redacted)")
	}
	return err
}

func (c *Client) statusError(method string, status int) error {
	switch {
	case status == http.StatusTooManyRequests:
		return errs.New(errs.CodeRateLimited, method+": provider rate limit").WithField("http_status", status).WithField("retryable", true)
	case status >= 500:
		return errs.New(errs.CodeProviderUnavailable, method+": provider error").WithField("http_status", status).WithField("retryable", true)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return errs.New(errs.CodeProviderUnavailable, method+": provider rejected credentials").WithField("http_status", status).WithField("retryable", false)
	default:
		return errs.New(errs.CodeProviderUnavailable, method+": unexpected http status").WithField("http_status", status).WithField("retryable", false)
	}
}

// rpcErrorToErrs maps a JSON-RPC error. Request-side codes (-32600 invalid
// request, -32601 method not found, -32602 invalid params, -32700 parse
// error) are VALIDATION_FAILED and never retried; -32004 (block not
// available) and -32005 (node unhealthy) are ASSUMED retryable
// (solana-rpc.md: numeric codes unverified); everything else is
// PROVIDER_UNAVAILABLE without retry.
func rpcErrorToErrs(method string, e *rpcError) error {
	msg := e.Message
	if len(msg) > 200 {
		msg = msg[:200]
	}
	switch e.Code {
	case -32600, -32601, -32602, -32700:
		return errs.New(errs.CodeValidationFailed, method+": rpc rejected request: "+msg).WithField("rpc_code", e.Code).WithField("retryable", false)
	case -32004, -32005:
		return errs.New(errs.CodeProviderUnavailable, method+": node unavailable: "+msg).WithField("rpc_code", e.Code).WithField("retryable", true)
	default:
		return errs.New(errs.CodeProviderUnavailable, method+": rpc error: "+msg).WithField("rpc_code", e.Code).WithField("retryable", false)
	}
}

// retryable reads the classification attached by the constructors above.
func retryable(err error) bool {
	e, ok := errs.As(err)
	if !ok {
		return false
	}
	v, ok := e.Fields["retryable"].(bool)
	return ok && v
}

// retryAfterOf parses Retry-After (seconds or HTTP date) for 429 responses.
func retryAfterOf(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// JitteredBackoff is full-jitter exponential backoff for attempt (1-based):
// uniform in [0, min(maxDelay, base·2^(attempt−1))]. Shared with the Helius
// stream reconnect loop.
func JitteredBackoff(attempt int, base, maxDelay time.Duration) time.Duration {
	return backoffDelay(attempt, base, maxDelay)
}

// backoffDelay is full-jitter exponential backoff: uniform in
// [0, min(maxDelay, base·2^(attempt−1))].
func backoffDelay(attempt int, base, maxDelay time.Duration) time.Duration {
	d := base
	for i := 1; i < attempt && d < maxDelay; i++ {
		d *= 2
	}
	if d > maxDelay {
		d = maxDelay
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(cryptoJitter(int64(d)))
}

// cryptoJitter returns a uniform value in [0, n) from crypto/rand; the
// stakes are low but it avoids a global math/rand dependency.
func cryptoJitter(n int64) int64 {
	if n <= 0 {
		return 0
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return n / 2
	}
	v := int64(binary.LittleEndian.Uint64(b[:]) & (1<<63 - 1))
	return v % n
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func dedupKey(method string, req, resp []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write(req)
	h.Write([]byte{0})
	h.Write(resp)
	return hex.EncodeToString(h.Sum(nil))
}

func contentTypeOf(resp *http.Response) string {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		return ct
	}
	return "application/json"
}
