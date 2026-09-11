package httpapi

// F-146, raised as F-cfg-11 by the config-deploy audit (goal Section 54).
//
// postLoginDestination returned the caller's own `return_to` verbatim as the
// redirect Location when CP_AUTH_POST_LOGIN_URL was empty, and the only guard
// on return_to rejected "//" but not "/\".
//
// internal/identity/login.go:98 was the whole validation:
//
//	if req.ReturnTo != "" && (!strings.HasPrefix(req.ReturnTo, "/") ||
//	    strings.HasPrefix(req.ReturnTo, "//")) { ... refuse }
//
// "/\evil.example" passed it. internal/httpapi/handlers_auth.go then did:
//
//	if base == "" { return returnTo }
//
// so GetAuthCallback answered `Location: /\evil.example` in the same response
// that sets the session cookie. Every browser implementing the WHATWG URL
// standard resolves a special-scheme relative reference beginning "/\" through
// the relative-slash state into the authority state -- "\" is a slash for
// http(s) -- so the browser goes to https://evil.example/. That is the
// standard backslash bypass of a "//" open-redirect check.
//
// It was NOT reachable on the shipped launch tier: render.yaml sets
// CP_AUTH_POST_LOGIN_URL=https://app-nodal.actorvia.xyz/, and with a non-empty
// base the host is pinned and the backslash lands in the path. It was
// reachable on any deployment that left the variable unset, which nothing
// prevented: CP_AUTH_POST_LOGIN_URL was `opt(...)` in internal/config/load.go
// and no rule required it in STAGING/PROD.
//
// # What the fix changed, and what this file now asserts
//
// Three things, so that no single one of them is load-bearing:
//
//   - identity.IsLocalPath refuses a second character of "/" or "\", any
//     control character, and anything url.Parse reads as carrying a scheme or
//     a host. identity.Begin uses it, so the row is never written.
//   - postLoginDestination applies the same test at the point of USE and falls
//     back to "/" -- including when a base IS configured. The auditor's note
//     that "with a configured origin the backslash lands in the path, which is
//     correct" is true, and the destination is dropped anyway: a redirect built
//     from a stored value should not depend on the writer of that value having
//     been careful.
//   - RuleOIDCConfigured requires CP_AUTH_POST_LOGIN_URL in STAGING and PROD,
//     so the empty base the finding needs is no longer a configuration a
//     deployed environment can have (internal/config/validate_test.go).

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/identity"
)

func TestAuditConfigDeploy_PostLoginDestinationCannotLeaveTheOrigin(t *testing.T) {
	t.Parallel()

	// Two things the old guard in internal/identity accepted. Reproduced as
	// literals rather than by calling Begin, because the guard is a string test
	// and calling Begin needs a database.
	for _, returnTo := range []string{`/\evil.example`, `/\/evil.example`} {
		t.Run(returnTo, func(t *testing.T) {
			t.Parallel()

			// The guard internal/identity/login.go:98 used to apply. Written as
			// the refusal it expressed rather than transcribed, because the
			// original's double negative is what made it easy to get wrong.
			refusedBefore := returnTo != "" && (returnTo[0] != '/' || (len(returnTo) > 1 && returnTo[1] == '/'))
			acceptedBefore := !refusedBefore
			require.True(t, acceptedBefore,
				"the OLD guard already refused %q; this reproduction no longer reproduces anything", returnTo)

			// And the one that replaced it does not.
			assert.Falsef(t, identity.IsLocalPath(returnTo),
				"identity.Begin would still record %q, so the row the redirect is built from is attacker-shaped", returnTo)

			// A deployment that set the variable: the host is pinned, and the
			// malformed path is dropped rather than resolved beneath it.
			withBase := postLoginDestination("https://app-nodal.actorvia.xyz/", returnTo)
			assert.Equal(t, "https://app-nodal.actorvia.xyz/", withBase,
				"with a configured origin the backslash lands in the path, which is harmless -- and it is "+
					"still refused, because the point of use must not depend on the writer having been careful")
			u, err := url.Parse(withBase)
			require.NoError(t, err)
			assert.Equal(t, "app-nodal.actorvia.xyz", u.Host)

			// A deployment that did not: the Location was the caller's string.
			withoutBase := postLoginDestination("", returnTo)
			assert.NotEqual(t, returnTo, withoutBase,
				"GetAuthCallback answers `Location: %s` while setting the session cookie. A browser "+
					"resolves a special-scheme reference beginning \"/\\\" through the authority state, "+
					"so it lands on https://evil.example/ -- an open redirect off the freshly "+
					"authenticated callback", returnTo)
			assert.Equal(t, "/", withoutBase, "the fallback is the app's own root")
		})
	}
}

// TestPostLoginDestination_HonoursARealPathAndRefusesEverythingElse.
//
// The companion to the reproduction above: the refusal is worth nothing if it
// also refuses the deep link a customer was actually reading, which is the
// whole reason return_to exists.
func TestPostLoginDestination_HonoursARealPathAndRefusesEverythingElse(t *testing.T) {
	t.Parallel()

	const base = "https://app-nodal.actorvia.xyz/"

	t.Run("a local path is honoured", func(t *testing.T) {
		t.Parallel()
		for path, want := range map[string]string{
			"/portfolio":                    "https://app-nodal.actorvia.xyz/portfolio",
			"/markets/abc?tab=trades":       "https://app-nodal.actorvia.xyz/markets/abc?tab=trades",
			"/markets/abc#chart":            "https://app-nodal.actorvia.xyz/markets/abc#chart",
			"/":                             "https://app-nodal.actorvia.xyz/",
			"/settings/security":            "https://app-nodal.actorvia.xyz/settings/security",
			"/activity?after=01J0000000000": "https://app-nodal.actorvia.xyz/activity?after=01J0000000000",
		} {
			require.Truef(t, identity.IsLocalPath(path), "%q is a path a customer can be on", path)
			assert.Equal(t, want, postLoginDestination(base, path))
			assert.Equal(t, path, postLoginDestination("", path), "same-origin deployments keep the path as-is")
		}
	})

	t.Run("anything that can leave the origin is refused", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{
			"//evil.example",
			`/\evil.example`,
			`/\/evil.example`,
			`//\evil.example`,
			"https://evil.example/x",
			"http://evil.example",
			"evil.example",
			"javascript:alert(1)", //nolint:gosec // G101 is not in play; this is a refused input
			"/\tevil.example",     // a tab a URL parser strips, leaving "//evil.example"
			"/\nevil.example",
			"/\revil.example",
			"/\x00evil.example",
			"\\\\evil.example",
			"",
		} {
			assert.Falsef(t, identity.IsLocalPath(bad), "identity.IsLocalPath accepted %q", bad)
			assert.Equalf(t, "/", postLoginDestination("", bad), "postLoginDestination(\"\", %q) left the origin", bad)

			got := postLoginDestination(base, bad)
			u, err := url.Parse(got)
			require.NoErrorf(t, err, "%q produced an unparseable destination %q", bad, got)
			assert.Equalf(t, "app-nodal.actorvia.xyz", u.Host,
				"postLoginDestination(base, %q) = %q, which is not the app's origin", bad, got)
		}
	})
}
