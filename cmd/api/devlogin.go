package main

import (
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/nodal/controlplane/internal/auth/devidp"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
)

// The development identity provider sends the browser to a picker page instead
// of a real authorization server. internal/auth/devidp defines the contract —
// the page receives the OIDC parameters and must come back to the application's
// callback with code=<identity>[:mfa] and the same state — but it deliberately
// ships no HTTP handler, because serving one is a composition decision. This
// file is that handler, and it exists only where a dev identity provider is
// allowed at all.
//
// It cannot exist elsewhere, three times over: devLoginHandler refuses to be
// constructed outside LOCAL, TEST and DEV; it re-checks on every request;
// and httpapi.New refuses to mount any route outside the /v1 contract unless
// the environment allows development authentication. config.Validate refuses
// auth mode "dev" outside those environments before any of this runs.

// devLoginPath is where devidp.AuthCodeURL points by default.
const devLoginPath = devidp.DefaultLoginURL

// defaultCallbackPath is the application's OIDC callback within the v1 surface.
const defaultCallbackPath = "/v1/auth/callback"

// maxDevStateLength bounds the state echoed back into the redirect.
const maxDevStateLength = 512

var devLoginTemplate = template.Must(template.New("devlogin").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<title>Development sign-in</title>
<meta name="robots" content="noindex,nofollow">
<style>
 body{font:14px system-ui,sans-serif;margin:3rem auto;max-width:34rem;color:#111}
 h1{font-size:1.1rem} li{margin:.4rem 0} code{background:#f2f2f2;padding:.1rem .3rem}
 .env{background:#fee;border:1px solid #c00;padding:.5rem;border-radius:4px}
</style></head><body>
<p class="env">Development identity provider — environment {{.Env}}. These identities are
fixtures. This page does not exist in STAGING or PROD.</p>
<h1>Choose an identity</h1>
<ul>
{{range .Identities}}<li><code>{{.Name}}</code>:
 <a href="{{.URL}}">sign in</a> &middot;
 <a href="{{.MFAURL}}">sign in with MFA</a></li>
{{end}}</ul>
<p>“with MFA” yields a strong <code>amr</code>, which is what the step-up
endpoints require.</p>
</body></html>
`))

type devLoginChoice struct {
	Name   string
	URL    string
	MFAURL string
}

type devLoginPage struct {
	Env        string
	Identities []devLoginChoice
}

// devLoginHandler serves the development identity picker.
//
// GET with no identity renders the picker. GET with ?identity=<name> (and
// optionally &mfa=1) redirects to the application's callback with the code the
// dev provider's Exchange understands. The identity is validated against
// devidp's closed table, and the redirect target comes from configuration —
// never from a query parameter — so the page cannot be used as an open
// redirect.
func devLoginHandler(env config.Environment, callbackURL string, log *slog.Logger) (http.Handler, error) {
	if !env.AllowsDevAuth() || !devidp.Allowed(string(env)) {
		return nil, fmt.Errorf("%w: env %q", errors.New("dev login page is not allowed"), env)
	}
	if callbackURL == "" {
		callbackURL = defaultCallbackPath
	}
	known := make(map[string]struct{}, len(devidp.Identities()))
	for _, name := range devidp.Identities() {
		known[name] = struct{}{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Re-checked per request so a mis-wiring cannot serve this page.
		if !devidp.Allowed(string(env)) {
			writeDevProblem(w, r, errs.New(errs.CodeNotFound, "no such resource"))
			return
		}
		if r.Method != http.MethodGet {
			writeDevProblem(w, r, errs.New(errs.CodeValidationFailed, "method not allowed for this resource"))
			return
		}

		q := r.URL.Query()
		state := q.Get("state")
		if state == "" || len(state) > maxDevStateLength {
			writeDevProblem(w, r, errs.New(errs.CodeValidationFailed,
				"the dev sign-in page must be reached through /v1/auth/login"))
			return
		}

		identity := q.Get("identity")
		if identity == "" {
			renderDevPicker(w, r, env, state, known, log)
			return
		}
		if _, ok := known[identity]; !ok {
			writeDevProblem(w, r, errs.New(errs.CodeValidationFailed, "unknown development identity"))
			return
		}

		code := identity
		if isTruthy(q.Get("mfa")) {
			code += ":" + devidp.MFASuffix
		}
		location, err := devCallbackURL(callbackURL, code, state)
		if err != nil {
			writeDevProblem(w, r, errs.Wrap(err, errs.CodeInternal, "internal error"))
			return
		}
		log.LogAttrs(r.Context(), slog.LevelInfo, "development sign-in",
			slog.String("identity", identity), slog.String("env", string(env)))
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, location, http.StatusFound)
	}), nil
}

func renderDevPicker(w http.ResponseWriter, r *http.Request, env config.Environment,
	state string, known map[string]struct{}, log *slog.Logger,
) {
	choices := make([]devLoginChoice, 0, len(known))
	for _, name := range devidp.Identities() {
		base := url.Values{"state": {state}, "identity": {name}}
		mfa := url.Values{"state": {state}, "identity": {name}, "mfa": {"1"}}
		choices = append(choices, devLoginChoice{
			Name:   name,
			URL:    devLoginPath + "?" + base.Encode(),
			MFAURL: devLoginPath + "?" + mfa.Encode(),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := devLoginTemplate.Execute(w, devLoginPage{Env: string(env), Identities: choices}); err != nil {
		log.LogAttrs(r.Context(), slog.LevelWarn, "dev sign-in page could not be rendered",
			slog.String("error", err.Error()))
	}
}

// devCallbackURL appends the code and state to the configured callback. The
// host and path always come from configuration.
func devCallbackURL(callback, code, state string) (string, error) {
	u, err := url.Parse(callback)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("code", code)
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func writeDevProblem(w http.ResponseWriter, r *http.Request, err error) {
	errs.WriteError(w, err, r.URL.Path, observability.RequestID(r.Context()))
}
