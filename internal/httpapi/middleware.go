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
func captureBody(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if r.Body != nil && r.ContentLength != 0 {
				limited := http.MaxBytesReader(w, r.Body, maxBytes)
				buf, err := io.ReadAll(limited)
				if err != nil {
					var tooLarge *http.MaxBytesError
					if errors.As(err, &tooLarge) {
						writeProblem(w, r, errs.New(errs.CodeValidationFailed, "request body is too large"))
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
					attribute.String("method", r.Method),
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
	const allowMethods = "GET, POST, DELETE, OPTIONS"
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
	// General applies to every authenticated request.
	General *ratelimit.Limiter
	// Auth applies to the unauthenticated login endpoints.
	Auth *ratelimit.Limiter
	// Quote applies to quote previews, which are the most provider-expensive
	// read.
	Quote *ratelimit.Limiter
	// Command applies to every mutating endpoint.
	Command *ratelimit.Limiter
}

// principalKey keys rate-limit counters by principal when there is one and by
// remote address otherwise, so one tenant cannot exhaust another's budget.
// ratelimit.ByRemoteIP already returns a namespaced key ("ip:<host>").
func principalKey(r *http.Request) string {
	if p, ok := security.PrincipalFrom(r.Context()); ok && p.SubjectID != "" {
		return "sub:" + p.SubjectID
	}
	return ratelimit.ByRemoteIP(r)
}

// rateLimit selects the limiter that applies to the request. The order is
// specific first: auth endpoints, quote previews, any other command, then the
// general per-principal budget.
func rateLimit(l RateLimits) func(http.Handler) http.Handler {
	pick := func(r *http.Request) *ratelimit.Limiter {
		p := r.URL.Path
		switch {
		case l.Auth != nil && strings.Contains(p, "/auth/"):
			return l.Auth
		case l.Quote != nil && strings.HasSuffix(p, "/quotes/preview"):
			return l.Quote
		case l.Command != nil && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions:
			return l.Command
		default:
			return l.General
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limiter := pick(r)
			if limiter == nil {
				next.ServeHTTP(w, r)
				return
			}
			ratelimit.Middleware(limiter, principalKey)(next).ServeHTTP(w, r)
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
func clientIP(r *http.Request, trusted []*net.IPNet) string {
	remote := plainIP(r.RemoteAddr)
	if remote == "" || len(trusted) == 0 {
		return remote
	}
	peer, err := netip.ParseAddr(remote)
	if err != nil {
		return remote
	}
	isTrusted := false
	for _, n := range trusted {
		if n.Contains(net.IP(peer.AsSlice())) {
			isTrusted = true
			break
		}
	}
	if !isTrusted {
		return remote
	}
	fwd := r.Header.Get("X-Forwarded-For")
	if fwd == "" {
		return remote
	}
	// The left-most entry is the original client as recorded by the first
	// trusted proxy.
	first := plainIP(strings.TrimSpace(strings.Split(fwd, ",")[0]))
	if first == "" {
		return remote
	}
	return first
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
