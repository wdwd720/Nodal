package security_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/security"
)

// An operator may READ any account and may not ACT on one.
//
// RoleAdmin is every permission except the dual-control and agent-only sets,
// so it holds `account:read_any` AND the customer surface — `native_market:trade`,
// `commerce:buy`, `payout:create`, `withdrawal:create`. Those customer
// permissions exist so an operator can use their own account. Combined with the
// read override on the write routes, one ADMIN session could trade, buy and
// reserve a payout out of any customer's balance, with no second signature and
// no admin_actions row (F-36).
//
// The two functions below are the fix, and this is what they must each answer.

const (
	victim   = "8a1c9f2e-0000-7000-8000-000000000001"
	operator = "8a1c9f2e-0000-7000-8000-000000000002"
)

func adminPrincipal() security.Principal {
	return security.Principal{
		SubjectID:  "op-1",
		ActorType:  security.ActorOperator,
		Roles:      []security.Role{security.RoleAdmin},
		AccountIDs: []string{operator},
		AuthTime:   time.Now().UTC(),
		AMR:        []string{"pwd", "mfa"},
	}
}

func TestRequireAccountOwner_AnOperatorMayReadAnyAccountAndActOnNone(t *testing.T) {
	t.Parallel()
	ctx := security.WithPrincipal(context.Background(), adminPrincipal())

	// The permission that makes the difference is present.
	require.NoError(t, security.Require(ctx, security.PermAccountReadAny),
		"this test is about an operator who CAN read any account; without that it proves nothing")

	assert.NoError(t, security.RequireAccount(ctx, victim),
		"an operator reading a customer's account is the point of account:read_any")
	assert.ErrorIs(t, security.RequireAccountOwner(ctx, victim), security.ErrCrossTenant,
		"a read permission must not authorize a write on somebody else's account")

	// Their own account is theirs on both.
	assert.NoError(t, security.RequireAccount(ctx, operator))
	assert.NoError(t, security.RequireAccountOwner(ctx, operator))
}

func TestRequireAccountOwner_ACustomerIsUnaffected(t *testing.T) {
	t.Parallel()
	ctx := security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: "user-1", ActorType: security.ActorUser,
		AccountIDs: []string{victim}, AuthTime: time.Now().UTC(),
	})
	assert.NoError(t, security.RequireAccountOwner(ctx, victim))
	assert.ErrorIs(t, security.RequireAccountOwner(ctx, operator), security.ErrCrossTenant)
}

// TestRequireAccountOwner_RefusesTheSameShapesAsRequireAccount: an empty id, an
// anonymous context and an agent are refused by both. A new function that
// forgot one of those would be a hole opened by the fix for a hole.
func TestRequireAccountOwner_RefusesTheSameShapesAsRequireAccount(t *testing.T) {
	t.Parallel()
	anon := context.Background()
	assert.Error(t, security.RequireAccountOwner(anon, victim))

	ctx := security.WithPrincipal(context.Background(), adminPrincipal())
	assert.ErrorIs(t, security.RequireAccountOwner(ctx, ""), security.ErrCrossTenant)
	assert.ErrorIs(t, security.RequireAccount(ctx, ""), security.ErrCrossTenant)

	agent := security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: "agent-1", ActorType: security.ActorAgent,
		AccountIDs: []string{operator}, AuthTime: time.Now().UTC(),
	})
	assert.ErrorIs(t, security.RequireAccountOwner(agent, victim), security.ErrCrossTenant)
	assert.NoError(t, security.RequireAccountOwner(agent, operator),
		"an agent bound to an account still acts on that one account")
}
