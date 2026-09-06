package capital

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// Every EnvelopeAdmin method decides authorization before touching the
// database, so these run with a nil transaction: reaching the database
// would panic, which is exactly the failure we want to see if the order
// ever regresses.
func TestEnvelopeService_AgentPrincipalCannotAdminister(t *testing.T) {
	svc := NewEnvelopeService(clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)), &recordingEmitter{})
	account := accounts.NewAccountID()
	agentCtx := security.WithPrincipal(context.Background(), security.AgentPrincipal("agent-1", account.String()))
	env := validEnvelope()
	env.AccountID = account

	calls := map[string]func(ctx context.Context) error{
		"Create": func(ctx context.Context) error { _, err := svc.Create(ctx, nil, env); return err },
		"Update": func(ctx context.Context) error {
			_, err := svc.Update(ctx, nil, NewEnvelopeID(), EnvelopeAuthorityPatch{Allocation: ptrUSD(1), Reason: "x"})
			return err
		},
		"SetStatus": func(ctx context.Context) error {
			_, err := svc.SetStatus(ctx, nil, NewEnvelopeID(), EnvelopeActive, "x")
			return err
		},
		"Get":            func(ctx context.Context) error { _, err := svc.Get(ctx, nil, NewEnvelopeID()); return err },
		"ListForAccount": func(ctx context.Context) error { _, err := svc.ListForAccount(ctx, nil, account.String()); return err },
	}
	for name, call := range calls {
		t.Run(name+"/agent is FORBIDDEN even on its own account", func(t *testing.T) {
			err := call(agentCtx)
			require.Error(t, err)
			assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
		})
		t.Run(name+"/anonymous is UNAUTHENTICATED", func(t *testing.T) {
			err := call(context.Background())
			require.Error(t, err)
			assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
		})
		t.Run(name+"/structurally invalid principal is FORBIDDEN", func(t *testing.T) {
			bad := security.WithPrincipal(context.Background(), security.Principal{SubjectID: "", ActorType: security.ActorUser})
			err := call(bad)
			require.Error(t, err)
			assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
		})
	}

	t.Run("Create/customer of another account is FORBIDDEN before any database access", func(t *testing.T) {
		other := security.WithPrincipal(context.Background(), security.Principal{
			SubjectID: "user-2", ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
			AccountIDs: []string{accounts.NewAccountID().String()},
		})
		_, err := svc.Create(other, nil, env)
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})
	t.Run("ListForAccount/customer of another account is FORBIDDEN before any database access", func(t *testing.T) {
		other := security.WithPrincipal(context.Background(), security.Principal{
			SubjectID: "user-2", ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
			AccountIDs: []string{accounts.NewAccountID().String()},
		})
		_, err := svc.ListForAccount(other, nil, account.String())
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})
}

func TestConstructors_RefuseNilDependencies(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	assert.Panics(t, func() { NewService(nil, &recordingEmitter{}) })
	assert.Panics(t, func() { NewService(clk, nil) })
	assert.Panics(t, func() { NewEnvelopeService(nil, &recordingEmitter{}) })
	assert.Panics(t, func() { NewEnvelopeService(clk, nil) })
	assert.NotNil(t, NewService(clk, &recordingEmitter{}))
	assert.NotNil(t, NewEnvelopeService(clk, &recordingEmitter{}))
}

// Reserve validates before touching the database, so a malformed request
// with a nil transaction fails with VALIDATION_FAILED rather than a panic.
func TestService_ReserveValidatesBeforeDatabase(t *testing.T) {
	svc := NewService(clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)), &recordingEmitter{})
	_, err := svc.Reserve(context.Background(), nil, ReserveRequest{})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = svc.Release(context.Background(), nil, NewReservationID(), "")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(svc.LockForOrder(context.Background(), nil, NewReservationID(), "")))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(svc.LockForOrder(context.Background(), nil, NewReservationID(), "order-1")))
	_, err = svc.Consume(context.Background(), nil, NewReservationID(), validReserveRequest().Quantity, -1, "")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = svc.Undeploy(context.Background(), nil, NewEnvelopeID(), usd(0))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}
