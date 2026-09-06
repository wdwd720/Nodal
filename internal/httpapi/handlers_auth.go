package httpapi

import (
	"context"
	"net/http"

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
// once, so a replayed callback never yields a session.
func (s *Server) GetAuthLogin(ctx context.Context, request api.GetAuthLoginRequestObject) (api.GetAuthLoginResponseObject, error) {
	if s.opts.Ports.Identity == nil {
		return nil, errNotWired("interactive login")
	}
	r, ok := requestFrom(ctx)
	if !ok {
		return nil, errs.New(errs.CodeInternal, "internal error")
	}
	stepUp := request.Params.StepUp != nil && *request.Params.StepUp
	res, err := s.opts.Ports.Identity.Begin(ctx, identity.BeginRequest{
		StepUp:    stepUp,
		IP:        clientIP(r, s.trusted),
		UserAgent: userAgent(r),
	})
	if err != nil {
		return nil, err
	}
	return redirectResponse{location: res.RedirectURL}, nil
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
	done, err := s.opts.Ports.Identity.Complete(ctx, identity.CompleteRequest{
		Code:      request.Params.Code,
		State:     request.Params.State,
		IP:        clientIP(r, s.trusted),
		UserAgent: userAgent(r),
		RequestID: observability.RequestID(ctx),
	})
	if err != nil {
		return nil, err
	}

	dest := "/"
	if done.ReturnTo != "" {
		dest = done.ReturnTo
	}
	token := done.Issued.Token
	return redirectResponse{
		location: dest,
		before: func(w http.ResponseWriter) {
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
	return api.GetMe200JSONResponse(toAPIPrincipal(p)), nil
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
