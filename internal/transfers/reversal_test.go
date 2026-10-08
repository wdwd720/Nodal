package transfers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

func TestReversalProposal_StatusIsDerivedFromItsDecision(t *testing.T) {
	p := ReversalProposal{}
	assert.Equal(t, ProposalPending, p.Status(), "no decision is PENDING")
	approved, rejected := ReversalApproved, ReversalRejected
	p.Decision = &approved
	assert.Equal(t, ProposalApproved, p.Status())
	p.Decision = &rejected
	assert.Equal(t, ProposalRejected, p.Status())

	assert.Equal(t, []ReversalDecision{ReversalApproved, ReversalRejected}, AllReversalDecisions())
	for _, d := range AllReversalDecisions() {
		assert.True(t, d.Valid(), d)
	}
	assert.False(t, ReversalDecision("PENDING").Valid(), "PENDING is a status, never a decision")
	assert.False(t, ReversalDecision("").Valid())
	assert.Equal(t, []ReversalProposalStatus{ProposalPending, ProposalApproved, ProposalRejected}, AllReversalProposalStatuses())
	for _, s := range AllReversalProposalStatuses() {
		assert.True(t, s.Valid(), s)
	}
	assert.False(t, ReversalProposalStatus("CONSUMED").Valid())
	assert.True(t, validReasonCode(rejectionSelfApproval))

	// A copy: callers cannot change the declared sets.
	ds := AllReversalDecisions()
	ds[0] = "X"
	assert.Equal(t, ReversalApproved, AllReversalDecisions()[0])
}

func TestProposeReversalRequest_RefusesAnythingButAStableCodeAndAReason(t *testing.T) {
	good := ProposeReversalRequest{TransferID: NewTransferID(), ReasonCode: "FRAUD_CONFIRMED", Reason: "sender reported account takeover"}
	require.NoError(t, good.validate())
	for name, mutate := range map[string]func(*ProposeReversalRequest){
		"no transfer":        func(r *ProposeReversalRequest) { r.TransferID = TransferID{} },
		"lower-case code":    func(r *ProposeReversalRequest) { r.ReasonCode = "fraud" },
		"short code":         func(r *ProposeReversalRequest) { r.ReasonCode = "FR" },
		"code with a space":  func(r *ProposeReversalRequest) { r.ReasonCode = "FRAUD CONFIRMED" },
		"empty reason":       func(r *ProposeReversalRequest) { r.Reason = "" },
		"blank reason":       func(r *ProposeReversalRequest) { r.Reason = "   " },
		"reason too long":    func(r *ProposeReversalRequest) { r.Reason = strings.Repeat("x", maxReversalReason+1) },
		"invalid utf-8":      func(r *ProposeReversalRequest) { r.Reason = "bad \xff byte" },
		"correlation id too": func(r *ProposeReversalRequest) { r.CorrelationID = strings.Repeat("c", 201) },
	} {
		t.Run(name, func(t *testing.T) {
			r := good
			mutate(&r)
			err := r.validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
	r := good
	r.Reason = strings.Repeat("x", maxReversalReason)
	assert.NoError(t, r.validate(), "exactly the bound is accepted")
}

func TestDecideReversalRequest_ARejectionStatesItsReasonAnApprovalMay(t *testing.T) {
	pid := NewReversalProposalID()
	assert.NoError(t, DecideReversalRequest{ProposalID: pid}.validate(ReversalApproved), "an approval needs no reason")
	assert.NoError(t, DecideReversalRequest{ProposalID: pid, Reason: "checked with the sender"}.validate(ReversalApproved))
	for name, c := range map[string]struct {
		r DecideReversalRequest
		d ReversalDecision
	}{
		"rejection without a reason": {DecideReversalRequest{ProposalID: pid}, ReversalRejected},
		"blank rejection reason":     {DecideReversalRequest{ProposalID: pid, Reason: " "}, ReversalRejected},
		"blank approval reason":      {DecideReversalRequest{ProposalID: pid, Reason: "\t"}, ReversalApproved},
		"approval reason too long":   {DecideReversalRequest{ProposalID: pid, Reason: strings.Repeat("y", 501)}, ReversalApproved},
		"no proposal":                {DecideReversalRequest{Reason: "x"}, ReversalRejected},
		"correlation id too long":    {DecideReversalRequest{ProposalID: pid, CorrelationID: strings.Repeat("c", 201)}, ReversalApproved},
	} {
		t.Run(name, func(t *testing.T) {
			err := c.r.validate(c.d)
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// unitService is a Service with only what the admission rules read: the
// clock and the policy's step-up age.
func unitService(now time.Time) *Service {
	return &Service{clk: clock.NewFake(now), policy: DefaultPolicy()}
}

func financeOperator(at time.Time) security.Principal {
	return security.Principal{SubjectID: uuid.NewString(), ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleFinance}, AuthTime: at, AMR: []string{"mfa"}}
}

// TestReversal_OnlyAStronglyAuthenticatedCorrectionOperatorActs: the
// admission rule every proposal and decision passes first.
func TestReversal_OnlyAStronglyAuthenticatedCorrectionOperatorActs(t *testing.T) {
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	s := unitService(now)

	op := financeOperator(now.Add(-time.Minute))
	_, actor, err := s.correctionOperator(context.Background(), op)
	require.NoError(t, err)
	assert.Equal(t, econ.Actor{Type: security.ActorOperator, ID: op.SubjectID}, actor)

	stale := financeOperator(now.Add(-s.policy.StepUpMaxAge() - time.Second))
	noMFA := financeOperator(now)
	noMFA.AMR = []string{"pwd"}
	ops := financeOperator(now)
	ops.Roles = []security.Role{security.RoleOperations}
	customer := security.Principal{SubjectID: uuid.NewString(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{uuid.NewString()}, AuthTime: now, AMR: []string{"mfa"}}
	agent := security.AgentPrincipal(uuid.NewString(), uuid.NewString())
	for name, c := range map[string]struct {
		p    security.Principal
		code errs.Code
	}{
		"customer":                       {customer, errs.CodeForbidden},
		"agent":                          {agent, errs.CodeForbidden},
		"operator without the authority": {ops, errs.CodeForbidden},
		"stale step-up":                  {stale, errs.CodeStepUpRequired},
		"no strong method":               {noMFA, errs.CodeStepUpRequired},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := s.correctionOperator(context.Background(), c.p)
			require.Error(t, err)
			assert.Equal(t, c.code, errs.CodeOf(err), "%v", err)
		})
	}

	// Reading proposals needs an operator who may read any account.
	assert.True(t, s.mayReadReversals(op))
	assert.True(t, s.mayReadReversals(ops), "any operator who reads every account reads the proposals")
	assert.False(t, s.mayReadReversals(customer))
	assert.False(t, s.mayReadReversals(agent))
}

func TestReversal_EveryMutatorNeedsATransaction(t *testing.T) {
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	s := unitService(now)
	op := financeOperator(now)
	_, err := s.ProposeReversal(context.Background(), nil, op, ProposeReversalRequest{})
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
	_, err = s.ApproveReversal(context.Background(), nil, op, DecideReversalRequest{})
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
	_, err = s.RejectReversal(context.Background(), nil, op, DecideReversalRequest{})
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
}

func TestReversal_TheDatabasesRefusalsKeepStableCodes(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code, Message: "raised"} }
	// Anything else keeps the package's ordinary mapping (dbErr).
	for state, want := range map[string]errs.Code{
		"23505": errs.CodeConflict, "PT006": errs.CodeForbidden, "PT008": errs.CodeConflict,
		"42501": errs.CodeForbidden, "XX000": errs.CodeInternal,
	} {
		err := reversalDBErr("record reversal proposal", pg(state))
		assert.Equal(t, want, errs.CodeOf(err), state)
	}
	// A retryable failure is returned as it is, so the transaction can be
	// re-run.
	retry := pg("40001")
	assert.True(t, errors.Is(reversalDBErr("record", retry), retry))
	selfApproval := reversalDBErr("record", pg("PT006"))
	e, ok := errs.As(selfApproval)
	require.True(t, ok)
	assert.Equal(t, rejectionSelfApproval, e.Fields["rejection_code"])
}
