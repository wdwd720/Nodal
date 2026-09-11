package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/profile"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/terms"
)

// The profile, terms and account-lifecycle surface.
//
// Every `/me/...` route is self-scoped by construction: there is no identifier
// in the path, and the subject comes from the request principal, so a caller
// cannot name somebody else's record. Cross-tenant access is not prevented by a
// check that could be forgotten -- it is unrepresentable. The two routes that
// DO name a user are under `/admin/`, behind operator permissions, and one of
// them is a read.

// actorFrom builds the domain actor from the request principal. It is the only
// place a subject enters internal/profile.
func (s *Server) actorFrom(ctx context.Context) (profile.Actor, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return profile.Actor{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	a := profile.Actor{
		UserID:    p.SubjectID,
		ActorType: p.ActorType,
		SessionID: p.SessionID,
		RequestID: observability.RequestID(ctx),
	}
	if r, ok := requestFrom(ctx); ok {
		a.IP = clientIP(r, s.trusted)
		a.UserAgent = r.UserAgent()
	}
	return a, nil
}

// PostMeProfile updates the caller's own profile.
func (s *Server) PostMeProfile(ctx context.Context, request api.PostMeProfileRequestObject) (api.PostMeProfileResponseObject, error) {
	if s.opts.Ports.Profile == nil {
		return nil, errNotWired("user profiles")
	}
	if request.Body == nil {
		return nil, errs.New(errs.CodeValidationFailed, "a body is required")
	}
	actor, err := s.actorFrom(ctx)
	if err != nil {
		return nil, err
	}
	patch := profile.Patch{
		DisplayName: request.Body.DisplayName,
		Handle:      request.Body.Handle,
		Locale:      request.Body.Locale,
		TimeZone:    request.Body.TimeZone,
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.UserProfile, commandMeta, error) {
			p, uerr := s.opts.Ports.Profile.Update(ctx, actor, patch)
			if uerr != nil {
				return api.UserProfile{}, commandMeta{}, uerr
			}
			return toAPIProfile(p), commandMeta{
				Status: http.StatusOK, ResourceType: "user_profile", ResourceID: p.UserID,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostMeProfile200JSONResponse(res.Value), nil
}

// GetMeTermsAcceptances reports the documents, and which the caller has accepted.
func (s *Server) GetMeTermsAcceptances(ctx context.Context, _ api.GetMeTermsAcceptancesRequestObject) (api.GetMeTermsAcceptancesResponseObject, error) {
	if s.opts.Ports.Profile == nil {
		return nil, errNotWired("user profiles")
	}
	actor, err := s.actorFrom(ctx)
	if err != nil {
		return nil, err
	}
	view, err := s.opts.Ports.Profile.Terms(ctx, actor)
	if err != nil {
		return nil, err
	}
	return api.GetMeTermsAcceptances200JSONResponse(toAPITermsState(view, true)), nil
}

// PostMeTermsAcceptances records acceptance of the named documents.
func (s *Server) PostMeTermsAcceptances(ctx context.Context, request api.PostMeTermsAcceptancesRequestObject) (api.PostMeTermsAcceptancesResponseObject, error) {
	if s.opts.Ports.Profile == nil {
		return nil, errNotWired("user profiles")
	}
	if request.Body == nil || len(request.Body.DocumentIds) == 0 {
		return nil, errs.New(errs.CodeValidationFailed, "name at least one document to accept")
	}
	actor, err := s.actorFrom(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]terms.DocumentID, 0, len(request.Body.DocumentIds))
	for _, d := range request.Body.DocumentIds {
		ids = append(ids, terms.DocumentID(d))
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.TermsState, commandMeta, error) {
			view, aerr := s.opts.Ports.Profile.Accept(ctx, actor, ids)
			if aerr != nil {
				return api.TermsState{}, commandMeta{}, aerr
			}
			return toAPITermsState(view, false), commandMeta{
				Status: http.StatusOK, ResourceType: "terms_acceptance", ResourceID: actor.UserID,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostMeTermsAcceptances200JSONResponse(res.Value), nil
}

// GetMeAccount reports the standing of the caller's own account.
func (s *Server) GetMeAccount(ctx context.Context, _ api.GetMeAccountRequestObject) (api.GetMeAccountResponseObject, error) {
	if s.opts.Ports.Profile == nil {
		return nil, errNotWired("user profiles")
	}
	actor, err := s.actorFrom(ctx)
	if err != nil {
		return nil, err
	}
	view, err := s.opts.Ports.Profile.Account(ctx, actor)
	if err != nil {
		return nil, err
	}
	return api.GetMeAccount200JSONResponse(toAPIMyAccount(view, s.clk.Now())), nil
}

// PostMeAccountClose opens a closure request for the caller's own account.
func (s *Server) PostMeAccountClose(ctx context.Context, request api.PostMeAccountCloseRequestObject) (api.PostMeAccountCloseResponseObject, error) {
	if s.opts.Ports.Profile == nil {
		return nil, errNotWired("user profiles")
	}
	actor, err := s.actorFrom(ctx)
	if err != nil {
		return nil, err
	}
	reason := ""
	if request.Body != nil && request.Body.Reason != nil {
		reason = *request.Body.Reason
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MyAccount, commandMeta, error) {
			view, cerr := s.opts.Ports.Profile.RequestClosure(ctx, actor, reason)
			if cerr != nil {
				return api.MyAccount{}, commandMeta{}, cerr
			}
			id := ""
			if view.Closure != nil {
				id = view.Closure.ID
			}
			return toAPIMyAccount(view, s.clk.Now()), commandMeta{
				Status: http.StatusOK, ResourceType: "account_closure_request", ResourceID: id,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostMeAccountClose200JSONResponse(res.Value), nil
}

// PostMeAccountCloseCancel stops the caller's own pending closure request.
func (s *Server) PostMeAccountCloseCancel(ctx context.Context, request api.PostMeAccountCloseCancelRequestObject) (api.PostMeAccountCloseCancelResponseObject, error) {
	if s.opts.Ports.Profile == nil {
		return nil, errNotWired("user profiles")
	}
	actor, err := s.actorFrom(ctx)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MyAccount, commandMeta, error) {
			view, cerr := s.opts.Ports.Profile.CancelClosure(ctx, actor)
			if cerr != nil {
				return api.MyAccount{}, commandMeta{}, cerr
			}
			return toAPIMyAccount(view, s.clk.Now()), commandMeta{
				Status: http.StatusOK, ResourceType: "account_closure_request", ResourceID: actor.UserID,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostMeAccountCloseCancel200JSONResponse(res.Value), nil
}

// GetMeSecurity summarises what the system knows about the caller's security.
//
// Everything here is derived from the session store and the claims the current
// session already carries. It adds no state, holds no secret, and cannot be
// used to enumerate anything: the subject is the caller.
func (s *Server) GetMeSecurity(ctx context.Context, _ api.GetMeSecurityRequestObject) (api.GetMeSecurityResponseObject, error) {
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
	window := effectiveStepUpMaxAge(s.opts.StepUpMaxAge)
	now := s.clk.Now()
	out := api.SecuritySummary{
		Amr:                 p.AMR,
		MfaPresent:          security.HasStrongAMR(p.AMR),
		StepUpMaxAgeSeconds: int(window / time.Second),
		CurrentSessionId:    parseUUIDText(p.SessionID),
	}
	if out.Amr == nil {
		out.Amr = []string{}
	}
	for _, sess := range list {
		if sess.RevokedAt != nil || !sess.ExpiresAt.After(now) {
			continue
		}
		out.ActiveSessions++
		if out.LastLoginAt == nil || sess.CreatedAt.After(*out.LastLoginAt) {
			at := sess.CreatedAt.UTC()
			out.LastLoginAt = &at
		}
	}
	// The last strong authentication is the current session's auth_time, and
	// only when the provider actually asserted a strong method: reporting
	// auth_time as a step-up for a password-only login would tell a user they
	// have an MFA they do not have.
	if out.MfaPresent && !p.AuthTime.IsZero() {
		at := p.AuthTime.UTC()
		out.LastStepUpAt = &at
		until := at.Add(window)
		out.StepUpValidUntil = &until
	}
	return api.GetMeSecurity200JSONResponse(out), nil
}

// GetAdminUsersUserId is the operator support view of one user (PART 38). It is
// a read: nothing on this route changes anything.
func (s *Server) GetAdminUsersUserId(ctx context.Context, request api.GetAdminUsersUserIdRequestObject) (api.GetAdminUsersUserIdResponseObject, error) {
	if s.opts.Ports.Profile == nil {
		return nil, errNotWired("user profiles")
	}
	view, err := s.opts.Ports.Profile.AdminUser(ctx, request.UserId.String())
	if err != nil {
		return nil, err
	}
	return api.GetAdminUsersUserId200JSONResponse(toAPIAdminUser(view, s.clk.Now())), nil
}

// PostAdminUsersUserIdClosure decides a user's open closure request.
func (s *Server) PostAdminUsersUserIdClosure(ctx context.Context, request api.PostAdminUsersUserIdClosureRequestObject) (api.PostAdminUsersUserIdClosureResponseObject, error) {
	if s.opts.Ports.Profile == nil {
		return nil, errNotWired("user profiles")
	}
	if request.Body == nil {
		return nil, errs.New(errs.CodeValidationFailed, "a body is required")
	}
	actor, err := s.actorFrom(ctx)
	if err != nil {
		return nil, err
	}
	target := request.UserId.String()
	decision := profile.ClosureDecision(request.Body.Decision)
	reason := request.Body.Reason
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdminUserView, commandMeta, error) {
			view, derr := s.opts.Ports.Profile.Decide(ctx, actor, target, decision, reason)
			if derr != nil {
				return api.AdminUserView{}, commandMeta{}, derr
			}
			return toAPIAdminUser(view, s.clk.Now()), commandMeta{
				Status: http.StatusOK, ResourceType: "account_closure_request", ResourceID: target,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdminUsersUserIdClosure200JSONResponse(res.Value), nil
}
