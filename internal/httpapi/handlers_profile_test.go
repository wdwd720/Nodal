package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/profile"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/terms"
)

// fakeProfile records what it was asked and answers from fields a test sets.
type fakeProfile struct {
	me      profile.Me
	prof    profile.Profile
	view    profile.AccountView
	terms   profile.TermsView
	admin   profile.AdminUserView
	err     error
	seenSub []string
	decided struct {
		userID   string
		decision profile.ClosureDecision
		reason   string
	}
}

func (f *fakeProfile) record(a profile.Actor) { f.seenSub = append(f.seenSub, a.UserID) }

func (f *fakeProfile) Me(_ context.Context, a profile.Actor) (profile.Me, error) {
	f.record(a)
	return f.me, f.err
}

func (f *fakeProfile) Update(_ context.Context, a profile.Actor, _ profile.Patch) (profile.Profile, error) {
	f.record(a)
	return f.prof, f.err
}

func (f *fakeProfile) Terms(_ context.Context, a profile.Actor) (profile.TermsView, error) {
	f.record(a)
	return f.terms, f.err
}

func (f *fakeProfile) Accept(_ context.Context, a profile.Actor, _ []terms.DocumentID) (profile.TermsView, error) {
	f.record(a)
	return f.terms, f.err
}

func (f *fakeProfile) Account(_ context.Context, a profile.Actor) (profile.AccountView, error) {
	f.record(a)
	return f.view, f.err
}

func (f *fakeProfile) RequestClosure(_ context.Context, a profile.Actor, _ string) (profile.AccountView, error) {
	f.record(a)
	return f.view, f.err
}

func (f *fakeProfile) CancelClosure(_ context.Context, a profile.Actor) (profile.AccountView, error) {
	f.record(a)
	return f.view, f.err
}

func (f *fakeProfile) AdminUser(_ context.Context, userID string) (profile.AdminUserView, error) {
	f.seenSub = append(f.seenSub, userID)
	return f.admin, f.err
}

func (f *fakeProfile) Decide(_ context.Context, op profile.Actor, userID string, d profile.ClosureDecision, reason string) (profile.AdminUserView, error) {
	f.record(op)
	f.decided.userID, f.decided.decision, f.decided.reason = userID, d, reason
	return f.admin, f.err
}

func sampleProfile() profile.Profile {
	return profile.Profile{
		UserID: testUserID.String(), DisplayName: "Ada", Handle: "ada",
		Locale: "en", TimeZone: "UTC", AvatarSeed: profile.AvatarSeedFor(testUserID.String()),
		Onboarding: profile.Onboarding{StartedAt: testNow},
		CreatedAt:  testNow, UpdatedAt: testNow,
	}
}

// newProfileHarness is newHarness with the profile port wired. It builds its own
// server rather than mutating the shared fixtures, so nothing here changes what
// any other test in this package sees.
func newProfileHarness(t *testing.T, port ProfilePort) (*harness, *fakeProfile) {
	t.Helper()
	h := newHarness(t)
	ports := h.ports.ports()
	ports.Profile = port
	srv, err := New(Options{
		Env:           config.EnvTest,
		BuildVersion:  "test-build",
		ConfigHash:    "hash-1",
		PublicBaseURL: "https://app.test",
		CORSOrigins:   []string{"https://app.test"},
		CookieName:    "cp_session",
		SessionTTL:    time.Hour,
		Clock:         clock.NewFake(testNow),
		Authenticator: h.server.opts.Authenticator,
		Ports:         ports,
	})
	require.NoError(t, err)
	h.server = srv
	fp, _ := port.(*fakeProfile)
	return h, fp
}

func newKeyed() string { return "prof-" + testSessionID }

// ---------------------------------------------------------------- self-scoping

// The strongest statement this package can make about cross-tenant access is
// that it is unrepresentable: no /me route takes an identifier, so there is no
// value a caller could supply to name somebody else.
// meRoutesNamingTheCallersOwnObject are the exceptions, each one an identifier
// of an object that belongs to the caller and is read back together with the
// principal's subject, so the value names one of the caller's own rows or
// nothing (internal/notifications: a mark-read under another subject is a
// not-found, never somebody else's row). Adding a route here needs the same
// argument in its own package's tests.
var meRoutesNamingTheCallersOwnObject = map[string]bool{
	"/v1/me/notifications/{notificationId}/read": true,
	"/v1/me/payout-destinations/{destinationId}": true, // internal/payout: a destination is loaded by (account, id); another account's id is not found
	"/v1/me/verification/sessions/{sessionId}":   true, // internal/verification: a session is loaded under the caller's profile; another person's id is not found
}

func TestProfile_NoSelfServiceRouteTakesAnIdentifier(t *testing.T) {
	t.Parallel()
	h, _ := newProfileHarness(t, &fakeProfile{})
	routes, ok := h.server.Router().(chi.Routes)
	require.True(t, ok, "the router must be walkable")
	var checked int
	err := chi.Walk(routes, func(_, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/v1/me") {
			return nil
		}
		checked++
		if meRoutesNamingTheCallersOwnObject[route] {
			return nil
		}
		assert.NotContainsf(t, route, "{", "%s takes a path parameter; a /me route must name nobody but the caller", route)
		return nil
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, checked, 6, "the walk found no /v1/me routes; this proves nothing")
}

// And the subject the domain sees is the principal's, never anything from the
// request.
func TestProfile_TheSubjectComesFromThePrincipal(t *testing.T) {
	t.Parallel()
	fake := &fakeProfile{prof: sampleProfile()}
	h, _ := newProfileHarness(t, fake)
	p := customerPrincipal()
	h.as(&p)

	res := h.do(http.MethodPost, "/v1/me/profile", map[string]any{"display_name": "Ada"},
		"Idempotency-Key", newKeyed())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.NotEmpty(t, fake.seenSub)
	assert.Equal(t, p.SubjectID, fake.seenSub[len(fake.seenSub)-1])
}

// ------------------------------------------------------------------- authz

// Requesting closure ends every session and cannot be undone once effected, so
// it demands a recent strong authentication.
func TestProfile_ClosureRequestRequiresStepUp(t *testing.T) {
	t.Parallel()
	h, _ := newProfileHarness(t, &fakeProfile{})
	stale := customerPrincipal()
	stale.AuthTime = testNow.Add(-2 * time.Hour)
	h.as(&stale)

	res := h.do(http.MethodPost, "/v1/me/account/close", map[string]any{}, "Idempotency-Key", newKeyed())
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, string(errs.CodeStepUpRequired), problemCode(t, res))

	// A password-only login is not a step-up either, however recent.
	weak := customerPrincipal()
	weak.AMR = []string{"pwd"}
	h.as(&weak)
	res = h.do(http.MethodPost, "/v1/me/account/close", map[string]any{}, "Idempotency-Key", newKeyed())
	assert.Equal(t, string(errs.CodeStepUpRequired), problemCode(t, res))
}

// Cancelling must never be harder than requesting, or a user who cannot step up
// could not undo a request made from a session that could.
func TestProfile_CancellingAClosureDoesNotRequireStepUp(t *testing.T) {
	t.Parallel()
	h, _ := newProfileHarness(t, &fakeProfile{view: profile.AccountView{
		UserID: testUserID.String(), UserStatus: "ACTIVE", CoolingOff: profile.DefaultCoolingOff,
	}})
	stale := customerPrincipal()
	stale.AuthTime = testNow.Add(-2 * time.Hour)
	stale.AMR = []string{"pwd"}
	h.as(&stale)

	res := h.do(http.MethodPost, "/v1/me/account/close/cancel", nil, "Idempotency-Key", newKeyed())
	assert.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
}

func TestProfile_ACustomerCannotReachTheSupportView(t *testing.T) {
	t.Parallel()
	h, fake := newProfileHarness(t, &fakeProfile{admin: profile.AdminUserView{UserID: testUserID.String()}})
	p := customerPrincipal()
	h.as(&p)

	other := accounts.NewUserID().String()
	res := h.do(http.MethodGet, "/v1/admin/users/"+other, nil)
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Empty(t, fake.seenSub, "the handler ran for a customer")

	res = h.do(http.MethodPost, "/v1/admin/users/"+other+"/closure",
		map[string]any{"decision": "EFFECT", "reason": "customer request"}, "Idempotency-Key", newKeyed())
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Empty(t, fake.seenSub)
}

// An operator holding only read permissions can look, and cannot decide.
func TestProfile_SupportReadOnlyCanLookButNotDecide(t *testing.T) {
	t.Parallel()
	h, _ := newProfileHarness(t, &fakeProfile{admin: profile.AdminUserView{
		UserID: testUserID.String(), UserStatus: "ACTIVE", AuditStream: "account:x",
	}})
	p := security.Principal{
		SubjectID: accounts.NewUserID().String(), ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleSupportReadOnly}, SessionID: testSessionID,
		AuthTime: testNow.Add(-time.Minute), AMR: []string{"pwd", "mfa"},
	}
	h.as(&p)

	res := h.do(http.MethodGet, "/v1/admin/users/"+testUserID.String(), nil)
	assert.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	res = h.do(http.MethodPost, "/v1/admin/users/"+testUserID.String()+"/closure",
		map[string]any{"decision": "EFFECT", "reason": "customer request"}, "Idempotency-Key", newKeyed())
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
}

// Deciding a closure is an account status change, so it takes the same step-up
// PostAdminAccountsAccountIdStatus takes.
func TestProfile_DecidingAClosureRequiresStepUp(t *testing.T) {
	t.Parallel()
	h, _ := newProfileHarness(t, &fakeProfile{admin: profile.AdminUserView{UserID: testUserID.String()}})
	op := security.Principal{
		SubjectID: accounts.NewUserID().String(), ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleCompliance}, SessionID: testSessionID,
		AuthTime: testNow.Add(-2 * time.Hour), AMR: []string{"pwd", "mfa"},
	}
	h.as(&op)
	res := h.do(http.MethodPost, "/v1/admin/users/"+testUserID.String()+"/closure",
		map[string]any{"decision": "REFUSE", "reason": "an unsettled payout"}, "Idempotency-Key", newKeyed())
	assert.Equal(t, string(errs.CodeStepUpRequired), problemCode(t, res))
}

// ------------------------------------------------------- wiring and conversion

// A deployment with no profile service says so, rather than answering an empty
// profile that reads as "you have not set a name".
func TestProfile_ANilPortAnswersUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p := customerPrincipal()
	h.as(&p)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/me/profile"},
		{http.MethodGet, "/v1/me/terms-acceptances"},
		{http.MethodPost, "/v1/me/terms-acceptances"},
		{http.MethodGet, "/v1/me/account"},
		{http.MethodPost, "/v1/me/account/close"},
		{http.MethodPost, "/v1/me/account/close/cancel"},
	} {
		var body any
		if tc.method == http.MethodPost {
			body = map[string]any{"display_name": "Ada", "document_ids": []string{"TERMS_OF_SERVICE"}}
		}
		res := h.do(tc.method, tc.path, body, "Idempotency-Key", newKeyed())
		assert.Equalf(t, string(errs.CodeUnsupported), problemCode(t, res), "%s %s", tc.method, tc.path)
	}
}

// GET /v1/me keeps every field it had, and gains two.
func TestProfile_MeIsExtendedAdditively(t *testing.T) {
	t.Parallel()
	prof := sampleProfile()
	prof.Onboarding.DisplayNameSetAt = &testNow
	h, _ := newProfileHarness(t, &fakeProfile{me: profile.Me{Profile: prof, Onboarding: prof.Onboarding}})
	p := customerPrincipal()
	h.as(&p)

	res := h.do(http.MethodGet, "/v1/me", nil)
	require.Equal(t, http.StatusOK, res.Code)
	var out api.Principal
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))

	// Unchanged.
	assert.Equal(t, p.SubjectID, out.SubjectId.String())
	assert.Equal(t, api.PrincipalActorType(p.ActorType), out.ActorType)
	assert.Equal(t, []string{string(security.RoleCustomer)}, out.Roles)
	assert.NotNil(t, out.StepUpValidUntil)

	// Added.
	require.NotNil(t, out.Profile)
	assert.Equal(t, "ada", *out.Profile.Handle)
	assert.Equal(t, prof.AvatarSeed, out.Profile.AvatarSeed)
	require.NotNil(t, out.Onboarding)
	assert.False(t, out.Onboarding.Complete)
	require.NotNil(t, out.Onboarding.NextStep)
	assert.Equal(t, api.OnboardingNextStep(profile.StepTerms), *out.Onboarding.NextStep)
}

// A profile that cannot be read must not log everybody out: /v1/me is what the
// web app asks to find out whether it has a session at all.
func TestProfile_MeStillAnswersWhenTheProfileCannotBeRead(t *testing.T) {
	t.Parallel()
	h, _ := newProfileHarness(t, &fakeProfile{err: errs.New(errs.CodeInternal, "database down")})
	p := customerPrincipal()
	h.as(&p)

	res := h.do(http.MethodGet, "/v1/me", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.Principal
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, p.SubjectID, out.SubjectId.String())
	assert.Nil(t, out.Profile)
	assert.Nil(t, out.Onboarding)
}

// The security page is derived from data that already exists: sessions and the
// current session's claims.
func TestProfile_SecuritySummaryIsDerivedFromSessions(t *testing.T) {
	t.Parallel()
	h, _ := newProfileHarness(t, &fakeProfile{})
	p := customerPrincipal()
	h.as(&p)

	res := h.do(http.MethodGet, "/v1/me/security", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.SecuritySummary
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, 1, out.ActiveSessions)
	assert.True(t, out.MfaPresent)
	assert.Equal(t, []string{"pwd", "mfa"}, out.Amr)
	require.NotNil(t, out.LastStepUpAt)
	assert.Equal(t, p.AuthTime.UTC(), out.LastStepUpAt.UTC())
	require.NotNil(t, out.LastLoginAt)
	assert.Equal(t, int(effectiveStepUpMaxAge(h.server.opts.StepUpMaxAge)/time.Second), out.StepUpMaxAgeSeconds)
	require.NotNil(t, out.CurrentSessionId)
	assert.Equal(t, testSessionID, out.CurrentSessionId.String())
}

// A password-only login is reported as having no MFA and no step-up, rather
// than as having one that is merely old.
func TestProfile_SecuritySummaryDoesNotReportAStepUpThatNeverHappened(t *testing.T) {
	t.Parallel()
	h, _ := newProfileHarness(t, &fakeProfile{})
	p := customerPrincipal()
	p.AMR = []string{"pwd"}
	h.as(&p)

	res := h.do(http.MethodGet, "/v1/me/security", nil)
	require.Equal(t, http.StatusOK, res.Code)
	var out api.SecuritySummary
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.False(t, out.MfaPresent)
	assert.Nil(t, out.LastStepUpAt)
	assert.Nil(t, out.StepUpValidUntil)
}

// The terms collection carries the bytes it records acceptance of, and says
// which documents still need counsel.
func TestProfile_TermsCollectionCarriesTheDocumentAndItsWarning(t *testing.T) {
	t.Parallel()
	docs, err := terms.Current()
	require.NoError(t, err)
	view := profile.TermsView{}
	for _, d := range docs {
		view.Documents = append(view.Documents, profile.DocumentStatus{Document: d})
		if d.Requirement == terms.AtOnboarding {
			view.Outstanding = append(view.Outstanding, d.ID)
		}
	}
	h, _ := newProfileHarness(t, &fakeProfile{terms: view})
	p := customerPrincipal()
	h.as(&p)

	res := h.do(http.MethodGet, "/v1/me/terms-acceptances", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.TermsState
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	require.Len(t, out.Documents, len(docs))
	for _, d := range out.Documents {
		assert.True(t, d.CounselReviewRequired, "%s", d.DocumentId)
		require.NotNil(t, d.Body, "%s carries no text, so a client cannot show what it is recording", d.DocumentId)
		assert.NotEmpty(t, *d.Body)
		assert.Len(t, d.ContentHash, 64)
	}
	assert.Len(t, out.Outstanding, len(terms.RequiredAt(terms.AtOnboarding)))
}

// The support view reports a verification level only when one is known, and
// never invents NONE.
func TestProfile_SupportViewDoesNotInventAVerificationLevel(t *testing.T) {
	t.Parallel()
	unknown := profile.AdminUserView{UserID: testUserID.String(), UserStatus: "ACTIVE", AuditStream: "account:x"}
	got := toAPIAdminUser(unknown, testNow)
	assert.False(t, got.Verification.Known)
	assert.Nil(t, got.Verification.Level)

	known := unknown
	known.Verification, known.VerificationKnown = "NODAL_IDENTITY", true
	got = toAPIAdminUser(known, testNow)
	assert.True(t, got.Verification.Known)
	require.NotNil(t, got.Verification.Level)
	assert.Equal(t, "NODAL_IDENTITY", *got.Verification.Level)
}

// The decision the operator sent is the decision the domain is asked for; the
// handler invents nothing.
func TestProfile_DecisionIsPassedThroughUnchanged(t *testing.T) {
	t.Parallel()
	h, fake := newProfileHarness(t, &fakeProfile{admin: profile.AdminUserView{
		UserID: testUserID.String(), UserStatus: "CLOSED", AuditStream: "account:x",
	}})
	op := security.Principal{
		SubjectID: accounts.NewUserID().String(), ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleCompliance}, SessionID: testSessionID,
		AuthTime: testNow.Add(-time.Minute), AMR: []string{"pwd", "mfa"},
	}
	h.as(&op)
	res := h.do(http.MethodPost, "/v1/admin/users/"+testUserID.String()+"/closure",
		map[string]any{"decision": "EFFECT", "reason": "the cooling-off period has passed"},
		"Idempotency-Key", newKeyed())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testUserID.String(), fake.decided.userID)
	assert.Equal(t, profile.DecisionEffect, fake.decided.decision)
	assert.Equal(t, "the cooling-off period has passed", fake.decided.reason)
}

// A cooling-off period expressed in days is what the surface shows, and it must
// come from the service rather than being restated in the handler.
func TestProfile_CoolingOffDaysComeFromTheService(t *testing.T) {
	t.Parallel()
	view := profile.AccountView{
		UserID: testUserID.String(), UserStatus: "ACTIVE", CoolingOff: profile.DefaultCoolingOff,
	}
	assert.Equal(t, 14, toAPIMyAccount(view, testNow).CoolingOffDays)
	view.CoolingOff = 48 * time.Hour
	assert.Equal(t, 2, toAPIMyAccount(view, testNow).CoolingOffDays)
}

// ---------------------------------------------------- the operator console

// An account names the user who owns it. Without this an operator surface can
// list accounts and cannot reach the person who holds one, which is what the
// support view exists for.
func TestProfile_AnAccountNamesItsOwner(t *testing.T) {
	t.Parallel()
	acct := accounts.Account{
		ID: testAccountID, OwnerUserID: testUserID, Kind: accounts.KindCustomer,
		Status: accounts.StatusActive, CreatedAt: testNow,
	}
	got := toAPIAccount(acct)
	require.NotNil(t, got.OwnerUserId, "an account with an owner reported none")
	assert.Equal(t, testUserID.String(), got.OwnerUserId.String())
	assert.Equal(t, testAccountID.String(), got.Id.String())

	// It reaches the wire on the operator listing, which is the route the
	// console reads.
	h, _ := newProfileHarness(t, &fakeProfile{})
	op := operatorPrincipal()
	h.as(&op)
	res := h.do(http.MethodGet, "/v1/admin/accounts", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var page struct {
		Items []api.Account `json:"items"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &page))
	require.NotEmpty(t, page.Items)
	require.NotNil(t, page.Items[0].OwnerUserId)
	assert.Equal(t, testUserID.String(), page.Items[0].OwnerUserId.String())
}

// A live break-glass elevation reports when it expires, so a console can say how
// long is left rather than only that one exists.
func TestProfile_MeReportsTheBreakGlassExpiry(t *testing.T) {
	t.Parallel()
	h, _ := newProfileHarness(t, &fakeProfile{})
	until := testNow.Add(30 * time.Minute)
	op := operatorPrincipal()
	op.Roles = append(op.Roles, security.RoleBreakGlass)
	op.BreakGlassUntil = &until
	require.NoError(t, op.Validate())
	h.as(&op)

	res := h.do(http.MethodGet, "/v1/me", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.Principal
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	require.NotNil(t, out.BreakGlassUntil, "a live elevation reported no expiry")
	assert.Equal(t, until.UTC(), out.BreakGlassUntil.UTC())
	assert.Contains(t, out.Roles, string(security.RoleBreakGlass))

	// A principal with no elevation reports none rather than a zero time, which
	// would read as an elevation that expired in 1970.
	plain := customerPrincipal()
	h.as(&plain)
	res = h.do(http.MethodGet, "/v1/me", nil)
	require.Equal(t, http.StatusOK, res.Code)
	// A fresh value: unmarshalling into the one above would leave the previous
	// pointer in place and the assertion would pass for the wrong reason.
	var noElevation api.Principal
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &noElevation))
	assert.Nil(t, noElevation.BreakGlassUntil)
}

func problemCode(t *testing.T, res *response) string {
	t.Helper()
	var p struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &p), "body=%s", res.Body.String())
	return p.Code
}
