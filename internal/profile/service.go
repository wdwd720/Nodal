package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/terms"
)

// The audit actions this package writes. They are the consequential ones PART 52
// names -- signup, profile changes, consent, and account status -- and nothing
// else. Reading a profile is not audited: an audit trail full of page views is
// an audit trail nobody reads.
const (
	ActionProfileCreated   = "profile.created"
	ActionProfileUpdated   = "profile.updated"
	ActionTermsAccepted    = "terms.accepted"
	ActionClosureRequested = "account.closure_requested"
	ActionClosureCancelled = "account.closure_cancelled"
	ActionClosureRefused   = "account.closure_refused"
	ActionClosureEffected  = "account.closure_effected"
	ActionOnboardingDone   = "onboarding.completed"
)

// SessionEnder revokes every session a subject holds. *auth.Manager satisfies it.
type SessionEnder interface {
	RevokeAllForSubject(ctx context.Context, q auth.Querier, subjectID string) (int, error)
}

// Deps are the collaborators of the profile service.
type Deps struct {
	DB       *db.DB
	Repo     *Repository
	Accounts *accounts.Repository
	Audit    audit.Writer
	Clock    clock.Clock
	// Sessions ends a closed account's sessions. Nil means they are left to
	// expire, which is why cmd/api always supplies one and
	// TestService_RequiresEverySessionEnder asserts a nil is refused.
	Sessions SessionEnder
	// CoolingOff overrides DefaultCoolingOff. Zero uses the default; it exists
	// so a test can prove the wait rather than wait it out.
	CoolingOff time.Duration
	// Verification, when set, answers the support view's "what has this account
	// established". Nil is reported as "not known in this deployment" and never
	// as NONE: a support view that invents a level is worse than one that says
	// it does not have one.
	Verification VerificationResolver
}

// Service is the profile, terms and account-lifecycle surface.
type Service struct{ d Deps }

// New validates the dependencies and returns a Service.
func New(d Deps) (*Service, error) {
	switch {
	case d.DB == nil, d.Repo == nil, d.Accounts == nil, d.Audit == nil, d.Clock == nil, d.Sessions == nil:
		return nil, errors.New("profile: missing dependency")
	}
	if d.CoolingOff <= 0 {
		d.CoolingOff = DefaultCoolingOff
	}
	return &Service{d: d}, nil
}

// Actor is who is acting, and from where. It is built from the request
// principal by the handler; nothing in this package reads a caller-supplied
// identity.
type Actor struct {
	UserID        string
	ActorType     security.ActorType
	SessionID     string
	RequestID     string
	IP            string
	UserAgent     string
	CorrelationID string
}

func (a Actor) validate() error {
	if a.UserID == "" {
		return errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	if a.ActorType == security.ActorAgent {
		return errs.New(errs.CodeForbidden, "an agent has no profile and may not act on one")
	}
	return nil
}

// Me is what GET /v1/me adds to the principal.
type Me struct {
	Profile    Profile
	Onboarding Onboarding
}

// Me returns the caller's profile, creating it on first use.
func (s *Service) Me(ctx context.Context, a Actor) (Me, error) {
	if err := a.validate(); err != nil {
		return Me{}, err
	}
	now := s.d.Clock.Now()
	var out Me
	err := s.d.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		p, created, err := s.d.Repo.Ensure(ctx, tx, a.UserID, now)
		if err != nil {
			return err
		}
		if created {
			// Signup, from the product's point of view: this is the moment a
			// person becomes a Nodal user rather than an authenticated subject.
			if err := s.append(ctx, tx, a, ActionProfileCreated, "user_profile", a.UserID,
				map[string]any{"avatar_seed": p.AvatarSeed}, now); err != nil {
				return err
			}
		}
		out = Me{Profile: p, Onboarding: p.Onboarding}
		return nil
	})
	return out, err
}

// Update applies a validated patch to the caller's own profile.
func (s *Service) Update(ctx context.Context, a Actor, raw Patch) (Profile, error) {
	if err := a.validate(); err != nil {
		return Profile{}, err
	}
	clean, changed, err := normalizePatch(raw)
	if err != nil {
		return Profile{}, err
	}
	if !changed {
		return Profile{}, errs.New(errs.CodeValidationFailed, "the request changes nothing")
	}
	now := s.d.Clock.Now()
	var out Profile
	err = s.d.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		before, _, err := s.d.Repo.Ensure(ctx, tx, a.UserID, now)
		if err != nil {
			return err
		}
		p, err := s.d.Repo.Apply(ctx, tx, a.UserID, clean, now)
		if err != nil {
			return err
		}
		if p, err = s.d.Repo.CompleteIfReady(ctx, tx, a.UserID, now); err != nil {
			return err
		}
		// The payload names the fields that changed, never their old values: an
		// audit record of a person's previous display name is a record of
		// something they chose to stop showing.
		if err := s.append(ctx, tx, a, ActionProfileUpdated, "user_profile", a.UserID,
			map[string]any{"fields": changedFields(clean)}, now); err != nil {
			return err
		}
		if before.Onboarding.CompletedAt == nil && p.Onboarding.CompletedAt != nil {
			if err := s.append(ctx, tx, a, ActionOnboardingDone, "user_profile", a.UserID, nil, now); err != nil {
				return err
			}
		}
		out = p
		return nil
	})
	return out, err
}

func changedFields(p Patch) []string {
	var out []string
	if p.DisplayName != nil {
		out = append(out, "display_name")
	}
	if p.Handle != nil {
		out = append(out, "handle")
	}
	if p.Locale != nil {
		out = append(out, "locale")
	}
	if p.TimeZone != nil {
		out = append(out, "time_zone")
	}
	return out
}

func normalizePatch(in Patch) (Patch, bool, error) {
	var out Patch
	changed := false
	if in.DisplayName != nil {
		v, err := ValidateDisplayName(*in.DisplayName)
		if err != nil {
			return Patch{}, false, err
		}
		out.DisplayName, changed = &v, true
	}
	if in.Handle != nil {
		// The empty string clears the handle; anything else must be valid.
		if *in.Handle == "" {
			empty := ""
			out.Handle, changed = &empty, true
		} else {
			v, err := ValidateHandle(*in.Handle)
			if err != nil {
				return Patch{}, false, err
			}
			out.Handle, changed = &v, true
		}
	}
	if in.Locale != nil {
		v, err := ValidateLocale(*in.Locale)
		if err != nil {
			return Patch{}, false, err
		}
		out.Locale, changed = &v, true
	}
	if in.TimeZone != nil {
		v, err := ValidateTimeZone(*in.TimeZone)
		if err != nil {
			return Patch{}, false, err
		}
		out.TimeZone, changed = &v, true
	}
	return out, changed, nil
}

// DocumentStatus is one legal document and whether the caller has accepted the
// current version of it.
type DocumentStatus struct {
	Document   terms.Document
	Accepted   bool
	AcceptedAt *time.Time
}

// TermsView is the answer to "what have I accepted, and what is outstanding".
type TermsView struct {
	Documents []DocumentStatus
	// Outstanding lists the documents required at onboarding that are not
	// accepted at their current bytes. It is the field a first-run experience
	// drives from.
	Outstanding []terms.DocumentID
	Acceptances []Acceptance
}

// Terms returns the caller's acceptance state.
func (s *Service) Terms(ctx context.Context, a Actor) (TermsView, error) {
	if err := a.validate(); err != nil {
		return TermsView{}, err
	}
	list, err := s.d.Repo.Acceptances(ctx, s.d.DB, a.UserID)
	if err != nil {
		return TermsView{}, err
	}
	return buildTermsView(list), nil
}

func buildTermsView(list []Acceptance) TermsView {
	// An acceptance counts only when the version AND the bytes match what is
	// served now. D-056: a document edited without its version being bumped is
	// a document nobody has agreed to.
	accepted := make(map[terms.DocumentID]time.Time, len(list))
	for _, acc := range list {
		d, ok := terms.Get(acc.DocumentID)
		if !ok || acc.Version != d.Version || acc.ContentHash != d.ContentHash {
			continue
		}
		if at, seen := accepted[acc.DocumentID]; !seen || acc.AcceptedAt.Before(at) {
			accepted[acc.DocumentID] = acc.AcceptedAt
		}
	}
	view := TermsView{Acceptances: list}
	for _, d := range terms.MustCurrent() {
		st := DocumentStatus{Document: d}
		if at, ok := accepted[d.ID]; ok {
			at := at
			st.Accepted, st.AcceptedAt = true, &at
		}
		view.Documents = append(view.Documents, st)
		if !st.Accepted && d.Requirement == terms.AtOnboarding {
			view.Outstanding = append(view.Outstanding, d.ID)
		}
	}
	return view
}

// Accept records acceptance of the named documents at their current versions.
func (s *Service) Accept(ctx context.Context, a Actor, ids []terms.DocumentID) (TermsView, error) {
	if err := a.validate(); err != nil {
		return TermsView{}, err
	}
	if len(ids) == 0 {
		return TermsView{}, errs.New(errs.CodeValidationFailed, "name at least one document to accept")
	}
	docs := make([]terms.Document, 0, len(ids))
	seen := map[terms.DocumentID]bool{}
	for _, want := range ids {
		d, ok := terms.Get(want)
		if !ok {
			return TermsView{}, errs.Newf(errs.CodeValidationFailed, "unknown document %q", want).
				WithField("field", "document_id")
		}
		if seen[want] {
			continue
		}
		seen[want] = true
		docs = append(docs, d)
	}
	now := s.d.Clock.Now()
	var out TermsView
	err := s.d.DB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, _, err := s.d.Repo.Ensure(ctx, tx, a.UserID, now); err != nil {
			return err
		}
		for _, d := range docs {
			rec, err := s.d.Repo.RecordAcceptance(ctx, tx, Acceptance{
				UserID: a.UserID, DocumentID: d.ID, Version: d.Version, ContentHash: d.ContentHash,
				ActorType: string(a.ActorType), ActorID: a.UserID, SessionID: a.SessionID,
			}, a.IP, a.UserAgent, now)
			if err != nil {
				return err
			}
			if rec.Replayed {
				continue
			}
			if err := s.append(ctx, tx, a, ActionTermsAccepted, "terms_acceptance", rec.ID, map[string]any{
				"document_id": string(d.ID), "version": d.Version, "content_hash": d.ContentHash,
				"counsel_review_required": d.CounselReviewRequired,
			}, now); err != nil {
				return err
			}
		}
		list, err := s.d.Repo.Acceptances(ctx, tx, a.UserID)
		if err != nil {
			return err
		}
		out = buildTermsView(list)
		if len(out.Outstanding) == 0 {
			before, err := s.d.Repo.Get(ctx, tx, a.UserID)
			if err != nil {
				return err
			}
			p, err := s.d.Repo.StampTermsAccepted(ctx, tx, a.UserID, now)
			if err != nil {
				return err
			}
			if before.Onboarding.CompletedAt == nil && p.Onboarding.CompletedAt != nil {
				if err := s.append(ctx, tx, a, ActionOnboardingDone, "user_profile", a.UserID, nil, now); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return out, err
}

// append writes one audit event on the caller's own stream.
func (s *Service) append(ctx context.Context, tx pgx.Tx, a Actor, action, resourceType, resourceID string, payload map[string]any, now time.Time) error {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("profile: audit payload: %w", err)
		}
		raw = b
	}
	stream, err := s.streamFor(ctx, tx, a.UserID)
	if err != nil {
		return err
	}
	actorType := a.ActorType
	if actorType == "" {
		actorType = security.ActorUser
	}
	_, err = s.d.Audit.Append(ctx, tx, audit.Event{
		Stream: stream, ActorType: string(actorType), ActorID: a.UserID, Action: action,
		ResourceType: resourceType, ResourceID: resourceID, RequestID: a.RequestID,
		CorrelationID: a.CorrelationID, SourceIP: a.IP, Device: a.UserAgent,
		Payload: raw, OccurredAt: now,
	})
	return err
}

func mustJSON(v map[string]any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// A map[string]any of strings, ints and bools cannot fail to marshal;
		// if it somehow does, an empty object is a truthful payload and losing
		// the audit row would not be.
		return json.RawMessage("{}")
	}
	return b
}

// streamFor puts a user's events on their first account's stream, matching what
// internal/identity does for a login, so one person's history is one stream.
func (s *Service) streamFor(ctx context.Context, q db.Querier, userID string) (string, error) {
	uid, err := accounts.ParseUserID(userID)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeInternal, "profile: subject is not a user id")
	}
	owned, err := s.d.Accounts.ListByOwner(ctx, q, uid)
	if err != nil {
		return "", err
	}
	for _, acct := range owned {
		return audit.AccountStream(acct.ID.String()), nil
	}
	return audit.SystemStream, nil
}
