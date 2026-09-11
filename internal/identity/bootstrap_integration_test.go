//go:build integration

package identity_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/auth/devidp"
	"github.com/nodal/controlplane/internal/auth/pgstore"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/identity"
	"github.com/nodal/controlplane/internal/operatorroles"
	"github.com/nodal/controlplane/internal/security"
)

// newServiceWithBootstrap is newService plus a declared bootstrap.
func newServiceWithBootstrap(t *testing.T, raw string) (*identity.Service, *db.DB, *clock.Fake) {
	t.Helper()
	base, d, clk := newService(t)
	_ = base

	decls, err := operatorroles.ParseDeclarations(raw)
	require.NoError(t, err)
	boot, err := identity.NewOperatorBootstrap(decls, audit.NewWriter())
	require.NoError(t, err)

	idp, err := devidp.New("TEST", devidp.Config{Now: clk.Now})
	require.NoError(t, err)
	mgr, err := auth.NewManager(pgstore.New(), auth.ManagerConfig{Now: clk.Now})
	require.NoError(t, err)
	svc, err := identity.New(identity.Deps{
		IdP: idp, DB: d, Accounts: accounts.NewRepository(), Sessions: mgr,
		Audit: audit.NewWriter(), Clock: clk, Operators: boot,
	})
	require.NoError(t, err)
	return svc, d, clk
}

func login(t *testing.T, svc *identity.Service, code string) identity.Completed {
	t.Helper()
	ctx := context.Background()
	begin, err := svc.Begin(ctx, identity.BeginRequest{})
	require.NoError(t, err)
	done, err := svc.Complete(ctx, identity.CompleteRequest{Code: code, State: begin.State})
	require.NoError(t, err)
	return done
}

// A deployment with no operator gets one: the declared identity logs in, the
// directory row appears, and the session carries the role.
func TestIntegration_Bootstrap_GrantsTheDeclaredRoleAtLogin(t *testing.T) {
	svc, d, _ := newServiceWithBootstrap(t, devidp.Name+"|dev:support=ADMIN")
	ctx := context.Background()

	done := login(t, svc, "support:mfa")
	assert.Equal(t, security.ActorOperator, done.Issued.Session.ActorType)
	assert.Equal(t, []security.Role{security.RoleAdmin}, done.Issued.Session.Roles)

	// The row is in the directory, with a provenance a reviewer can read.
	var reason string
	var grantedBy *string
	require.NoError(t, d.Pool().QueryRow(ctx,
		`SELECT reason, granted_by::text FROM operator_roles WHERE user_id = $1 AND role = 'ADMIN'`,
		done.User.ID).Scan(&reason, &grantedBy))
	assert.Equal(t, operatorroles.GrantReason, reason)
	assert.Nil(t, grantedBy, "a configuration grant must not claim a person granted it")

	// And it is an audited event on the admin stream, as the SYSTEM actor named
	// for the variable rather than for a person.
	var actorType, actorID string
	require.NoError(t, d.Pool().QueryRow(ctx,
		`SELECT actor_type, actor_id FROM audit_events WHERE action = $1 AND resource_id = $2`,
		operatorroles.AuditAction, done.User.ID.String()+"/ADMIN").Scan(&actorType, &actorID))
	assert.Equal(t, string(security.ActorSystem), actorType)
	assert.Equal(t, operatorroles.ActorID, actorID)
}

// It is idempotent: a second login writes no second row and no second event.
func TestIntegration_Bootstrap_IsIdempotent(t *testing.T) {
	svc, d, _ := newServiceWithBootstrap(t, devidp.Name+"|dev:operations=OPERATIONS")
	ctx := context.Background()

	first := login(t, svc, "operations:mfa")
	second := login(t, svc, "operations:mfa")
	require.Equal(t, first.User.ID, second.User.ID)

	var rows, events int
	require.NoError(t, d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM operator_roles WHERE user_id = $1`, first.User.ID).Scan(&rows))
	require.NoError(t, d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE action = $1 AND resource_id = $2`,
		operatorroles.AuditAction, first.User.ID.String()+"/OPERATIONS").Scan(&events))
	assert.Equal(t, 1, rows)
	assert.Equal(t, 1, events, "a re-offered grant wrote a second audit event")
	assert.Equal(t, []security.Role{security.RoleOperations}, second.Issued.Session.Roles)
}

// A revoked grant stays revoked. Removing the declaration is how you stop it
// being offered; nothing here undoes an operator's revocation.
func TestIntegration_Bootstrap_DoesNotRestoreARevokedGrant(t *testing.T) {
	svc, d, _ := newServiceWithBootstrap(t, devidp.Name+"|dev:risk=RISK")
	ctx := context.Background()

	first := login(t, svc, "risk:mfa")
	require.Equal(t, []security.Role{security.RoleRisk}, first.Issued.Session.Roles)

	// The revocation is a transition row, not an UPDATE: 00799 took the
	// directory out of the application role's UPDATE reach, and the trigger
	// behind this row is what writes revoked_at.
	_, err := d.Pool().Exec(ctx,
		`INSERT INTO operator_role_transitions (id, user_id, role, action, actor_type, actor_id, reason)
		 VALUES ($1, $2, 'RISK', 'REVOKE', 'OPERATOR', 'an operator', 'no longer on the risk desk')`,
		id.New[id.Any](), first.User.ID)
	require.NoError(t, err)

	second := login(t, svc, "risk:mfa")
	assert.Equal(t, security.ActorUser, second.Issued.Session.ActorType)
	assert.Equal(t, []security.Role{security.RoleCustomer}, second.Issued.Session.Roles,
		"a revoked bootstrap grant came back at the next login")

	var revoked int
	require.NoError(t, d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM operator_roles WHERE user_id = $1 AND revoked_at IS NOT NULL`,
		first.User.ID).Scan(&revoked))
	assert.Equal(t, 1, revoked)
}

// An identity the declaration does not name gets nothing, whatever the identity
// provider says about it. The dev "compliance" identity carries a COMPLIANCE
// role in its own claims, and it still logs in as a plain customer.
//
// (The identities used here are deliberately ones the rest of this package's
// suite does not touch: the tests share one database, so an identity used twice
// would make each test depend on the order they ran in.)
func TestIntegration_Bootstrap_GrantsNothingToAnUndeclaredIdentity(t *testing.T) {
	svc, d, _ := newServiceWithBootstrap(t, devidp.Name+"|dev:finance=FINANCE")
	ctx := context.Background()

	done := login(t, svc, "compliance:mfa")
	assert.Equal(t, security.ActorUser, done.Issued.Session.ActorType)
	assert.Equal(t, []security.Role{security.RoleCustomer}, done.Issued.Session.Roles)

	var rows int
	require.NoError(t, d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM operator_roles WHERE user_id = $1`, done.User.ID).Scan(&rows))
	assert.Zero(t, rows, "a provider role claim reached the operator directory")
}

// The issuer is part of the identity. A declaration naming a different issuer
// does not match, so moving providers cannot silently carry a grant across.
func TestIntegration_Bootstrap_MatchesOnIssuerAsWellAsSubject(t *testing.T) {
	svc, d, _ := newServiceWithBootstrap(t, "https://some-other-issuer.example|dev:security=SECURITY")
	ctx := context.Background()

	done := login(t, svc, "security:mfa")
	assert.Equal(t, []security.Role{security.RoleCustomer}, done.Issued.Session.Roles)
	var rows int
	require.NoError(t, d.Pool().QueryRow(ctx,
		`SELECT count(*) FROM operator_roles WHERE user_id = $1`, done.User.ID).Scan(&rows))
	assert.Zero(t, rows)
}

// The directory refuses a role no permission matrix knows, at the INSERT rather
// than at the login it would otherwise break (00760).
func TestIntegration_OperatorDirectoryRefusesAnUnknownRoleAndBreakGlass(t *testing.T) {
	svc, d, _ := newServiceWithBootstrap(t, "")
	ctx := context.Background()
	done := login(t, svc, "finance:mfa")

	for _, role := range []string{"ADMlN", "admin", "SUPERUSER", "BREAK_GLASS"} {
		_, err := d.Pool().Exec(ctx,
			`INSERT INTO operator_roles (user_id, role, reason) VALUES ($1, $2, 'test')`, done.User.ID, role)
		require.Errorf(t, err, "the directory accepted the role %q", role)
		assert.Equal(t, db.SQLStateCheckViolation, db.SQLState(err), "role %q: %v", role, err)
	}
}
