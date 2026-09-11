package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/verification"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// The verification surface (goal §19, §20, §21, §24).
//
// One sentence governs every response in this file, and it is the distinction
// §19 says the architecture must preserve:
//
//	"Verify your identity to enable withdrawal eligibility."
//	NOT "Verify now to turn your Credits into cash."
//
// So nothing here returns a balance, a Credit quantity or an amount of money.
// It returns a state, a level, the evidence behind that level, and what to do
// next. What the level then permits is `/me/eligibility`'s answer, and it is a
// different answer computed by a different engine from different inputs.

func toAPIVerificationSession(s verification.Session) api.VerificationSession {
	out := api.VerificationSession{
		SessionId: toUUID(s.ID),
		Status:    api.VerificationSessionStatus(s.Status),
		Purpose:   api.VerificationSessionPurpose(s.Purpose),
		Provider:  s.Provider,
		Sandbox:   s.Sandbox,
		CreatedAt: s.CreatedAt.UTC(),
	}
	if s.ProviderRef != "" {
		out.ProviderRef = ptr(s.ProviderRef)
	}
	if s.JurisdictionCountry != "" {
		out.JurisdictionCountry = ptr(s.JurisdictionCountry)
	}
	if s.JurisdictionRegion != "" {
		out.JurisdictionRegion = ptr(s.JurisdictionRegion)
	}
	if s.RulesVersion != "" {
		out.RulesVersion = ptr(s.RulesVersion)
	}
	if s.FailureReason != "" {
		out.FailureReason = ptr(s.FailureReason)
	}
	if s.ExpiresAt != nil {
		out.ExpiresAt = ptr(s.ExpiresAt.UTC())
	}
	return out
}

func toAPIVerificationProfile(s verification.Snapshot) api.VerificationProfile {
	out := api.VerificationProfile{
		AccountId:             toUUID(s.AccountID),
		State:                 api.VerificationState(s.State),
		Level:                 api.VerificationLevel(s.Level),
		PayoutReady:           s.PayoutReady(),
		JurisdictionSupported: s.JurisdictionSupported,
		MinimumAge:            s.MinimumAge,
		AgeVerified:           s.AgeVerified,
		SanctionsState:        api.VerificationProfileSanctionsState(s.SanctionsState),
		Sandbox:               s.Sandbox,
		RulesVersion:          s.RulesVersion,
	}
	if s.Jurisdiction.Country != "" {
		out.JurisdictionCountry = ptr(s.Jurisdiction.Country)
	}
	if s.Jurisdiction.Region != "" {
		out.JurisdictionRegion = ptr(s.Jurisdiction.Region)
	}
	if len(s.JurisdictionRefusals) > 0 {
		out.JurisdictionRefusals = ptr(append([]string(nil), s.JurisdictionRefusals...))
	}
	if len(s.Restrictions) > 0 {
		out.Restrictions = ptr(append([]string(nil), s.Restrictions...))
	}
	if s.VerifiedAt != nil {
		out.VerifiedAt = ptr(s.VerifiedAt.UTC())
	}
	if s.ExpiresAt != nil {
		out.ExpiresAt = ptr(s.ExpiresAt.UTC())
	}
	if s.Session != nil {
		out.Session = ptr(toAPIVerificationSession(*s.Session))
	}
	checks := make([]api.VerificationCheck, 0, len(s.Checks))
	for _, c := range s.Checks {
		item := api.VerificationCheck{
			Kind:         api.VerificationCheckKind(c.Kind),
			Outcome:      api.VerificationOutcome(c.Outcome),
			Provider:     c.Provider,
			RulesVersion: c.RulesVersion,
			Sandbox:      c.Sandbox,
			RecordedAt:   c.RecordedAt.UTC(),
		}
		if c.ProviderRef != "" {
			item.ProviderRef = ptr(c.ProviderRef)
		}
		if c.Detail != "" {
			item.Detail = ptr(c.Detail)
		}
		checks = append(checks, item)
	}
	out.Checks = ptr(checks)
	missing := make([]api.VerificationRequirement, 0, len(s.Missing))
	for _, m := range s.Missing {
		missing = append(missing, api.VerificationRequirement{
			Code: m.Code, Detail: m.Detail, Action: api.VerificationRequirementAction(m.Action),
		})
	}
	out.Missing = ptr(missing)
	if s.Provider != "" {
		out.Provider = ptr(s.Provider)
	}
	out.ProviderAvailability = ptr(api.VerificationProfileProviderAvailability(s.ProviderAvailability))
	return out
}

// GetMeVerification returns the financial profile area of §24.
func (s *Server) GetMeVerification(ctx context.Context, request api.GetMeVerificationRequestObject) (api.GetMeVerificationResponseObject, error) {
	if s.opts.Ports.Verification == nil {
		return nil, errNotWired("identity verification")
	}
	accountID, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.opts.Ports.Verification.Profile(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return api.GetMeVerification200JSONResponse(toAPIVerificationProfile(snapshot)), nil
}

// PostMeVerificationSessions opens a provider-hosted verification.
//
// The jurisdiction comes from the request body and from nowhere else. It is
// NOT derived from the caller's address: a geolocated IP is a legal
// determination wearing a network header's clothes, and this system refuses to
// make one.
func (s *Server) PostMeVerificationSessions(ctx context.Context, request api.PostMeVerificationSessionsRequestObject) (api.PostMeVerificationSessionsResponseObject, error) {
	if s.opts.Ports.Verification == nil {
		return nil, errNotWired("identity verification")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	purpose := verification.PurposePayoutKYC
	if request.Body.Purpose != nil {
		parsed, perr := verification.ParsePurpose(string(*request.Body.Purpose))
		if perr != nil {
			return nil, validationError("purpose", "purpose must be PAYOUT_KYC or ENHANCED")
		}
		purpose = parsed
	}
	country := strings.ToUpper(strings.TrimSpace(request.Body.JurisdictionCountry))
	if len(country) != 2 {
		return nil, validationError("jurisdiction_country",
			"jurisdiction_country must be an ISO 3166-1 alpha-2 code; it is never inferred from your network address")
	}
	region := ""
	if request.Body.JurisdictionRegion != nil {
		region = strings.ToUpper(strings.TrimSpace(*request.Body.JurisdictionRegion))
	}

	cmd := StartVerification{
		AccountID:     accountID,
		Purpose:       purpose,
		Jurisdiction:  rules.Jurisdiction{Country: country, Region: region},
		CorrelationID: observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StartedVerification, commandMeta, error) {
			started, serr := s.opts.Ports.Verification.Start(ctx, cmd)
			if serr != nil {
				return api.StartedVerification{}, commandMeta{}, serr
			}
			out := api.StartedVerification{
				Session: toAPIVerificationSession(started.Session),
				Sandbox: started.Sandbox,
			}
			if started.HostedURL != "" {
				out.HostedUrl = ptr(started.HostedURL)
			}
			if started.ExpiresAt != nil {
				out.ExpiresAt = ptr(started.ExpiresAt.UTC())
			}
			if started.SandboxControlPath != "" {
				out.SandboxControlPath = ptr(started.SandboxControlPath)
			}
			return out, commandMeta{
				Status:       http.StatusCreated,
				ResourceType: "verification_session",
				ResourceID:   started.Session.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostMeVerificationSessions200JSONResponse(res.Value), nil
	}
	return api.PostMeVerificationSessions201JSONResponse(res.Value), nil
}

// GetMeVerificationSessionsSessionId polls the provider and records what it
// says.
//
// It is a GET that changes state, which is deliberate and is the only shape
// that works: the customer's browser comes back from a hosted flow and the
// server has to find out what happened. It is idempotent — a status that has
// not moved records nothing — and it never trusts the redirect itself.
//
// Because it changes state it is scoped with accountScopeWrite, not with the
// read-grade helper. The distinction is F-36's: the operator override on
// accountScope is a READ permission, and using it to authorize a write let an
// ADMIN act as any customer. This route was the one place a GET could be aimed
// at somebody else's account and MOVE something -- an operator holding
// account:read_any drove another person's verification forward, which is the
// F-102 shape on a route the /me walk could not see because its identifier is a
// query parameter (F-178). An operator who needs to know where a customer's
// verification stands reads the admin plane, which does not poll.
func (s *Server) GetMeVerificationSessionsSessionId(ctx context.Context, request api.GetMeVerificationSessionsSessionIdRequestObject) (api.GetMeVerificationSessionsSessionIdResponseObject, error) {
	if s.opts.Ports.Verification == nil {
		return nil, errNotWired("identity verification")
	}
	accountID, err := accountScopeWrite(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	sessionID, err := verification.ParseSessionID(request.SessionId.String())
	if err != nil || sessionID.IsZero() {
		return nil, validationError("sessionId", "sessionId must be a canonical UUID")
	}
	session, err := s.opts.Ports.Verification.Poll(ctx, accountID, sessionID)
	if err != nil {
		return nil, err
	}
	return api.GetMeVerificationSessionsSessionId200JSONResponse(toAPIVerificationSession(session)), nil
}

// PostMeVerificationSandboxOutcome chooses what a rehearsal verification
// decides. SANDBOX TIER ONLY.
//
// Three independent refusals stand in front of it, and all three are checked
// before anything is written: this handler refuses a deployment that is not a
// sandbox tier, the service refuses again on the same condition, and the
// database CHECK refuses a sandbox row in PROD. Any one of them alone would be
// enough; all three exist because a fabricated approval is the single worst
// thing this system could produce.
func (s *Server) PostMeVerificationSandboxOutcome(ctx context.Context, request api.PostMeVerificationSandboxOutcomeRequestObject) (api.PostMeVerificationSandboxOutcomeResponseObject, error) {
	if s.opts.Ports.Verification == nil {
		return nil, errNotWired("identity verification")
	}
	if !s.opts.Ports.Verification.SandboxTier() {
		return nil, errs.New(errs.CodeForbidden,
			"choosing a verification outcome is a sandbox-tier affordance; this deployment is not a sandbox tier")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	outcome, err := verification.ParseSandboxOutcome(string(request.Body.Outcome))
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.VerificationSession, commandMeta, error) {
			session, serr := s.opts.Ports.Verification.SandboxOutcome(ctx, accountID, outcome)
			if serr != nil {
				return api.VerificationSession{}, commandMeta{}, serr
			}
			return toAPIVerificationSession(session), commandMeta{
				Status:       http.StatusOK,
				ResourceType: "verification_session",
				ResourceID:   session.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostMeVerificationSandboxOutcome200JSONResponse(res.Value), nil
}
