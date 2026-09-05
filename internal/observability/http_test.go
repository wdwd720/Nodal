package observability

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type seen struct {
	requestID, correlationID string
	spanValid                bool
	traceID                  string
}

func serve(t *testing.T, headers map[string]string) (*httptest.ResponseRecorder, seen, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	var got seen
	h := HTTPMiddleware("api", otelhttp.WithTracerProvider(tp))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.requestID = RequestID(r.Context())
		got.correlationID = CorrelationID(r.Context())
		sc := trace.SpanContextFromContext(r.Context())
		got.spanValid = sc.IsValid()
		got.traceID = sc.TraceID().String()
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w, got, rec
}

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestHTTPMiddleware_GeneratesAndEchoesRequestID(t *testing.T) {
	t.Parallel()
	w, got, rec := serve(t, nil)
	assert.Equal(t, http.StatusNoContent, w.Code)
	rid := w.Header().Get(HeaderRequestID)
	assert.Regexp(t, hex32, rid)
	assert.Equal(t, rid, got.requestID, "handler sees the same id that is echoed")
	assert.Equal(t, rid, got.correlationID, "correlation defaults to the request id")
	assert.True(t, got.spanValid, "otelhttp span is active inside the handler")

	spans := rec.Ended()
	require.Len(t, spans, 1)
	// otelhttp names server spans per the HTTP semantic conventions (method
	// and, when known, route); the operation string labels the handler.
	assert.NotEmpty(t, spans[0].Name())
	attrs := attribute.NewSet(spans[0].Attributes()...)
	v, ok := attrs.Value(AttrRequestID)
	require.True(t, ok, "request id attached to span")
	assert.Equal(t, rid, v.AsString())
	assert.Equal(t, got.traceID, spans[0].SpanContext().TraceID().String())
}

func TestHTTPMiddleware_HonoursWellFormedHeaders(t *testing.T) {
	t.Parallel()
	w, got, rec := serve(t, map[string]string{HeaderRequestID: "client-req-0001", HeaderCorrelationID: "corr:abc.123"})
	assert.Equal(t, "client-req-0001", w.Header().Get(HeaderRequestID))
	assert.Equal(t, "client-req-0001", got.requestID)
	assert.Equal(t, "corr:abc.123", got.correlationID)
	attrs := attribute.NewSet(rec.Ended()[0].Attributes()...)
	v, _ := attrs.Value(AttrCorrelationID)
	assert.Equal(t, "corr:abc.123", v.AsString())
}

func TestHTTPMiddleware_ReplacesMalformedHeaders(t *testing.T) {
	t.Parallel()
	for name, bad := range map[string]string{
		"too short": "abc",
		"too long":  strings.Repeat("a", 129),
		"space":     "has space here",
		"newline":   "line1\nline2",
		"unicode":   "réquest-id-1234",
		"json":      `{"x":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w, got, _ := serve(t, map[string]string{HeaderRequestID: bad, HeaderCorrelationID: bad})
			rid := w.Header().Get(HeaderRequestID)
			assert.Regexp(t, hex32, rid)
			assert.NotEqual(t, bad, rid)
			assert.Equal(t, rid, got.requestID)
			assert.Equal(t, rid, got.correlationID)
		})
	}
}

func TestValidRequestID(t *testing.T) {
	t.Parallel()
	assert.True(t, ValidRequestID("12345678"))
	assert.True(t, ValidRequestID("01J8Z0K6V3Q9X7PZ1Y2W3A4B5C"))
	assert.True(t, ValidRequestID("a-b.c:d_e-f.g:h"))
	assert.True(t, ValidRequestID(strings.Repeat("z", 128)))
	assert.False(t, ValidRequestID(""))
	assert.False(t, ValidRequestID("1234567"))
	assert.False(t, ValidRequestID(strings.Repeat("z", 129)))
	assert.False(t, ValidRequestID("with space"))
	assert.False(t, ValidRequestID("tab\tin"))
}

func TestNewRequestID(t *testing.T) {
	t.Parallel()
	ids := map[string]bool{}
	for range 1000 {
		id := NewRequestID()
		assert.Regexp(t, hex32, id)
		assert.True(t, ValidRequestID(id))
		assert.False(t, ids[id], "ids must be unique")
		ids[id] = true
	}
}
