package observability

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// HTTP headers used for request correlation.
const (
	HeaderRequestID     = "X-Request-Id"
	HeaderCorrelationID = "X-Correlation-Id"
)

// requestIDRe bounds what is accepted from a client-supplied id so that log
// lines and headers cannot be polluted with arbitrary bytes.
var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

// NewRequestID returns a fresh 128-bit random hex id.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// ValidRequestID reports whether id is acceptable as a request/correlation
// id.
func ValidRequestID(id string) bool { return requestIDRe.MatchString(id) }

// HTTPMiddleware returns middleware that wraps the handler in an otelhttp
// server span named operation and propagates identifiers: the X-Request-Id
// header is honoured when well-formed, generated otherwise, always echoed on
// the response and placed in the context (RequestID); X-Correlation-Id is
// honoured when well-formed and defaults to the request id (CorrelationID).
// Both are attached to the active span. Extra otelhttp options (for example
// WithSpanNameFormatter for route-based names) are passed through.
func HTTPMiddleware(operation string, opts ...otelhttp.Option) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := r.Header.Get(HeaderRequestID)
			if !ValidRequestID(rid) {
				rid = NewRequestID()
			}
			cid := r.Header.Get(HeaderCorrelationID)
			if !ValidRequestID(cid) {
				cid = rid
			}
			w.Header().Set(HeaderRequestID, rid)
			ctx := WithCorrelationID(WithRequestID(r.Context(), rid), cid)
			if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
				span.SetAttributes(attribute.String(AttrRequestID, rid), attribute.String(AttrCorrelationID, cid))
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
		return otelhttp.NewHandler(inner, operation, opts...)
	}
}
