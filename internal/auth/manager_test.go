package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/auth/authtest"
	"github.com/nodal/controlplane/internal/security"
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

type fixture struct {
	clk   *authtest.Clock
	store *authtest.MemorySessionStore
	mgr   *auth.Manager
}

func newFixture(t *testing.T, cfg auth.ManagerConfig) fixture {
	t.Helper()
	clk := authtest.NewClock(t0)
	store := authtest.NewMemorySessionStore(clk.Now)
	cfg.Now = clk.Now
	mgr, err := auth.NewManager(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{clk: clk, store: store, mgr: mgr}
}

func customerParams(sub string) auth.IssueParams {
	return auth.IssueParams{
		SubjectID: sub, ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{"acct-" + sub}, AuthTime: t0, AMR: []string{"pwd", "mfa"},
		IP: "203.0.113.7", UserAgent: "test-agent", DeviceLabel: "laptop",
	}
}

func mustIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got %v, want errors.Is(%v)", err, target)
	}
}

func TestNewManager_Config(t *testing.T) {
	store := authtest.NewMemorySessionStore(nil)
	if _, err := auth.NewManager(nil, auth.ManagerConfig{}); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := auth.NewManager(store, auth.ManagerConfig{TTL: -1}); err == nil {
		t.Fatal("negative ttl accepted")
	}
	if _, err := auth.NewManager(store, auth.ManagerConfig{TTL: time.Minute, IdleTimeout: time.Hour}); err == nil {
		t.Fatal("idle > ttl accepted")
	}
	m, err := auth.NewManager(store, auth.ManagerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if now := m.Now(); now.Location() != time.UTC || time.Since(now) > time.Minute {
		t.Fatalf("default clock not UTC wall clock: %v", now)
	}
}

func TestManager_IssueAndAuthenticate(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{TTL: time.Hour, IdleTimeout: 30 * time.Minute})
	ctx := context.Background()
	iss, err := f.mgr.Issue(ctx, nil, customerParams("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.ValidateToken(iss.Token); err != nil {
		t.Fatal(err)
	}
	s := iss.Session
	if s.ID == "" || s.TokenHash != auth.HashToken(iss.Token) || s.TokenHash == iss.Token {
		t.Fatalf("session not keyed by hash: %+v", s)
	}
	if !s.CreatedAt.Equal(t0) || !s.LastSeenAt.Equal(t0) || !s.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("timestamps wrong: %+v", s)
	}
	if _, err := f.store.Get(ctx, nil, iss.Token); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatal("store must not be keyed by raw token")
	}
	got, err := f.mgr.Authenticate(ctx, nil, iss.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != s.ID || got.SubjectID != "alice" {
		t.Fatalf("authenticated wrong session: %+v", got)
	}
	p, err := got.Principal()
	if err != nil {
		t.Fatal(err)
	}
	if p.SubjectID != "alice" || p.SessionID != s.ID || !p.HasRole(security.RoleCustomer) || !p.OwnsAccount("acct-alice") || !security.HasStrongAMR(p.AMR) {
		t.Fatalf("principal wrong: %+v", p)
	}
	if err := security.RequireAccount(security.WithPrincipal(ctx, p), "acct-alice"); err != nil {
		t.Fatal(err)
	}
	if err := security.RequireAccount(security.WithPrincipal(ctx, p), "acct-bob"); !errors.Is(err, security.ErrCrossTenant) {
		t.Fatalf("cross-tenant allowed: %v", err)
	}
}

func TestManager_MalformedTokenNeverHitsStore(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{})
	f.store.FailNext(errors.New("store must not be called"))
	_, err := f.mgr.Authenticate(context.Background(), nil, "not-a-token")
	mustIs(t, err, auth.ErrInvalidToken)
	mustIs(t, err, security.ErrUnauthenticated)
	f.store.FailNext(nil)
}

func TestManager_UnknownToken(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{})
	raw, _ := auth.NewToken()
	_, err := f.mgr.Authenticate(context.Background(), nil, raw)
	mustIs(t, err, auth.ErrSessionNotFound)
	mustIs(t, err, auth.ErrInvalidSession)
	mustIs(t, err, security.ErrUnauthenticated)
}

func TestManager_Expiry(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{TTL: time.Hour, IdleTimeout: time.Hour, TouchInterval: time.Second})
	ctx := context.Background()
	iss, _ := f.mgr.Issue(ctx, nil, customerParams("alice"))
	f.clk.Set(t0.Add(time.Hour - time.Second))
	if _, err := f.mgr.Authenticate(ctx, nil, iss.Token); err != nil {
		t.Fatalf("just before expiry: %v", err)
	}
	f.clk.Set(t0.Add(time.Hour))
	_, err := f.mgr.Authenticate(ctx, nil, iss.Token)
	mustIs(t, err, auth.ErrSessionExpired)
	mustIs(t, err, security.ErrUnauthenticated)
}

func TestManager_IdleTimeoutAndTouch(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{TTL: 24 * time.Hour, IdleTimeout: 30 * time.Minute, TouchInterval: time.Minute})
	ctx := context.Background()
	iss, _ := f.mgr.Issue(ctx, nil, customerParams("alice"))

	// Activity every 20 minutes keeps the session alive well past 30m.
	for i := 1; i <= 5; i++ {
		f.clk.Advance(20 * time.Minute)
		s, err := f.mgr.Authenticate(ctx, nil, iss.Token)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if !s.LastSeenAt.Equal(f.clk.Now()) {
			t.Fatalf("round %d: LastSeenAt not touched: %v", i, s.LastSeenAt)
		}
	}
	if f.store.Touches() != 5 {
		t.Fatalf("touches = %d, want 5", f.store.Touches())
	}
	// Exactly at the idle limit is still valid; one nanosecond past is not.
	f.clk.Advance(30 * time.Minute)
	if _, err := f.mgr.Authenticate(ctx, nil, iss.Token); err != nil {
		t.Fatalf("at idle limit: %v", err)
	}
	f.clk.Advance(30*time.Minute + time.Nanosecond)
	_, err := f.mgr.Authenticate(ctx, nil, iss.Token)
	mustIs(t, err, auth.ErrSessionIdle)
	mustIs(t, err, security.ErrUnauthenticated)
}

func TestManager_TouchRateLimited(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{TouchInterval: time.Minute})
	ctx := context.Background()
	iss, _ := f.mgr.Issue(ctx, nil, customerParams("alice"))
	f.clk.Advance(30 * time.Second)
	s, _ := f.mgr.Authenticate(ctx, nil, iss.Token)
	if f.store.Touches() != 0 || !s.LastSeenAt.Equal(t0) {
		t.Fatal("touched before the interval elapsed")
	}
	f.clk.Advance(30 * time.Second)
	s, _ = f.mgr.Authenticate(ctx, nil, iss.Token)
	if f.store.Touches() != 1 || !s.LastSeenAt.Equal(t0.Add(time.Minute)) {
		t.Fatal("not touched once the interval elapsed")
	}
}

func TestManager_Revoke(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{})
	ctx := context.Background()
	iss, _ := f.mgr.Issue(ctx, nil, customerParams("alice"))
	if err := f.mgr.Revoke(ctx, nil, iss.Session.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.mgr.Authenticate(ctx, nil, iss.Token)
	mustIs(t, err, auth.ErrSessionRevoked)
	mustIs(t, err, security.ErrUnauthenticated)
	if err := f.mgr.Revoke(ctx, nil, iss.Session.ID); err != nil {
		t.Fatalf("revoke must be idempotent: %v", err)
	}
	mustIs(t, f.mgr.Revoke(ctx, nil, "no-such-id"), auth.ErrSessionNotFound)
}

func TestManager_RevokeAllForSubject(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{})
	ctx := context.Background()
	var alice []auth.Issued
	for i := 0; i < 3; i++ {
		iss, _ := f.mgr.Issue(ctx, nil, customerParams("alice"))
		alice = append(alice, iss)
	}
	bob, _ := f.mgr.Issue(ctx, nil, customerParams("bob"))
	n, err := f.mgr.RevokeAllForSubject(ctx, nil, "alice")
	if err != nil || n != 3 {
		t.Fatalf("revoked %d, err %v", n, err)
	}
	for _, iss := range alice {
		if _, err := f.mgr.Authenticate(ctx, nil, iss.Token); !errors.Is(err, auth.ErrSessionRevoked) {
			t.Fatalf("alice session still valid: %v", err)
		}
	}
	if _, err := f.mgr.Authenticate(ctx, nil, bob.Token); err != nil {
		t.Fatalf("bob was logged out: %v", err)
	}
	if n, _ := f.mgr.RevokeAllForSubject(ctx, nil, "alice"); n != 0 {
		t.Fatalf("second revoke-all counted %d", n)
	}
}

func TestManager_Rotate(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{TTL: time.Hour, IdleTimeout: time.Hour})
	ctx := context.Background()
	first, _ := f.mgr.Issue(ctx, nil, customerParams("alice"))
	f.clk.Advance(10 * time.Minute)

	rot := auth.RotationFrom(first.Session)
	until := f.clk.Now().Add(15 * time.Minute)
	rot.Roles = []security.Role{security.RoleCustomer}
	rot.AuthTime = f.clk.Now()
	rot.AMR = []string{"pwd", "webauthn"}
	rot.BreakGlassUntil = &until
	second, err := f.mgr.Rotate(ctx, nil, first.Session, rot)
	if err != nil {
		t.Fatal(err)
	}
	if second.Token == first.Token || second.Session.ID == first.Session.ID {
		t.Fatal("rotation reused token or id")
	}
	if second.Session.RotatedFrom != first.Session.ID {
		t.Fatal("RotatedFrom not set")
	}
	if !second.Session.ExpiresAt.Equal(first.Session.ExpiresAt) {
		t.Fatalf("rotation extended absolute expiry: %v vs %v", second.Session.ExpiresAt, first.Session.ExpiresAt)
	}
	if second.Session.IP != first.Session.IP || second.Session.DeviceLabel != first.Session.DeviceLabel {
		t.Fatal("device facts not carried over")
	}
	if !second.Session.AuthTime.Equal(f.clk.Now()) || second.Session.BreakGlassUntil == nil {
		t.Fatalf("privilege state not applied: %+v", second.Session)
	}
	_, err = f.mgr.Authenticate(ctx, nil, first.Token)
	mustIs(t, err, auth.ErrSessionRevoked)
	if _, err := f.mgr.Authenticate(ctx, nil, second.Token); err != nil {
		t.Fatal(err)
	}
	// A revoked session cannot be rotated again.
	if _, err := f.mgr.Rotate(ctx, nil, first.Session, rot); !errors.Is(err, auth.ErrSessionRevoked) {
		t.Fatalf("rotated a revoked session: %v", err)
	}
	// An expired session cannot be rotated into a live one.
	f.clk.Set(first.Session.ExpiresAt)
	if _, err := f.mgr.Rotate(ctx, nil, second.Session, rot); !errors.Is(err, auth.ErrSessionExpired) {
		t.Fatalf("rotated an expired session: %v", err)
	}
}

func TestManager_AgentSessions(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{})
	ctx := context.Background()
	_, err := f.mgr.Issue(ctx, nil, auth.IssueParams{
		SubjectID: "agent-1", ActorType: security.ActorAgent, Roles: []security.Role{security.RoleAdmin}, AccountIDs: []string{"acct-1"},
	})
	if err == nil {
		t.Fatal("agent session with roles was issued")
	}
	_, err = f.mgr.Issue(ctx, nil, auth.IssueParams{SubjectID: "agent-1", ActorType: security.ActorAgent, AccountIDs: []string{"a", "b"}})
	if err == nil {
		t.Fatal("agent session with two accounts was issued")
	}
	iss, err := f.mgr.Issue(ctx, nil, auth.IssueParams{SubjectID: "agent-1", ActorType: security.ActorAgent, AccountIDs: []string{"acct-1"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := iss.Session.Principal()
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsAgent() || len(p.Roles) != 0 || p.SessionID != iss.Session.ID {
		t.Fatalf("agent principal wrong: %+v", p)
	}
	pctx := security.WithPrincipal(ctx, p)
	if err := security.Require(pctx, security.PermKillActivate); !errors.Is(err, security.ErrForbidden) {
		t.Fatal("agent session satisfied kill:activate")
	}
	if err := security.Require(pctx, security.PermPredictionCommit); err != nil {
		t.Fatal(err)
	}
}

func TestSession_PrincipalFailsClosedOnCorruptRow(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{})
	iss, _ := f.mgr.Issue(context.Background(), nil, customerParams("alice"))
	s := iss.Session
	s.Roles = []security.Role{"ROOT"}
	if _, err := s.Principal(); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("corrupt row produced a principal: %v", err)
	}
	if err := f.mgr.Check(s, t0); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("Check accepted corrupt row: %v", err)
	}
}

func TestManager_StoreFailureIsNotUnauthenticated(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{})
	ctx := context.Background()
	iss, _ := f.mgr.Issue(ctx, nil, customerParams("alice"))
	outage := errors.New("connection refused")
	f.store.FailNext(outage)
	_, err := f.mgr.Authenticate(ctx, nil, iss.Token)
	if !errors.Is(err, outage) || errors.Is(err, security.ErrUnauthenticated) {
		t.Fatalf("store outage must surface as an error, not anonymous: %v", err)
	}
	f.store.FailNext(outage)
	if _, err := f.mgr.Issue(ctx, nil, customerParams("bob")); !errors.Is(err, outage) {
		t.Fatalf("issue error: %v", err)
	}
}

func TestManager_ListForSubject(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{})
	ctx := context.Background()
	a, _ := f.mgr.Issue(ctx, nil, customerParams("alice"))
	f.clk.Advance(time.Minute)
	b, _ := f.mgr.Issue(ctx, nil, customerParams("alice"))
	f.mgr.Issue(ctx, nil, customerParams("bob")) //nolint:errcheck // listing test
	_ = f.mgr.Revoke(ctx, nil, a.Session.ID)

	list, err := f.mgr.ListForSubject(ctx, nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != b.Session.ID || list[1].ID != a.Session.ID {
		t.Fatalf("list wrong: %+v", list)
	}
	if list[1].RevokedAt == nil || list[0].RevokedAt != nil {
		t.Fatalf("revocation not reflected: %+v", list)
	}
	if list[0].DeviceLabel != "laptop" || list[0].IP != "203.0.113.7" {
		t.Fatalf("device facts missing: %+v", list[0])
	}
}

func TestSession_Validate(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{})
	iss, _ := f.mgr.Issue(context.Background(), nil, customerParams("alice"))
	good := iss.Session
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*auth.Session){
		"empty id":         func(s *auth.Session) { s.ID = "" },
		"raw token stored": func(s *auth.Session) { s.TokenHash = iss.Token },
		"upper hex":        func(s *auth.Session) { s.TokenHash = "ABCDEF" + s.TokenHash[6:] },
		"zero created":     func(s *auth.Session) { s.CreatedAt = time.Time{} },
		"expires before":   func(s *auth.Session) { s.ExpiresAt = s.CreatedAt },
		"seen before":      func(s *auth.Session) { s.LastSeenAt = s.CreatedAt.Add(-time.Second) },
		"empty subject":    func(s *auth.Session) { s.SubjectID = "" },
		"bad actor":        func(s *auth.Session) { s.ActorType = "X" },
		"break glass":      func(s *auth.Session) { s.Roles = append(s.Roles, security.RoleBreakGlass) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s := good
			s.Roles = append([]security.Role(nil), good.Roles...)
			mutate(&s)
			if s.Validate() == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// TestElevate_BoundsAndScope pins what Manager.Elevate refuses. The store
// enforces *which* sessions are eligible; the manager enforces that the
// deadline is a real, bounded one, because an unbounded or already-past
// elevation is either a permanent privilege or a silent no-op, and both read
// as "granted" to anything that only checks the error.
func TestElevate_BoundsAndScope(t *testing.T) {
	f := newFixture(t, auth.ManagerConfig{TTL: 12 * time.Hour, IdleTimeout: time.Hour})
	ctx := context.Background()

	op := auth.IssueParams{
		SubjectID: "op-1", ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleAdmin}, AuthTime: t0, AMR: []string{"pwd", "mfa"},
	}
	if _, err := f.mgr.Issue(ctx, nil, op); err != nil {
		t.Fatalf("issue: %v", err)
	}

	for name, until := range map[string]time.Time{
		"already past":       t0.Add(-time.Second),
		"exactly now":        t0,
		"beyond the ceiling": t0.Add(auth.MaxBreakGlassElevation + time.Second),
	} {
		if n, err := f.mgr.Elevate(ctx, nil, "op-1", until); err == nil {
			t.Fatalf("%s: elevation accepted (n=%d)", name, n)
		}
	}
	if _, err := f.mgr.Elevate(ctx, nil, "", t0.Add(time.Minute)); err == nil {
		t.Fatal("empty subject accepted")
	}

	// The ceiling itself is allowed, and the elevation lands on the session.
	n, err := f.mgr.Elevate(ctx, nil, "op-1", t0.Add(auth.MaxBreakGlassElevation))
	if err != nil {
		t.Fatalf("elevate: %v", err)
	}
	if n != 1 {
		t.Fatalf("elevated %d sessions, want 1", n)
	}

	// A subject with no session is not an error, and elevates nothing: the
	// caller decides whether that is acceptable.
	if n, err := f.mgr.Elevate(ctx, nil, "op-2", t0.Add(time.Minute)); err != nil || n != 0 {
		t.Fatalf("unknown subject: n=%d err=%v", n, err)
	}
}
