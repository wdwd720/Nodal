package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/security"
)

// Handler tests of the prediction market hardening: dual-controlled voids,
// held payouts and their claims, and durable sandbox oracle observations.

// TestPredmarket_VoidIsAProposalThenASecondOperatorsApproval: proposing a void
// answers 201 with the proposal and refunds nothing; approving names the
// proposal from the path, passes the principal and its note, and answers the
// refund record. The dual-control rules themselves are the service's.
func TestPredmarket_VoidIsAProposalThenASecondOperatorsApproval(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	op := pmOperator()
	h.as(&op)
	operator := econ.Actor{Type: security.ActorOperator, ID: pmOperatorID.String()}

	res := h.do(http.MethodPost, pmPath+"/void-proposals",
		map[string]any{"reason": "the event was cancelled by its organiser", "evidence": map[string]any{"notice": "cancelled"}},
		"Idempotency-Key", "pm-void-propose-0001")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, pmMarketID, fp.lastProposal.MarketID)
	assert.Equal(t, operator, fp.lastProposal.Actor)
	assert.Equal(t, "the event was cancelled by its organiser", fp.lastProposal.Reason)
	assert.Equal(t, map[string]interface{}{"notice": "cancelled"}, fp.lastProposal.Evidence)
	var pr api.PredictionVoidProposalResult
	res.json(&pr)
	assert.Equal(t, api.ValueDomainSimulated("SIMULATED"), pr.ValueDomain)
	assert.Equal(t, fp.proposal.ID.String(), pr.Proposal.Id.String())
	assert.Equal(t, api.PredictionMarketStatus("DISPUTED"), pr.Proposal.MarketStatus)
	require.NotNil(t, pr.Proposal.ResolutionId)
	assert.Nil(t, pr.Proposal.Approval, "a proposal alone voids nothing")
	assert.Zero(t, fp.count("ApproveVoid"))

	// A proposal sent without evidence reaches the service with none, and the
	// service's "a proposal carries its evidence" rule decides.
	res = h.do(http.MethodPost, pmPath+"/void-proposals", map[string]any{"reason": "no evidence attached here"},
		"Idempotency-Key", "pm-void-propose-0002")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	assert.Nil(t, fp.lastProposal.Evidence)

	approve := pmPath + "/void-proposals/" + pmProposalID.String() + "/approve"
	res = h.do(http.MethodPost, approve, map[string]any{"reason": "a second operator confirms the cancellation"},
		"Idempotency-Key", "pm-void-approve-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, pmProposalID, fp.lastApproval.ProposalID)
	assert.Equal(t, pmMarketID, fp.lastApproval.MarketID)
	assert.Equal(t, operator, fp.lastApproval.Actor)
	assert.Equal(t, "a second operator confirms the cancellation", fp.lastApproval.Reason)
	var sr api.PredictionSettlementResult
	res.json(&sr)
	assert.Equal(t, fp.settlement.ID.String(), sr.Settlement.Id.String())

	res = h.do(http.MethodPost, approve, map[string]any{}, "Idempotency-Key", "pm-void-approve-0002")
	require.Equal(t, http.StatusOK, res.Code, "an approval's note is optional; body=%s", res.Body.String())
	assert.Empty(t, fp.lastApproval.Reason)

	res = h.do(http.MethodPost, pmPath+"/void-proposals/not-a-uuid/approve", map[string]any{}, "Idempotency-Key", "pm-void-approve-0003")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 2, fp.count("ApproveVoid"), "a malformed proposal id never reaches the service")

	fp.err = errs.New(errs.CodeConflict, "predmarket: market PMHTTP has another resolution in force than when this void was proposed")
	res = h.do(http.MethodPost, approve, map[string]any{}, "Idempotency-Key", "pm-void-approve-0004")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeConflict, res.problem().Code)
}

// TestPredmarket_Claim_ActsForTheOwnerUnderANamespacedIntentKey: a held payout
// is claimed by its owner only, under a namespaced intent key, and the answer
// states the claimed payout and the action that delivered it.
func TestPredmarket_Claim_ActsForTheOwnerUnderANamespacedIntentKey(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	claim := pmPath + "/held-payouts/claim"
	res := h.do(http.MethodPost, claim, map[string]any{"account_id": testAccountID.String()}, "Idempotency-Key", "pm-claim-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	req := fp.lastClaim
	assert.Equal(t, pmMarketID, req.MarketID)
	assert.Equal(t, testAccountID, req.AccountID)
	assert.Equal(t, "http:predmarket:claim:pm-claim-0001", req.IdempotencyKey, "the client key is namespaced for the economic intent")
	assert.Equal(t, security.ActorUser, req.ActorType)
	assert.Equal(t, testUserID.String(), req.ActorID)
	var cr api.PredictionClaimResult
	res.json(&cr)
	assert.Equal(t, api.ValueDomainSimulated("SIMULATED"), cr.ValueDomain)
	assert.False(t, cr.Replay)
	assert.True(t, cr.HeldPayout.Claimed)
	require.NotNil(t, cr.HeldPayout.Claim)
	assert.Equal(t, "2000000", cr.HeldPayout.Claim.Amount)
	assert.Equal(t, fp.claim.Action.Intent.ID.String(), cr.Intent.Id.String())
	require.NotNil(t, cr.JournalTransactionId)
	assert.Equal(t, fp.claim.Action.JournalTransactionID.String(), cr.JournalTransactionId.String())
	require.NotNil(t, cr.RiskDecisionHash)
	assert.Equal(t, "decision-hash-claim", *cr.RiskDecisionHash)

	res = h.do(http.MethodPost, claim, map[string]any{"account_id": testOtherAcct.String()}, "Idempotency-Key", "pm-claim-foreign")
	require.Equal(t, http.StatusForbidden, res.Code, "a customer claims only their own account's payout: %s", res.Body.String())
	op := pmOperator()
	h.as(&op)
	res = h.do(http.MethodPost, claim, map[string]any{"account_id": testAccountID.String()}, "Idempotency-Key", "pm-claim-operator")
	require.Equal(t, http.StatusForbidden, res.Code, "an operator never claims a customer's payout: %s", res.Body.String())
	assert.Equal(t, 1, fp.count("Claim"), "refused accounts never reach the service")

	h2, fp2 := newPredmarketHarness(t)
	fp2.err = errs.New(errs.CodeRiskRejected, "risk kernel rejected RESOLVE_PREDICTION_POSITION: [RISK_ACCOUNT_STATUS]")
	res = h2.do(http.MethodPost, claim, map[string]any{"account_id": testAccountID.String()}, "Idempotency-Key", "pm-claim-refused")
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeRiskRejected, res.problem().Code)
}

// TestPredmarket_ReadsShowVoidProposalsHeldPayoutsAndTheHeldEscrow: the market
// detail lists its void proposals, an account's positions carry the payout
// the market holds for it (and only its own), and the books state what the
// escrow holds for holders.
func TestPredmarket_ReadsShowVoidProposalsHeldPayoutsAndTheHeldEscrow(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	res := h.do(http.MethodGet, pmPath, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var detail api.PredictionMarketDetail
	res.json(&detail)
	require.Len(t, detail.VoidProposals, 1)
	assert.Equal(t, fp.proposal.ID.String(), detail.VoidProposals[0].Id.String())
	assert.Equal(t, "the event was cancelled by its organiser", detail.VoidProposals[0].Reason)
	require.NotNil(t, detail.Settlement)
	assert.Equal(t, "2000000", detail.Settlement.TotalHeld)

	res = h.do(http.MethodGet, pmPath+"/positions?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var mine api.PredictionPositionList
	res.json(&mine)
	assert.Nil(t, mine.HeldPayout, "the market holds nothing for this account")

	op := pmOperator()
	h.as(&op)
	res = h.do(http.MethodGet, pmPath+"/positions?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var theirs api.PredictionPositionList
	res.json(&theirs)
	require.NotNil(t, theirs.HeldPayout)
	assert.Equal(t, testOtherAcct.String(), theirs.HeldPayout.AccountId.String())
	assert.Equal(t, "2000000", theirs.HeldPayout.Amount)
	assert.Equal(t, []string{"RISK_ACCOUNT_STATUS"}, theirs.HeldPayout.ReasonCodes)
	assert.False(t, theirs.HeldPayout.Claimed)

	fp.mu.Lock()
	fp.books.Held = qty("2000000")
	fp.mu.Unlock()
	res = h.do(http.MethodGet, pmPath+"/books", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "2000000", res.raw()["held"])
}

// TestPredmarket_Observation_IsRecordedAsThePrincipal: the observing operator
// is the actor the service records, and the answer names the stored
// observation.
func TestPredmarket_Observation_IsRecordedAsThePrincipal(t *testing.T) {
	t.Parallel()
	h, fp := newPredmarketHarness(t)
	op := pmOperator()
	h.as(&op)
	res := h.do(http.MethodPost, pmPath+"/oracle-observations",
		map[string]any{"outcome": "YES", "observed_at": testNow.Add(time.Hour), "evidence": `{"certified":"YES"}`},
		"Idempotency-Key", "pm-observe-actor-1")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, econ.Actor{Type: security.ActorOperator, ID: pmOperatorID.String()}, fp.lastObserve.Actor)
	assert.Equal(t, pmMarketID, fp.lastObserve.MarketID)
	assert.Equal(t, "YES", fp.lastObserve.Outcome)
	assert.Equal(t, `{"certified":"YES"}`, string(fp.lastObserve.Evidence))
	var out api.PredictionOracleObservation
	res.json(&out)
	assert.Equal(t, pmOperatorID.String(), out.RecordedBy.ActorId)
	assert.False(t, out.Replay)
	assert.NotEmpty(t, out.ObservationId.String())
}
