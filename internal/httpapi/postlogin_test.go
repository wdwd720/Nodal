package httpapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The API's own root is a 404 problem document, so the callback must send a
// customer to the web app's origin when the app is hosted apart from the API
// -- and must keep sending them to "/" when it is not, which is what every
// existing browser test relies on.
func TestPostLoginDestination(t *testing.T) {
	t.Parallel()
	cases := []struct{ base, returnTo, want string }{
		{"", "", "/"},
		{"", "/portfolio", "/portfolio"},
		{"https://app.test/", "", "https://app.test/"},
		{"https://app.test", "", "https://app.test/"},
		{"https://app.test/", "/portfolio", "https://app.test/portfolio"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, postLoginDestination(c.base, c.returnTo), "base=%q returnTo=%q", c.base, c.returnTo)
	}
}
