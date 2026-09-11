package httpapi

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/agents"
)

// Every timestamp this API publishes is UTC, and no converter is trusted to
// remember (F-172).
//
// openapi.yaml says "Timestamps are RFC 3339 UTC". Sixty-odd `toAPI*` functions
// turn a domain value into a generated struct, pgx hands a `timestamptz` back in
// the PROCESS's zone, and `.UTC()` is one call any of them can forget --
// `toAPIStrategy` and `toAPIStrategyVersion` did, so GET/POST /v1/strategies
// published the server's timezone offset while every other route published Z.
//
// A per-converter table would be sixty fixtures that the sixty-first converter
// is not in. This works on the finished response instead: an RFC 3339 instant
// is UTC if and only if its text ends in `Z`, so the check is a scan of the
// encoded body, and every test in this package that performs a request runs it
// through `harness.do`. A converter that forgets is caught by whichever route
// test already covers it, rather than by a fixture somebody has to write.
//
// It reads the body rather than reflecting over the struct deliberately: the
// body is what a client receives, and a `time.Time` carrying a zone that
// happens to be UTC-offset-zero would pass a reflection check and still be
// published as "+00:00" by some encoders.
var offsetTimestamp = regexp.MustCompile(`"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?[+-][0-9]{2}:?[0-9]{2}"`)

// checkedResponse wraps a recorded response and fails the test if the body
// carries a timestamp that is not UTC.
func checkedResponse(t *testing.T, rec *httptest.ResponseRecorder) *response {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "json") {
		if found := offsetTimestamp.FindAllString(rec.Body.String(), -1); len(found) > 0 {
			t.Errorf("the response publishes %d timestamp(s) in a zone offset rather than UTC: %v\n"+
				"the contract says RFC 3339 UTC; a converter is missing .UTC()", len(found), found)
		}
	}
	return &response{ResponseRecorder: rec, t: t}
}

// TestTimestampScanner_SeesAnOffsetAndAcceptsZ keeps the check above from being
// vacuous: a scanner that matched nothing would pass every response forever.
func TestTimestampScanner_SeesAnOffsetAndAcceptsZ(t *testing.T) {
	t.Parallel()
	local := time.FixedZone("TEST-7", -7*3600)
	at := time.Date(2026, 9, 10, 20, 39, 30, 0, local)

	assert.Regexp(t, offsetTimestamp, `{"created_at":"`+at.Format(time.RFC3339)+`"}`)
	assert.NotRegexp(t, offsetTimestamp, `{"created_at":"`+at.UTC().Format(time.RFC3339)+`"}`)
	assert.NotRegexp(t, offsetTimestamp, `{"created_at":"`+at.UTC().Format(time.RFC3339Nano)+`"}`)
	// A date with no time is not a timestamp and must not be flagged.
	assert.NotRegexp(t, offsetTimestamp, `{"day":"2026-09-10"}`)
}

// TestStrategyConverters_PublishUTC is the direct form of the same claim for the
// two converters that forgot, including the nested version whose BuiltAt is
// reached through a pointer.
func TestStrategyConverters_PublishUTC(t *testing.T) {
	t.Parallel()
	local := time.FixedZone("TEST+11", 11*3600)
	at := time.Date(2026, 9, 10, 20, 39, 30, 0, local)

	out := (&Server{}).toAPIStrategy(agents.Strategy{
		ID: "01a08e8c-8ca6-7bc9-aefb-3a6e1d9ffee4", AccountID: "01a08e87-d0de-738a-9f86-dec48d5f1580",
		Name: "s", Description: "d", SourceKind: "NATURAL_LANGUAGE", Status: "ACTIVE",
		CreatedAt: at, UpdatedAt: at,
		CurrentVersion: &agents.StrategyVersion{
			ID: "01a08e8c-8ca6-7bc9-aefb-3a6e1d9ffee5", BuiltAt: at.Add(time.Minute),
		},
	})

	assert.Equal(t, time.UTC, out.CreatedAt.Location())
	require.NotNil(t, out.UpdatedAt)
	assert.Equal(t, time.UTC, out.UpdatedAt.Location())
	require.NotNil(t, out.CurrentVersion)
	require.NotNil(t, out.CurrentVersion.BuiltAt)
	assert.Equal(t, time.UTC, out.CurrentVersion.BuiltAt.Location())

	// A strategy with no version and no update carries neither, and asking for
	// the zone of a timestamp that was never set is not a thing to assert.
	bare := (&Server{}).toAPIStrategy(agents.Strategy{
		ID: "01a08e8c-8ca6-7bc9-aefb-3a6e1d9ffee4", AccountID: "01a08e87-d0de-738a-9f86-dec48d5f1580",
		CreatedAt: at,
	})
	assert.Nil(t, bare.UpdatedAt)
	assert.Nil(t, bare.CurrentVersion)
}
