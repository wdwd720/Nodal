package transfers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuation"
)

func TestTransitions_TheTransferStatusMachine(t *testing.T) {
	assert.True(t, CanTransition(StatusPending, StatusCompleted))
	assert.True(t, CanTransition(StatusPending, StatusRejected))
	assert.True(t, CanTransition(StatusCompleted, StatusReversed))
	for _, s := range AllStatuses() {
		assert.True(t, s.Valid())
		assert.False(t, CanTransition(s, s), "%s self transition", s)
		assert.False(t, CanTransition(s, StatusPending), "nothing returns to PENDING")
	}
	assert.False(t, CanTransition(StatusRejected, StatusCompleted), "a refusal is final")
	assert.False(t, CanTransition(StatusReversed, StatusCompleted), "a reversal is final")
	assert.False(t, CanTransition(StatusPending, StatusReversed), "only a completed transfer is reversed")
	assert.True(t, StatusRejected.Terminal())
	assert.True(t, StatusReversed.Terminal())
	assert.False(t, StatusCompleted.Terminal())
	assert.False(t, Status("DONE").Valid())

	assert.Equal(t, []gates.Mode{gates.ModePaper, gates.ModeSandbox}, SupportedModes())
	assert.Equal(t, []RuleKind{RuleAllow, RuleBlock, RuleClear}, AllRuleKinds())
	assert.Equal(t, []SenderMode{SenderOpen, SenderAllowlistOnly}, AllSenderModes())
	assert.Len(t, AllRejectionCodes(), 10)
	for _, c := range AllRejectionCodes() {
		assert.True(t, validReasonCode(c), c)
	}
	assert.Equal(t, econ.EffectApplied, EffectStatusFor(StatusCompleted))
	assert.Equal(t, econ.EffectCompensated, EffectStatusFor(StatusReversed))
	assert.Empty(t, EffectStatusFor(StatusRejected))
}

// TestRisk_ATransferNeedsBalanceVelocityAndDestination pins the Risk V2
// contract this package builds its input against: TRANSFER_VALUE requires
// those three sections, and a missing one is a rejection, never a pass.
func TestRisk_ATransferNeedsBalanceVelocityAndDestination(t *testing.T) {
	in := risk.AdvancedInput{Intent: risk.AdvancedIntent{Kind: string(econ.KindTransferValue), ActorType: "USER"}}
	assert.Subset(t, risk.RequiredSections(in), []string{risk.SecAccount, risk.SecIdentity, risk.SecBalance, risk.SecVelocity, risk.SecDestination})
	in.Intent.AccountID = accounts.NewAccountID().String()
	in.Stage = risk.StageFinal
	in.Now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	in.Identity = &risk.IdentitySnapshot{Verified: true}
	in.Account = &risk.AdvancedAccountSnapshot{Status: "ACTIVE"}
	in.Balance = &risk.BalanceSnapshot{Available: usd(100), Required: usd(10)}
	in.Velocity = &risk.VelocitySnapshot{}
	d := risk.EvaluateAdvanced(risk.DefaultAdvancedPolicy(), in)
	assert.Equal(t, risk.Reject, d.Verdict)
	assert.Equal(t, []string{risk.SecDestination}, d.MissingSections)
	in.Destination = &risk.DestinationSnapshot{Allowlisted: true}
	assert.Equal(t, risk.Allow, risk.EvaluateAdvanced(risk.DefaultAdvancedPolicy(), in).Verdict)
}

// TestRoute_TheCompilerForbidsAgentTransfersAtEveryLevel: econ.Route sends a
// TRANSFER_VALUE intent through the settlement compiler, which refuses an
// agent at every authority level and permits a person.
func TestRoute_TheCompilerForbidsAgentTransfersAtEveryLevel(t *testing.T) {
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	q := money.QuantityFromInt64(5)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	subject := settlement.Subject{Type: settlement.SubjectAccount, ID: accounts.NewAccountID().String(), AssetID: assets.NewAssetID().String()}
	base := econ.Intent{ID: econ.NewIntentID(), AccountID: accounts.NewAccountID(), Kind: econ.KindTransferValue,
		Mode: gates.ModeSandbox, Capability: gates.P2PTransfers, Quantity: &q, IdempotencyKey: "k"}
	for _, l := range agentauthority.AllLevels() {
		in := base
		in.ActorType, in.ActorID, in.AgentID = security.ActorAgent, "agent", accounts.NewAccountID().String()
		r, err := econ.Route(econ.RouteRequest{Intent: in, Subject: subject, AgentAuthority: l}, router, now)
		require.NoError(t, err)
		assert.False(t, r.Permitted, "level %s", l)
		assert.True(t, r.HasReason(settlement.ReasonAgentActionForbidden), "level %s: %v", l, r.Reasons)
	}
	person := base
	person.ActorType, person.ActorID = security.ActorUser, "user"
	r, err := econ.Route(econ.RouteRequest{Intent: person, Subject: subject}, router, now)
	require.NoError(t, err)
	assert.True(t, r.MayExecuteNow(), "%v", r.Reasons)
}

func TestQuote_ParValuationIsExactAndRoundsAsAsked(t *testing.T) {
	par := Quote{Method: ValuationPar, Decimals: 6}
	for _, c := range []struct {
		qty      int64
		up, down int64
	}{{1, 1, 0}, {10_000, 1, 1}, {10_001, 2, 1}, {12_345_678, 1235, 1234}, {1_000_000_000, 1_000_00, 1_000_00}, {0, 0, 0}} {
		u, err := par.USD(money.QuantityFromInt64(c.qty), money.RoundUp)
		require.NoError(t, err)
		d, err := par.USD(money.QuantityFromInt64(c.qty), money.RoundDown)
		require.NoError(t, err)
		assert.Equal(t, c.up, u.Minor(), "%d up", c.qty)
		assert.Equal(t, c.down, d.Minor(), "%d down", c.qty)
	}
	_, err := par.USD(money.QuantityFromInt64(-1), money.RoundUp)
	require.Error(t, err)
	_, err = Quote{Method: ValuationMark, Decimals: 9}.USD(money.QuantityFromInt64(1), money.RoundUp)
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err), "a mark quote without a price is not a price")
	_, err = Quote{Method: "GUESS"}.USD(money.QuantityFromInt64(1), money.RoundUp)
	require.Error(t, err)

	// 1.5 GOLD (9 decimals) at 2000.5 SBX (6 decimals) = 3000.75 SBX = $3,000.75.
	price, err := money.NewPrice(money.QuantityFromInt64(2_000_500_000), 6, "sbx", "itest", time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	mark := Quote{Method: ValuationMark, Decimals: 9, Price: &price, QuoteDecimals: 6}
	u, err := mark.USD(money.QuantityFromInt64(1_500_000_000), money.RoundUp)
	require.NoError(t, err)
	assert.Equal(t, int64(3_000_75), u.Minor())
}

// TestProp_ValuationRoundsInTheStatedDirection: rounded up is never below
// rounded down, and they differ by at most one cent -- the limits see the
// conservative side of the same exact value.
func TestProp_ValuationRoundsInTheStatedDirection(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		q := Quote{Method: ValuationPar, Decimals: uint8(rapid.IntRange(0, 18).Draw(rt, "decimals"))}
		qty := money.QuantityFromInt64(rapid.Int64Range(0, 1<<52).Draw(rt, "qty"))
		up, err := q.USD(qty, money.RoundUp)
		if err != nil {
			return
		}
		down, err := q.USD(qty, money.RoundDown)
		if err != nil {
			rt.Fatalf("down failed where up did not: %v", err)
		}
		if up.Cmp(down) < 0 || up.Minor()-down.Minor() > 1 {
			rt.Fatalf("up %s down %s", up, down)
		}
	})
}

func TestValuer_RegistryValuerFailsClosed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	sbx := assets.Asset{ID: assets.NewAssetID(), Kind: assets.KindSimulated, Decimals: 6, IsStablecoin: true, PegCurrency: valuation.USDPeg}
	q, err := RegistryValuer{}.Quote(ctx, nil, sbx, now)
	require.NoError(t, err)
	assert.Equal(t, ValuationPar, q.Method)

	gold := assets.Asset{ID: assets.NewAssetID(), Kind: assets.KindSimulated, Decimals: 9}
	_, err = RegistryValuer{}.Quote(ctx, nil, gold, now)
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err), "no price source: refused, not valued at zero")

	real := assets.Asset{ID: assets.NewAssetID(), Kind: assets.KindSPLToken, Decimals: 6, IsStablecoin: true, PegCurrency: valuation.USDPeg}
	_, err = RegistryValuer{}.Quote(ctx, nil, real, now)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "a real stablecoin is never a transfer asset")
	assert.False(t, ParValued(real))
	assert.False(t, ParValued(gold))
	assert.True(t, ParValued(sbx))
}

func TestListScreen_BlocksTheConfiguredPartiesDeterministically(t *testing.T) {
	blockedAccount, blockedUser := accounts.NewAccountID(), accounts.NewUserID()
	s, err := NewListScreen("v1", map[string]string{blockedAccount.String(): "SANCTIONS_LIST_MATCH", blockedUser.String(): "FRAUD_RING"})
	require.NoError(t, err)
	ctx := context.Background()
	clear := ScreenRequest{SenderAccountID: accounts.NewAccountID(), SenderUserID: accounts.NewUserID(),
		RecipientAccountID: accounts.NewAccountID(), RecipientUserID: accounts.NewUserID()}
	res, err := s.Screen(ctx, clear)
	require.NoError(t, err)
	assert.Equal(t, ScreenClear, res.Decision)
	assert.Equal(t, ListScreenName, res.Screen)

	for role, mutate := range map[string]func(*ScreenRequest){
		"sender_account":    func(r *ScreenRequest) { r.SenderAccountID = blockedAccount },
		"recipient_account": func(r *ScreenRequest) { r.RecipientAccountID = blockedAccount },
		"sender_user":       func(r *ScreenRequest) { r.SenderUserID = blockedUser },
		"recipient_user":    func(r *ScreenRequest) { r.RecipientUserID = blockedUser },
	} {
		req := clear
		mutate(&req)
		a, err := s.Screen(ctx, req)
		require.NoError(t, err)
		b, err := s.Screen(ctx, req)
		require.NoError(t, err)
		assert.Equal(t, a, b, "deterministic")
		assert.Equal(t, ScreenBlock, a.Decision, role)
		assert.Equal(t, role, a.Matched)
	}
	_, err = NewListScreen("v1", map[string]string{"x": "not a code"})
	require.Error(t, err)
	_, err = NewListScreen("", nil)
	require.Error(t, err)
}

func TestService_ConstructionAndRefusalMapping(t *testing.T) {
	_, err := NewService(Deps{})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	for code, want := range map[string]errs.Code{
		RejectRecipientBlocked: errs.CodeForbidden, RejectRecipientNotAllowed: errs.CodeForbidden,
		RejectComplianceBlocked: errs.CodeForbidden, RejectRouteRefused: errs.CodeForbidden,
		RejectComplianceUnavailable: errs.CodeProviderUnavailable, RejectInsufficientFunds: errs.CodeInsufficientBuyingPower,
		risk.ReasonInsufficientBalance: errs.CodeInsufficientBuyingPower, RejectVelocityCount: errs.CodeRiskRejected,
		RejectLimitDaily: errs.CodeRiskRejected, risk.ReasonIdentityUnverified: errs.CodeRiskRejected,
		string(errs.CodeAccountFrozen): errs.CodeAccountFrozen,
	} {
		assert.Equal(t, want, errorCodeFor(code), code)
	}
	assert.NoError(t, Transfer{Status: StatusCompleted}.Err())
	err = Transfer{Status: StatusRejected, RejectionCode: RejectLimitDaily, ReasonCodes: []string{RejectLimitDaily}}.Err()
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err))
	assert.Equal(t, []string{"A", "B"}, dedupeKeepOrder([]string{"A", "", "B", "A"}))
}
