package httpapi

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
