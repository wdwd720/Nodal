//go:build integration

package httpapi

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_PredictionMarketVoidNeedsTwoOperatorsAndAFrozenHolderClaimsLater
// drives the hardened void and the held payout over HTTP against the real
// adapter and database: a proposal alone refunds nothing; its proposer cannot
// approve it; a second operator's approval voids the market and refunds the
// active holder while the frozen holder's refund is held; the frozen holder's
// claim is refused until the account is active again, then delivers exactly
// the held amount, once, and the books return to empty.
func TestIntegration_PredictionMarketVoidNeedsTwoOperatorsAndAFrozenHolderClaimsLater(t *testing.T) {
	d := openTestDB(t)
	w := newPMWorld(t, d)
	creator, proposer, approver := w.user(), w.user(), w.user()
	activeUser, active := w.customer(10)
	frozenUser, frozen := w.customer(10)
	h := newIntegrationHarness(t, d, active, activeUser)
	h.server.opts.Ports.PredictionMarkets = w.ports.PredictionMarkets
	opA := pmPrincipal(creator, security.ActorOperator, security.RoleAdmin)
	opB := pmPrincipal(proposer, security.ActorOperator, security.RoleAdmin)
	opC := pmPrincipal(approver, security.ActorOperator, security.RoleAdmin)
	custActive := pmPrincipal(activeUser, security.ActorUser, security.RoleCustomer, active)
	custFrozen := pmPrincipal(frozenUser, security.ActorUser, security.RoleCustomer, frozen)
	key := func() string { return "pm-itest-void-" + uuid.NewString() }

	h.as(&opA)
	res := h.do(http.MethodPost, "/v1/advanced/prediction-markets", map[string]any{
		"symbol":   "ITPMV-" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:10]),
		"question": "Will the sandbox event take place at all?", "outcomes": []string{"YES", "NO"},
		"mode": "SANDBOX", "open_at": w.clk.Now(), "close_at": w.clk.Now().Add(time.Hour), "dispute_window_ms": 0,
		"oracle_source": w.oracle.Source(), "oracle_public_key": hex.EncodeToString(w.oracle.PublicKey()),
	}, "Idempotency-Key", key())
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var market api.PredictionMarket
	res.json(&market)
	base := "/v1/advanced/prediction-markets/" + market.Id.String()
	require.Equal(t, http.StatusOK, h.do(http.MethodPost, base+"/open", nil, "Idempotency-Key", key()).Code)

	for _, c := range []struct {
		p    security.Principal
		acct accounts.AccountID
		sets string
	}{{custActive, active, "4000000"}, {custFrozen, frozen, "2000000"}} {
		p := c.p
		h.as(&p)
		res = h.do(http.MethodPost, base+"/issuances", map[string]any{"account_id": c.acct.String(), "quantity": c.sets}, "Idempotency-Key", key())
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	}
	require.NoError(t, d.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := accounts.NewRepository().Transition(ctx, tx, frozen, accounts.StatusChange{
			To: accounts.StatusFrozen, ActorType: "OPERATOR", ActorID: "compliance", Reason: "itest"}, w.clk.Now())
		return err
	}))

	// One operator proposes; nothing is refunded yet, and they cannot approve.
	h.as(&opB)
	res = h.do(http.MethodPost, base+"/void-proposals", map[string]any{
		"reason": "the event was cancelled by its organiser", "evidence": map[string]any{"notice": "cancelled"},
	}, "Idempotency-Key", key())
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var proposal api.PredictionVoidProposalResult
	res.json(&proposal)
	assert.Equal(t, "OPEN", proposal.Market.Status)
	assert.Equal(t, proposer.String(), proposal.Proposal.ProposedBy.ActorId)
	assert.Equal(t, "6000000", w.wallet(active, econ.SandboxDollarID()), "a proposal refunds nothing")
	approve := base + "/void-proposals/" + proposal.Proposal.Id.String() + "/approve"
	res = h.do(http.MethodPost, approve, map[string]any{}, "Idempotency-Key", key())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)

	// A second operator approves: the market is voided.
	h.as(&opC)
	res = h.do(http.MethodPost, approve, map[string]any{"reason": "confirmed with the organiser"}, "Idempotency-Key", key())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var voided api.PredictionSettlementResult
	res.json(&voided)
	assert.Equal(t, "VOIDED", voided.Market.Status)
	assert.Equal(t, "REFUND", voided.Settlement.Kind)
	assert.Equal(t, "6000000", voided.Settlement.TotalPaid)
	assert.Equal(t, "2000000", voided.Settlement.TotalHeld)
	assert.Equal(t, approver.String(), voided.Settlement.SettledBy.ActorId)
	require.Len(t, voided.HeldPayouts, 1)
	assert.Equal(t, frozen.String(), voided.HeldPayouts[0].AccountId.String())
	held := 0
	for _, l := range voided.Lines {
		if l.Held {
			held++
			assert.Equal(t, frozen.String(), l.AccountId.String())
		}
	}
	assert.Equal(t, 1, held)
	assert.Equal(t, "10000000", w.wallet(active, econ.SandboxDollarID()), "the active holder is refunded")
	assert.Equal(t, "8000000", w.wallet(frozen, econ.SandboxDollarID()), "the frozen holder's refund is held")

	// The market's detail shows the approved proposal; the books hold the
	// held refund and reconcile.
	res = h.do(http.MethodGet, base, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var detail api.PredictionMarketDetail
	res.json(&detail)
	require.Len(t, detail.VoidProposals, 1)
	require.NotNil(t, detail.VoidProposals[0].Approval)
	assert.Equal(t, approver.String(), detail.VoidProposals[0].Approval.ApprovedBy.ActorId)
	res = h.do(http.MethodGet, base+"/books", nil)
	var books api.PredictionEscrowBooks
	res.json(&books)
	assert.Equal(t, "2000000", books.Escrow)
	assert.Equal(t, "2000000", books.Held)
	assert.True(t, books.Reconciles, "violations: %v", books.Violations)

	// The frozen holder sees the held refund, and cannot claim it yet.
	h.as(&custFrozen)
	res = h.do(http.MethodGet, base+"/positions?account_id="+frozen.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var positions api.PredictionPositionList
	res.json(&positions)
	require.NotNil(t, positions.HeldPayout)
	assert.Equal(t, "2000000", positions.HeldPayout.Amount)
	assert.False(t, positions.HeldPayout.Claimed)
	claim := base + "/held-payouts/claim"
	res = h.do(http.MethodPost, claim, map[string]any{"account_id": frozen.String()}, "Idempotency-Key", key())
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeRiskRejected, res.problem().Code)

	// Eligible again: the claim delivers exactly the held amount, once.
	require.NoError(t, d.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := accounts.NewRepository().Transition(ctx, tx, frozen, accounts.StatusChange{
			To: accounts.StatusActive, ActorType: "OPERATOR", ActorID: "compliance", Reason: "itest"}, w.clk.Now())
		return err
	}))
	claimKey := key()
	res = h.do(http.MethodPost, claim, map[string]any{"account_id": frozen.String()}, "Idempotency-Key", claimKey)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var claimed api.PredictionClaimResult
	res.json(&claimed)
	assert.True(t, claimed.HeldPayout.Claimed)
	assert.Equal(t, "COMPLETED", claimed.Intent.Status)
	assert.Equal(t, "10000000", w.wallet(frozen, econ.SandboxDollarID()))
	again := h.do(http.MethodPost, claim, map[string]any{"account_id": frozen.String()}, "Idempotency-Key", claimKey)
	require.Equal(t, http.StatusOK, again.Code, "the same request replays: %s", again.Body.String())
	res = h.do(http.MethodPost, claim, map[string]any{"account_id": frozen.String()}, "Idempotency-Key", key())
	require.Equal(t, http.StatusConflict, res.Code, "a second claim is refused: %s", res.Body.String())
	assert.Equal(t, "10000000", w.wallet(frozen, econ.SandboxDollarID()))

	h.as(&opC)
	res = h.do(http.MethodGet, base+"/books", nil)
	res.json(&books)
	assert.Equal(t, "0", books.Escrow)
	assert.Equal(t, "0", books.Held)
	assert.True(t, books.Reconciles, "violations: %v", books.Violations)
}
