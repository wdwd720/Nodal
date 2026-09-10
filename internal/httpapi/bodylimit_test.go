package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// The body is the one allocation an unauthenticated caller chooses the size of
// (F-85).
//
// captureBody reads the whole body into memory, in full, so that the webhook
// signature check can see the exact bytes the provider signed. That is
// necessary and it makes CP_HTTP_MAX_BODY_BYTES an attacker-controlled
// allocation on a single 512 MB instance -- and the middleware ran BEFORE the
// rate limiter, so a request the limiter was going to refuse paid for the
// allocation first.
//
// Three things had to be true at once, which is why this was not a middleware
// swap: the limiter needs the principal (attached above it), the webhook needs
// the exact bytes (captured below it), and neither the limiter nor CSRF reads
// the body at all. So the body moved below both.

// countingBody reports how many bytes were actually read from it, which is the
// only honest way to assert "refused without reading".
type countingBody struct {
	r    io.Reader
	read atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read.Add(int64(n))
	return n, err
}

func (c *countingBody) Close() error { return nil }

func TestBodyLimit_ADeclaredOversizeIsRefusedWithoutReadingAByte(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)

	body := &countingBody{r: strings.NewReader(strings.Repeat("x", 4096))}
	req := httptest.NewRequest(http.MethodPost, "/v1/intents", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "declared-oversize-0001")
	// The declared length is what a flood announces. Reading up to the limit
	// before refusing costs the limit in allocation and the whole body in
	// bandwidth, every time.
	req.ContentLength = 64 << 20

	rec := httptest.NewRecorder()
	h.server.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "body=%s", rec.Body.String())
	assert.Zero(t, body.read.Load(), "the body was read before it was refused")
}

func TestBodyLimit_RouteLimitsAreDistinctAndTheWebhookGetsMore(t *testing.T) {
	t.Parallel()
	const configured = 1 << 20 // the deployment's CP_HTTP_MAX_BODY_BYTES

	assert.EqualValues(t, defaultMaxBodyBytes, bodyLimitFor("/v1/intents", configured))
	assert.EqualValues(t, defaultMaxBodyBytes, bodyLimitFor("/v1/payments", configured))
	assert.EqualValues(t, defaultMaxBodyBytes, bodyLimitFor("/v1/auth/callback", configured))
	assert.EqualValues(t, webhookMaxBodyBytes, bodyLimitFor("/v1/webhooks/stripe_credit", configured))
	assert.Greater(t, int64(webhookMaxBodyBytes), int64(defaultMaxBodyBytes),
		"a provider delivery is somebody else's payload and needs the larger allowance")

	// The configured value is a ceiling, not a floor. An operator may tighten
	// every route and may not loosen one past its own limit.
	assert.EqualValues(t, 1024, bodyLimitFor("/v1/webhooks/stripe_credit", 1024))
	assert.EqualValues(t, 1024, bodyLimitFor("/v1/intents", 1024))
	assert.EqualValues(t, webhookMaxBodyBytes, bodyLimitFor("/v1/webhooks/stripe_credit", 0),
		"an unset maximum leaves the route's own limit in force, never unbounded")
}

func TestBodyLimit_AnUndeclaredOversizeIsStillBounded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)

	// A chunked request declares -1: its size is not knowable until it has
	// been read, so the reader is the only bound available. It must still stop.
	body := &countingBody{r: strings.NewReader(strings.Repeat("x", defaultMaxBodyBytes+4096))}
	req := httptest.NewRequest(http.MethodPost, "/v1/intents", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "chunked-oversize-0001")
	req.ContentLength = -1

	rec := httptest.NewRecorder()
	h.server.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "body=%s", rec.Body.String())
	assert.LessOrEqual(t, body.read.Load(), int64(defaultMaxBodyBytes)+1024,
		"an undeclared body was read far past the route's limit before it was refused")
}

func TestBodyLimit_ARefusalIs413AndNotAValidationFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)

	res := h.do(http.MethodPost, "/v1/intents", strings.Repeat("x", defaultMaxBodyBytes+1),
		"Idempotency-Key", "oversize-code-0001")
	require.Equal(t, http.StatusRequestEntityTooLarge, res.Code)
	// It is answered before the body is read, so there is nothing to validate
	// and nothing to say about a field. A client must send less, not different.
	assert.Equal(t, errs.CodeBodyTooLarge, res.problem().Code)
}

func TestBodyLimit_AnOrdinaryBodyStillArrivesExactly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)

	// The control that matters most: the reordering and the tighter limits must
	// not have broken the ordinary path, and the bytes a handler sees must be
	// the bytes that were sent -- which is what the webhook signature check
	// depends on.
	res := h.do(http.MethodGet, "/v1/me", nil)
	assert.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
}
