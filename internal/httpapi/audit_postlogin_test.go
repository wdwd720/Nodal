package httpapi

// F-cfg-11 (goal Section 54, config-deploy audit).
//
// postLoginDestination returns the caller's own `return_to` verbatim as the
// redirect Location when CP_AUTH_POST_LOGIN_URL is empty, and the only guard
// on return_to rejects "//" but not "/\".
//
// internal/identity/login.go:98 is the whole validation:
//
//	if req.ReturnTo != "" && (!strings.HasPrefix(req.ReturnTo, "/") ||
//	    strings.HasPrefix(req.ReturnTo, "//")) { ... refuse }
//
// "/\evil.example" passes it. internal/httpapi/handlers_auth.go:204-212 then
// does:
//
//	if base == "" { return returnTo }
//
// so GetAuthCallback answers `Location: /\evil.example` in the same response
// that sets the session cookie. Every browser implementing the WHATWG URL
// standard resolves a special-scheme relative reference beginning "/\" through
// the relative-slash state into the authority state -- "\" is a slash for
// http(s) -- so the browser goes to https://evil.example/. That is the
// standard backslash bypass of a "//" open-redirect check.
//
// It is NOT reachable on the shipped launch tier: render.yaml sets
// CP_AUTH_POST_LOGIN_URL=https://app-nodal.actorvia.xyz/, and with a non-empty
// base the host is pinned and the backslash lands in the path. It is reachable
// on any deployment that leaves the variable unset, which nothing prevents:
// CP_AUTH_POST_LOGIN_URL is `opt(...)` in internal/config/load.go:546, no rule
// requires it in STAGING/PROD, and its LOCAL/TEST default is only applied where
// defaults are allowed. openapi.yaml:47 describes return_to as "appended to the
// app origin the deployment configures", which is true only when one is.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditConfigDeploy_PostLoginDestinationCannotLeaveTheOrigin(t *testing.T) {
	t.Parallel()

	// Two things the guard in internal/identity accepts. Reproduced as literals
	// rather than by calling Begin, because the guard is one string comparison
	// and calling Begin needs a database.
	for _, returnTo := range []string{`/\evil.example`, `/\/evil.example`} {
		t.Run(returnTo, func(t *testing.T) {
			// The guard internal/identity/login.go:98 applies, verbatim.
			accepted := !(returnTo != "" && (returnTo[0] != '/' || (len(returnTo) > 1 && returnTo[1] == '/')))
			require.True(t, accepted,
				"identity.Begin already refuses %q; this finding is closed", returnTo)

			// A deployment that set the variable: the host is pinned.
			withBase := postLoginDestination("https://app-nodal.actorvia.xyz/", returnTo)
			assert.Equal(t, "https://app-nodal.actorvia.xyz"+returnTo, withBase,
				"with a configured origin the backslash lands in the path, which is correct")

			// A deployment that did not: the Location is the caller's string.
			withoutBase := postLoginDestination("", returnTo)
			assert.NotEqual(t, returnTo, withoutBase,
				"GetAuthCallback answers `Location: %s` while setting the session cookie. A browser "+
					"resolves a special-scheme reference beginning \"/\\\" through the authority state, "+
					"so it lands on https://evil.example/ -- an open redirect off the freshly "+
					"authenticated callback. CP_AUTH_POST_LOGIN_URL is optional and no rule requires "+
					"it in STAGING/PROD, so an empty base is a supported configuration.", returnTo)
		})
	}
}
