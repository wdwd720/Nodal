package httpapi

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// canonicalPathIdentifiers refuses a request whose path spells an identifier in
// any form but the canonical one.
//
// openapi.yaml declares every identifier as "UUIDv7 in canonical form", but
// google/uuid's parser is generous: it accepts "{01a0…}", "urn:uuid:01a0…" and
// the 32-character undashed form, and the generated server normalizes all of
// them before a handler ever sees the value. So one resource was addressable by
// four different strings.
//
// That was not an authorization bypass — the tenant check still ran and still
// returned only the caller's own account. It is refused anyway, because every
// control that keys off the raw path rather than the parsed value silently
// splits across those spellings: per-resource rate limit buckets, cache keys, a
// WAF or proxy rule naming a resource, and the audit trail an operator greps
// when reconstructing who touched what. Accepting one spelling and one only
// keeps those controls counting the same thing, and it costs a legitimate
// client nothing: every identifier this system emits is already canonical.
//
// Only segments that genuinely parse as a UUID are judged. A segment that is
// not UUID-shaped at all is left alone, so opaque path values are unaffected.
func canonicalPathIdentifiers() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The DECODED path is deliberate, and it is what chi routes on.
			// Judging the escaped form instead lets any spelling hide behind
			// percent-encoding: "{01a0…}" arrives as "%7B01a0…%7D", which does
			// not parse as a UUID, so the check waves it through and the router
			// then decodes it and binds it anyway. Checking the same string the
			// router uses is the only way the two cannot disagree.
			for _, seg := range strings.Split(r.URL.Path, "/") {
				if seg == "" {
					continue
				}
				if bad, canonical := nonCanonicalUUID(seg); bad {
					writeProblem(w, r, validationError("path",
						"identifier must be a canonical UUID (expected "+canonical+")"))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// nonCanonicalUUID reports whether seg parses as a UUID but is not written in
// canonical 8-4-4-4-12 form, and returns the canonical spelling for the error.
// Case is not the test: RFC 4122 makes hex input case-insensitive, and an
// uppercase canonical UUID addresses the same resource under every control that
// lowercases it. Shape is the test.
func nonCanonicalUUID(seg string) (bool, string) {
	u, err := uuid.Parse(seg)
	if err != nil {
		return false, "" // not an identifier; not ours to judge
	}
	if strings.EqualFold(seg, u.String()) {
		return false, ""
	}
	return true, u.String()
}
