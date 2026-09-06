package intent_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/idempotency"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/security"
)

func newService(t *testing.T) *intent.Service {
	t.Helper()
	clk := clock.NewFake(t0)
	repo := intent.NewRepository(clk, event.NewOutbox(clk), audit.NewWriter())
	return intent.NewService(repo, idempotency.NewStore(clk.Now), clk, 0)
}

func customer(subject string, accountIDs ...string) security.Principal {
	return security.Principal{SubjectID: subject, ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer}, AccountIDs: accountIDs, AuthTime: t0}
}

func submitReq(accountID string) intent.SubmitRequest {
	return intent.SubmitRequest{
		AccountID: accountID, Action: intent.ActionAcquireNotional, InstrumentID: instruments.NewInstrumentID(),
		NotionalUSD: usd(100_00), IdempotencyKey: "k1", Mode: intent.ModeLive, Deadline: t0.Add(time.Minute),
	}
}

// TestSubmit_AuthorizationBeforeAnyIO: every authorization failure is
// decided from the principal alone and never reaches the database (the
// service is handed a nil *db.DB and must not touch it).
func TestSubmit_AuthorizationBeforeAnyIO(t *testing.T) {
	t.Parallel()
	svc := newService(t)
	acctA, acctB := accounts.NewAccountID().String(), accounts.NewAccountID().String()
	agentID := uuidStr()
	cases := []struct {
		name string
		p    security.Principal
		req  intent.SubmitRequest
		code errs.Code
	}{
		{"invalid principal", security.Principal{}, submitReq(acctA), errs.CodeForbidden},
		{"service actor cannot submit", security.Principal{SubjectID: "svc", ActorType: security.ActorService}, submitReq(acctA), errs.CodeForbidden},
		{"system actor cannot submit", security.Principal{SubjectID: "sys", ActorType: security.ActorSystem}, submitReq(acctA), errs.CodeForbidden},
		{"customer without trade:create", security.Principal{SubjectID: "u", ActorType: security.ActorUser, Roles: []security.Role{security.RoleSupportReadOnly}, AccountIDs: []string{acctA}}, submitReq(acctA), errs.CodeForbidden},
		{"principal A cannot submit for account B", customer("user-a", acctA), submitReq(acctB), errs.CodeForbidden},
		{"agent cannot submit for another account", security.AgentPrincipal(agentID, acctA), submitReq(acctB), errs.CodeForbidden},
		{"agent cannot submit USER intents", security.AgentPrincipal(agentID, acctA), func() intent.SubmitRequest {
			r := submitReq(acctA)
			r.ActorType = security.ActorUser
			return r
		}(), errs.CodeForbidden},
		{"user cannot submit AGENT intents", customer("user-a", acctA), func() intent.SubmitRequest {
			r := submitReq(acctA)
			r.ActorType = security.ActorAgent
			return r
		}(), errs.CodeForbidden},
		{"operator without account:read_any is scoped", security.Principal{SubjectID: "op", ActorType: security.ActorOperator, Roles: []security.Role{security.RoleCustomer}}, submitReq(acctA), errs.CodeForbidden},
		{"empty account id", customer("user-a", acctA), submitReq(""), errs.CodeForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := svc.Submit(context.Background(), nil, tc.p, tc.req)
			require.Error(t, err)
			assert.Equal(t, tc.code, errs.CodeOf(err), "%v", err)
		})
	}
}

func TestSubmit_ValidationBeforeAnyIO(t *testing.T) {
	t.Parallel()
	svc := newService(t)
	acct := accounts.NewAccountID().String()
	t.Run("agent without prediction", func(t *testing.T) {
		t.Parallel()
		_, err := svc.Submit(context.Background(), nil, security.AgentPrincipal(uuidStr(), acct), submitReq(acct))
		require.Error(t, err)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		f := fieldsOf(t, err)
		assert.Contains(t, f, "prediction_id")
		assert.Contains(t, f, "strategy_version_id")
	})
	t.Run("user with agent linkage", func(t *testing.T) {
		t.Parallel()
		req := submitReq(acct)
		req.PredictionID, req.StrategyVersionID = uuidStr(), uuidStr()
		_, err := svc.Submit(context.Background(), nil, customer("u", acct), req)
		require.Error(t, err)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	})
	t.Run("missing idempotency key", func(t *testing.T) {
		t.Parallel()
		req := submitReq(acct)
		req.IdempotencyKey = ""
		_, err := svc.Submit(context.Background(), nil, customer("u", acct), req)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	})
	t.Run("missing mode", func(t *testing.T) {
		t.Parallel()
		req := submitReq(acct)
		req.Mode = ""
		_, err := svc.Submit(context.Background(), nil, customer("u", acct), req)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		assert.Contains(t, fieldsOf(t, err), "mode")
	})
	t.Run("valid request needs a database", func(t *testing.T) {
		t.Parallel()
		_, err := svc.Submit(context.Background(), nil, customer("u", acct), submitReq(acct))
		assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
	})
	t.Run("context principal must match", func(t *testing.T) {
		t.Parallel()
		ctx := security.WithPrincipal(context.Background(), customer("someone-else", acct))
		_, err := svc.Submit(ctx, nil, customer("u", acct), submitReq(acct))
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})
}

func TestConstructors_RejectNil(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	assert.Panics(t, func() { intent.NewRepository(nil, event.NewOutbox(clk), audit.NewWriter()) })
	assert.Panics(t, func() { intent.NewRepository(clk, nil, audit.NewWriter()) })
	assert.Panics(t, func() { intent.NewRepository(clk, event.NewOutbox(clk), nil) })
	repo := intent.NewRepository(clk, event.NewOutbox(clk), audit.NewWriter())
	assert.Panics(t, func() { intent.NewService(nil, idempotency.NewStore(nil), clk, 0) })
	assert.Panics(t, func() { intent.NewService(repo, nil, clk, 0) })
	assert.Panics(t, func() { intent.NewService(repo, idempotency.NewStore(nil), nil, 0) })
	assert.NotNil(t, intent.NewService(repo, idempotency.NewStore(nil), clk, -1))
}
