package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/auth/devidp"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
)

func devHandler(t *testing.T, env config.Environment) http.Handler {
	t.Helper()
	h, err := devLoginHandler(env, defaultCallbackPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	return h
}

// TestDevLoginHandlerIsRefusedOutsideDevelopmentEnvironments: the picker
// bypasses the per-operation authorization table, so it must be impossible to
// construct where real money is at stake.
func TestDevLoginHandlerIsRefusedOutsideDevelopmentEnvironments(t *testing.T) {
	t.Parallel()
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		h, err := devLoginHandler(env, defaultCallbackPath, slog.New(slog.DiscardHandler))
		require.Error(t, err, "%s", env)
		assert.Nil(t, h)
	}
	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev} {
		h, err := devLoginHandler(env, defaultCallbackPath, slog.New(slog.DiscardHandler))
		require.NoError(t, err, "%s", env)
		assert.NotNil(t, h)
	}
}

// TestDevLoginPickerListsEveryIdentity renders the page the dev provider's
// AuthCodeURL points at, and checks it offers every fixed identity in both a
// plain and a strong-authentication form.
func TestDevLoginPickerListsEveryIdentity(t *testing.T) {
	t.Parallel()
	h := devHandler(t, config.EnvLocal)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, devLoginPath+"?state=abc123&nonce=n&code_challenge=c", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	body := rec.Body.String()
	for _, name := range devidp.Identities() {
		assert.Contains(t, body, "identity="+name+"&amp;state=abc123", name)
		assert.Contains(t, body, "identity="+name+"&amp;mfa=1&amp;state=abc123", name)
	}
	assert.Contains(t, body, "LOCAL", "the page names the environment it is serving")
}

// TestDevLoginRedirectsToTheCallbackWithTheProvidersCode: the code shape is
// the one devidp.Exchange understands, and the state is echoed unchanged so
// the single-use login attempt can be found.
func TestDevLoginRedirectsToTheCallbackWithTheProvidersCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		query    string
		wantCode string
	}{
		{"plain", "identity=admin&state=st-1", "admin"},
		{"mfa", "identity=admin&mfa=1&state=st-1", "admin:" + devidp.MFASuffix},
		{"mfa true", "identity=customer-a&mfa=true&state=st-1", "customer-a:" + devidp.MFASuffix},
		{"mfa off", "identity=customer-a&mfa=0&state=st-1", "customer-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := devHandler(t, config.EnvLocal)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, devLoginPath+"?"+tc.query, nil))

			require.Equal(t, http.StatusFound, rec.Code)
			loc, err := url.Parse(rec.Header().Get("Location"))
			require.NoError(t, err)
			assert.Equal(t, defaultCallbackPath, loc.Path)
			assert.Equal(t, tc.wantCode, loc.Query().Get("code"))
			assert.Equal(t, "st-1", loc.Query().Get("state"))
		})
	}
}

// TestDevLoginRefusesBadInput: an unknown identity, a missing state and a
// non-GET method are all refused as problem+json, and nothing is redirected.
func TestDevLoginRefusesBadInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		method string
		target string
		status int
		code   errs.Code
	}{
		{"unknown identity", http.MethodGet, devLoginPath + "?identity=root&state=st-1", 400, errs.CodeValidationFailed},
		{"missing state", http.MethodGet, devLoginPath + "?identity=admin", 400, errs.CodeValidationFailed},
		{"oversized state", http.MethodGet, devLoginPath + "?identity=admin&state=" + strings.Repeat("s", 600), 400, errs.CodeValidationFailed},
		{"post", http.MethodPost, devLoginPath + "?identity=admin&state=st-1", 400, errs.CodeValidationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := devHandler(t, config.EnvLocal)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))
			require.Equal(t, tc.status, rec.Code, "body=%s", rec.Body.String())
			assert.Equal(t, errs.ContentType, rec.Header().Get("Content-Type"))
			assert.Contains(t, rec.Body.String(), string(tc.code))
			assert.Empty(t, rec.Header().Get("Location"))
		})
	}
}

// TestDevLoginIsNotAnOpenRedirect: the callback comes from configuration, so
// no query parameter can move the destination to another host.
func TestDevLoginIsNotAnOpenRedirect(t *testing.T) {
	t.Parallel()
	h := devHandler(t, config.EnvLocal)
	for _, attack := range []string{
		"&redirect_uri=https://evil.test/steal",
		"&callback=https://evil.test/steal",
		"&state=st-1%26redirect_uri=https://evil.test",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
			devLoginPath+"?identity=admin&state=st-1"+attack, nil))
		require.Equal(t, http.StatusFound, rec.Code)
		loc := rec.Header().Get("Location")
		assert.NotContains(t, loc, "evil.test", "the redirect target must come from configuration only")
		parsed, err := url.Parse(loc)
		require.NoError(t, err)
		assert.Empty(t, parsed.Host, "the default callback is same-host and relative")
		assert.Equal(t, defaultCallbackPath, parsed.Path)
	}
}

// TestDevCallbackURLKeepsTheConfiguredTarget covers an explicitly configured
// absolute callback.
func TestDevCallbackURLKeepsTheConfiguredTarget(t *testing.T) {
	t.Parallel()
	got, err := devCallbackURL("https://app.dev.test/v1/auth/callback?tenant=x", "admin:mfa", "st-9")
	require.NoError(t, err)
	u, err := url.Parse(got)
	require.NoError(t, err)
	assert.Equal(t, "app.dev.test", u.Host)
	assert.Equal(t, "/v1/auth/callback", u.Path)
	assert.Equal(t, "admin:mfa", u.Query().Get("code"))
	assert.Equal(t, "st-9", u.Query().Get("state"))
	assert.Equal(t, "x", u.Query().Get("tenant"), "existing query parameters survive")
}

// TestDevLoginPageBodyIsFullyConsumed guards against a template that silently
// writes nothing.
func TestDevLoginPageBodyIsFullyConsumed(t *testing.T) {
	t.Parallel()
	h := devHandler(t, config.EnvDev)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, devLoginPath+"?state=s", nil))
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	assert.Greater(t, len(body), 200)
}
