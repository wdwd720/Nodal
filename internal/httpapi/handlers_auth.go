package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/nodal/controlplane/internal/auth/httpmw"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/identity"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/webhook"
)

func webhookRequestMeta(s *Server, r *http.Request) webhook.RequestMeta {
	return webhook.RequestMeta{
		RemoteIP:  clientIP(r, s.trusted),
		UserAgent: userAgent(r),
		RequestID: observability.RequestID(r.Context()),
	}
}

// GetAuthLogin starts the OIDC authorization-code + PKCE exchange. The state,
// nonce and verifier are persisted by internal/identity and consumed exactly
// once, so a replayed callback never yields a session -- and the state's digest
// also goes to the browser, so a callback that did not BEGIN in this browser
// never yields one either (F-87; see httpmw.SetLoginState for the attack the
// second half stops).
func (s *Server) GetAuthLogin(ctx context.Context, request api.GetAuthLoginRequestObject) (api.GetAuthLoginResponseObject, error) {
	if s.opts.Ports.Identity == nil {
		return nil, errNotWired("interactive login")
	}
	r, ok := requestFrom(ctx)
	if !ok {
		return nil, errs.New(errs.CodeInternal, "internal error")
	}
	stepUp := request.Params.StepUp != nil && *request.Params.StepUp
	returnTo := ""
	if request.Params.ReturnTo != nil {
		returnTo = *request.Params.ReturnTo
	}
	// The identity service refuses anything that is not a local path, so an
	// open redirect cannot be built here; the app origin is configuration.
	res, err := s.opts.Ports.Identity.Begin(ctx, identity.BeginRequest{
		StepUp:    stepUp,
		ReturnTo:  returnTo,
		IP:        clientIP(r, s.trusted),
		UserAgent: userAgent(r),
	})
	if err != nil {
		return nil, err
	}
	return redirectResponse{
		location: res.RedirectURL,
		before: func(w http.ResponseWriter) {
			httpmw.SetLoginState(w, res.State, s.opts.CookieDomain, s.opts.CookieSecure, identity.DefaultAttemptTTL)
		},
	}, nil
}

// GetAuthCallback completes the exchange and establishes the server-side
// session. The raw token leaves this process exactly once, in the cookie.
func (s *Server) GetAuthCallback(ctx context.Context, request api.GetAuthCallbackRequestObject) (api.GetAuthCallbackResponseObject, error) {
	if s.opts.Ports.Identity == nil {
		return nil, errNotWired("interactive login")
	}
	r, ok := requestFrom(ctx)
	if !ok {
		return nil, errs.New(errs.CodeInternal, "internal error")
	}
	// The browser that finishes the flow must be the one that started it.
	// Without this the state is only a server-side lookup key: it stops a
	// callback being replayed and does nothing about one being planted, and a
	// planted callback signs the victim in as the attacker (F-87).
	//
	// This runs before Complete, so a planted callback does not consume the
	// attacker's attempt row either -- the refusal costs the victim nothing and
	// leaves the attacker's own flow to expire on its own.
	//
	// The copy names the likely cause first (D-103). The login-state cookie is
	// one slot at Path=/, so a second sign-in -- a second tab, or a step-up
	// begun beside a sign-in already in progress -- overwrites the first flow's
	// digest and the older tab lands here. That is the common case by a long
	// way, and telling somebody their sign-in "did not start in this browser"
	// describes an attack when what happened is two tabs. The control does not
	// change: this still refuses, and one slot is what makes the digest
	// unguessable-from-outside rather than merely present (F-182).
	if !httpmw.LoginStateMatches(r, request.Params.State, s.opts.CookieDomain, s.opts.CookieSecure) {
		return nil, errs.New(errs.CodeUnauthenticated,
			"this sign-in did not start in this browser, or a newer sign-in replaced it; start again")
	}
	// The session the browser already holds, when it holds one. A step-up
	// begun from inside the product arrives here with it, and internal/identity
	// rotates that session rather than issuing a second one beside it: a
	// step-up is a privilege change, and leaving the weaker session live means
	// the credential the step-up defends against still works (PART 192, F-177).
	// A cold sign-in carries none and is issued a new session.
	complete := identity.CompleteRequest{
		Code:      request.Params.Code,
		State:     request.Params.State,
		IP:        clientIP(r, s.trusted),
		UserAgent: userAgent(r),
		RequestID: observability.RequestID(ctx),
	}
	if sess, ok := httpmw.SessionFrom(ctx); ok {
		complete.Current = &sess
	}
	done, err := s.opts.Ports.Identity.Complete(ctx, complete)
	if err != nil {
		return nil, err
	}

	dest := postLoginDestination(s.opts.PostLoginURL, done.ReturnTo)
	token := done.Issued.Token
	return redirectResponse{
		location: dest,
		before: func(w http.ResponseWriter) {
			// The login-state cookie has done its work; a flow that completed
			// leaves nothing behind for a later one to match against.
			httpmw.ClearLoginState(w, s.opts.CookieDomain, s.opts.CookieSecure)
			httpmw.SetSessionCookie(w, s.opts.CookieName, token, s.opts.CookieDomain, s.opts.CookieSecure, s.opts.SessionTTL)
		},
	}, nil
}

// PostAuthLogout revokes the current session and clears the cookie.
func (s *Server) PostAuthLogout(ctx context.Context, _ api.PostAuthLogoutRequestObject) (api.PostAuthLogoutResponseObject, error) {
	out := logoutResponse{name: s.opts.CookieName, domain: s.opts.CookieDomain, secure: s.opts.CookieSecure}
	sess, ok := httpmw.SessionFrom(ctx)
	if !ok {
		// Nothing to revoke; still clear the cookie so a stale value goes
		// away. Logout is idempotent by construction.
		return out, nil
	}
	if s.opts.Ports.Identity == nil {
		return nil, errNotWired("interactive login")
	}
	r, _ := requestFrom(ctx)
	req := identity.CompleteRequest{RequestID: observability.RequestID(ctx)}
	if r != nil {
		req.IP = clientIP(r, s.trusted)
		req.UserAgent = userAgent(r)
	}
	if err := s.opts.Ports.Identity.Logout(ctx, sess, req); err != nil {
		return nil, err
	}
	return out, nil
}

// GetMe returns the principal the session resolved to. Roles come from the
// operator directory, never from identity-provider claims.
func (s *Server) GetMe(ctx context.Context, _ api.GetMeRequestObject) (api.GetMeResponseObject, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return nil, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	out := toAPIPrincipal(p, effectiveStepUpMaxAge(s.opts.StepUpMaxAge))
	// Additive: the product surfaces attach the profile and onboarding objects
	// when this deployment has them, and change nothing above (handlers_me.go).
	s.meExtras(ctx, p, &out)
	return api.GetMe200JSONResponse(out), nil
}

// GetSessions lists the caller's own sessions (PART 192 device listing).
func (s *Server) GetSessions(ctx context.Context, _ api.GetSessionsRequestObject) (api.GetSessionsResponseObject, error) {
	if s.opts.Ports.Sessions == nil {
		return nil, errNotWired("session management")
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return nil, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	list, err := s.opts.Ports.Sessions.ListForSubject(ctx, p.SubjectID)
	if err != nil {
		return nil, err
	}
	return api.GetSessions200JSONResponse(toAPISessions(list, p.SessionID)), nil
}

// DeleteSessionsSessionId revokes one of the caller's own sessions. A session
// that does not belong to the caller is reported as not found, so the endpoint
// cannot be used to probe for other people's session ids.
func (s *Server) DeleteSessionsSessionId(ctx context.Context, request api.DeleteSessionsSessionIdRequestObject) (api.DeleteSessionsSessionIdResponseObject, error) {
	if s.opts.Ports.Sessions == nil {
		return nil, errNotWired("session management")
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return nil, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	list, err := s.opts.Ports.Sessions.ListForSubject(ctx, p.SubjectID)
	if err != nil {
		return nil, err
	}
	target := request.SessionId.String()
	owned := false
	for _, sess := range list {
		if sess.ID == target {
			owned = true
			break
		}
	}
	if !owned {
		return nil, errs.New(errs.CodeNotFound, "no such session")
	}
	if err := s.opts.Ports.Sessions.Revoke(ctx, target); err != nil {
		return nil, err
	}
	return api.DeleteSessionsSessionId204Response{}, nil
}

// postLoginDestination is where the browser goes once the cookie is set.
//
// The API's own root is a 404 problem document, so "/" is only right when the
// web app is served from the API's origin. When the app has its own origin the
// deployment names it, and a local return-to path is resolved beneath that
// origin rather than beneath the API's.
//
// # Why the path is checked here as well
//
// The sentence this comment used to end with -- "Neither input is the user's,
// so this is not an open redirect: the base is configuration and the path is
// what the login service recorded" -- was half true and therefore wrong. The
// base is configuration. The path is the CALLER's `return_to`, recorded
// verbatim by internal/identity, and that service's guard rejected "//host"
// and accepted "/\host". With CP_AUTH_POST_LOGIN_URL empty -- an optional
// variable, which no rule required until now -- the Location header on the
// callback that sets the session cookie was the caller's string, and a browser
// resolves "/\evil.example" through the authority state to
// https://evil.example/ (F-146).
//
// identity.IsLocalPath now refuses those at the door. This is the second
// refusal, at the point of use, because a redirect built from a stored value
// should not depend on the writer of that value having been careful: the row
// could predate the guard, or arrive by some other path tomorrow. Anything
// that is not a plain local path becomes "/", which is always safe and, on a
// deployment with a base, is the app's own home.
func postLoginDestination(base, returnTo string) string {
	if returnTo == "" || !identity.IsLocalPath(returnTo) {
		returnTo = "/"
	}
	if base == "" {
		return returnTo
	}
	return strings.TrimRight(base, "/") + returnTo
}
