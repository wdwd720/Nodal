package jupiter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/provider"
)

// Doer is the subset of *http.Client the Client needs.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Dependencies are injected into NewClient. Archive and Clock are
// required; everything else has a safe default.
type Dependencies struct {
	// HTTP performs requests. Defaults to an *http.Client that refuses
	// redirects (so the API key can never follow a redirect off-host) and
	// relies on per-call context timeouts.
	HTTP Doer
	// Secrets resolves Config.APIKeyRef. Required when the ref is set.
	Secrets config.Resolver
	// Clock stamps ReceivedAt/ExpiresAt and health samples. Required.
	Clock clock.Clock
	// Archive receives every request/response. Required.
	Archive RawArchive
	// Security receives 401/403 notifications. Optional.
	Security SecurityEventSink
	// Tracker receives one health sample per HTTP attempt. Optional.
	Tracker *provider.Tracker
	// Logger defaults to observability.LoggerFrom(ctx) per call.
	Logger *slog.Logger
	// Sleep waits between retries; tests inject a no-op.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter returns [0, n); defaults to a crypto/rand source.
	Jitter func(n int64) int64
}

// Client is the live Swap API V2 client. It is safe for concurrent use.
type Client struct {
	cfg       Config
	deps      Dependencies
	base      *url.URL
	apiKey    observability.Secret
	programID solana.PublicKey
}

// Compile-time check.
var _ Service = (*Client)(nil)

// NewClient validates cfg (live mode only), resolves the API key once and
// returns a Client. Key rotation follows the documented guidance (new key,
// move traffic, delete old), which means a restart with the new reference.
func NewClient(ctx context.Context, cfg Config, deps Dependencies) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Mode != config.ProviderModeLive {
		return nil, errors.New("jupiter: NewClient requires live mode; use NewFake for fake mode")
	}
	if deps.Clock == nil {
		return nil, errors.New("jupiter: a Clock is required")
	}
	if deps.Archive == nil {
		return nil, errors.New("jupiter: a RawArchive is required (evidence is mandatory)")
	}
	if deps.HTTP == nil {
		deps.HTTP = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if deps.Sleep == nil {
		deps.Sleep = sleepCtx
	}
	if deps.Jitter == nil {
		deps.Jitter = cryptoJitter
	}
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("jupiter: base URL: %w", err)
	}
	base.Path = strings.TrimRight(base.Path, "/")

	var key string
	if !cfg.APIKeyRef.IsZero() {
		if deps.Secrets == nil {
			return nil, errors.New("jupiter: a secret Resolver is required to resolve the API key")
		}
		key, err = deps.Secrets.Resolve(ctx, cfg.APIKeyRef)
		if err != nil {
			return nil, fmt.Errorf("jupiter: resolve api key: %w", err)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, errors.New("jupiter: resolved api key is empty")
		}
	}
	return &Client{cfg: cfg, deps: deps, base: base, apiKey: observability.Secret(key), programID: cfg.programID()}, nil
}

// Name implements Service.
func (c *Client) Name() string { return ProviderName }

// VerificationLabel implements Service. CODE_COMPLETE until a live key is
// available (EB-011).
func (c *Client) VerificationLabel() provider.VerificationLabel { return provider.CodeComplete }

// ValidateQuote implements Service.
func (c *Client) ValidateQuote(o Order, now time.Time, p ValidationPolicy) error {
	return ValidateQuote(o, now, p)
}

// Status implements Service: UNSUPPORTED. Swap API V2 documents no status
// endpoint; the executor observes the signature through the chain
// observers (getSignatureStatuses / getTransaction).
func (c *Client) Status(_ context.Context, signature string) (StatusResult, error) {
	return StatusResult{Signature: signature}, errs.New(errs.CodeUnsupported,
		"jupiter: Swap API V2 has no status endpoint; observe the signature through the chain observers").
		WithField(fieldOperation, string(OpStatus)).WithField(fieldSignature, signature)
}

// ---- transport -----------------------------------------------------------

type call struct {
	op      Operation
	method  string
	path    string
	query   url.Values
	body    []byte
	timeout time.Duration
}

type attempt struct {
	n            int
	status       int
	headers      http.Header
	body         []byte
	gatewayID    string
	rateLimit    RateLimitInfo
	transportErr error
	timedOut     bool
	notSent      bool
	rawRef       string
	rawHash      []byte
	archiveErr   error
	sentAt       time.Time
	receivedAt   time.Time
	latency      time.Duration
}

func (a attempt) ok() bool { return a.status >= 200 && a.status <= 299 }

// healthy is the health-sample verdict: the provider answered and the
// answer was not an availability or credential failure.
func (a attempt) healthy() bool {
	if a.transportErr != nil {
		return false
	}
	switch {
	case a.status >= 500, a.status == http.StatusTooManyRequests,
		a.status == http.StatusUnauthorized, a.status == http.StatusForbidden:
		return false
	}
	return true
}

func (a attempt) meta() responseMeta {
	return responseMeta{receivedAt: a.receivedAt, rawRef: a.rawRef, rawHash: a.rawHash, gatewayID: a.gatewayID, rateLimit: a.rateLimit}
}

// archiveTimeout bounds the evidence write after the caller's context may
// already be done: evidence must still be attempted.
const archiveTimeout = 5 * time.Second

// doOnce performs exactly one HTTP attempt: build, send, read (bounded),
// sample health, archive evidence, log. It never retries.
func (c *Client) doOnce(ctx context.Context, cl call, n int) attempt {
	a := attempt{n: n}
	u := *c.base
	u.Path += cl.path
	u.RawQuery = cl.query.Encode()

	actx, cancel := context.WithTimeout(ctx, cl.timeout)
	defer cancel()
	var bodyReader io.Reader
	if cl.body != nil {
		bodyReader = bytes.NewReader(cl.body)
	}
	req, err := http.NewRequestWithContext(actx, cl.method, u.String(), bodyReader)
	if err != nil {
		a.transportErr, a.notSent = err, true
		return a
	}
	req.Header.Set("Accept", "application/json")
	if cl.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key := c.apiKey.Reveal(); key != "" {
		req.Header.Set(HeaderAPIKey, key)
	}

	ev := Evidence{
		Provider: ProviderName, Operation: cl.op, Attempt: n,
		RequestMethod: cl.method, RequestURL: u.String(),
		RequestHeaders: redactHeaders(req.Header), RequestBody: cl.body,
	}
	a.sentAt = c.deps.Clock.Now()
	ev.SentAt = a.sentAt
	stop := clock.Start()
	resp, err := c.deps.HTTP.Do(req)
	a.latency = stop()
	a.receivedAt = c.deps.Clock.Now()
	ev.ReceivedAt = a.receivedAt

	if err != nil {
		a.transportErr, a.timedOut = err, isTimeout(err)
		ev.TransportError = observability.MaskString(err.Error())
		ev.ResponseHash = sha256Of(nil)
	} else {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseBytes+1))
		_ = resp.Body.Close()
		a.status = resp.StatusCode
		a.headers = resp.Header.Clone()
		a.gatewayID = resp.Header.Get(HeaderGatewayRequestID)
		a.rateLimit = parseRateLimit(resp.Header)
		switch {
		case readErr != nil:
			a.transportErr, a.timedOut = readErr, isTimeout(readErr)
			ev.TransportError = observability.MaskString(readErr.Error())
		case int64(len(body)) > c.cfg.MaxResponseBytes:
			a.transportErr = errors.New("jupiter: response body exceeds MaxResponseBytes")
			ev.TransportError = a.transportErr.Error()
			body = body[:c.cfg.MaxResponseBytes]
		}
		a.body = body
		ev.ResponseStatus, ev.ResponseHeaders, ev.ResponseBody = a.status, a.headers, body
		ev.GatewayRequestID = a.gatewayID
		ev.ResponseHash = sha256Of(body)
	}
	a.rawHash = ev.ResponseHash

	if c.deps.Tracker != nil {
		c.deps.Tracker.Observe(a.receivedAt, a.healthy(), a.latency)
	}
	arcCtx, arcCancel := context.WithTimeout(context.WithoutCancel(ctx), archiveTimeout)
	a.rawRef, a.archiveErr = c.deps.Archive.Archive(arcCtx, ev)
	arcCancel()
	c.logAttempt(ctx, cl.op, a)
	return a
}

func (c *Client) logAttempt(ctx context.Context, op Operation, a attempt) {
	log := c.deps.Logger
	if log == nil {
		log = observability.LoggerFrom(ctx)
	}
	attrs := []slog.Attr{
		slog.String("provider", ProviderName),
		slog.String("operation", string(op)),
		slog.Int("attempt", a.n),
		slog.Int("http_status", a.status),
		slog.String("gateway_request_id", a.gatewayID),
		slog.Int64("latency_ms", a.latency.Milliseconds()),
		slog.Bool("timed_out", a.timedOut),
		slog.String("raw_ref", a.rawRef),
	}
	if a.rateLimit.Present {
		attrs = append(attrs, slog.Int64("ratelimit_remaining", a.rateLimit.Remaining))
	}
	level := slog.LevelInfo
	if a.transportErr != nil {
		attrs = append(attrs, slog.String("transport_error", observability.MaskString(a.transportErr.Error())))
		level = slog.LevelWarn
	} else if !a.ok() {
		level = slog.LevelWarn
	}
	if a.archiveErr != nil {
		attrs = append(attrs, slog.String("archive_error", observability.MaskString(a.archiveErr.Error())))
		level = slog.LevelError
	}
	log.LogAttrs(ctx, level, "jupiter provider call", attrs...)
}

// doRead runs a SAFE_RETRY call with bounded, jittered retries on
// transport errors, 429 and 5xx. It returns the final attempt and, when the
// budget is exhausted or the context ends, the mapped error. A returned
// attempt with a nil error may still be a non-2xx (4xx) that the caller
// maps.
func (c *Client) doRead(ctx context.Context, cl call) (attempt, *errs.Error) {
	maxAttempts := 1 + c.cfg.MaxRetries
	var last attempt
	for n := 1; n <= maxAttempts; n++ {
		if err := ctx.Err(); err != nil {
			return last, withGateway(errs.Wrap(err, errs.CodeProviderUnavailable, "jupiter: context done before the request"), cl.op, 0, "")
		}
		a := c.doOnce(ctx, cl, n)
		last = a
		if a.archiveErr != nil {
			return a, withGateway(errs.Wrap(a.archiveErr, errs.CodeInternal, "jupiter: evidence archive failed"), cl.op, a.status, a.gatewayID)
		}
		var wait time.Duration
		switch {
		case a.transportErr != nil:
			if ctx.Err() != nil {
				return a, withGateway(errs.Wrap(a.transportErr, errs.CodeProviderUnavailable, "jupiter: context done during the request").
					WithField(fieldTimedOut, a.timedOut), cl.op, 0, a.gatewayID)
			}
			wait = backoffDelay(n, c.cfg.RetryBaseDelay, c.cfg.RetryMaxDelay, c.deps.Jitter)
		case a.status == http.StatusTooManyRequests:
			hint := retryAfterFrom(a.headers, a.receivedAt)
			if hint > c.cfg.MaxRetryAfterWait {
				return a, mapReadStatus(cl.op, a.status, a.body, a.gatewayID, hint, n)
			}
			wait = hint
			if wait <= 0 {
				wait = backoffDelay(n, c.cfg.RetryBaseDelay, c.cfg.RetryMaxDelay, c.deps.Jitter)
			}
		case a.status >= 500:
			wait = backoffDelay(n, c.cfg.RetryBaseDelay, c.cfg.RetryMaxDelay, c.deps.Jitter)
		default:
			return a, nil
		}
		if n == maxAttempts {
			break
		}
		if err := c.deps.Sleep(ctx, wait); err != nil {
			return a, withGateway(errs.Wrap(err, errs.CodeProviderUnavailable, "jupiter: context done while backing off"), cl.op, a.status, a.gatewayID)
		}
	}
	if last.transportErr != nil {
		return last, withGateway(errs.Wrap(last.transportErr, errs.CodeProviderUnavailable, "jupiter: provider unreachable").
			WithField(fieldTimedOut, last.timedOut).WithField(fieldAttempts, maxAttempts), cl.op, 0, last.gatewayID)
	}
	return last, mapReadStatus(cl.op, last.status, last.body, last.gatewayID, retryAfterFrom(last.headers, last.receivedAt), maxAttempts)
}

// notifySecurity emits a security event for credential/permission
// rejections.
func (c *Client) notifySecurity(ctx context.Context, op Operation, a attempt) {
	if c.deps.Security == nil || (a.status != http.StatusUnauthorized && a.status != http.StatusForbidden) {
		return
	}
	c.deps.Security.SecurityEvent(ctx, SecurityEvent{
		Kind: SecurityEventProviderAuthRejected, Provider: ProviderName, Operation: op,
		HTTPStatus: a.status, GatewayRequestID: a.gatewayID,
		Detail: "provider rejected the API credentials or permission", At: a.receivedAt,
	})
}

func sleepCtx(ctx context.Context, d time.Duration) error {
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

// ---- Order -----------------------------------------------------------------

// Order implements Service (GET /order).
func (c *Client) Order(ctx context.Context, req OrderRequest) (Order, error) {
	q, err := orderQuery(req)
	if err != nil {
		return Order{}, err
	}
	a, ferr := c.doRead(ctx, call{op: OpOrder, method: http.MethodGet, path: "/order", query: q, timeout: c.cfg.OrderTimeout})
	c.notifySecurity(ctx, OpOrder, a)
	if ferr != nil {
		return Order{}, ferr
	}
	if !a.ok() {
		return Order{}, mapReadStatus(OpOrder, a.status, a.body, a.gatewayID, retryAfterFrom(a.headers, a.receivedAt), a.n)
	}
	d, err := decodeOrderResponse(a.body)
	if err != nil {
		return Order{}, decorateDecodeError(err, OpOrder, a)
	}
	if d.errorCode != 0 {
		return Order{}, mapOrderErrorCode(d.router, d.errorCode, d.errorMessage, d.requestID, a.gatewayID)
	}
	o, err := assembleOrder(req, d, a.meta(), c.cfg.AssumedOrderTTL)
	if err != nil {
		return Order{}, decorateDecodeError(err, OpOrder, a)
	}
	return o, nil
}

func decorateDecodeError(err error, op Operation, a attempt) error {
	if e, ok := errs.As(err); ok {
		return withGateway(e, op, a.status, a.gatewayID).WithField("raw_ref", a.rawRef)
	}
	return withGateway(errs.Wrap(err, errs.CodeValidationFailed, "jupiter: response decode failed"), op, a.status, a.gatewayID)
}

// requestError is the VALIDATION_FAILED for a bad local request (nothing
// was sent).
func requestError(field, detail string) *errs.Error {
	return errs.New(errs.CodeValidationFailed, "jupiter: "+field+": "+detail).WithField("field", field).WithField(fieldSubmitted, false)
}

func validPubkey(s string) bool {
	_, err := solana.PublicKeyFromBase58(s)
	return err == nil
}

var documentedRouters = map[string]struct{}{"metis": {}, "jupiterz": {}, "dflow": {}, "okx": {}}

func csvParam(values []string, field string) (string, error) {
	for _, v := range values {
		if strings.TrimSpace(v) == "" || strings.Contains(v, ",") {
			return "", requestError(field, "entries must be non-empty and contain no commas")
		}
	}
	return strings.Join(values, ","), nil
}

// orderQuery validates an OrderRequest and renders the documented query.
func orderQuery(req OrderRequest) (url.Values, error) {
	q := url.Values{}
	if !validPubkey(req.InputMint) {
		return nil, requestError("inputMint", "not a base58 public key")
	}
	if !validPubkey(req.OutputMint) {
		return nil, requestError("outputMint", "not a base58 public key")
	}
	if req.InputMint == req.OutputMint {
		return nil, requestError("outputMint", "must differ from inputMint")
	}
	if !req.Amount.IsPositive() {
		return nil, requestError("amount", "must be positive")
	}
	q.Set("inputMint", req.InputMint)
	q.Set("outputMint", req.OutputMint)
	q.Set("amount", req.Amount.String())
	if req.TakerPubkey != "" {
		if !validPubkey(req.TakerPubkey) {
			return nil, requestError("taker", "not a base58 public key")
		}
		q.Set("taker", req.TakerPubkey)
	}
	if req.Receiver != "" {
		if !validPubkey(req.Receiver) {
			return nil, requestError("receiver", "not a base58 public key")
		}
		if req.Receiver == req.TakerPubkey {
			return nil, requestError("receiver", "must differ from taker")
		}
		q.Set("receiver", req.Receiver)
	}
	switch req.SwapMode {
	case "":
	case SwapModeExactIn:
		q.Set("swapMode", SwapModeExactIn)
	default:
		return nil, requestError("swapMode", "only ExactIn is supported")
	}
	if req.SlippageBPS != nil {
		if *req.SlippageBPS < 0 || *req.SlippageBPS > 10_000 {
			return nil, requestError("slippageBps", "must be within 0..10000")
		}
		q.Set("slippageBps", strconv.FormatInt(int64(*req.SlippageBPS), 10))
	}
	switch {
	case req.ReferralAccount != "" && req.ReferralFeeBPS == nil, req.ReferralAccount == "" && req.ReferralFeeBPS != nil:
		return nil, requestError("referralAccount", "referralAccount and referralFee must be given together")
	case req.ReferralAccount != "":
		if !validPubkey(req.ReferralAccount) {
			return nil, requestError("referralAccount", "not a base58 public key")
		}
		if *req.ReferralFeeBPS < 50 || *req.ReferralFeeBPS > 255 {
			return nil, requestError("referralFee", "must be within 50..255 bps")
		}
		q.Set("referralAccount", req.ReferralAccount)
		q.Set("referralFee", strconv.FormatInt(int64(*req.ReferralFeeBPS), 10))
	}
	if req.Payer != "" {
		if !validPubkey(req.Payer) {
			return nil, requestError("payer", "not a base58 public key")
		}
		q.Set("payer", req.Payer)
	}
	if req.PriorityFeeLamports != nil {
		if req.PriorityFeeLamports.IsNegative() {
			return nil, requestError("priorityFeeLamports", "must not be negative")
		}
		q.Set("priorityFeeLamports", req.PriorityFeeLamports.String())
	}
	if req.JitoTipLamports != nil {
		if req.JitoTipLamports.IsNegative() {
			return nil, requestError("jitoTipLamports", "must not be negative")
		}
		q.Set("jitoTipLamports", req.JitoTipLamports.String())
	}
	switch req.BroadcastFeeType {
	case "":
	case BroadcastFeeMaxCap, BroadcastFeeExactFee:
		q.Set("broadcastFeeType", req.BroadcastFeeType)
	default:
		return nil, requestError("broadcastFeeType", "must be maxCap or exactFee")
	}
	if len(req.ExcludeRouters) > 0 {
		for _, r := range req.ExcludeRouters {
			if _, ok := documentedRouters[r]; !ok {
				return nil, requestError("excludeRouters", "unknown router "+r)
			}
		}
		q.Set("excludeRouters", strings.Join(req.ExcludeRouters, ","))
	}
	if len(req.ExcludeDexes) > 0 {
		csv, err := csvParam(req.ExcludeDexes, "excludeDexes")
		if err != nil {
			return nil, err
		}
		q.Set("excludeDexes", csv)
	}
	return q, nil
}

// responseMeta is what the transport layer contributes to a result.
type responseMeta struct {
	receivedAt time.Time
	rawRef     string
	rawHash    []byte
	gatewayID  string
	rateLimit  RateLimitInfo
}

// assembleOrder turns a decoded body into an Order, cross-checking the
// response against the request and the transaction bytes. Shared by the
// live client and the Fake.
func assembleOrder(req OrderRequest, d decodedOrder, m responseMeta, assumedTTL time.Duration) (Order, error) {
	o := Order{
		QuoteID: d.requestID, Mode: d.mode, Router: d.router,
		InputMint: req.InputMint, OutputMint: req.OutputMint, Taker: req.TakerPubkey, Receiver: req.Receiver,
		InAmount: d.inAmount, OutAmount: d.outAmount, OtherAmountThreshold: d.otherAmountThreshold,
		SlippageBPS: d.slippageBPS, FeeBPS: d.feeBPS, PlatformFee: d.platformFee,
		PriceImpactBPS: d.priceImpactBPS, PriceImpactUnavailable: !d.priceImpactOK,
		Route: d.route, RoutePlanSummary: d.routeSummary, RouteHash: d.routeHash,
		LastValidBlockHeight: d.lastValidBlockHeight, LastValidBlockHeightRaw: d.lastValidBlockHeightRaw,
		SignatureFeeLamports: d.signatureFeeLamports, PrioritizationFeeLamports: d.prioritizationFeeLamports,
		RentFeeLamports: d.rentFeeLamports, Gasless: d.gasless,
		ReceivedAt: m.receivedAt, RawRef: m.rawRef, RawHash: m.rawHash, GatewayRequestID: m.gatewayID, RateLimit: m.rateLimit,
	}
	if d.priceImpactOK {
		o.PriceImpactSource = "priceImpact"
	}
	if !d.inAmount.Equal(req.Amount) {
		return Order{}, fieldError("inAmount", "does not equal the requested ExactIn amount")
	}
	if d.otherAmountThreshold.Cmp(d.outAmount) > 0 {
		return Order{}, fieldError("otherAmountThreshold", "exceeds outAmount")
	}
	if len(d.route) > 0 && (d.route[0].InputMint != req.InputMint || d.route[len(d.route)-1].OutputMint != req.OutputMint) {
		return Order{}, fieldError("routePlan", "route endpoints do not match the requested mints")
	}
	if req.TakerPubkey != "" && !d.hasTransaction {
		return Order{}, fieldError("transaction", "taker was given but no transaction was returned")
	}
	if d.hasTransaction {
		s, err := summarizeTransaction(d.transaction)
		if err != nil {
			if e, ok := errs.As(err); ok {
				return Order{}, e.WithField("field", "transaction")
			}
			return Order{}, fieldError("transaction", err.Error())
		}
		o.UnsignedTransaction = d.transaction
		o.HasTransaction = true
		o.TransactionHash = sha256Of(d.transaction)
		o.FeePayer, o.RecentBlockhash = s.feePayer, s.recentBlockhash
		o.RequiredSigners, o.ProgramIDs = s.requiredSigners, s.programIDs
		if s.hasUnitLimit {
			o.ComputeUnitLimit, o.ComputeUnitLimitSource = s.computeUnitLimit, "transaction"
		}
		if s.hasUnitPrice {
			o.ComputeUnitPriceMicroLamports = s.computeUnitPrice
		}
	}
	if d.rfqQuoteID != "" || d.maker != "" || !d.expireAt.IsZero() {
		o.RFQ = &RFQInfo{QuoteID: d.rfqQuoteID, Maker: d.maker, ExpireAt: d.expireAt}
	}
	if !d.expireAt.IsZero() {
		o.ExpiresAt = d.expireAt
	} else {
		o.ExpiresAt = m.receivedAt.Add(assumedTTL)
		o.ExpiresAtAssumed = true
	}
	return o, nil
}

// ---- Build -----------------------------------------------------------------

// Build implements Service (GET /build).
func (c *Client) Build(ctx context.Context, req BuildRequest) (BuildResult, error) {
	q, err := buildQuery(req)
	if err != nil {
		return BuildResult{}, err
	}
	a, ferr := c.doRead(ctx, call{op: OpBuild, method: http.MethodGet, path: "/build", query: q, timeout: c.cfg.BuildTimeout})
	c.notifySecurity(ctx, OpBuild, a)
	if ferr != nil {
		return BuildResult{}, ferr
	}
	if !a.ok() {
		return BuildResult{}, mapReadStatus(OpBuild, a.status, a.body, a.gatewayID, retryAfterFrom(a.headers, a.receivedAt), a.n)
	}
	d, err := decodeBuildResponse(a.body)
	if err != nil {
		return BuildResult{}, decorateDecodeError(err, OpBuild, a)
	}
	r, err := assembleBuild(req, d, a.meta(), c.cfg.AssumedOrderTTL)
	if err != nil {
		return BuildResult{}, decorateDecodeError(err, OpBuild, a)
	}
	return r, nil
}

var cuPricePercentiles = map[string]struct{}{"medium": {}, "high": {}, "veryHigh": {}}

// buildQuery validates a BuildRequest and renders the documented query.
func buildQuery(req BuildRequest) (url.Values, error) {
	q := url.Values{}
	if !validPubkey(req.InputMint) {
		return nil, requestError("inputMint", "not a base58 public key")
	}
	if !validPubkey(req.OutputMint) {
		return nil, requestError("outputMint", "not a base58 public key")
	}
	if !req.Amount.IsPositive() {
		return nil, requestError("amount", "must be positive")
	}
	if !validPubkey(req.Taker) {
		return nil, requestError("taker", "required and must be a base58 public key")
	}
	q.Set("inputMint", req.InputMint)
	q.Set("outputMint", req.OutputMint)
	q.Set("amount", req.Amount.String())
	q.Set("taker", req.Taker)
	switch {
	case req.SlippageBPS != nil && req.SlippageRTSE:
		return nil, requestError("slippageBps", "slippageBps and rtse are mutually exclusive")
	case req.SlippageRTSE:
		q.Set("slippageBps", "rtse")
	case req.SlippageBPS != nil:
		if *req.SlippageBPS < 0 || *req.SlippageBPS > 10_000 {
			return nil, requestError("slippageBps", "must be within 0..10000")
		}
		q.Set("slippageBps", strconv.FormatInt(int64(*req.SlippageBPS), 10))
	}
	if req.Mode != "" {
		if req.Mode != "fast" {
			return nil, requestError("mode", "only fast is documented")
		}
		q.Set("mode", req.Mode)
	}
	if len(req.Dexes) > 0 && len(req.ExcludeDexes) > 0 {
		return nil, requestError("dexes", "dexes and excludeDexes are mutually exclusive")
	}
	if len(req.Dexes) > 0 {
		csv, err := csvParam(req.Dexes, "dexes")
		if err != nil {
			return nil, err
		}
		q.Set("dexes", csv)
	}
	if len(req.ExcludeDexes) > 0 {
		csv, err := csvParam(req.ExcludeDexes, "excludeDexes")
		if err != nil {
			return nil, err
		}
		q.Set("excludeDexes", csv)
	}
	if req.PlatformFeeBPS != nil {
		if *req.PlatformFeeBPS < 0 || *req.PlatformFeeBPS > 10_000 {
			return nil, requestError("platformFeeBps", "must be within 0..10000")
		}
		if !validPubkey(req.FeeAccount) {
			return nil, requestError("feeAccount", "required with platformFeeBps and must be a base58 public key")
		}
		q.Set("platformFeeBps", strconv.FormatInt(int64(*req.PlatformFeeBPS), 10))
		q.Set("feeAccount", req.FeeAccount)
	} else if req.FeeAccount != "" {
		return nil, requestError("feeAccount", "requires platformFeeBps")
	}
	if req.MaxAccounts != 0 {
		if req.MaxAccounts < 1 || req.MaxAccounts > 64 {
			return nil, requestError("maxAccounts", "must be within 1..64")
		}
		q.Set("maxAccounts", strconv.Itoa(req.MaxAccounts))
	}
	if req.Payer != "" {
		if !validPubkey(req.Payer) {
			return nil, requestError("payer", "not a base58 public key")
		}
		q.Set("payer", req.Payer)
	}
	if req.WrapAndUnwrapSOL != nil {
		q.Set("wrapAndUnwrapSol", strconv.FormatBool(*req.WrapAndUnwrapSOL))
	}
	if req.DestinationTokenAccount != "" && req.NativeDestinationAccount != "" {
		return nil, requestError("destinationTokenAccount", "destinationTokenAccount and nativeDestinationAccount are mutually exclusive")
	}
	if req.DestinationTokenAccount != "" {
		if !validPubkey(req.DestinationTokenAccount) {
			return nil, requestError("destinationTokenAccount", "not a base58 public key")
		}
		q.Set("destinationTokenAccount", req.DestinationTokenAccount)
	}
	if req.NativeDestinationAccount != "" {
		if !validPubkey(req.NativeDestinationAccount) {
			return nil, requestError("nativeDestinationAccount", "not a base58 public key")
		}
		q.Set("nativeDestinationAccount", req.NativeDestinationAccount)
	}
	if req.BlockhashSlotsToExpiry != 0 {
		if req.BlockhashSlotsToExpiry < 1 || req.BlockhashSlotsToExpiry > 300 {
			return nil, requestError("blockhashSlotsToExpiry", "must be within 1..300")
		}
		q.Set("blockhashSlotsToExpiry", strconv.Itoa(req.BlockhashSlotsToExpiry))
	}
	if req.TipAmount != nil {
		if req.TipAmount.IsNegative() {
			return nil, requestError("tipAmount", "must not be negative")
		}
		q.Set("tipAmount", req.TipAmount.String())
	}
	if p := req.ComputeUnitPricePercentile; p != "" {
		if _, ok := cuPricePercentiles[p]; !ok {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 || n > 10_000 {
				return nil, requestError("computeUnitPricePercentile", "must be medium|high|veryHigh or 0..10000")
			}
		}
		q.Set("computeUnitPricePercentile", p)
	}
	if req.ForJitoBundle {
		q.Set("forJitoBundle", "true")
	}
	return q, nil
}

// assembleBuild turns a decoded /build body into a BuildResult.
func assembleBuild(req BuildRequest, d decodedBuild, m responseMeta, assumedTTL time.Duration) (BuildResult, error) {
	if !d.inAmount.Equal(req.Amount) {
		return BuildResult{}, fieldError("inAmount", "does not equal the requested amount")
	}
	if d.otherAmountThreshold.Cmp(d.outAmount) > 0 {
		return BuildResult{}, fieldError("otherAmountThreshold", "exceeds outAmount")
	}
	if len(d.route) > 0 && (d.route[0].InputMint != req.InputMint || d.route[len(d.route)-1].OutputMint != req.OutputMint) {
		return BuildResult{}, fieldError("routePlan", "route endpoints do not match the requested mints")
	}
	return BuildResult{
		InputMint: req.InputMint, OutputMint: req.OutputMint, Taker: req.Taker,
		InAmount: d.inAmount, OutAmount: d.outAmount, OtherAmountThreshold: d.otherAmountThreshold,
		SlippageBPS: d.slippageBPS, PriceImpactBPS: d.priceImpactBPS, PriceImpactUnavailable: !d.priceImpactOK,
		Route: d.route, RoutePlanSummary: d.routeSummary, RouteHash: d.routeHash,
		ComputeBudgetInstructions: d.computeBudget, SetupInstructions: d.setup, SwapInstruction: d.swap,
		CleanupInstruction: d.cleanup, OtherInstructions: d.other, TipInstruction: d.tip,
		AddressLookupTables: d.lookupTables, ComputeUnitLimitUnavailable: true,
		Blockhash: d.blockhash, LastValidBlockHeight: d.lastValidBlockHeight, BlockhashFetchedAt: d.fetchedAt,
		ReceivedAt: m.receivedAt, ExpiresAt: m.receivedAt.Add(assumedTTL), ExpiresAtAssumed: true,
		RawRef: m.rawRef, RawHash: m.rawHash, GatewayRequestID: m.gatewayID, RateLimit: m.rateLimit,
	}, nil
}

// ---- Execute ---------------------------------------------------------------

// executeBody is the documented POST /execute body. lastValidBlockHeight is
// sent as the /order string unchanged (OpenAPI: string) when present.
type executeBody struct {
	SignedTransaction    string `json:"signedTransaction"`
	RequestID            string `json:"requestId"`
	LastValidBlockHeight string `json:"lastValidBlockHeight,omitempty"`
}

// Execute implements Service (POST /execute). Exactly one HTTP attempt is
// ever made. Local validation failures and a context that is already done
// are the only errors that carry Fields["submitted"] == false; everything
// after the request leaves this process is SUBMISSION_STATE_UNKNOWN unless
// the gateway provably rejected it before broadcast (see mapExecuteStatus).
func (c *Client) Execute(ctx context.Context, req ExecuteRequest) (ExecuteResult, error) {
	if strings.TrimSpace(req.RequestID) == "" {
		return ExecuteResult{}, requestError("requestId", "required (Order.QuoteID)")
	}
	sig, err := signatureOfSigned(req.SignedTransaction)
	if err != nil {
		if e, ok := errs.As(err); ok {
			return ExecuteResult{}, e.WithField("field", "signedTransaction").WithField(fieldSubmitted, false)
		}
		return ExecuteResult{}, requestError("signedTransaction", err.Error())
	}
	if req.LastValidBlockHeight != "" && (!isIntegerToken(req.LastValidBlockHeight) || strings.HasPrefix(req.LastValidBlockHeight, "-")) {
		return ExecuteResult{}, requestError("lastValidBlockHeight", "must be the unsigned integer string from the order")
	}
	if err := ctx.Err(); err != nil {
		return ExecuteResult{}, withGateway(errs.Wrap(err, errs.CodeProviderUnavailable, "jupiter: context done before submission").
			WithField(fieldSubmitted, false).WithField(fieldSignature, sig), OpExecute, 0, "")
	}
	body, err := json.Marshal(executeBody{
		SignedTransaction:    base64.StdEncoding.EncodeToString(req.SignedTransaction),
		RequestID:            req.RequestID,
		LastValidBlockHeight: req.LastValidBlockHeight,
	})
	if err != nil {
		return ExecuteResult{}, requestError("body", "could not encode request")
	}

	a := c.doOnce(ctx, call{op: OpExecute, method: http.MethodPost, path: "/execute", body: body, timeout: c.cfg.ExecuteTimeout}, 1)
	res := ExecuteResult{
		Signature: sig, SubmittedAt: a.sentAt, ReceivedAt: a.receivedAt,
		RawRef: a.rawRef, RawHash: a.rawHash, GatewayRequestID: a.gatewayID, RateLimit: a.rateLimit,
	}
	if a.notSent {
		return res, withGateway(errs.Wrap(a.transportErr, errs.CodeProviderUnavailable, "jupiter: request could not be built; nothing was sent").
			WithField(fieldSubmitted, false), OpExecute, 0, "")
	}
	if a.transportErr != nil {
		return res, submissionUnknown(a.transportErr, "jupiter: no response to the submission; landing state unknown", sig, a.gatewayID, a.timedOut)
	}
	if a.archiveErr != nil {
		return res, submissionUnknown(a.archiveErr, "jupiter: evidence archive failed after submission; landing state unknown", sig, a.gatewayID, false)
	}
	c.notifySecurity(ctx, OpExecute, a)
	if !a.ok() {
		return res, mapExecuteStatus(a.status, a.body, a.gatewayID, sig, retryAfterFrom(a.headers, a.receivedAt))
	}
	d, err := decodeExecuteResponse(a.body)
	if err != nil {
		return res, submissionUnknown(err, "jupiter: submission response could not be decoded; landing state unknown", sig, a.gatewayID, false)
	}
	switch ExecuteStatus(d.status) {
	case ExecuteSuccess, ExecuteFailed:
	default:
		return res, submissionUnknown(errors.New("status "+d.status), "jupiter: submission response carries an unexpected status; landing state unknown", sig, a.gatewayID, false).
			WithField("provider_status", d.status)
	}
	if d.signature == "" && ExecuteStatus(d.status) == ExecuteSuccess {
		return res, submissionUnknown(errors.New("missing signature"), "jupiter: success response without a signature; landing state unknown", sig, a.gatewayID, false)
	}
	if d.signature != "" && d.signature != sig {
		return res, submissionUnknown(errors.New("signature mismatch"), "jupiter: provider signature differs from the signed transaction; landing state unknown", sig, a.gatewayID, false).
			WithField("provider_signature", d.signature)
	}
	res.Status = ExecuteStatus(d.status)
	res.Slot = d.slot
	res.ProviderCode, res.ProviderError = d.code, d.errText
	res.TotalInputAmount, res.TotalOutputAmount = d.totalInputAmount, d.totalOutputAmount
	res.InputAmountResult, res.OutputAmountResult = d.inputAmountResult, d.outputAmountResult
	res.SwapEvents = d.swapEvents
	return res, nil
}
