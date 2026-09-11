package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/agents"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ratelimit"
)

func auditTrustedPrivate(t *testing.T) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8"} {
		_, n, err := net.ParseCIDR(c)
		require.NoError(t, err)
		out = append(out, n)
	}
	return out
}

// AUDIT (platform-hardening) — F-platform-2.
//
// clientIP takes the LEFT-MOST X-Forwarded-For entry whenever the immediate
// peer is one of CP_HTTP_TRUSTED_PROXY_CIDRS, and render.yaml trusts the whole
// private space because that is the only way the container is reachable.
//
// The left-most entry is not the proxy's word for who called. A reverse proxy
// APPENDS to whatever X-Forwarded-For the client sent (nginx's
// `$proxy_add_x_forwarded_for` is the canonical form of this), so the header
// the app reads is "<whatever the client wrote>, <the address the proxy saw>".
// The value the proxy vouches for is the RIGHT-most one it added; everything to
// its left is untrusted client input.
//
// So a caller that sets its own X-Forwarded-For chooses:
//
//   - its transport rate-limit key, for every unauthenticated class including
//     the 30/min auth budget that exists to stop brute force;
//   - the `ip` recorded in sessions, login_attempts, security_events,
//     terms_acceptances.source_ip and every audit record clientIP feeds.
//
// The correct read is to walk the list from the RIGHT and take the first entry
// that is not itself a trusted proxy address.
func TestAudit_ForgedXForwardedForChoosesTheRateLimitKeyAndTheAuditAddress(t *testing.T) {
	t.Parallel()
	trusted := auditTrustedPrivate(t)
	key := principalKey(trusted)

	// What the app sees when a client sent `X-Forwarded-For: 9.9.9.9` and the
	// platform proxy appended the address it really came from.
	req := func(forwarded string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		r.RemoteAddr = "10.201.0.5:44321" // the platform router
		r.Header.Set("X-Forwarded-For", forwarded)
		return r
	}

	const realCaller = "198.51.100.77"
	got := clientIP(req("9.9.9.9, "+realCaller), trusted)
	assert.Equal(t, realCaller, got,
		"the address written to sessions, login_attempts and security_events is the one the client typed")

	// And therefore the rate-limit bucket is the client's to choose: one
	// caller, one thousand budgets.
	seen := map[string]struct{}{}
	for i := 0; i < 1000; i++ {
		seen[key(req(fmt.Sprintf("10.%d.%d.%d, %s", i/256, i%256, i%251, realCaller)))] = struct{}{}
	}
	assert.Equal(t, 1, len(seen),
		"one caller produced %d distinct rate-limit keys by rewriting one header", len(seen))
}

// AUDIT (platform-hardening) — F-platform-2, end to end through the middleware.
//
// The auth budget is 30/min in render.yaml. This drives the real rateLimit
// middleware with a budget of 3 and shows an unauthenticated caller taking 300.
func TestAudit_TheAuthBudgetIsBypassedByRewritingOneHeader(t *testing.T) {
	t.Parallel()
	now := func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) }
	store := ratelimit.NewMemoryStore()
	auth, err := ratelimit.NewLimiter("auth", store, ratelimit.Limit{Requests: 3, Window: time.Minute}, now, false)
	require.NoError(t, err)

	h := rateLimit(RateLimits{Auth: auth}, auditTrustedPrivate(t))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	send := func(forwarded string) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/auth/login", nil)
		r.RemoteAddr = "10.201.0.5:44321"
		r.Header.Set("X-Forwarded-For", forwarded)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	admitted := 0
	for i := 0; i < 300; i++ {
		if send(fmt.Sprintf("203.0.113.%d, 198.51.100.77", i%256)) == http.StatusOK {
			admitted++
		}
	}
	assert.LessOrEqual(t, admitted, 3,
		"the auth budget is 3 a minute and this caller took %d of them", admitted)
}

// AUDIT (platform-hardening) — F-platform-3.
//
// ratelimit.MemoryStore documents itself as having "bounded memory: expired
// windows are dropped lazily on access and by Sweep". Neither half holds for a
// key that is never revisited: the lazy drop only replaces the entry for a key
// that comes back in a later window, and nothing in cmd/ or internal/ calls
// Sweep — `grep -rn "\.Sweep(" cmd internal` finds only reconciliation and
// archive sweeps.
//
// So the map retains one entry per distinct (limiter, key) pair for the life of
// the process. Combined with F-platform-2 the key cardinality is an
// unauthenticated caller's to choose, which turns the rate limiter into an
// unbounded allocation on a 512 MB instance — the exact failure the F-117
// comment in middleware.go describes for metric labels.
//
// Sweep is used here only as a MEASUREMENT of how many entries the store is
// holding; production never calls it, which is the defect.
func TestAudit_TheMemoryRateLimitStoreIsNeverSwept(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	store := ratelimit.NewMemoryStore()

	const distinct = 50_000
	for i := 0; i < distinct; i++ {
		_, _, err := store.Incr(nil, fmt.Sprintf("auth:ip:203.0.113.%d.%d", i/256, i%256), time.Minute, now) //nolint:staticcheck // MemoryStore ignores ctx
		require.NoError(t, err)
	}

	// Two windows later every one of those windows is long dead. A store that
	// swept would be holding none of them.
	retained := store.Sweep(now.Add(2*time.Minute), time.Minute)
	assert.Zero(t, retained,
		"the store is still holding %d expired windows; nothing in the process ever removes them", retained)
}

// AUDIT (platform-hardening) — F-platform-5.
//
// BODY_TOO_LARGE is declared (internal/errs/codes.go:25), mapped to 413 in the
// status registry (codes.go:245) and emitted by captureBody on every route, but
// it is absent from `allCodes` — so `errs.AllCodes()` returns 53 of the 54
// declared codes and every test and document that enumerates the set skips it:
// TestCodeStatusMapping (whose independent `expectedStatus` table also omits
// it, and whose length assertion therefore passes), TestAllCodes_*,
// TestToProblem_NeverExposesCauseForAnyCode. The published contract's
// Problem.code description does not name it either, and
// apps/web/src/api/problem.ts has no case for it.
func TestAudit_BodyTooLargeIsMissingFromTheRegisteredCodeSet(t *testing.T) {
	t.Parallel()
	assert.Contains(t, errs.AllCodes(), errs.CodeBodyTooLarge,
		"the transport returns this code on every route and AllCodes does not list it, "+
			"so nothing that enumerates the code set covers it")
}

// AUDIT (platform-hardening) — F-platform-6.
//
// openapi.yaml's own description says "Timestamps are RFC 3339 UTC". Every
// other converter in this package lands `.UTC()` on the way out (convert.go
// 122, 189, 669, 727, 825; handlers_funding.go 186). toAPIStrategy and
// toAPIStrategyVersion do not, and pgx hands a timestamptz back in the
// process's local zone — so GET/POST /v1/strategies is the one route that
// publishes the server's timezone offset.
func TestAudit_StrategyTimestampsAreNotUTC(t *testing.T) {
	t.Parallel()
	local := time.FixedZone("AUDIT-7", -7*3600)
	at := time.Date(2026, 9, 10, 20, 39, 30, 0, local)
	built := at.Add(time.Minute)

	out := (&Server{}).toAPIStrategy(agents.Strategy{
		ID: "01a08e8c-8ca6-7bc9-aefb-3a6e1d9ffee4", AccountID: "01a08e87-d0de-738a-9f86-dec48d5f1580",
		Name: "s", Description: "d", SourceKind: "NATURAL_LANGUAGE", Status: "ACTIVE",
		CreatedAt: at, UpdatedAt: at,
		CurrentVersion: &agents.StrategyVersion{ID: "01a08e8c-8ca6-7bc9-aefb-3a6e1d9ffee5", BuiltAt: built},
	})

	assert.Equal(t, "2026-09-11T03:39:30Z", out.CreatedAt.Format(time.RFC3339), "created_at")
	require.NotNil(t, out.UpdatedAt)
	assert.Equal(t, "2026-09-11T03:39:30Z", out.UpdatedAt.Format(time.RFC3339), "updated_at")
	require.NotNil(t, out.CurrentVersion)
	require.NotNil(t, out.CurrentVersion.BuiltAt)
	assert.Equal(t, "2026-09-11T03:40:30Z", out.CurrentVersion.BuiltAt.Format(time.RFC3339), "built_at")
}
