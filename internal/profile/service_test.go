package profile

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/terms"
)

type stubSessions struct{}

func (stubSessions) RevokeAllForSubject(context.Context, auth.Querier, string) (int, error) {
	return 0, nil
}

// Every dependency is required, and a nil one is refused at construction rather
// than discovered at the first request that needed it.
func TestNew_RefusesAMissingDependency(t *testing.T) {
	t.Parallel()
	full := Deps{
		DB: &db.DB{}, Repo: NewRepository(), Accounts: accounts.NewRepository(),
		Audit: audit.NewWriter(), Clock: clock.NewFake(testTime()), Sessions: stubSessions{},
	}
	_, err := New(full)
	require.NoError(t, err)

	for name, mutate := range map[string]func(*Deps){
		"no database":   func(d *Deps) { d.DB = nil },
		"no repository": func(d *Deps) { d.Repo = nil },
		"no accounts":   func(d *Deps) { d.Accounts = nil },
		"no audit":      func(d *Deps) { d.Audit = nil },
		"no clock":      func(d *Deps) { d.Clock = nil },
		// A closed account whose sessions are left live is closed only on paper.
		"no session ender": func(d *Deps) { d.Sessions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			d := full
			mutate(&d)
			_, err := New(d)
			assert.Error(t, err)
		})
	}
}

func TestNew_DefaultsTheCoolingOffPeriod(t *testing.T) {
	t.Parallel()
	s, err := New(Deps{
		DB: &db.DB{}, Repo: NewRepository(), Accounts: accounts.NewRepository(),
		Audit: audit.NewWriter(), Clock: clock.NewFake(testTime()), Sessions: stubSessions{},
	})
	require.NoError(t, err)
	assert.Equal(t, DefaultCoolingOff, s.d.CoolingOff)

	s, err = New(Deps{
		DB: &db.DB{}, Repo: NewRepository(), Accounts: accounts.NewRepository(),
		Audit: audit.NewWriter(), Clock: clock.NewFake(testTime()), Sessions: stubSessions{},
		CoolingOff: time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, time.Minute, s.d.CoolingOff)
}

// An AGENT has no profile and must not be able to act on one. It is refused in
// the domain as well as at the boundary (where authz refuses every agent), so
// the rule survives a future caller that is not an HTTP handler.
func TestActor_RefusesAnAgentAndAnEmptySubject(t *testing.T) {
	t.Parallel()
	err := Actor{UserID: "u", ActorType: security.ActorAgent}.validate()
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	err = Actor{ActorType: security.ActorUser}.validate()
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))

	require.NoError(t, Actor{UserID: "u", ActorType: security.ActorUser}.validate())
	require.NoError(t, Actor{UserID: "u", ActorType: security.ActorOperator}.validate())
}

func TestBuildTermsView_OutstandingIsOnboardingOnly(t *testing.T) {
	t.Parallel()
	view := buildTermsView(nil)
	require.Len(t, view.Documents, len(terms.MustCurrent()))
	assert.Len(t, view.Outstanding, len(terms.RequiredAt(terms.AtOnboarding)))
	for _, id := range view.Outstanding {
		d, ok := terms.Get(id)
		require.True(t, ok)
		assert.Equal(t, terms.AtOnboarding, d.Requirement,
			"the withdrawal disclosure is outstanding at signup, which is the frontloading the goal forbids")
	}
}

func TestBuildTermsView_AcceptanceCountsOnlyAtTheCurrentBytes(t *testing.T) {
	t.Parallel()
	doc, ok := terms.Get(terms.TermsOfService)
	require.True(t, ok)
	at := testTime()

	current := []Acceptance{{
		DocumentID: doc.ID, Version: doc.Version, ContentHash: doc.ContentHash, AcceptedAt: at,
	}}
	view := buildTermsView(current)
	assert.NotContains(t, view.Outstanding, doc.ID)
	assert.True(t, statusOf(t, view, doc.ID).Accepted)

	// D-053: the same version with different bytes is a document nobody has
	// agreed to, and the API asks again rather than treating it as accepted.
	stale := []Acceptance{{
		DocumentID: doc.ID, Version: doc.Version,
		ContentHash: "0000000000000000000000000000000000000000000000000000000000000000",
		AcceptedAt:  at,
	}}
	view = buildTermsView(stale)
	assert.Contains(t, view.Outstanding, doc.ID)
	assert.False(t, statusOf(t, view, doc.ID).Accepted)

	// An older version is not the current one either.
	old := []Acceptance{{
		DocumentID: doc.ID, Version: "2000-01-01.1", ContentHash: doc.ContentHash, AcceptedAt: at,
	}}
	assert.Contains(t, buildTermsView(old).Outstanding, doc.ID)

	// A row naming a document the registry no longer serves is ignored rather
	// than crashing or counting for something.
	assert.Contains(t, buildTermsView([]Acceptance{{
		DocumentID: "COOKIE_POLICY", Version: doc.Version, ContentHash: doc.ContentHash, AcceptedAt: at,
	}}).Outstanding, doc.ID)
}

// The reported acceptance time is the EARLIEST one, so re-reading the terms
// does not move the date a person agreed.
func TestBuildTermsView_ReportsTheEarliestAcceptance(t *testing.T) {
	t.Parallel()
	doc, ok := terms.Get(terms.PrivacyPolicy)
	require.True(t, ok)
	first := testTime()
	second := first.Add(24 * time.Hour)
	view := buildTermsView([]Acceptance{
		{DocumentID: doc.ID, Version: doc.Version, ContentHash: doc.ContentHash, AcceptedAt: second},
		{DocumentID: doc.ID, Version: doc.Version, ContentHash: doc.ContentHash, AcceptedAt: first},
	})
	st := statusOf(t, view, doc.ID)
	require.NotNil(t, st.AcceptedAt)
	assert.Equal(t, first, *st.AcceptedAt)
}

func statusOf(t *testing.T, v TermsView, id terms.DocumentID) DocumentStatus {
	t.Helper()
	for _, d := range v.Documents {
		if d.Document.ID == id {
			return d
		}
	}
	t.Fatalf("%s is not in the view", id)
	return DocumentStatus{}
}

// A user's restrictions are composed from status, never from the free text an
// operator wrote for other operators.
func TestRestrictionsFor(t *testing.T) {
	t.Parallel()
	acctID := accounts.NewAccountID()
	frozen := accounts.Account{ID: acctID, Status: accounts.StatusFrozen, StatusReason: "ticket OPS-4471, suspected mule network"}

	got := restrictionsFor("ACTIVE", []accounts.Account{frozen}, nil)
	require.Len(t, got, 1)
	assert.Equal(t, RestrictionAccountFrozen, got[0].Code)
	assert.Equal(t, acctID.String(), got[0].AccountID)
	assert.NotContains(t, got[0].Message, "OPS-4471")
	assert.NotContains(t, got[0].Message, "mule")

	assert.Empty(t, restrictionsFor("ACTIVE", []accounts.Account{{ID: acctID, Status: accounts.StatusActive}}, nil))

	suspended := restrictionsFor("SUSPENDED", nil, nil)
	require.Len(t, suspended, 1)
	assert.Equal(t, RestrictionUserSuspended, suspended[0].Code)

	pending := restrictionsFor("ACTIVE", nil, &ClosureRequest{State: ClosurePending})
	require.Len(t, pending, 1)
	assert.Equal(t, RestrictionClosurePending, pending[0].Code)
}

// TransitionClosure refuses an illegal edge before it reaches the database, so
// the error a caller sees names the states rather than a trigger.
func TestTransitionClosure_RefusesAnIllegalEdgeAndAnEmptyReason(t *testing.T) {
	t.Parallel()
	r := NewRepository()
	decided := ClosureRequest{ID: "r", State: ClosureCancelled}
	_, err := r.TransitionClosure(context.Background(), nilTx{}, decided, ClosureEffected, "OPERATOR", "op", "because", "", testTime())
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	pending := ClosureRequest{ID: "r", State: ClosurePending}
	_, err = r.TransitionClosure(context.Background(), nilTx{}, pending, ClosureState("DELETED"), "OPERATOR", "op", "because", "", testTime())
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	_, err = r.TransitionClosure(context.Background(), nilTx{}, pending, ClosureCancelled, "USER", "u", "", "", testTime())
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// nilTx satisfies pgx.Tx for the guards above, which must all refuse before any
// statement is issued. Every method panics, so a guard that let a call through
// fails loudly rather than silently reaching a nil connection.
type nilTx struct{ pgx.Tx }
