package verification

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// ValidityWindow is how long a verification decision stands before it must be
// renewed.
//
// One year, chosen rather than inherited: no provider in PROVIDER_BOUNDARY §5
// publishes a re-verification interval Nodal could adopt, and a decision that
// never expires is a decision about a person as they were, forever. A year is
// the interval the sanctions and PEP screens in §21 are worth re-running at,
// and it is a version-controlled constant rather than configuration because
// shortening or lengthening it is a compliance decision, not a deployment one
// (D-061).
const ValidityWindow = 365 * 24 * time.Hour

// Deps are what the service needs. Nothing is discovered: the clock, the
// environment and whether this deployment is a sandbox tier are all supplied,
// so the same inputs always produce the same behaviour.
type Deps struct {
	Repo       *Repository
	Compliance *compliance.Repository
	Providers  *Registry
	Clock      clock.Clock
	// Environment is written onto every session and every check, so a
	// STAGING answer can never be read as a PROD one.
	Environment string
	// SandboxTier is `cfg.SandboxTier()`. It is the single condition for
	// every sandbox affordance (ADR-0023): a sandbox result is refused
	// outright when it is false, before anything is written.
	SandboxTier bool
	// ReturnURL and RefreshURL are where a hosted provider sends the person
	// back to. Empty is legal: a provider that requires them refuses, which is
	// better than inventing a URL that goes nowhere.
	ReturnURL  string
	RefreshURL string
}

// Service starts verification sessions, ingests provider results, and reports
// what a person has established and what is missing.
type Service struct {
	deps Deps
}

// NewService returns a Service. The repository, the compliance repository, the
// registry and the clock are required.
func NewService(d Deps) (*Service, error) {
	if d.Repo == nil || d.Compliance == nil || d.Providers == nil || d.Clock == nil {
		return nil, errs.New(errs.CodeValidationFailed,
			"verification: a repository, a compliance repository, a provider registry and a clock are required")
	}
	if strings.TrimSpace(d.Environment) == "" {
		return nil, errs.New(errs.CodeValidationFailed, "verification: the environment must be stated")
	}
	if d.SandboxTier && d.Environment == "PROD" {
		// Belt and braces over config.Validate, which already refuses the
		// declaration in PROD. A service built this way would be able to write
		// a sandbox row, and the CHECK would refuse it — so refusing here
		// turns a constraint violation into a sentence.
		return nil, errs.New(errs.CodeForbidden,
			"verification: a sandbox tier cannot be PROD; a rehearsal cannot exist where real value moves")
	}
	return &Service{deps: d}, nil
}

// SandboxTier reports whether this deployment may produce sandbox outcomes.
func (s *Service) SandboxTier() bool { return s.deps.SandboxTier }

// Provider returns the configured verification provider, or an error saying
// there is none. A deployment with no identity vendor says so; it does not
// report that verification failed.
func (s *Service) Provider() (Provider, error) { return s.deps.Providers.Only() }

// StartRequest asks to open a verification session for the person who owns an
// account.
type StartRequest struct {
	AccountID accounts.AccountID
	Purpose   Purpose
	// Jurisdiction is where the person says they are. It is supplied by the
	// caller and NEVER inferred from an IP address; a session with no
	// jurisdiction is refused, because every rule in §21 is keyed on one.
	Jurisdiction  rules.Jurisdiction
	CorrelationID string
}

// Started is what a caller needs to send a person to a hosted flow.
type Started struct {
	Session Session
	// HostedURL is single-use and short-lived. It is returned here and stored
	// nowhere.
	HostedURL string
	ExpiresAt *time.Time
	Sandbox   bool
	// SandboxControlPath is the endpoint a sandbox operator uses to choose an
	// outcome explicitly. It is empty for a real provider, and its presence is
	// what makes a sandbox session visibly a rehearsal rather than a flow that
	// silently approves.
	SandboxControlPath string
}

// SandboxControlPath is where a sandbox tier chooses a verification outcome.
const SandboxControlPath = "/v1/me/verification/sandbox-outcome"

// Start opens a verification session.
//
// The provider call happens between two transactions and inside neither, for
// the reason D-049 recorded: a network call inside a transaction holds a pool
// connection across a hop, and a rollback erases the row while leaving the
// provider's session in existence.
func (s *Service) Start(ctx context.Context, database *db.DB, r StartRequest) (Started, error) {
	if !r.Purpose.Valid() {
		return Started{}, errs.Newf(errs.CodeValidationFailed, "unknown verification purpose %q", r.Purpose)
	}
	j := r.Jurisdiction.Normalized()
	if j.Country == "" {
		return Started{}, errs.New(errs.CodeValidationFailed,
			"verification needs the country you are in; it is never inferred from a network address")
	}
	provider, err := s.Provider()
	if err != nil {
		return Started{}, err
	}
	if ok, refusals := rules.CheckJurisdiction(j); !ok {
		return Started{}, errs.Newf(errs.CodeEligibilityJurisdiction,
			"verification is not offered in %s", j.String()).
			WithField("refusals", refusalStrings(refusals)).
			WithField("rules_version", rules.Version)
	}
	if ok, missing := provider.Capabilities().Supports(r.Purpose, j); !ok {
		return Started{}, errs.Newf(errs.CodeProviderUnavailable,
			"the configured verification provider cannot establish %s in %s", r.Purpose, j.String()).
			WithField("missing", missing).
			WithField("provider", provider.Name())
	}

	owner, err := s.owner(ctx, database, r.AccountID)
	if err != nil {
		return Started{}, err
	}
	now := s.deps.Clock.Now().UTC()

	var session Session
	if err := database.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.ensureProfile(ctx, tx, owner, j, r.CorrelationID); err != nil {
			return err
		}
		if open, ok, oerr := s.deps.Repo.OpenSession(ctx, tx, owner); oerr != nil {
			return oerr
		} else if ok {
			// One live attempt per person. Returning the existing one is the
			// useful answer; the caller resumes it rather than racing it.
			session = open
			return nil
		}
		var cerr error
		session, cerr = s.deps.Repo.CreateSession(ctx, tx, Session{
			UserID:              owner,
			Purpose:             r.Purpose,
			Provider:            provider.Name(),
			JurisdictionCountry: j.Country,
			JurisdictionRegion:  j.Region,
			RulesVersion:        rules.Version,
			Environment:         s.deps.Environment,
			Sandbox:             s.deps.SandboxTier,
		})
		if cerr != nil {
			return cerr
		}
		return s.moveProfileForStart(ctx, tx, owner, session, now, r.CorrelationID)
	}); err != nil {
		return Started{}, err
	}

	// Already open: reissue a link on the same session, which preserves the
	// failed-attempt history the provider counts against its own limits.
	if session.Status != SessionCreated {
		return s.resume(ctx, database, session, provider)
	}

	started, err := provider.Start(ctx, SessionRequest{
		SubjectRef:   session.ID.String(),
		Purpose:      session.Purpose,
		Jurisdiction: j,
		ReturnURL:    s.deps.ReturnURL,
		RefreshURL:   s.deps.RefreshURL,
	})
	if err != nil {
		return Started{}, err
	}
	if err := s.guardSandbox(started.Sandbox); err != nil {
		return Started{}, err
	}

	var out Session
	if err := database.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		expiry := expiryOf(started)
		if err := s.deps.Repo.SetProviderReference(ctx, tx, session.ID, started.ProviderRef, expiry); err != nil {
			return err
		}
		var terr error
		out, terr = s.deps.Repo.TransitionSession(ctx, tx, session.ID, SessionPendingUserAction, SessionChange{
			ActorType:     security.ActorSystem,
			ActorID:       "verification:" + provider.Name(),
			Reason:        "the provider issued a hosted session",
			ProviderEvent: started.ProviderRef,
			CorrelationID: r.CorrelationID,
			OccurredAt:    s.deps.Clock.Now().UTC(),
		})
		return terr
	}); err != nil {
		return Started{}, err
	}
	return Started{
		Session:            out,
		HostedURL:          started.HostedURL,
		ExpiresAt:          expiryOf(started),
		Sandbox:            started.Sandbox,
		SandboxControlPath: sandboxControlPathFor(started.Sandbox),
	}, nil
}

// Resume issues a fresh hosted link on the person's existing session.
func (s *Service) Resume(ctx context.Context, database *db.DB, accountID accounts.AccountID) (Started, error) {
	provider, err := s.Provider()
	if err != nil {
		return Started{}, err
	}
	owner, err := s.owner(ctx, database, accountID)
	if err != nil {
		return Started{}, err
	}
	session, ok, err := s.deps.Repo.OpenSession(ctx, database, owner)
	if err != nil {
		return Started{}, err
	}
	if !ok {
		return Started{}, errs.New(errs.CodeNotFound, "there is no verification in progress to resume")
	}
	return s.resume(ctx, database, session, provider)
}

func (s *Service) resume(ctx context.Context, database *db.DB, session Session, provider Provider) (Started, error) {
	if session.ProviderRef == "" {
		// A row written before the provider was called, whose call never
		// landed. Nothing can move it -- only a provider answer moves a session
		// the provider knows about -- so the expiry sweep closes it after
		// UnstartedSessionGrace and the person starts a new one. Until it does,
		// this is a Conflict rather than a silent second attempt, because the
		// one-open-session index would refuse the second anyway.
		return Started{}, errs.Newf(errs.CodeConflict,
			"that verification session was never handed to the provider; it is closed automatically "+
				"within %s and verification can be started again then", UnstartedSessionGrace)
	}
	started, err := provider.Resume(ctx, session.ProviderRef)
	if err != nil {
		return Started{}, err
	}
	if err := s.guardSandbox(started.Sandbox); err != nil {
		return Started{}, err
	}
	if expiry := expiryOf(started); expiry != nil {
		if err := database.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
			return s.deps.Repo.SetProviderReference(ctx, tx, session.ID, session.ProviderRef, expiry)
		}); err != nil {
			return Started{}, err
		}
	}
	return Started{
		Session:            session,
		HostedURL:          started.HostedURL,
		ExpiresAt:          expiryOf(started),
		Sandbox:            started.Sandbox || session.Sandbox,
		SandboxControlPath: sandboxControlPathFor(started.Sandbox || session.Sandbox),
	}, nil
}

// Poll asks the provider what happened and ingests the answer.
//
// It is the reconciliation half of PROVIDER_BOUNDARY §3's rule: never trust the
// redirect. A customer arriving at the return URL says they came back, not that
// they passed, and a webhook is never the sole source of truth.
func (s *Service) Poll(ctx context.Context, database *db.DB, accountID accounts.AccountID, sessionID SessionID) (Session, error) {
	owner, err := s.owner(ctx, database, accountID)
	if err != nil {
		return Session{}, err
	}
	session, err := s.deps.Repo.Session(ctx, database, sessionID)
	if err != nil {
		return Session{}, err
	}
	if session.UserID != owner {
		// NOT_FOUND rather than FORBIDDEN: a distinguishable refusal is a
		// membership oracle, and every other account-scoped read here holds
		// the same line.
		return Session{}, errs.New(errs.CodeNotFound, "no such verification session")
	}
	if session.Status.Terminal() {
		return session, nil
	}
	provider, err := s.deps.Providers.Get(session.Provider)
	if err != nil {
		return Session{}, err
	}
	if session.ProviderRef == "" {
		return session, nil
	}
	result, err := provider.Get(ctx, session.ProviderRef)
	if err != nil {
		return Session{}, err
	}
	return s.Ingest(ctx, database, session, result, "polled the provider for a decision", "")
}

// IngestWebhook verifies a provider's signature over the raw bytes and applies
// what it said. It is idempotent on redelivery: the transition is a no-op when
// the status has not moved, and the evidence rows are deduplicated by the
// unique constraint on (session, kind, outcome).
func (s *Service) IngestWebhook(ctx context.Context, database *db.DB, providerName string, headers http.Header, body []byte) (Session, error) {
	provider, err := s.deps.Providers.Get(providerName)
	if err != nil {
		return Session{}, err
	}
	result, err := provider.ParseWebhook(headers, body)
	if err != nil {
		return Session{}, err
	}
	session, err := s.deps.Repo.SessionByProviderRef(ctx, database, provider.Name(), result.ProviderRef)
	if err != nil {
		return Session{}, err
	}
	return s.Ingest(ctx, database, session, result, "the provider delivered a webhook", result.RawStatus)
}

// Ingest applies a provider result to a session, its evidence and the person's
// verification state, in one transaction.
//
// The sandbox guard runs BEFORE anything is written: a sandbox result on a
// deployment that is not a sandbox tier is refused outright rather than stored
// and labelled, because a rehearsal that reached a real deployment is a defect
// and not a row.
func (s *Service) Ingest(ctx context.Context, database *db.DB, session Session, result Result, reason, providerEvent string) (Session, error) {
	if err := s.guardSandbox(result.Sandbox); err != nil {
		return Session{}, err
	}
	if !result.Status.Valid() {
		return Session{}, errs.Newf(errs.CodeValidationFailed,
			"the provider reported an unknown session status %q", result.Status)
	}
	now := s.deps.Clock.Now().UTC()
	sandbox := result.Sandbox || session.Sandbox

	var out Session
	err := database.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		// A provider does not report every step: a person completes a hosted
		// flow and the provider decides before the next poll, so a session
		// last seen PENDING_USER_ACTION is answered APPROVED. Each inferred
		// step gets its own transition row saying it was inferred, so the edges
		// stay meaningful and the trail stays honest.
		moved := session
		for i, step := range SessionPath(session.Status, result.Status) {
			stepReason := reason
			if i < len(SessionPath(session.Status, result.Status))-1 {
				stepReason = "inferred from the provider's later status " + string(result.Status) + ": " + reason
			}
			var terr error
			moved, terr = s.deps.Repo.TransitionSession(ctx, tx, session.ID, step, SessionChange{
				ActorType:     security.ActorSystem,
				ActorID:       "verification:" + session.Provider,
				Reason:        stepReason,
				ProviderEvent: providerEvent,
				FailureReason: result.FailureReason,
				OccurredAt:    now,
			})
			if terr != nil {
				return terr
			}
		}
		if moved.Status != result.Status && session.Status != result.Status {
			return errs.Newf(errs.CodeInvalidStateTransition,
				"a verification session cannot reach %s from %s by any sequence of legal steps",
				result.Status, session.Status).
				WithField("session_id", session.ID.String())
		}
		out = moved

		// The evidence, AFTER the status it belongs to.
		//
		// It used to be written first, and migration 00806 is why the order is
		// now load-bearing: a check row attaches only to a session in a status a
		// provider ANSWER produces, because four PASS rows against a session
		// nobody was ever sent to made the resolver report PAYOUT_KYC (F-227).
		// Nothing else depends on the order -- the path walked above is computed
		// from the two statuses and never from the checks.
		for _, c := range sortedChecks(result.Checks) {
			if err := s.deps.Repo.RecordCheck(ctx, tx, Check{
				SessionID:    session.ID,
				UserID:       session.UserID,
				Kind:         c.Kind,
				Outcome:      c.Outcome,
				Provider:     session.Provider,
				ProviderRef:  result.ProviderRef,
				RulesVersion: session.RulesVersion,
				Environment:  s.deps.Environment,
				Sandbox:      sandbox,
				Detail:       c.Detail,
				RecordedAt:   now,
			}); err != nil {
				return err
			}
		}

		checks, err := s.deps.Repo.ChecksForSession(ctx, tx, session.ID)
		if err != nil {
			return err
		}
		return s.applyToProfile(ctx, tx, moved, result, checks, now)
	})
	if err != nil {
		return Session{}, err
	}
	return out, nil
}

// applyToProfile moves the person's verification state to match what the
// session now says, and writes the attribute half of the profile from the
// decisions the provider returned.
//
// It writes no Credit, no lot and no balance. There is no code path from here
// into internal/credit, and there is not meant to be one: a verification
// decision changes what a person may ASK for.
func (s *Service) applyToProfile(ctx context.Context, tx pgx.Tx, session Session, result Result, checks []Check, now time.Time) error {
	target, ok := profileStateFor(session.Status, session.Purpose, checks)
	if !ok {
		return nil
	}
	// The attributes the provider decided, recorded before the state moves so
	// that a reader who sees VERIFIED also sees the jurisdiction it was
	// verified in.
	j := result.Jurisdiction.Normalized()
	if j.Country == "" {
		j = rules.Jurisdiction{Country: session.JurisdictionCountry, Region: session.JurisdictionRegion}
	}
	profile, err := s.deps.Compliance.Get(ctx, tx, session.UserID)
	if err != nil && errs.CodeOf(err) != errs.CodeNotFound {
		return err
	}
	profile.UserID = session.UserID
	profile.JurisdictionCountry = j.Country
	profile.JurisdictionRegion = j.Region
	if profile.ResidencyCountry == "" {
		profile.ResidencyCountry = j.Country
	}
	profile.AgeVerified = outcomeOf(checks, CheckAge) == OutcomePass
	profile.SanctionsState = sanctionsStateFrom(checks)
	profile.Provider = session.Provider
	profile.ProviderRef = result.ProviderRef
	profile.PolicyVersion = session.RulesVersion
	if profile.Restrictions == nil {
		profile.Restrictions = []string{}
	}
	if _, err := s.deps.Compliance.Upsert(ctx, tx, profile, compliance.Change{
		ActorType: security.ActorSystem,
		ActorID:   "verification:" + session.Provider,
		Reason:    "provider decision recorded for session " + session.ID.String(),
	}); err != nil {
		return err
	}

	current, _, _, err := s.deps.Repo.ProfileState(ctx, tx, session.UserID)
	if err != nil {
		return err
	}
	// An abandoned or expired SESSION does not undo a standing some other
	// session established. A person who verified in March and let an April
	// re-verification link expire is still verified; downgrading them on the
	// strength of an unfinished attempt would be the product taking something
	// away that nothing decided to take.
	if (current == StateVerified || current == StateRestricted) &&
		(session.Status == SessionCancelled || session.Status == SessionExpired) {
		return nil
	}
	path := Path(current, target)
	if current == target || path == nil {
		// Not an error. A provider redelivering a webhook, or a poll racing
		// one, arrives at a state the profile already reached; and a verdict
		// whose standing is unreachable from here leaves the standing alone
		// rather than forcing it.
		return nil
	}
	id := session.ID
	for i, step := range path {
		t := ProfileTransition{
			UserID:      session.UserID,
			To:          step,
			ActorType:   security.ActorSystem,
			ActorID:     "verification:" + session.Provider,
			Reason:      "provider session " + string(session.Status),
			Provider:    session.Provider,
			ProviderRef: result.ProviderRef,
			SessionID:   &id,
			OccurredAt:  now,
		}
		if i < len(path)-1 {
			t.Reason = "inferred on the way to " + string(target) + ": " + t.Reason
		}
		if step == StateVerified {
			verified := now
			expires := now.Add(ValidityWindow)
			t.VerifiedAt = &verified
			t.ExpiresAt = &expires
		}
		if _, err := s.deps.Repo.TransitionProfile(ctx, tx, t); err != nil {
			return err
		}
	}
	return nil
}

// profileStateFor maps a session status onto the verification state it implies,
// and reports whether it implies one at all.
//
// APPROVED is the only status that can produce VERIFIED, and it only does so
// when the evidence supports the level. An approval with a failed sub-check
// produces RESTRICTED rather than VERIFIED: the provider said yes and something
// in §21 said not entirely, and collapsing that to "verified" is how a
// sanctions hit becomes invisible.
func profileStateFor(status SessionStatus, purpose Purpose, checks []Check) (State, bool) {
	switch status {
	case SessionCreated:
		return "", false
	case SessionPendingUserAction:
		return StateStarted, true
	case SessionProcessing, SessionManualReview:
		return StatePending, true
	case SessionRequiresInput:
		return StateNeedsInformation, true
	case SessionApproved:
		if ok, _ := EvidenceSatisfies(purpose, checks); ok {
			return StateVerified, true
		}
		return StateRestricted, true
	case SessionDeclined:
		return StateRejected, true
	case SessionCancelled:
		return StateRequired, true
	case SessionExpired:
		return StateExpired, true
	}
	return "", false
}

// sortedChecks orders a provider's answers by kind so two identical results
// write their rows in the same order, which is what makes a redelivery a
// byte-for-byte no-op rather than a differently-ordered one.
func sortedChecks(in []CheckResult) []CheckResult {
	out := append([]CheckResult(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

func outcomeOf(checks []Check, kind CheckKind) Outcome {
	if c, ok := latest(checks)[kind]; ok {
		return c.Outcome
	}
	return OutcomeUnknown
}

// sanctionsStateFrom maps the sanctions and PEP sub-checks onto the compliance
// profile's sanctions column. A PEP flag is a REVIEW rather than a HIT: being
// politically exposed is a reason for diligence, not a prohibition.
func sanctionsStateFrom(checks []Check) compliance.SanctionsState {
	switch outcomeOf(checks, CheckSanctions) {
	case OutcomeFail:
		return compliance.SanctionsHit
	case OutcomePass:
		if outcomeOf(checks, CheckPEP) == OutcomeFail {
			return compliance.SanctionsReview
		}
		return compliance.SanctionsClear
	case OutcomeNeedsInformation:
		return compliance.SanctionsReview
	}
	return compliance.SanctionsUnknown
}

// ensureProfile creates the profile row a transition needs, without touching
// its state: the row is born UNVERIFIED and every later state is a transition.
func (s *Service) ensureProfile(ctx context.Context, tx pgx.Tx, owner accounts.UserID, j rules.Jurisdiction, correlationID string) error {
	profile, err := s.deps.Compliance.Get(ctx, tx, owner)
	if err != nil && errs.CodeOf(err) != errs.CodeNotFound {
		return err
	}
	profile.UserID = owner
	if profile.SanctionsState == "" {
		profile.SanctionsState = compliance.SanctionsUnknown
	}
	if profile.JurisdictionCountry == "" {
		profile.JurisdictionCountry = j.Country
	}
	if profile.JurisdictionRegion == "" {
		profile.JurisdictionRegion = j.Region
	}
	if profile.Restrictions == nil {
		profile.Restrictions = []string{}
	}
	_, err = s.deps.Compliance.Upsert(ctx, tx, profile, compliance.Change{
		ActorType:     security.ActorSystem,
		ActorID:       "verification-service",
		Reason:        "a verification session was requested",
		CorrelationID: correlationID,
	})
	return err
}

// moveProfileForStart records that verification is now under way. A person in
// UNVERIFIED goes to REQUIRED first, so the trail shows that the requirement
// existed before the attempt did.
func (s *Service) moveProfileForStart(ctx context.Context, tx pgx.Tx, owner accounts.UserID, session Session, now time.Time, correlationID string) error {
	current, _, _, err := s.deps.Repo.ProfileState(ctx, tx, owner)
	if err != nil {
		return err
	}
	id := session.ID
	move := func(to State, reason string) error {
		if current == to || !CanTransition(current, to) {
			return nil
		}
		next, terr := s.deps.Repo.TransitionProfile(ctx, tx, ProfileTransition{
			UserID:        owner,
			To:            to,
			ActorType:     security.ActorSystem,
			ActorID:       "verification-service",
			Reason:        reason,
			Provider:      session.Provider,
			SessionID:     &id,
			CorrelationID: correlationID,
			OccurredAt:    now,
		})
		if terr != nil {
			return terr
		}
		current = next
		return nil
	}
	if err := move(StateRequired, "a verification is needed for what this person asked to do"); err != nil {
		return err
	}
	return move(StateStarted, "a verification session was opened")
}

func (s *Service) owner(ctx context.Context, q db.Querier, accountID accounts.AccountID) (accounts.UserID, error) {
	if accountID.IsZero() {
		return accounts.UserID{}, errs.New(errs.CodeValidationFailed, "an account is required")
	}
	return s.deps.Repo.OwnerOf(ctx, q, accountID)
}

// guardSandbox refuses a rehearsal result on a deployment that is not a sandbox
// tier. It is the application half of the CHECK in migration 00762, and it runs
// before anything is written so the refusal is a sentence rather than a
// constraint violation.
func (s *Service) guardSandbox(sandbox bool) error {
	if sandbox && !s.deps.SandboxTier {
		return errs.New(errs.CodeForbidden,
			"a sandbox verification outcome can only exist on a sandbox tier; this deployment is not one")
	}
	return nil
}

func expiryOf(st StartedSession) *time.Time {
	if st.ExpiresAt.IsZero() {
		return nil
	}
	t := st.ExpiresAt.UTC()
	return &t
}

func sandboxControlPathFor(sandbox bool) string {
	if !sandbox {
		return ""
	}
	return SandboxControlPath
}

func refusalStrings(rs []rules.Refusal) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, string(r))
	}
	return out
}

// levelFrom is the composite level rule, shared by the resolver and the
// snapshot so the two can never disagree.
//
// A level is EARNED from evidence, never asserted by a state. A profile in
// VERIFIED with no sub-checks reports the base level, because a state without
// evidence is somebody's assertion and a state with evidence is a decision that
// can be shown to a regulator.
func levelFrom(base valuedomain.VerificationLevel, state State, expiresAt *time.Time, checks []Check, now time.Time) valuedomain.VerificationLevel {
	if base == valuedomain.VerificationNone {
		// The account itself is not in good standing, or the identity provider
		// never asserted a verified e-mail address. Nothing above it is
		// reachable however good the KYC evidence is.
		return valuedomain.VerificationNone
	}
	if state != StateVerified {
		return base
	}
	if expired(expiresAt, now) {
		// The window has passed and nothing has moved the state to EXPIRED yet
		// — a sweep runs, and until it does the level must already be the
		// lower one. Reporting PAYOUT_KYC on the strength of a stale row is
		// how an expired verification pays somebody out.
		return base
	}
	if ok, _ := EvidenceSatisfies(PurposePayoutKYC, checks); !ok {
		return base
	}
	if ok, _ := EvidenceSatisfies(PurposeEnhanced, checks); ok {
		return valuedomain.VerificationEnhanced
	}
	return valuedomain.VerificationPayoutKYC
}
