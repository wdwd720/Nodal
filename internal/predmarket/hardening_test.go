package predmarket

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
)

// TestHolderScopedRefusal_HoldsOnlyTheHoldersOwnRefusals: a payout refused
// for the holder's own account, identity, eligibility or jurisdiction is held;
// any refusal that names anything else -- a kill switch, the instrument, the
// balance, missing input, an internal error -- stops the whole settlement.
func TestHolderScopedRefusal_HoldsOnlyTheHoldersOwnRefusals(t *testing.T) {
	riskRefusal := func(codes ...string) error {
		return risk.AdvancedDecision{Verdict: risk.Reject, ReasonCodes: codes, IntentKind: "RESOLVE_PREDICTION_POSITION"}.RejectionError()
	}
	routeRefusal := func(reasons ...string) error {
		return errs.New(errs.CodeForbidden, "refused").WithField("reasons", reasons).WithField("refusal_stage", "route")
	}
	held := map[string]error{
		"a frozen account":          riskRefusal(risk.ReasonAccountStatus),
		"an unverified identity":    riskRefusal(risk.ReasonIdentityUnverified),
		"both":                      riskRefusal(risk.ReasonAccountStatus, risk.ReasonIdentityUnverified),
		"an ineligible holder":      riskRefusal(risk.ReasonEligibilityDenied),
		"a blocked jurisdiction":    riskRefusal(risk.ReasonJurisdictionBlocked),
		"verification at routing":   routeRefusal(settlement.ReasonVerificationRequired),
		"the legal router's denial": routeRefusal(settlement.ReasonLegalRouterDenied),
	}
	for name, err := range held {
		reasons, ok := holderScopedRefusal(err)
		assert.True(t, ok, name)
		assert.NotEmpty(t, reasons, name)
	}
	stops := map[string]error{
		"a kill switch":                        riskRefusal(risk.ReasonKillSwitch),
		"the holder's account and a kill":      riskRefusal(risk.ReasonAccountStatus, risk.ReasonKillSwitch),
		"the instrument":                       riskRefusal(risk.ReasonInstrumentNotTradable),
		"the escrow's balance":                 riskRefusal(risk.ReasonInsufficientBalance),
		"missing input":                        riskRefusal(risk.ReasonInputInvalid),
		"a refusal naming no reason":           riskRefusal(),
		"a rail the build cannot execute":      routeRefusal(settlement.ReasonRailNotImplemented),
		"a forbidden route without its stage":  errs.New(errs.CodeForbidden, "refused").WithField("reasons", []string{settlement.ReasonVerificationRequired}),
		"dual control":                         errs.New(errs.CodeForbidden, "predmarket: dual control"),
		"an internal error":                    errs.New(errs.CodeInternal, "boom"),
		"a plain error":                        assert.AnError,
		"reconciliation":                       errs.New(errs.CodeReconciliationRequired, "books"),
		"a route refusal also naming the rail": routeRefusal(settlement.ReasonLegalRouterDenied, settlement.ReasonRailNotImplemented),
	}
	for name, err := range stops {
		_, ok := holderScopedRefusal(err)
		assert.False(t, ok, name)
	}
}

// TestClaimPosting_PaysTheHeldAmountOutOfTheEscrowAndMovesNoShare: a claim is
// the cash half of a payout: the holder's wallet gains exactly the amount, the
// market's escrow and collateral lose it, and no share moves.
func TestClaimPosting_PaysTheHeldAmountOutOfTheEscrowAndMovesNoShare(t *testing.T) {
	m := testMarket(3)
	a := accounts.NewAccountID()
	f := m.Facility()
	cash := m.CollateralAssetID
	for _, kind := range AllSettlementKinds() {
		p, err := ClaimPosting(m, kind, a, q(1_250_000), meta("claim-"+string(kind)))
		require.NoError(t, err)
		want := ledger.KindPredictionPayout
		if kind == SettlementRefund {
			want = ledger.KindPredictionRefund
		}
		assert.Equal(t, want, p.Kind)
		d := deltas(p)
		assert.Equal(t, "1250000", d[ledger.CustomerAccount(a, ledger.CodeWallet, cash)].String())
		assert.Equal(t, "1250000", d[ledger.CustomerAccount(a, ledger.CodeTradingInflow, cash)].String())
		assert.Equal(t, "-1250000", d[f.Account(ledger.CodePredictionEscrow, cash)].String())
		assert.Equal(t, "-1250000", d[f.Account(ledger.CodePredictionCollateral, cash)].String())
		for _, e := range p.Entries {
			assert.Equal(t, cash, e.Account.AssetID, "no share moves in a claim")
		}
	}
	_, err := ClaimPosting(m, SettlementPayout, a, money.Quantity{}, meta("zero"))
	require.Error(t, err, "a claim delivers something")
	_, err = ClaimPosting(m, SettlementKind("BONUS"), a, q(1), meta("bonus"))
	require.Error(t, err)
}

// TestProp_AHeldLineRetiresTheSharesOfADeliveredOneAndKeepsTheCash: the
// posting that holds a holder's payout is the delivered payout with its cash
// legs removed, and the delivered payout is exactly the held one plus its
// claim -- so holding and claiming later moves the same value as paying now.
func TestProp_AHeldLineRetiresTheSharesOfADeliveredOneAndKeepsTheCash(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(MinOutcomes, 5).Draw(rt, "outcomes")
		m := testMarket(n)
		line := PlanLine{AccountID: accounts.NewAccountID(), Shares: make([]money.Quantity, n)}
		for i := range line.Shares {
			line.Shares[i] = q(rapid.Int64Range(0, 9_000_000).Draw(rt, "shares"))
		}
		line.Shares[rapid.IntRange(0, n-1).Draw(rt, "nonzero")] = q(rapid.Int64Range(1, 9_000_000).Draw(rt, "held"))
		line.Amount = q(rapid.Int64Range(1, 9_000_000).Draw(rt, "amount"))
		kind := AllSettlementKinds()[rapid.IntRange(0, 1).Draw(rt, "kind")]

		paid, err := SettlementPosting(m, kind, line, meta("paid"))
		if err != nil {
			rt.Fatalf("paid: %v", err)
		}
		retire := line
		retire.Amount = money.Quantity{}
		held, err := SettlementPosting(m, kind, retire, meta("held"))
		if err != nil {
			rt.Fatalf("held: %v", err)
		}
		claim, err := ClaimPosting(m, kind, line.AccountID, line.Amount, meta("claim"))
		if err != nil {
			rt.Fatalf("claim: %v", err)
		}
		together := deltas(held)
		for acct, d := range deltas(claim) {
			together[acct] = together[acct].Add(d)
		}
		for acct, d := range deltas(paid) {
			if together[acct].Cmp(d) != 0 {
				rt.Fatalf("%v moves %s when paid and %s when held then claimed", acct, d, together[acct])
			}
		}
		for acct := range together {
			if acct.AssetID == m.CollateralAssetID && deltas(held)[acct].Sign() != 0 {
				rt.Fatalf("holding moved collateral on %v", acct)
			}
		}
	})
}

func TestEscrowBooks_HeldPayoutsArePartOfTheIdentity(t *testing.T) {
	settled := EscrowBooks{
		Escrow: q(5), Collateral: q(5), Held: q(5),
		SharesIssued: []money.Quantity{q(0), q(0)}, SharesOutstanding: []money.Quantity{q(0), q(0)},
		HeldByCustomers: []money.Quantity{q(0), q(0)},
	}
	assert.Empty(t, settled.Violations(money.Quantity{}), "the escrow holds exactly the held payouts")
	voided := settled
	voided.Escrow, voided.Collateral = q(7), q(7)
	assert.Empty(t, voided.Violations(q(2)), "plus a refund's residual")
	missing := settled
	missing.Held = q(4)
	assert.NotEmpty(t, missing.Violations(money.Quantity{}), "escrow no held payout explains")
	negative := settled
	negative.Held = q(-1)
	assert.NotEmpty(t, negative.Violations(money.Quantity{}))
}

func TestHardeningEnums_MatchTheirDefinitions(t *testing.T) {
	assert.Equal(t, []MarketStatus{StatusDraft, StatusOpen, StatusClosed, StatusResolving, StatusResolved, StatusDisputed},
		VoidableStatuses(), "every status but SETTLED and VOIDED, the two a market never leaves")
	for _, s := range VoidableStatuses() {
		assert.True(t, CanTransition(s, StatusVoided), "%s can be voided", s)
	}
	assert.Equal(t, []security.ActorType{security.ActorUser, security.ActorService}, ClaimActorTypes())

	r1, r2 := id.New[resolutionKind](), id.New[resolutionKind]()
	assert.True(t, sameResolution(nil, nil))
	assert.True(t, sameResolution(&r1, &r1))
	assert.False(t, sameResolution(&r1, &r2))
	assert.False(t, sameResolution(nil, &r1))
	assert.False(t, sameResolution(&r1, nil))
}

func TestClaimRequest_ValidateFailsClosed(t *testing.T) {
	good := ClaimRequest{MarketID: NewMarketID(), AccountID: accounts.NewAccountID(), IdempotencyKey: "k",
		ActorType: security.ActorUser, ActorID: "user"}
	require.NoError(t, good.validate())
	for name, mutate := range map[string]func(*ClaimRequest){
		"no market":       func(r *ClaimRequest) { r.MarketID = MarketID{} },
		"no account":      func(r *ClaimRequest) { r.AccountID = accounts.AccountID{} },
		"an agent":        func(r *ClaimRequest) { r.ActorType = security.ActorAgent },
		"an operator":     func(r *ClaimRequest) { r.ActorType = security.ActorOperator },
		"no actor":        func(r *ClaimRequest) { r.ActorID = " " },
		"no key":          func(r *ClaimRequest) { r.IdempotencyKey = "" },
		"a padded key":    func(r *ClaimRequest) { r.IdempotencyKey = " k " },
		"an oversize key": func(r *ClaimRequest) { r.IdempotencyKey = string(make([]byte, 257)) },
	} {
		r := good
		mutate(&r)
		err := r.validate()
		require.Error(t, err, name)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), name)
	}
}
