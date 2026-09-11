package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/ratelimit"
	"github.com/nodal/controlplane/internal/security"
)

type ctxKey int

const (
	ctxKeyRequest ctxKey = iota + 1
	ctxKeyBody
)

// withRequest stores the request itself so the few handlers that must reach
// the transport (the SSE stream, the OIDC redirects and cookie writes, the
// webhook raw body) can do so without the generated interface handing them a
// ResponseWriter. The value never outlives the request.
func withRequest(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, ctxKeyRequest, r)
}

func requestFrom(ctx context.Context) (*http.Request, bool) {
	r, ok := ctx.Value(ctxKeyRequest).(*http.Request)
	return r, ok
}

func withBody(ctx context.Context, body []byte) context.Context {
	return context.WithValue(ctx, ctxKeyBody, body)
}

// rawBody returns the exact bytes the client sent. Provider signatures are
// verified over these bytes; a re-encoded document would not verify.
func rawBody(ctx context.Context) []byte {
	b, _ := ctx.Value(ctxKeyBody).([]byte)
	return b
}

// captureBody buffers the request body (bounded by maxBytes), puts the bytes
// and the request in the context, and restores a reader so the generated
// server can still decode. An oversized body is refused with 413 before any
// handler runs.
// Route body limits.
//
// # Why these are per route and not one number
//
// The body is read into memory, in full, so that the webhook signature check
// can see the exact bytes the provider signed. On a single 512 MB instance that
// makes CP_HTTP_MAX_BODY_BYTES an allocation an unauthenticated caller controls,
// and one number has to be large enough for the most demanding route -- so
// every other route inherited it (F-85).
//
// The largest field any schema in the published contract declares is
// largestSchemaFieldChars, so 64 KiB is generous for an ordinary command --
// eight times the largest single field, which leaves room for the rest of the
// document and for multi-byte characters in it. Provider deliveries get more,
// because they are somebody else's payload and the webhook package documents
// 256 KiB for exactly that reason.
//
// The configured maximum is a CEILING, never a floor: an operator may lower it
// and may not raise a route past its own limit.
const (
	defaultMaxBodyBytes = 64 << 10
	webhookMaxBodyBytes = 256 << 10
	// largestSchemaFieldChars is the largest `maxLength` in openapi.yaml:
	// CreateStrategyRequest.description. This comment used to say 5,000, which
	// was the largest when it was written and stopped being true when the
	// strategy schemas landed -- so the sentence justifying the body limit was
	// justifying it with the wrong number (F-173). It is derived rather than
	// restated: TestBodyLimit_IsDerivedFromTheLargestFieldTheContractDeclares
	// reads the spec and fails when a larger field is added.
	largestSchemaFieldChars = 8_000
)

// bodyLimitFor returns the smaller of the route's limit and the configured
// maximum. Matching is on the request path rather than the chi route pattern,
// because middleware runs before routing and the pattern is not known yet --
// which is a real limitation and the reason the table is prefixes rather than
// operation ids.
func bodyLimitFor(path string, configured int64) int64 {
	limit := int64(defaultMaxBodyBytes)
	if strings.HasPrefix(path, "/v1/webhooks/") {
		limit = webhookMaxBodyBytes
	}
	if configured > 0 && configured < limit {
		return configured
	}
	return limit
}

func captureBody(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if r.Body != nil && r.ContentLength != 0 {
				limit := bodyLimitFor(r.URL.Path, maxBytes)
				// A declared length over the limit is refused without reading
				// a byte. MaxBytesReader alone would read up to the limit
				// first, so a caller announcing 100 MB still cost the limit in
				// allocation and the whole body in bandwidth.
				//
				// A chunked request declares -1 and is bounded by the reader
				// below instead, which is the best available: its size is not
				// knowable until it has been read.
				if r.ContentLength > limit {
					writeProblem(w, r, errs.Newf(errs.CodeBodyTooLarge,
						"request body is %d bytes and this route accepts %d", r.ContentLength, limit))
					return
				}
				limited := http.MaxBytesReader(w, r.Body, limit)
				buf, err := io.ReadAll(limited)
				if err != nil {
					var tooLarge *http.MaxBytesError
					if errors.As(err, &tooLarge) {
						writeProblem(w, r, errs.Newf(errs.CodeBodyTooLarge,
							"request body is larger than the %d bytes this route accepts", limit))
						return
					}
					writeProblem(w, r, errs.Wrap(err, errs.CodeValidationFailed, "the request body could not be read"))
					return
				}
				_ = r.Body.Close()
				r.Body = io.NopCloser(bytes.NewReader(buf))
				ctx = withBody(ctx, buf)
			}
			r = r.WithContext(ctx)
			// The request must be stored after its own context is final so
			// handlers see the same value the server sees.
			r = r.WithContext(withRequest(r.Context(), r))
			next.ServeHTTP(w, r)
		})
	}
}

// statusWriter records the status code and whether the response has started,
// so recovery never tries to write a problem over a partially sent body (an
// SSE stream, for instance).
type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
	bytes   int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.written {
		return
	}
	w.status = code
	w.written = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer so SSE keeps working through the
// wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.written {
			w.WriteHeader(http.StatusOK)
		}
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// recoverer turns a panic into an INTERNAL problem+json response. The stack
// is logged, never sent. http.ErrAbortHandler keeps its documented meaning.
func recoverer(log *slog.Logger, m *httpMetrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw, ok := w.(*statusWriter)
			if !ok {
				sw = &statusWriter{ResponseWriter: w}
				w = sw
			}
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if err, isErr := rec.(error); isErr && errors.Is(err, http.ErrAbortHandler) {
					panic(rec) //nolint:forbidigo // documented net/http contract
				}
				if m != nil {
					m.panics.Add(r.Context(), 1, observability.WithSafeAttrs(
						attribute.String("route", routePattern(r)),
					))
				}
				observability.LoggerFrom(r.Context()).LogAttrs(r.Context(), slog.LevelError,
					"panic recovered in http handler",
					slog.String("route", routePattern(r)),
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
				)
				if sw.written {
					// The response already started; the client sees a
					// truncated body, which is the honest outcome.
					return
				}
				writeProblem(sw, r, errs.New(errs.CodeInternal, "internal error"))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// httpMetrics are the transport-level instruments. Attributes stay low
// cardinality: the chi route template and the status class, never an id.
type httpMetrics struct {
	requests metric.Int64Counter
	duration metric.Int64Histogram
	panics   metric.Int64Counter
}

func newHTTPMetrics(meter metric.Meter) (*httpMetrics, error) {
	if meter == nil {
		return nil, nil //nolint:nilnil // no meter configured is not an error
	}
	reqs, err := meter.Int64Counter("http_server_requests",
		metric.WithDescription("HTTP requests served by route and status class"),
		metric.WithUnit(observability.UnitCount))
	if err != nil {
		return nil, err
	}
	dur, err := meter.Int64Histogram("http_server_duration",
		metric.WithDescription("HTTP request duration"),
		metric.WithUnit(observability.UnitMilliseconds))
	if err != nil {
		return nil, err
	}
	panics, err := meter.Int64Counter("http_server_panics",
		metric.WithDescription("Panics recovered in HTTP handlers"),
		metric.WithUnit(observability.UnitCount))
	if err != nil {
		return nil, err
	}
	return &httpMetrics{requests: reqs, duration: dur, panics: panics}, nil
}

func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
		return rc.RoutePattern()
	}
	return "unmatched"
}

// knownMethod folds anything outside the HTTP method set into _OTHER.
//
// r.Method is the raw request-line token, and Go accepts any RFC 7230 token as
// one -- so this label took a value the caller chose. chi runs middleware
// before routing, so even a 405 recorded it. `route` was already safe (the chi
// template, or the constant "unmatched") and `status_class` has four values;
// this was the one dimension an attacker could enumerate (F-117).
//
// otelhttp normalises the identical value to _OTHER eighty lines away, which is
// where the spelling comes from: two label sets on the same request should
// agree about what a method is.
//
// It matters now rather than later because the meter provider is currently a
// no-op -- CP_TELEMETRY_OTLP_ENDPOINT is unset -- so nothing is stored today.
// Arming metrics on a 512 MB instance with an unbounded label would turn
// observability into the memory leak.
func knownMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return m
	default:
		return "_OTHER"
	}
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

// observe wraps the handler with the status recorder, structured access
// logging and the transport metrics. It logs identifiers and outcomes only:
// never a header, a cookie, a query string or a body.
func observe(log *slog.Logger, clk clock.Clock, m *httpMetrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := clk.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			ctx := observability.WithLogger(r.Context(), log)
			r = r.WithContext(ctx)

			next.ServeHTTP(sw, r)

			elapsed := clk.Now().Sub(start)
			route := routePattern(r)
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.Int("status", sw.status),
				slog.Int64("duration_ms", elapsed.Milliseconds()),
				slog.Int64("bytes", sw.bytes),
			}
			if p, ok := security.PrincipalFrom(r.Context()); ok {
				attrs = append(attrs,
					slog.String("actor_type", string(p.ActorType)),
					slog.String("subject_id", p.SubjectID),
				)
			}
			level := slog.LevelInfo
			if sw.status >= 500 {
				level = slog.LevelError
			} else if sw.status >= 400 {
				level = slog.LevelWarn
			}
			observability.LoggerFrom(r.Context()).LogAttrs(r.Context(), level, "http request", attrs...)

			if m != nil {
				opts := observability.WithSafeAttrs(
					attribute.String("route", route),
					attribute.String("method", knownMethod(r.Method)),
					attribute.String("status_class", statusClass(sw.status)),
				)
				m.requests.Add(r.Context(), 1, opts)
				m.duration.Record(r.Context(), elapsed.Milliseconds(), opts)
			}
		})
	}
}

// corsPolicy answers preflight requests for an explicit allow-list of
// origins. There is no wildcard: the API is credentialed (session cookie), so
// "*" would be both invalid and unsafe, and config.Validate already refuses it
// in production-like environments.
func corsPolicy(origins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(origins))
	for _, o := range origins {
		if o != "" && o != "*" {
			allowed[o] = struct{}{}
		}
	}
	const allowHeaders = "Content-Type, Idempotency-Key, X-Request-Id, X-Correlation-Id, Last-Event-ID"
	// PUT joined the list when the first PUT route did (GET/PUT
	// /v1/me/notification-preferences). A method the surface mounts and the
	// preflight does not name is a route that works from curl and fails from
	// the browser the product actually ships -- and the deployed topology is
	// cross-origin (app-nodal -> api-nodal), so every non-simple request here
	// is preflighted.
	const allowMethods = "GET, POST, PUT, DELETE, OPTIONS"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				if _, ok := allowed[origin]; ok {
					w.Header().Add("Vary", "Origin")
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Credentials", "true")
					w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After, RateLimit-Reset")
				}
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				if _, ok := allowed[origin]; !ok {
					// Not an allowed origin: answer without CORS headers,
					// which the browser treats as a refusal.
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Header().Set("Access-Control-Allow-Methods", allowMethods)
				w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimits are the transport rate limits (PART 180). They are not financial
// authority (PART 181): the risk kernel enforces order frequency from
// persisted state, so losing these counters can never widen a financial
// budget.
type RateLimits struct {
	// General applies to every request that is not an auth endpoint, a quote
	// preview or a command -- which since D-080 includes the two PUBLIC reads,
	// `GET /v1/terms` and `GET /v1/native-markets`. It is therefore also the
	// ANONYMOUS budget for those two, keyed by IP rather than by principal
	// (principalKey below), at 600 a minute.
	//
	// That is the right budget for them and it is worth saying why rather than
	// leaving it to be rediscovered (F-200). Both are reads of small, bounded
	// tables: the registry is a handful of documents, and the markets list is
	// one page of at most 100 rows over `native_assets`, which has one row per
	// launched asset. The `?q=` search adds an unanchored ILIKE that cannot use
	// an index, so its cost is a sequential scan of that table -- the same order
	// of work as the list itself, which scans the same join. If that table ever
	// stops being small the fix is a trigram index, not a tighter limit, and
	// "small" is a property somebody has to re-check rather than assume.
	General *ratelimit.Limiter
	// Auth applies to the unauthenticated endpoints: the login flow and the
	// provider webhooks. Both are reachable without a session, so their budget
	// is the one that decides how much work an anonymous caller can make this
	// service do.
	Auth *ratelimit.Limiter
	// Quote applies to quote previews, which are the most provider-expensive
	// read.
	Quote *ratelimit.Limiter
	// Command applies to every mutating endpoint.
	Command *ratelimit.Limiter
}

// principalKey keys rate-limit counters by principal when there is one and by
// caller address otherwise, so one tenant cannot exhaust another's budget.
//
// The address is the one clientIP derives, not r.RemoteAddr. Behind a load
// balancer that terminates TLS -- which is every deployment of this service --
// RemoteAddr is the balancer, identical for every caller, so keying on it
// collapses every unauthenticated bucket into ONE. The auth budget is the one
// that matters: 30 requests a minute shared by everybody means one client can
// hold the whole cohort out of logging in, and an attacker's attempts are not
// counted against them but against the crowd (F-88).
//
// clientIP reads X-Forwarded-For only when the peer is in a configured trusted
// network, so an untrusted caller still cannot forge its own key; and when it
// does read the header it takes the right-most entry no trusted hop added,
// because everything to the left of that is the caller's own writing (F-166).
// When no networks are trusted it returns the peer address unchanged -- the old
// behaviour -- which is why config.Validate now requires the list to be stated
// in a production-like environment.
func principalKey(trusted []*net.IPNet) func(*http.Request) string {
	return func(r *http.Request) string {
		if p, ok := security.PrincipalFrom(r.Context()); ok && p.SubjectID != "" {
			return "sub:" + p.SubjectID
		}
		if ip := clientIP(r, trusted); ip != "" {
			return "ip:" + ip
		}
		return ratelimit.ByRemoteIP(r)
	}
}

// rateLimit selects the limiter that applies to the request. The order is
// specific first: auth endpoints, quote previews, any other command, then the
// general per-principal budget.
func rateLimit(l RateLimits, trusted []*net.IPNet) func(http.Handler) http.Handler {
	pick := func(r *http.Request) *ratelimit.Limiter {
		p := r.URL.Path
		switch {
		case l.Auth != nil && strings.Contains(p, "/auth/"):
			return l.Auth
		// A webhook is unauthenticated -- rejection is what happens when the
		// signature does not verify -- and it shared the Command budget with
		// every admin command, at 120/min. Every rejected delivery writes a
		// durable security_events row that no role can delete, on a deployment
		// whose database ceiling halts every financial action when it is
		// reached (F-105). A provider's real delivery volume is a few a minute,
		// and a 429 makes it retry rather than lose the event.
		//
		// "no role can delete" is still true of a ROW and is no longer true of
		// the table: 00740 partitions security_events by month, so the owner can
		// drop a whole month once a retention period is chosen. Both halves
		// matter here. The rate limit is what keeps an unauthenticated caller
		// from choosing how fast the table grows; retention is what keeps it
		// from growing forever. Neither substitutes for the other.
		case l.Auth != nil && strings.HasPrefix(p, "/v1/webhooks/"):
			return l.Auth
		case l.Quote != nil && strings.HasSuffix(p, "/quotes/preview"):
			return l.Quote
		case l.Command != nil && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions:
			return l.Command
		default:
			return l.General
		}
	}
	key := principalKey(trusted)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limiter := pick(r)
			if limiter == nil {
				next.ServeHTTP(w, r)
				return
			}
			ratelimit.Middleware(limiter, key)(next).ServeHTTP(w, r)
		})
	}
}

// clientIP returns the request's remote address as a plain IP for audit,
// session and provider records: no port, no zone, no namespace prefix, because
// internal/audit refuses anything else and an unparsable value must not fail
// the operation it is describing.
//
// Proxy headers are honored only when the immediate peer is one of the
// configured trusted networks; an untrusted client must not be able to forge
// its own address. An address that cannot be parsed yields the empty string,
// which audit reads as "not recorded" rather than as a bad value.
//
// # Why the list is walked from the RIGHT (F-166)
//
// A reverse proxy APPENDS to whatever X-Forwarded-For the client sent --
// nginx's `$proxy_add_x_forwarded_for` is the canonical form of it -- so the
// header this process reads is "<whatever the client wrote>, <the address the
// proxy saw>". Only the right-most entry is the proxy's own word for who
// called; everything to its left is client input that arrived inside a header.
//
// Reading the LEFT-most entry therefore let any caller behind the platform
// router choose its own value for both things this function feeds: the
// rate-limit key for every unauthenticated class, including the auth budget
// that exists to stop brute force, and the address recorded in `sessions`,
// `login_attempts`, `security_events` and `terms_acceptances.source_ip`.
//
// So the walk is right to left, discarding entries that are themselves trusted
// proxies -- a chain of two balancers appends twice -- and the first entry that
// is not one is the closest address any trusted hop actually observed. A list
// of nothing but trusted addresses, or nothing parsable, falls back to
// RemoteAddr, which is the peer and is never forgeable.
func clientIP(r *http.Request, trusted []*net.IPNet) string {
	remote := plainIP(r.RemoteAddr)
	if remote == "" || len(trusted) == 0 {
		return remote
	}
	if !isTrustedProxy(remote, trusted) {
		return remote
	}
	fwd := r.Header.Get("X-Forwarded-For")
	if fwd == "" {
		return remote
	}
	entries := strings.Split(fwd, ",")
	// The list is caller-controlled and unbounded; only the right-hand end of
	// it can say anything, so a header with thousands of entries is truncated
	// to its last few before a single one is parsed. 32 is far more hops than
	// any real deployment has and bounds the work one request can ask for.
	if len(entries) > maxForwardedForEntries {
		entries = entries[len(entries)-maxForwardedForEntries:]
	}
	for i := len(entries) - 1; i >= 0; i-- {
		ip := plainIP(entries[i])
		switch {
		case ip == "":
			// An unparsable entry is not evidence of anything, and stopping
			// here would let a caller end the walk on a value it chose.
			continue
		case isTrustedProxy(ip, trusted):
			continue
		default:
			return ip
		}
	}
	return remote
}

// maxForwardedForEntries bounds how much of an X-Forwarded-For list is parsed.
const maxForwardedForEntries = 32

// isTrustedProxy reports whether a plain IP is inside one of the configured
// trusted networks.
func isTrustedProxy(ip string, trusted []*net.IPNet) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	as := net.IP(addr.AsSlice())
	for _, n := range trusted {
		if n.Contains(as) {
			return true
		}
	}
	return false
}

// plainIP normalises "host:port", "[v6]:port" or a bare address to a canonical
// textual IP, and returns "" for anything else.
func plainIP(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(v); err == nil {
		v = host
	}
	v = strings.TrimPrefix(strings.TrimSuffix(v, "]"), "[")
	addr, err := netip.ParseAddr(v)
	if err != nil {
		return ""
	}
	// A zone ("fe80::1%eth0") is not a plain address.
	return addr.WithZone("").String()
}

// parseCIDRs parses the configured trusted proxy networks.
func parseCIDRs(in []string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// userAgent returns a bounded User-Agent for session and audit records.
func userAgent(r *http.Request) string {
	ua := r.Header.Get("User-Agent")
	if len(ua) > 512 {
		return ua[:512]
	}
	return ua
}
