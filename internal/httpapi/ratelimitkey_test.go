package httpapi

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/ratelimit"
	"github.com/nodal/controlplane/internal/security"
)

// Behind a load balancer, two callers are two buckets (F-88).
//
// principalKey used to read r.RemoteAddr directly. Every deployment of this
// service terminates TLS at a balancer, so that address is the balancer's and
// is the same for every caller: one shared counter for all unauthenticated
// traffic, including the auth budget that exists to stop brute force. Thirty
// requests a minute for the whole cohort together is not a per-client control,
// and the client being counted against is the crowd rather than the attacker.
func TestRateLimitKeyDistinguishesCallersBehindAProxy(t *testing.T) {
	t.Parallel()
	_, private, err := net.ParseCIDR("10.0.0.0/8")
	require.NoError(t, err)
	trusted := []*net.IPNet{private}

	req := func(remote, forwarded string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/auth/login", nil)
		r.RemoteAddr = remote
		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
		}
		return r
	}

	behind := principalKey(trusted)
	first := behind(req("10.1.2.3:44321", "203.0.113.7"))
	second := behind(req("10.1.2.3:51002", "198.51.100.9"))
	assert.NotEqual(t, first, second,
		"two callers through one balancer shared a rate-limit bucket")
	assert.Equal(t, "ip:203.0.113.7", first)

	// The same caller through the same balancer is the same bucket, whatever
	// ephemeral port the balancer used.
	assert.Equal(t, first, behind(req("10.9.9.9:1", "203.0.113.7")))

	// A caller that is not on a trusted network cannot choose its own key: the
	// header is ignored and it is counted against where it really came from.
	assert.Equal(t, "ip:198.51.100.200",
		behind(req("198.51.100.200:1234", "203.0.113.7")),
		"an untrusted caller forged its rate-limit key")

	// With nothing trusted the old behaviour stands, which is why
	// config.Validate now requires the list in STAGING and PROD.
	none := principalKey(nil)
	assert.Equal(t, none(req("10.1.2.3:1", "203.0.113.7")), none(req("10.1.2.3:2", "198.51.100.9")))

	// An authenticated caller is keyed by subject regardless of address, so a
	// shared office address cannot exhaust one person's budget.
	r := req("10.1.2.3:1", "203.0.113.7")
	r = r.WithContext(security.WithPrincipal(r.Context(), security.Principal{
		SubjectID: "01a0754e-1111-7000-8000-000000000001", ActorType: security.ActorUser,
	}))
	assert.Equal(t, "sub:01a0754e-1111-7000-8000-000000000001", behind(r))
}

// The unauthenticated routes share the strict budget (F-105).
//
// A webhook used to fall through to the Command bucket, at 120/min, alongside
// every admin command. It is reachable without a session -- rejection is what
// happens when the signature does not verify -- and every rejected delivery
// wrote a durable security_events row no role can delete, on a deployment whose
// database ceiling halts every financial action when it is reached. A
// provider's real delivery volume is a few a minute, and a 429 makes it retry
// rather than lose the event.
//
// Since 00740 the table is partitioned and a whole MONTH can be dropped once a
// retention period is chosen, so the ceiling is no longer a countdown. The rate
// limit is still what it was for: it stops an unauthenticated caller choosing
// how fast the table grows, which retention does not.
func TestRateLimit_TheUnauthenticatedRoutesShareTheStrictBudget(t *testing.T) {
	t.Parallel()
	now := func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) }
	store := ratelimit.NewMemoryStore()
	auth, err := ratelimit.NewLimiter("auth", store, ratelimit.Limit{Requests: 1, Window: time.Minute}, now, false)
	require.NoError(t, err)
	command, err := ratelimit.NewLimiter("command", store, ratelimit.Limit{Requests: 1000, Window: time.Minute}, now, false)
	require.NoError(t, err)

	h := rateLimit(RateLimits{Auth: auth, Command: command}, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	)

	send := func(path string) int {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "203.0.113.9:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// One request exhausts the strict budget; the second webhook is refused,
	// which it would not be on the 1000-wide command budget.
	require.Equal(t, http.StatusOK, send("/v1/webhooks/stripe_credit"))
	assert.Equal(t, http.StatusTooManyRequests, send("/v1/webhooks/stripe_credit"),
		"a webhook is drawing on the command budget, not the unauthenticated one")

	// The control: an ordinary command is unaffected, so the change narrowed
	// the webhook rather than everything.
	assert.Equal(t, http.StatusOK, send("/v1/intents"))
	assert.Equal(t, http.StatusOK, send("/v1/intents"))
}

// No metric label takes a value the caller chose (F-117).
//
// r.Method is the raw request-line token and Go accepts any RFC 7230 token as
// one, so the `method` label was an unbounded dimension. chi runs middleware
// before routing, so even a 405 recorded it. The meter provider is a no-op
// today (CP_TELEMETRY_OTLP_ENDPOINT is unset), which is exactly why this is
// worth closing now: arming metrics on a 512 MB instance with a label an
// attacker enumerates turns observability into the memory leak.
func TestObserve_TheMethodLabelIsBounded(t *testing.T) {
	t.Parallel()
	for _, m := range []string{
		http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions,
		http.MethodConnect, http.MethodTrace,
	} {
		assert.Equal(t, m, knownMethod(m), "a real method must survive unchanged")
	}
	for _, m := range []string{
		"", "get", "PROPFIND", "NOTIFY", strings.Repeat("A", 4096),
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "M-SEARCH",
	} {
		assert.Equal(t, "_OTHER", knownMethod(m), "%q became its own label", m)
	}
}
