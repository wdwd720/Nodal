package httpapi

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/profile"
	"github.com/nodal/controlplane/internal/security"
)

// meExtras adds the product-level fields to the principal GET /v1/me already
// returned. It is additive: every field toAPIPrincipal set is untouched, and a
// deployment with no profile port answers exactly what it answered before.
//
// A failure to read or create the profile is NOT fatal here. /v1/me is what the
// web app calls to find out whether it has a session at all, and answering 500
// to that because a product-level table was unreachable would log everybody out
// over something that is not about their session. The profile fields are simply
// absent, and the client treats them as optional because the schema says they
// are.
func (s *Server) meExtras(ctx context.Context, p security.Principal, out *api.Principal) {
	if s.opts.Ports.Profile == nil || p.IsAgent() {
		return
	}
	actor, err := s.actorFrom(ctx)
	if err != nil {
		return
	}
	me, err := s.opts.Ports.Profile.Me(ctx, actor)
	if err != nil {
		return
	}
	prof := toAPIProfile(me.Profile)
	ob := toAPIOnboarding(me.Onboarding)
	out.Profile = &prof
	out.Onboarding = &ob
}

func toAPIProfile(p profile.Profile) api.UserProfile {
	out := api.UserProfile{
		AvatarSeed:  p.AvatarSeed,
		CreatedAt:   p.CreatedAt.UTC(),
		Locale:      p.Locale,
		TimeZone:    p.TimeZone,
		DisplayName: strPtr(p.DisplayName),
		Handle:      strPtr(p.Handle),
	}
	if u := parseUUIDText(p.UserID); u != nil {
		out.UserId = *u
	}
	if !p.UpdatedAt.IsZero() {
		at := p.UpdatedAt.UTC()
		out.UpdatedAt = &at
	}
	return out
}

func toAPIOnboarding(o profile.Onboarding) api.Onboarding {
	out := api.Onboarding{
		StartedAt: o.StartedAt.UTC(),
		Complete:  o.Complete(),
	}
	if o.CompletedAt != nil {
		at := o.CompletedAt.UTC()
		out.CompletedAt = &at
	}
	if next := o.NextStep(); next != "" {
		n := api.OnboardingNextStep(next)
		out.NextStep = &n
	}
	for _, st := range o.Steps() {
		step := struct {
			Complete    bool                   `json:"complete"`
			CompletedAt *api.Timestamp         `json:"completed_at,omitempty"`
			Key         api.OnboardingStepsKey `json:"key"`
		}{Complete: st.Complete, Key: api.OnboardingStepsKey(st.Key)}
		if st.CompletedAt != nil {
			at := st.CompletedAt.UTC()
			step.CompletedAt = &at
		}
		out.Steps = append(out.Steps, step)
	}
	return out
}

// toAPITermsState renders the acceptance state. withBody carries the document
// text on the collection read, so a client renders exactly the bytes whose hash
// it then records having accepted; the command response leaves it out, because
// the client has already displayed it.
func toAPITermsState(v profile.TermsView, withBody bool) api.TermsState {
	out := api.TermsState{
		Documents:   make([]api.LegalDocument, 0, len(v.Documents)),
		Outstanding: make([]string, 0, len(v.Outstanding)),
	}
	for _, d := range v.Documents {
		doc := api.LegalDocument{
			DocumentId:            api.LegalDocumentDocumentId(d.Document.ID),
			Version:               d.Document.Version,
			Title:                 d.Document.Title,
			ContentHash:           d.Document.ContentHash,
			Requirement:           api.LegalDocumentRequirement(d.Document.Requirement),
			CounselReviewRequired: d.Document.CounselReviewRequired,
			Accepted:              d.Accepted,
		}
		if withBody {
			body := d.Document.Body
			doc.Body = &body
		}
		if d.AcceptedAt != nil {
			at := d.AcceptedAt.UTC()
			doc.AcceptedAt = &at
		}
		out.Documents = append(out.Documents, doc)
	}
	for _, id := range v.Outstanding {
		out.Outstanding = append(out.Outstanding, string(id))
	}
	if acc := toAPIAcceptances(v.Acceptances); len(acc) > 0 {
		out.Acceptances = &acc
	}
	return out
}

func toAPIAcceptances(in []profile.Acceptance) []api.TermsAcceptance {
	out := make([]api.TermsAcceptance, 0, len(in))
	for _, a := range in {
		item := api.TermsAcceptance{
			DocumentId:  string(a.DocumentID),
			Version:     a.Version,
			ContentHash: a.ContentHash,
			AcceptedAt:  a.AcceptedAt.UTC(),
		}
		if u := parseUUIDText(a.ID); u != nil {
			item.Id = *u
		}
		out = append(out, item)
	}
	return out
}

func toAPIRestrictions(in []profile.Restriction) []api.AccountRestriction {
	out := make([]api.AccountRestriction, 0, len(in))
	for _, r := range in {
		item := api.AccountRestriction{
			Code:    api.AccountRestrictionCode(r.Code),
			Message: r.Message,
		}
		if r.AccountID != "" {
			item.AccountId = parseUUIDText(r.AccountID)
		}
		out = append(out, item)
	}
	return out
}

func toAPIClosure(c *profile.ClosureRequest, now time.Time) *api.ClosureRequest {
	if c == nil {
		return nil
	}
	effectable := c.Effectable(now)
	out := api.ClosureRequest{
		State:           api.ClosureRequestState(c.State),
		RequestedAt:     c.RequestedAt.UTC(),
		CoolingOffUntil: c.CoolingOffUntil.UTC(),
		Effectable:      &effectable,
		DecidedReason:   strPtr(c.DecidedReason),
	}
	if u := parseUUIDText(c.ID); u != nil {
		out.Id = *u
	}
	if c.DecidedAt != nil {
		at := c.DecidedAt.UTC()
		out.DecidedAt = &at
	}
	return &out
}

func toAPIMyAccount(v profile.AccountView, now time.Time) api.MyAccount {
	out := api.MyAccount{
		UserStatus:     api.MyAccountUserStatus(v.UserStatus),
		Accounts:       toAPIAccounts(v.Accounts),
		Restrictions:   toAPIRestrictions(v.Restrictions),
		ClosureRequest: toAPIClosure(v.Closure, now),
		CoolingOffDays: int(v.CoolingOff / (24 * time.Hour)),
	}
	if u := parseUUIDText(v.UserID); u != nil {
		out.UserId = *u
	}
	return out
}

func toAPIAdminUser(v profile.AdminUserView, now time.Time) api.AdminUserView {
	out := api.AdminUserView{
		UserStatus:     api.AdminUserViewUserStatus(v.UserStatus),
		IdpIssuer:      v.IdPIssuer,
		IdpSubject:     v.IdPSubject,
		EmailVerified:  v.EmailVerified,
		CreatedAt:      v.CreatedAt.UTC(),
		Accounts:       toAPIAccounts(v.Accounts),
		Restrictions:   toAPIRestrictions(v.Restrictions),
		ClosureRequest: toAPIClosure(v.Closure, now),
		ActiveSessions: v.ActiveSessions,
		AuditStream:    v.AuditStream,
	}
	if u := parseUUIDText(v.UserID); u != nil {
		out.UserId = *u
	}
	if v.Profile != nil {
		p := toAPIProfile(*v.Profile)
		ob := toAPIOnboarding(v.Profile.Onboarding)
		out.Profile, out.Onboarding = &p, &ob
	}
	if acc := toAPIAcceptances(v.Acceptances); len(acc) > 0 {
		out.Acceptances = &acc
	}
	out.Verification.Known = v.VerificationKnown
	if v.VerificationKnown {
		level := v.Verification
		out.Verification.Level = &level
	}
	// The three facts the closure decision needs, carried whether or not there
	// is a request to decide: an operator looking at a person before a request
	// exists is looking at the same account (F-179).
	out.ClosureBlockers.Clear = v.Blockers.Clear()
	out.ClosureBlockers.CreditBalance = v.Blockers.CreditBalance
	out.ClosureBlockers.OpenPayoutRequests = v.Blockers.OpenPayoutRequests
	out.ClosureBlockers.OpenNativePositions = v.Blockers.OpenNativePositions
	out.ClosureBlockers.Reasons = v.Blockers.Reasons()
	if out.ClosureBlockers.Reasons == nil {
		out.ClosureBlockers.Reasons = []string{}
	}
	if out.ClosureBlockers.CreditBalance == "" {
		out.ClosureBlockers.CreditBalance = "0"
	}
	return out
}
