package httpapi

import (
	"encoding/json"
	"sort"
)

// What must not be written down, per route (D-125, F-231).
//
// ADR-0025 §3, of the hosted verification link:
//
//	"The hosted URL is not stored. Those links are single-use, expire in
//	 minutes, and are a credential for resuming somebody else's identity
//	 check; one is handed to the browser that asked for it and written down
//	 nowhere."
//
// It was written down. `POST /v1/me/verification/sessions` is a Mutating
// operation, so runCommand marshalled its whole response into
// `idempotency_keys.response_body` and left it there for the idempotency TTL --
// twenty-four hours by default -- in a row `cp_readonly` and `cp_ops` may SELECT
// and `cp_app` may not DELETE. A credential the documents say is handed to one
// browser and kept nowhere was in a table two roles can read, for a day, for
// every session anybody started.
//
// The fix is not "do not make that route idempotent". Idempotency on a command
// route is how a retry stops starting a second identity check, and PART 36 is
// not negotiable for one endpoint. What is negotiable is what the RECORD
// contains: the caller gets the whole response, the store gets the response
// minus the fields the product documents as never stored, and a replay answers
// with the session and a sentence saying the link was not kept.

// neverStored names, per operation, the response fields that must never reach
// the idempotency record.
//
// It is keyed by operation rather than by type because the generated API types
// live in internal/gen/api and cannot carry a method; a route added to this
// area without an entry here is caught by
// TestNoNeverStoredFieldReachesTheIdempotencyRecord, which drives
// every command route in the withdrawal journey and reads what landed.
var neverStored = map[string][]string{
	// The single-use hosted link, and only it. The session's own expiry stays:
	// it is a fact about the attempt rather than a credential, a replay is
	// entitled to it, and the same field name on a quote is a price's expiry
	// that MUST be recorded -- which is why the list is per operation and per
	// field rather than a set of forbidden names.
	"PostMeVerificationSessions": {"hosted_url"},
}

// resumeSentence is what a replay says in place of the link.
//
// It is a sentence rather than an omission because the client has to do
// something: the key it retried with has a recorded answer, that answer cannot
// carry the credential, and the way to get another link is a new session under
// a new key. Saying nothing would leave a browser waiting for a field that is
// never coming.
const resumeSentence = "the single-use link for this session was handed to the browser that asked for it and is " +
	"not stored, so this replay cannot return it; start a new session to get another"

// emptyDocument is what this function stores when it cannot inspect a body that
// belongs to an operation declaring a never-stored field.
//
// It is an explicit empty JSON object rather than nil. Nil is CommandResult's
// sentinel for "there is nothing special to store, keep Body" -- so the two
// branches below that exist to refuse to store a body nobody could inspect were
// storing the WHOLE body, credential and all, while their comments said the
// opposite (F-267). An empty document replays as a body with no fields in it,
// which is the safe direction the comments always claimed.
var emptyDocument = []byte("{}")

// redactForStorage returns what may be persisted as the idempotent record for
// an operation, given the response body the caller is getting.
//
// A body with nothing to remove is returned unchanged, so the ordinary route
// pays one map lookup and nothing else.
func redactForStorage(operation string, body []byte) []byte {
	fields := neverStored[operation]
	if len(fields) == 0 || len(body) == 0 {
		return body
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		// Not an object: nothing to strip by name, and storing a body this
		// function cannot read would be storing something nobody has checked.
		// An empty record replays as "no body", which is the safe direction --
		// and saying so requires an empty DOCUMENT, because nil means "keep the
		// whole body" one function along.
		return emptyDocument
	}
	if doc == nil {
		// A literal `null` parses into a nil map without error. It carries no
		// field to strip and it is not a document either, so it is treated the
		// same way as a body that would not parse at all.
		return emptyDocument
	}
	removed := false
	for _, f := range fields {
		if _, ok := doc[f]; ok {
			delete(doc, f)
			removed = true
		}
	}
	if !removed {
		return body
	}
	doc["resume"] = mustJSON(resumeSentence)
	out, err := json.Marshal(doc)
	if err != nil {
		// The redacted document did not marshal. Whatever is in it, the body it
		// came from carries a field this operation declares must never be
		// written down, so the record gets nothing rather than the original.
		return emptyDocument
	}
	return out
}

// neverStoredFields is the flattened set, for the test that asserts no stored
// body anywhere in this area contains one.
func neverStoredFields() []string {
	seen := map[string]bool{}
	var out []string
	for _, fields := range neverStored {
		for _, f := range fields {
			if seen[f] {
				continue
			}
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// A string marshals or nothing does.
		return json.RawMessage(`""`)
	}
	return b
}
