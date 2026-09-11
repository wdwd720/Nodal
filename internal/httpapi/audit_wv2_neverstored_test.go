package httpapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reproduction for the second round of the withdrawal-verification audit
// (goal §54). It changes no product code.

// ---------------------------------------------------------------------------
// F-wv2-9 — redactForStorage fails OPEN: on the path its own comment calls
// "the safe direction" it returns nil, and nil means "store the whole body".
//
// The function:
//
//	if err := json.Unmarshal(body, &doc); err != nil {
//	    // ... "An empty record replays as "no body", which is the safe
//	    //      direction."
//	    return nil
//	}
//
// and CommandResult.stored():
//
//	if r.StoredBody != nil { return r.StoredBody }
//	return r.Body
//
// Nil is the sentinel for "there is nothing special to store, keep Body". So
// the two branches that exist to refuse to store a body nobody could inspect
// store the WHOLE body instead -- credential and all -- and the comment states
// the opposite of what happens.
//
// It is not reachable from today's routes, because every command response is a
// struct and marshals to a JSON object. It is one response type away from being
// reachable: a route that returns a list, or `null`, or a string, gets the
// fail-open. D-125 says a route in this area "has to declare" a credential and
// that forgetting "fails a test rather than an audit" -- and
// TestNoNeverStoredFieldReachesTheIdempotencyRecord drives three of the
// journey's command routes, none of which can take this branch.
// ---------------------------------------------------------------------------

func TestAuditWV2_RedactForStorageDoesNotFallBackToTheWholeBody(t *testing.T) {
	t.Parallel()

	const op = "PostMeVerificationSessions"
	require.Contains(t, neverStored, op, "fixture check: this operation declares a never-stored field")

	// A response body carrying the credential, in a shape redactForStorage
	// cannot parse into an object.
	body := []byte(`["https://verify.example/session/single-use-token"]`)
	redacted := redactForStorage(op, body)

	stored := CommandResult{Body: body, StoredBody: redacted}.stored()
	assert.NotEqual(t, string(body), string(stored),
		"F-wv2-9: redactForStorage returned nil for a body it could not inspect, and nil means "+
			"'store Body' -- so the record kept the whole answer, which is the opposite of the "+
			"comment's 'an empty record replays as no body, which is the safe direction'")
	assert.NotContains(t, string(stored), "single-use-token",
		"F-wv2-9: the credential reached idempotency_keys.response_body by the path that exists to keep it out")
}
