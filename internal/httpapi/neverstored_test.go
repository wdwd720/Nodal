package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Nothing the product documents as never stored reaches the idempotency record
// (F-231, D-125).
//
// ADR-0025 §3 says the hosted verification link is "handed to the browser that
// asked for it and written down nowhere". It was written to
// idempotency_keys.response_body, where cp_readonly and cp_ops may SELECT it,
// cp_app may not DELETE it, and it sat for the whole idempotency TTL.
//
// The test drives every COMMAND route in the withdrawal journey -- the ones
// runCommand records at all -- and reads what the store ended up holding. A
// route added to this area that returns a credential fails here unless it
// declares the field, which is the point: the defect was not that somebody
// stored a link on purpose, it was that storing the whole response was the
// default and nobody had to think about it.
func TestNoNeverStoredFieldReachesTheIdempotencyRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	for _, route := range []struct {
		name   string
		method string
		path   string
		body   map[string]any
		key    string
	}{
		{
			name: "start a verification session", method: http.MethodPost,
			path: "/v1/me/verification/sessions",
			body: map[string]any{
				"account_id": testAccountID.String(), "purpose": "PAYOUT_KYC",
				"jurisdiction_country": "US", "jurisdiction_region": "CA",
			},
			key: "neverstored-verify-1",
		},
		{
			name: "register a payout destination", method: http.MethodPost,
			path: "/v1/me/payout-destinations",
			body: map[string]any{
				"account_id": testAccountID.String(), "kind": "BANK",
				"provider_token": "sandbox-handle-neverstored", "country": "US",
			},
			key: "neverstored-dest-1",
		},
		{
			name: "quote a payout", method: http.MethodPost,
			path: "/v1/payouts/quote",
			body: map[string]any{
				"account_id":     testAccountID.String(),
				"destination_id": h.ports.conversion.destinations[0].ID.String(),
				"amount":         "500000000",
			},
			key: "neverstored-quote-1",
		},
	} {
		t.Run(route.name, func(t *testing.T) {
			res := h.do(route.method, route.path, route.body, "Idempotency-Key", route.key)
			require.Less(t, res.Code, 400, "%s: %s", route.name, res.Body.String())
		})
	}

	stored := h.ports.idem.storedBodies()
	require.NotEmpty(t, stored, "nothing was recorded at all; this test would pass by comparing nothing")
	require.Contains(t, stored, "PostMeVerificationSessions")

	fields := neverStoredFields()
	require.NotEmpty(t, fields)
	for operation, body := range stored {
		for _, field := range fields {
			assert.NotContainsf(t, body, `"`+field+`"`,
				"%s recorded %q, which the product documents as never stored", operation, field)
		}
	}

	// The positive control, twice over: the route that HAS a never-stored field
	// really produced one, and the record kept everything else.
	session := stored["PostMeVerificationSessions"]
	assert.NotContains(t, session, "sandbox:verification/",
		"the single-use link reached the record")
	assert.Contains(t, session, `"session"`, "the record still says which session the key produced")
	assert.True(t, strings.Contains(session, `"resume"`),
		"a replay has to be able to say why the link is not there")
}
