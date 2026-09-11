//go:build integration

package identity_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/auth/devidp"
	"github.com/nodal/controlplane/internal/auth/pgstore"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/identity"
	"github.com/nodal/controlplane/internal/pii"
	"github.com/nodal/controlplane/internal/security"
)

func newService(t *testing.T) (*identity.Service, *db.DB, *clock.Fake) {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(context.Background(), db.Config{URL: url, MaxConns: 4, MinConns: 1, AppName: "identity-test"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	idp, err := devidp.New("TEST", devidp.Config{Now: clk.Now})
	require.NoError(t, err)
	mgr, err := auth.NewManager(pgstore.New(), auth.ManagerConfig{Now: clk.Now})
	require.NoError(t, err)
	svc, err := identity.New(identity.Deps{IdP: idp, DB: d, Accounts: accounts.NewRepository(), Sessions: mgr, Audit: audit.NewWriter(), Clock: clk})
	require.NoError(t, err)
	return svc, d, clk
}

func TestIntegration_Login_FirstLoginCreatesUserAccountSession(t *testing.T) {
	svc, d, clk := newService(t)
	ctx := context.Background()

	begin, err := svc.Begin(ctx, identity.BeginRequest{ReturnTo: "/home", IP: "203.0.113.9", UserAgent: "ua"})
	require.NoError(t, err)
	assert.Contains(t, begin.RedirectURL, "code_challenge_method=S256")
	assert.Contains(t, begin.RedirectURL, "state="+begin.State)

	// The dev IdP returns a fresh subject per test run only if we vary the identity; use a unique
	// identity by relying on the issuer+subject uniqueness: first login creates, second reuses.
	done, err := svc.Complete(ctx, identity.CompleteRequest{Code: "customer-a", State: begin.State, IP: "203.0.113.9", UserAgent: "ua", RequestID: "req-1"})
	require.NoError(t, err)
	assert.NotEmpty(t, done.Issued.Token)
	assert.Equal(t, security.ActorUser, done.Issued.Session.ActorType)
	assert.Equal(t, []security.Role{security.RoleCustomer}, done.Issued.Session.Roles)
	require.NotEmpty(t, done.Accounts, "first login creates a CUSTOMER account")
	assert.Equal(t, "/home", done.ReturnTo)
	assert.Equal(t, done.User.ID.String(), done.Issued.Session.SubjectID, "session subject is the users.id")

	// Session is usable through the manager/store.
	mgr, _ := auth.NewManager(pgstore.New(), auth.ManagerConfig{Now: clk.Now})
	sess, err := mgr.Authenticate(ctx, d.Pool(), done.Issued.Token)
	require.NoError(t, err)
	p, err := sess.Principal()
	require.NoError(t, err)
	assert.True(t, p.HasRole(security.RoleCustomer))
	assert.ElementsMatch(t, []string{done.Accounts[0].ID.String()}, p.AccountIDs)

	// Replaying the same state is refused; the attempt is single-use.
	_, err = svc.Complete(ctx, identity.CompleteRequest{Code: "customer-a", State: begin.State})
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))

	// Second login of the same identity reuses the user (no duplicate account).
	begin2, err := svc.Begin(ctx, identity.BeginRequest{})
	require.NoError(t, err)
	done2, err := svc.Complete(ctx, identity.CompleteRequest{Code: "customer-a", State: begin2.State})
	require.NoError(t, err)
	assert.Equal(t, done.User.ID, done2.User.ID)
	assert.False(t, done2.Created)
	assert.Len(t, done2.Accounts, len(done.Accounts))

	// Audit and security events were recorded.
	var audits, secs int
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'auth.login' AND actor_id = $1`, done.User.ID.String()).Scan(&audits))
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM security_events WHERE kind = 'login' AND user_id = $1`, done.User.ID).Scan(&secs))
	assert.Equal(t, 2, audits)
	assert.Equal(t, 2, secs)

	// Logout revokes and records.
	require.NoError(t, svc.Logout(ctx, done2.Issued.Session, identity.CompleteRequest{RequestID: "req-2", IP: "203.0.113.9"}))
	_, err = mgr.Authenticate(ctx, d.Pool(), done2.Issued.Token)
	assert.ErrorIs(t, err, auth.ErrSessionRevoked)
}

func TestIntegration_Login_StepUpExpiryUnknownStateAndOperatorRoles(t *testing.T) {
	svc, d, clk := newService(t)
	ctx := context.Background()

	// Step-up requested but the provider only did a password login → refused, attempt marked FAILED.
	begin, err := svc.Begin(ctx, identity.BeginRequest{StepUp: true})
	require.NoError(t, err)
	_, err = svc.Complete(ctx, identity.CompleteRequest{Code: "customer-b", State: begin.State})
	require.ErrorIs(t, err, auth.ErrStepUpNotSatisfied)
	var outcome string
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT outcome FROM login_attempts WHERE state = $1`, begin.State).Scan(&outcome))
	assert.Equal(t, "FAILED", outcome)

	// With MFA the same step-up login succeeds and the session carries the strong AMR.
	begin, err = svc.Begin(ctx, identity.BeginRequest{StepUp: true})
	require.NoError(t, err)
	done, err := svc.Complete(ctx, identity.CompleteRequest{Code: "customer-b:mfa", State: begin.State})
	require.NoError(t, err)
	assert.Contains(t, done.Issued.Session.AMR, "mfa")
	assert.True(t, done.StepUp)

	// Expired attempt.
	begin, err = svc.Begin(ctx, identity.BeginRequest{})
	require.NoError(t, err)
	clk.Advance(identity.DefaultAttemptTTL + time.Second)
	_, err = svc.Complete(ctx, identity.CompleteRequest{Code: "customer-b", State: begin.State})
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))

	// Unknown state and unknown identity.
	_, err = svc.Complete(ctx, identity.CompleteRequest{Code: "customer-b", State: "forged-state"})
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
	begin, err = svc.Begin(ctx, identity.BeginRequest{})
	require.NoError(t, err)
	_, err = svc.Complete(ctx, identity.CompleteRequest{Code: "nobody", State: begin.State})
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))

	// Operator roles come from the operator directory, never from provider claims: the dev
	// "admin" identity logs in as a plain customer until a row in operator_roles grants ADMIN.
	begin, err = svc.Begin(ctx, identity.BeginRequest{})
	require.NoError(t, err)
	asCustomer, err := svc.Complete(ctx, identity.CompleteRequest{Code: "admin:mfa", State: begin.State})
	require.NoError(t, err)
	assert.Equal(t, security.ActorUser, asCustomer.Issued.Session.ActorType)
	_, err = d.Pool().Exec(ctx, `INSERT INTO operator_roles (user_id, role, reason) VALUES ($1, 'ADMIN', 'test grant')`, asCustomer.User.ID)
	require.NoError(t, err)
	begin, err = svc.Begin(ctx, identity.BeginRequest{})
	require.NoError(t, err)
	asOperator, err := svc.Complete(ctx, identity.CompleteRequest{Code: "admin:mfa", State: begin.State})
	require.NoError(t, err)
	assert.Equal(t, security.ActorOperator, asOperator.Issued.Session.ActorType)
	assert.Equal(t, []security.Role{security.RoleAdmin}, asOperator.Issued.Session.Roles)

	// return_to must be a local path.
	//
	// The backslash forms are here because the guard used to accept them: it
	// was HasPrefix("/") && !HasPrefix("//"), and a browser resolves "/\host"
	// through the relative-slash state into the AUTHORITY state, so the value
	// reached the Location header of the response that sets the session cookie
	// (F-146). identity.IsLocalPath's own unit test covers the whole set; these
	// four prove Begin applies it before the row is written.
	for _, bad := range []string{
		"https://evil.test/x",
		"//evil.test",
		`/\evil.test`,
		"/\tevil.test",
	} {
		_, err = svc.Begin(ctx, identity.BeginRequest{ReturnTo: bad})
		assert.Equalf(t, errs.CodeValidationFailed, errs.CodeOf(err), "Begin recorded %q", bad)
	}
	// And a real path still works, because a guard that refuses those too has
	// broken the feature return_to exists for.
	deep, derr := svc.Begin(ctx, identity.BeginRequest{ReturnTo: "/markets/abc?tab=trades"})
	require.NoError(t, derr)
	require.NotEmpty(t, deep.State)
}

// A verified e-mail address is kept, encrypted, beside the hash that finds it
// (F-47). The column is not the address; the hash agrees with it; and a login
// that finds the row already filled leaves it alone.
func TestIntegration_Login_StoresTheVerifiedEmailEncrypted(t *testing.T) {
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	d, err := db.Open(ctx, db.Config{URL: url, MaxConns: 4, MinConns: 1, AppName: "identity-pii-test"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	idp, err := devidp.New("TEST", devidp.Config{Now: clk.Now})
	require.NoError(t, err)
	mgr, err := auth.NewManager(pgstore.New(), auth.ManagerConfig{Now: clk.Now})
	require.NoError(t, err)

	key := make([]byte, pii.KeySize)
	_, err = rand.Read(key)
	require.NoError(t, err)
	kr, err := pii.ParseKeyring(`{"active": 1, "keys": {"1": "` + base64.StdEncoding.EncodeToString(key) + `"}}`)
	require.NoError(t, err)
	store := pii.NewStore(kr)

	svc, err := identity.New(identity.Deps{
		IdP: idp, DB: d, Accounts: accounts.NewRepository(), Sessions: mgr, Audit: audit.NewWriter(), Clock: clk,
		PII: store,
	})
	require.NoError(t, err)

	begin, err := svc.Begin(ctx, identity.BeginRequest{})
	require.NoError(t, err)
	done, err := svc.Complete(ctx, identity.CompleteRequest{Code: "customer-a", State: begin.State})
	require.NoError(t, err)
	userID := done.User.ID.String()

	rec, found, err := store.Read(ctx, d, userID)
	require.NoError(t, err)
	require.True(t, found, "the login stored nothing in identity_pii")
	assert.Equal(t, "customer-a@dev.invalid", rec.Email)

	// The column holds ciphertext, not the address.
	var raw []byte
	var updated time.Time
	require.NoError(t, d.QueryRow(ctx, `SELECT email_encrypted, updated_at FROM identity_pii WHERE user_id = $1`, done.User.ID).Scan(&raw, &updated))
	assert.NotContains(t, strings.ToLower(string(raw)), "dev.invalid")

	// The lookup hash and the sealed address agree, so they describe one
	// person.
	var hash []byte
	require.NoError(t, d.QueryRow(ctx, `SELECT email_hash FROM users WHERE id = $1`, done.User.ID).Scan(&hash))
	sum := sha256.Sum256([]byte("customer-a@dev.invalid"))
	assert.Equal(t, sum[:], hash)

	// A second login finds the row filled and does not rewrite it.
	begin2, err := svc.Begin(ctx, identity.BeginRequest{})
	require.NoError(t, err)
	_, err = svc.Complete(ctx, identity.CompleteRequest{Code: "customer-a", State: begin2.State})
	require.NoError(t, err)
	var updatedAgain time.Time
	require.NoError(t, d.QueryRow(ctx, `SELECT updated_at FROM identity_pii WHERE user_id = $1`, done.User.ID).Scan(&updatedAgain))
	assert.Equal(t, updated, updatedAgain, "a login rewrote a row that already had the address")
}
