package identity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsLocalPath (F-146).
//
// The guard this replaced was `HasPrefix("/") && !HasPrefix("//")`, and
// "/\evil.example" walked through it. A browser implementing the WHATWG URL
// standard resolves a special-scheme relative reference beginning "/\" through
// the relative-slash state into the authority state -- "\" is a slash for
// http(s) -- so the browser goes to https://evil.example/. That value reached
// the Location header of the response that sets the session cookie.
//
// The refused list is the point of this test and the ACCEPTED list is the rest
// of it: a guard that also refuses the deep link a customer was reading has
// broken the feature return_to exists for.
func TestIsLocalPath(t *testing.T) {
	t.Parallel()

	t.Run("a path inside this app", func(t *testing.T) {
		t.Parallel()
		for _, ok := range []string{
			"/",
			"/home",
			"/portfolio",
			"/markets/01J0000000000000000000000",
			"/markets/abc?tab=trades&after=01J0",
			"/markets/abc#chart",
			"/settings/security",
			"/a",
			"/deep/link/with/many/segments",
			"/with%20percent%20encoding",
			"/trailing/",
			"/?only=query",
			"/#only-fragment",
		} {
			assert.Truef(t, IsLocalPath(ok), "%q is a path a customer can be on and was refused", ok)
		}
	})

	t.Run("anything that can name a host", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{
			"",                // no destination at all
			"//evil.example",  // the form the old guard did catch
			`/\evil.example`,  // the one it did not: this is the finding
			`/\/evil.example`, // and the same trick with a slash after it
			`//\evil.example`,
			`/\\evil.example`,
			"https://evil.example/x",
			"http://evil.example",
			"//evil.example/looks/like/a/path",
			"evil.example",        // no leading slash: relative, resolves under the API
			"\\\\evil.example",    // a UNC-shaped reference
			"mailto:a@b.example",  // opaque
			"javascript:alert(1)", //nolint:gosec // a refused input, not a credential
			"/\tevil.example",     // a tab URL parsers strip, leaving "//evil.example"
			"/\nevil.example",     // the same with a newline
			"/\revil.example",     // and a carriage return
			"/\x00evil.example",   // a NUL, which truncates a C string
			"/\u0085evil.example", // NEL: a Unicode control character
			" /home",              // leading space: not a path, and space is not trimmed here
			"/home\nLocation: /x", // header injection shaped
			"\\evil.example",      // a bare backslash start
			"///evil.example",     // three slashes still reach the authority state
			"https:/evil.example", // one slash: still has a scheme
		} {
			assert.Falsef(t, IsLocalPath(bad), "IsLocalPath accepted %q", bad)
		}
	})

	// Two near-misses that are accepted, deliberately, because the standard
	// says they stay here.
	//
	// The WHATWG relative-slash state looks at the character AFTER the leading
	// "/" and enters the authority state only for "/" or (special scheme) "\\".
	// Anything else -- including "." and "%" -- goes to the path state with the
	// base URL's host, so a dot segment or a percent-encoded backslash lands in
	// the path of this app rather than naming a host. They are listed because a
	// future tightening that refuses them is refusing a local path, which is
	// the failure mode a guard like this actually has.
	t.Run("near misses that resolve here", func(t *testing.T) {
		t.Parallel()
		for _, ok := range []string{
			`/./\evil.example`,     // "." is not a slash, so the path state takes it
			"/%5Cevil.example@x/y", // %5C is not decoded before the authority decision
		} {
			assert.Truef(t, IsLocalPath(ok), "%q resolves under this origin and was refused", ok)
		}
	})
}
