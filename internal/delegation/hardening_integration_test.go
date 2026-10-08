//go:build integration

package delegation

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Review 3 (red team) regression tests: what a swap does is the direction of
// its value (F1), a strategy version's kill switch stops its agent (F2), an
// execution run takes only what can be signed now and every account in turn
// (F3), and a venue's house book is its own.

// sellRequest asks to sell amountIn base units of the token for the stable
// unit: a reduction of exposure, labelled as one.
func (f *fixture) sellRequest(c customer, cid CapabilityID, nonce uint64, amountIn int64) SwapRequest {
	req := f.swapRequest(c, cid, nonce, amountIn)
	req.Action, req.PayAssetID, req.ReceiveAssetID = ActionReduceExposure, f.tok.ID, f.usd.ID
	return req
}

func (f *fixture) restrictAccount(c customer) {
	f.t.Helper()
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := accounts.NewRepository().Transition(ctx, tx, c.account, accounts.StatusChange{To: accounts.StatusRestricted,
			ActorType: "OPERATOR", ActorID: "itest", Reason: "restricted: only risk reduction is permitted"}, f.clk.Now())
		return err
	}))
}

// requireMislabelRefused: err is the refusal of a swap whose label is not
// what it does, and it is recorded -- the intent REJECTED with
// DELEGATION_ACTION_MISMATCH -- with nothing reserved or admitted under it.
func (f *fixture) requireMislabelRefused(cid CapabilityID, nonce uint64, err error) {
	f.t.Helper()
	requireCode(f.t, err, errs.CodeForbidden)
	requireReason(f.t, err, ReasonActionMismatch)
	var intentID econ.IntentID
	var status, code string
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT id, status, coalesce(rejection_code, '') FROM economic_intents
		WHERE idempotency_key = $1`, "delegation:swap:"+cid.String()+":"+strconv.FormatUint(nonce, 10)).Scan(&intentID, &status, &code))
	assert.Equal(f.t, "REJECTED", status, "the refusal is recorded on the intent")
	assert.Equal(f.t, ReasonActionMismatch, code)
	assert.Equal(f.t, 0, f.count(`SELECT count(*) FROM asset_reservations WHERE economic_intent_id = $1`, intentID), "nothing reserved")
	assert.Equal(f.t, 0, f.count(`SELECT count(*) FROM delegated_swaps WHERE intent_id = $1`, intentID), "nothing admitted")
}

// riskIncreasing is what Risk V2 was told about an intent.
func (f *fixture) riskIncreasing(iid econ.IntentID) bool {
	f.t.Helper()
	var increasing bool
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT (input->'intent'->>'risk_increasing')::boolean FROM advanced_risk_decisions
		WHERE intent_id = $1`, iid).Scan(&increasing))
	return increasing
}

func (f *fixture) strategyVersionOf(c customer) string {
	f.t.Helper()
	var version string
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT strategy_version_id::text FROM agents WHERE id = $1`, c.agentID).Scan(&version))
	return version
}

// --- F1: what a swap does is the direction of its value ------------------------------

// TestIntegration_AReduceOnlyCapabilityNeverBuysExposure: a capability that
// grants only REDUCE_EXPOSURE cannot buy the token under either label -- the
// honest one is not granted, the false one is refused as false and recorded
// -- and still sells the token, which is what it grants.
func TestIntegration_AReduceOnlyCapabilityNeverBuysExposure(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)          // $10
	f.fundCustomer(c, f.tok, 50_000_000) // 0.05 token
	reduceOnly := f.grant(c, func(r *GrantRequest) { r.ActionClasses = []ActionClass{ActionReduceExposure} })
	tokBefore := f.balance(c.account, ledger.CodeWallet, f.tok)

	_, err := f.requestSwap(f.agent(c), f.swapRequest(c, reduceOnly.ID, 1, 1_000_000))
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Contains(t, err.Error(), ReasonActionNotGranted)

	buy := f.swapRequest(c, reduceOnly.ID, 2, 1_000_000) // pays dollars, receives the token
	buy.Action = ActionReduceExposure
	_, err = f.requestSwap(f.agent(c), buy)
	f.requireMislabelRefused(reduceOnly.ID, 2, err)
	_, err = f.requestSwap(f.agent(c), buy)
	requireReason(t, err, ReasonActionMismatch) // a replay is the same refusal
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM delegated_capability_usage WHERE capability_id = $1`, reduceOnly.ID),
		"nothing was used under the capability")
	assert.Equal(t, tokBefore.String(), f.balance(c.account, ledger.CodeWallet, f.tok).String())

	ticket, err := f.requestSwap(f.agent(c), f.sellRequest(c, reduceOnly.ID, 3, 20_000_000))
	require.NoError(t, err)
	assert.Equal(t, ActionReduceExposure, ticket.Swap.Action)
	assert.False(t, f.riskIncreasing(ticket.Swap.IntentID))
	res, err := f.svc.ExecuteSwap(f.ctx, ticket.Swap.ID)
	require.NoError(t, err)
	require.Equal(t, SwapSettled, res.Swap.Status)
	assert.Equal(t, "30000000", f.balance(c.account, ledger.CodeWallet, f.tok).String(), "the token only ever left the account")
}

// TestIntegration_ARestrictedAccountsAgentOnlyReducesExposure: in a
// RESTRICTED account (risk reduction only) a purchase is refused under either
// label, a purchase admitted before the restriction is never signed, and a
// sale -- admitted before or after -- settles. Risk V2 is told a purchase
// increases risk and a sale does not, whatever the label.
func TestIntegration_ARestrictedAccountsAgentOnlyReducesExposure(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	f.fundCustomer(c, f.tok, 50_000_000)
	capability := f.grant(c, nil)
	purchase := f.mustSwap(c, capability.ID, 1, 1_000_000)
	early, err := f.requestSwap(f.agent(c), f.sellRequest(c, capability.ID, 2, 10_000_000))
	require.NoError(t, err)
	f.restrictAccount(c)

	_, err = f.requestSwap(f.agent(c), f.swapRequest(c, capability.ID, 3, 1_000_000))
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Contains(t, err.Error(), risk.ReasonAccountStatus)
	mislabelled := f.swapRequest(c, capability.ID, 4, 1_000_000)
	mislabelled.Action = ActionReduceExposure
	_, err = f.requestSwap(f.agent(c), mislabelled)
	f.requireMislabelRefused(capability.ID, 4, err)

	_, err = f.svc.ExecuteSwap(f.ctx, purchase.ID)
	requireCode(t, err, errs.CodeSigningRejected)
	assert.Equal(t, SwapRejected, f.swap(purchase.ID).Status)
	assert.Equal(t, ReasonAccountNotActive, f.swap(purchase.ID).FailureCode)
	res, err := f.svc.ExecuteSwap(f.ctx, early.Swap.ID)
	require.NoError(t, err)
	assert.Equal(t, SwapSettled, res.Swap.Status)

	late, err := f.requestSwap(f.agent(c), f.sellRequest(c, capability.ID, 5, 10_000_000))
	require.NoError(t, err)
	res, err = f.svc.ExecuteSwap(f.ctx, late.Swap.ID)
	require.NoError(t, err)
	assert.Equal(t, SwapSettled, res.Swap.Status)

	assert.True(t, f.riskIncreasing(purchase.IntentID))
	assert.False(t, f.riskIncreasing(late.Swap.IntentID))
	assert.Equal(t, "30000000", f.balance(c.account, ledger.CodeWallet, f.tok).String(), "the restricted account only sold")
	var status string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status FROM accounts WHERE id = $1`, c.account).Scan(&status))
	assert.Equal(t, "RESTRICTED", status)
}

// TestIntegration_ThePlatformSingleTradeCapBindsEveryPurchase: the platform's
// own single-trade cap (Risk V2, $1M) refuses a $1.5M purchase whatever it is
// called, and -- applying to risk-increasing intents only -- never a $1.5M
// sale.
func TestIntegration_ThePlatformSingleTradeCapBindsEveryPurchase(t *testing.T) {
	f := newFixture(t)
	const usd15M = int64(1_500_000_000_000)  // $1.5M of the stable unit (6 decimals)
	const tok15M = int64(15_000_000_000_000) // 15,000 tokens at 100.00 = $1.5M (9 decimals)
	f.fundHouse(f.tok, 20_000_000_000_000)
	f.mintTo(f.venueKey.PublicKey(), f.tok, 20_000_000_000_000)
	f.fundHouse(f.usd, 2_000_000_000_000)
	f.mintTo(f.venueKey.PublicKey(), f.usd, 2_000_000_000_000)
	c := f.customer(usd15M)
	f.fundCustomer(c, f.tok, tok15M)
	capability := f.grant(c, func(r *GrantRequest) {
		r.MaxNotional, r.MaxDailyNotional, r.MaxSingleTrade =
			money.USDFromMinor(600_000_000), money.USDFromMinor(600_000_000), money.USDFromMinor(200_000_000) // $6M/$6M/$2M
	})

	_, err := f.requestSwap(f.agent(c), f.swapRequest(c, capability.ID, 1, usd15M))
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Contains(t, err.Error(), risk.ReasonMaxSingleTrade)
	buy := f.swapRequest(c, capability.ID, 2, usd15M)
	buy.Action = ActionReduceExposure
	_, err = f.requestSwap(f.agent(c), buy)
	f.requireMislabelRefused(capability.ID, 2, err)

	ticket, err := f.requestSwap(f.agent(c), f.sellRequest(c, capability.ID, 3, tok15M))
	require.NoError(t, err)
	assert.Equal(t, int64(150_000_000), ticket.Swap.NotionalUSD.Minor())
	assert.False(t, f.riskIncreasing(ticket.Swap.IntentID))
	res, err := f.svc.ExecuteSwap(f.ctx, ticket.Swap.ID)
	require.NoError(t, err)
	assert.Equal(t, SwapSettled, res.Swap.Status)
}

// TestIntegration_TheDatabaseBindsASwapsActionToItsDirection: the database
// refuses a swap row whose action class is not the direction of its value
// (DG007) -- a purchase stored as a reduction or a sale as an acquisition,
// even by the migration owner -- and refuses to sign one whose assets were
// reclassified after its admission; the service refuses to sign it first.
func TestIntegration_TheDatabaseBindsASwapsActionToItsDirection(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	f.fundCustomer(c, f.tok, 100_000_000)
	capability := f.grant(c, nil)
	purchase := f.mustSwap(c, capability.ID, 1, 1_000_000)
	sale, err := f.requestSwap(f.agent(c), f.sellRequest(c, capability.ID, 2, 10_000_000))
	require.NoError(t, err)
	sale2, err := f.requestSwap(f.agent(c), f.sellRequest(c, capability.ID, 3, 10_000_000))
	require.NoError(t, err)
	owner := ownerConn(t)

	forge := func(src Swap, action ActionClass) error {
		forged := sha256Of([]byte(uuid.NewString()))
		return rawTx(f.ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(f.ctx, `INSERT INTO delegated_swaps (id, capability_id, account_id, agent_id, request_nonce, intent_id,
				usage_id, reservation_id, venue_id, mode, action_class, chain_id, wallet, pay_asset_id, receive_asset_id, amount_in,
				amount_out, min_amount_out, notional_usd_minor, requested_at, quote_expires_at, requires_approval, plan, plan_hash, status)
				SELECT $1, capability_id, account_id, agent_id, request_nonce + 1000, intent_id, usage_id, reservation_id, venue_id, mode,
				       $2, chain_id, wallet, pay_asset_id, receive_asset_id, amount_in, amount_out, min_amount_out, notional_usd_minor,
				       requested_at, quote_expires_at, requires_approval, plan, $3, 'ADMITTED'
				  FROM delegated_swaps WHERE id = $4`, uuid.New(), action, forged[:], src.ID)
			return err
		})
	}
	requireSQLState(t, forge(purchase, ActionReduceExposure), "DG007")
	requireSQLState(t, forge(sale.Swap, ActionAcquireExposure), "DG007")

	// The stable unit stops being one after the sales were admitted: paying
	// the token for it is now a swap of one token for another.
	_, err = owner.Exec(f.ctx, `UPDATE assets SET is_stablecoin = false, peg_currency = NULL WHERE id = $1`, f.usd.ID)
	require.NoError(t, err)
	_, err = f.svc.ExecuteSwap(f.ctx, sale.Swap.ID)
	requireCode(t, err, errs.CodeSigningRejected)
	got := f.swap(sale.Swap.ID)
	assert.Equal(t, SwapRejected, got.Status)
	assert.Equal(t, ReasonActionMismatch, got.FailureCode)
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(got.ReservationID).Status)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM delegated_signing_decisions WHERE swap_id = $1`, got.ID), "nothing was signed")
	err = rawTx(f.ctx, owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `UPDATE delegated_swaps SET status = 'SIGNED', message = '\x01', signed_at = $2 WHERE id = $1`,
			sale2.Swap.ID, f.clk.Now())
		return err
	})
	requireSQLState(t, err, "DG007")
	assert.Equal(t, SwapAdmitted, f.swap(sale2.Swap.ID).Status)
}

// TestIntegration_SQLAndGoAgreeOnWhatASwapDoes: cp_delegation_instrument_class
// and cp_delegation_swap_action decide exactly what ClassifyAsset and
// SwapAction decide, for every combination of kind, peg and risk class.
func TestIntegration_SQLAndGoAgreeOnWhatASwapDoes(t *testing.T) {
	f := newFixture(t)
	repo := assets.NewRepository()
	var made []assets.Asset
	n := 0
	for _, stable := range []bool{true, false} {
		for _, peg := range []string{valuation.USDPeg, "EUR"} {
			for _, class := range []assets.RiskClass{assets.RiskSettlement, assets.RiskMajor, assets.RiskStandard,
				assets.RiskSpeculative, assets.RiskUnsupported} {
				n++
				symbol := "DM" + strconv.Itoa(n) + f.tag[:4]
				a, err := repo.Create(f.ctx, testDB, assets.Asset{Chain: string(devnethost.ChainSolana),
					MintAddress: ukey("matrix-" + f.tag + "-" + strconv.Itoa(n)).String(), Kind: assets.KindSimulated,
					ValueDomain: valuedomain.Simulated, Symbol: symbol, Name: symbol, Decimals: 6, IsStablecoin: stable,
					PegCurrency: peg, RiskClass: class, Status: assets.StatusActive})
				require.NoError(t, err)
				made = append(made, a)
			}
		}
	}
	fiat, err := repo.Create(f.ctx, testDB, assets.Asset{Chain: assets.FiatChain, MintAddress: "XQ" + f.tag[:6], Kind: assets.KindFiat,
		Symbol: "XQ" + f.tag[:6], Name: "fiat reference", Decimals: 2, RiskClass: assets.RiskUnsupported, Status: assets.StatusRestricted})
	require.NoError(t, err)
	made = append(made, fiat)
	ids := make([]string, 0, len(made))
	byID := map[string]assets.Asset{}
	for _, a := range made {
		ids = append(ids, a.ID.String())
		byID[a.ID.String()] = a
	}

	rows, err := testDB.Query(f.ctx, `SELECT a::text, coalesce(cp_delegation_instrument_class(a), '') FROM unnest($1::text[]::uuid[]) AS a`, ids)
	require.NoError(t, err)
	for rows.Next() {
		var aid, class string
		require.NoError(t, rows.Scan(&aid, &class))
		got, ok := ClassifyAsset(byID[aid])
		if !ok {
			got = ""
		}
		assert.Equal(t, string(got), class, "%+v", byID[aid])
	}
	rows.Close()
	require.NoError(t, rows.Err())

	rows, err = testDB.Query(f.ctx, `SELECT p::text, r::text, cp_delegation_swap_action(p, r)
		FROM unnest($1::text[]::uuid[]) AS p CROSS JOIN unnest($1::text[]::uuid[]) AS r WHERE p <> r`, ids)
	require.NoError(t, err)
	defer rows.Close()
	pairs, reductions := 0, 0
	for rows.Next() {
		var p, r, action string
		require.NoError(t, rows.Scan(&p, &r, &action))
		want := SwapAction(byID[p], byID[r])
		assert.Equal(t, string(want), action, "pay %+v receive %+v", byID[p], byID[r])
		pairs++
		if want == ActionReduceExposure {
			reductions++
		}
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, len(made)*(len(made)-1), pairs)
	assert.Positive(t, reductions, "the matrix holds reductions as well as acquisitions")
}

// --- F2: a strategy version's kill switch stops its agent ------------------------------

// TestIntegration_AStrategyVersionKillSwitchStopsItsAgentAtEveryStep: a
// STRATEGY_VERSION_DISABLE switch on the strategy version an agent runs stops
// the agent's swap at the request, at the signing and before the submission,
// and its withdrawal at the proposal, the approval, the signing and before
// the submission; Risk V2 is told the version. What was submitted before the
// switch still settles: settlement consults no switch.
func TestIntegration_AStrategyVersionKillSwitchStopsItsAgentAtEveryStep(t *testing.T) {
	f := newFixture(t)
	disable := func(c customer) { f.activateKill(killswitch.StrategyVersionDisable, f.strategyVersionOf(c)) }

	// Withdrawals first: each setup moves the clock, which would expire a
	// swap's quote.
	s5 := f.withdrawalSetup(nil, nil)
	s6 := f.withdrawalSetup(nil, nil)
	s7 := f.withdrawalSetup(nil, nil)
	s8 := f.withdrawalSetup(nil, nil)
	s9 := f.withdrawalSetup(nil, func(r *GrantRequest) { r.Mode = gates.ModeDevnet })
	w6 := f.mustPropose(s6, 1, 60_000_000)
	require.Equal(t, WithdrawalAwaitingApproval, w6.Status)
	w7 := f.mustPropose(s7, 1, 1_000_000)
	require.Equal(t, WithdrawalApproved, w7.Status)
	w8 := f.mustPropose(s8, 1, 1_000_000)
	w9 := f.mustPropose(s9, 1, 1_000_000)
	require.NoError(t, f.host.EnsureNativeBalance(f.ctx, devnethost.ChainSolana, s8.c.wallet, 1_000_000, 100_000_000))
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		out, err := f.svc.SignWithdrawal(ctx, tx, w8.ID)
		require.True(t, out.Fresh)
		return err
	}))
	res9, err := f.svc.ExecuteWithdrawal(f.ctx, w9.ID)
	require.NoError(t, err)
	require.Equal(t, WithdrawalSubmitted, res9.Withdrawal.Status, "DEVNET: submitted, not yet final")

	c1, c2, c3, c4 := f.customer(10_000_000), f.customer(10_000_000), f.customer(10_000_000), f.customer(10_000_000)
	cap1, cap2, cap3 := f.grant(c1, nil), f.grant(c2, nil), f.grant(c3, nil)
	cap4 := f.grant(c4, func(r *GrantRequest) { r.Mode = gates.ModeDevnet })
	admitted := f.mustSwap(c2, cap2.ID, 1, 1_000_000)
	signed := f.mustSwap(c3, cap3.ID, 1, 1_000_000)
	require.NoError(t, f.host.EnsureNativeBalance(f.ctx, devnethost.ChainSolana, c3.wallet, 1_000_000, 100_000_000))
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		out, err := f.svc.SignSwap(ctx, tx, signed.ID)
		require.True(t, out.Fresh)
		return err
	}))
	submitted := f.mustSwap(c4, cap4.ID, 1, 1_000_000)
	res4, err := f.svc.ExecuteSwap(f.ctx, submitted.ID)
	require.NoError(t, err)
	require.Equal(t, SwapSubmitted, res4.Swap.Status, "DEVNET: submitted, not yet final")

	// Risk V2 was told the version the agent runs.
	var told string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT coalesce(input->'intent'->>'strategy_version_id', '')
		FROM advanced_risk_decisions WHERE intent_id = $1`, admitted.IntentID).Scan(&told))
	assert.Equal(t, f.strategyVersionOf(c2), told)

	for _, c := range []customer{c1, c2, c3, c4, s5.c, s6.c, s7.c, s8.c, s9.c} {
		disable(c)
	}

	// Swaps: the request, the signing, the last check before the submission.
	_, err = f.requestSwap(f.agent(c1), f.swapRequest(c1, cap1.ID, 1, 1_000_000))
	requireCode(t, err, errs.CodeKillSwitchActive)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1 AND status = 'REJECTED'
		AND rejection_code = $2`, c1.account, RefusalKillSwitch), "recorded")
	_, err = f.svc.ExecuteSwap(f.ctx, admitted.ID)
	requireCode(t, err, errs.CodeSigningRejected)
	assert.Equal(t, SwapRejected, f.swap(admitted.ID).Status)
	assert.Equal(t, RefusalKillSwitch, f.swap(admitted.ID).FailureCode)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM delegated_signing_decisions WHERE swap_id = $1`, admitted.ID))
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.GateSwapSubmission(ctx, tx, signed.ID)
		if reasonOf(err) == ReasonBlockedBeforeSubmission {
			return nil
		}
		return err
	})
	require.NoError(t, err)
	gotSigned := f.swap(signed.ID)
	assert.Equal(t, SwapFailed, gotSigned.Status)
	assert.Equal(t, ReasonBlockedBeforeSubmission+":"+RefusalKillSwitch, gotSigned.FailureCode)
	st, err := f.host.Status(f.ctx, devnethost.ChainSolana, gotSigned.TxID)
	require.NoError(t, err)
	assert.Equal(t, devnet.TxNotFound, st.State, "never submitted")

	// Withdrawals: the proposal, the approval, the signing, the last check
	// before the submission.
	_, err = f.propose(f.agent(s5.c), f.proposal(s5, 1, 1_000_000))
	requireCode(t, err, errs.CodeKillSwitchActive)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1 AND status = 'REJECTED'
		AND rejection_code = $2`, s5.c.account, RefusalKillSwitch), "recorded")
	_, err = f.decideWithdrawal(f.owner(s6.c), WithdrawalDecision{WithdrawalID: w6.ID, PlanHash: w6.PlanHash, Approve: true, Reason: "go"})
	requireCode(t, err, errs.CodeKillSwitchActive)
	assert.Equal(t, WithdrawalAwaitingApproval, f.withdrawal(w6.ID).Status, "nothing changed")
	_, err = f.svc.ExecuteWithdrawal(f.ctx, w7.ID)
	requireCode(t, err, errs.CodeSigningRejected)
	assert.Equal(t, WithdrawalRejected, f.withdrawal(w7.ID).Status)
	assert.Equal(t, RefusalKillSwitch, f.withdrawal(w7.ID).FailureCode)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.GateWithdrawalSubmission(ctx, tx, w8.ID)
		if reasonOf(err) == ReasonBlockedBeforeSubmission {
			return nil
		}
		return err
	})
	require.NoError(t, err)
	got8 := f.withdrawal(w8.ID)
	assert.Equal(t, WithdrawalFailed, got8.Status)
	assert.Equal(t, ReasonBlockedBeforeSubmission+":"+RefusalKillSwitch, got8.FailureCode)
	st, err = f.host.Status(f.ctx, devnethost.ChainSolana, got8.TxID)
	require.NoError(t, err)
	assert.Equal(t, devnet.TxNotFound, st.State, "never submitted")
	assert.Equal(t, capital.ReservationReleased, f.reservationOf(got8.ReservationID).Status)

	// What was submitted before the switch settles.
	_, err = f.host.ProduceBlocks(f.ctx, devnethost.ChainSolana, 1+itDepth)
	require.NoError(t, err)
	_, err = f.svc.ReconcilePending(f.ctx, 100, false)
	require.NoError(t, err)
	assert.Equal(t, SwapSettled, f.swap(submitted.ID).Status)
	assert.Equal(t, WithdrawalSettled, f.withdrawal(w9.ID).Status)
}

// --- F3: an execution run takes only what can be signed now, every account in turn ------

// TestIntegration_AnExecutionRunTakesWhatCanBeSignedNowEveryAccountInTurn:
// ExecutableSubjects never takes a withdrawal still cooling or a swap past its
// quote, takes at most ExecutablePerAccount subjects of one account, and
// serves every account's first subject before any account's next -- so an
// account with many waiting subjects cannot hold another's swap back.
func TestIntegration_AnExecutionRunTakesWhatCanBeSignedNowEveryAccountInTurn(t *testing.T) {
	f := newFixture(t)
	const perAccount = 3
	f.rebuild(func(d *Deps) { d.Params.ExecutablePerAccount = perAccount })
	// Far past every other test's quotes, which would otherwise share the run.
	f.advance(72 * time.Hour)

	cooling := f.withdrawalSetup(func(p *WithdrawalPolicy) { p.CoolingPeriod = 24 * time.Hour }, nil)
	due := f.withdrawalSetup(nil, nil)
	coolingW := f.mustPropose(cooling, 1, 1_000_000)
	require.Equal(t, WithdrawalApproved, coolingW.Status)
	dueW := f.mustPropose(due, 1, 1_000_000)
	require.Equal(t, WithdrawalApproved, dueW.Status)

	d := f.customer(10_000_000)
	stale := f.mustSwap(d, f.grant(d, nil).ID, 1, 1_000_000)
	f.advance(3 * time.Minute) // the stale swap's quote has expired

	a := f.customer(10_000_000)
	capA := f.grant(a, nil)
	var many []Swap
	for i := 1; i <= 5; i++ {
		many = append(many, f.mustSwap(a, capA.ID, uint64(i), 100_000))
	}
	b := f.customer(10_000_000)
	victim := f.mustSwap(b, f.grant(b, nil).ID, 1, 100_000) // admitted after all of a's

	const limit = 500
	subjects, err := f.svc.ExecutableSubjects(f.ctx, testDB, limit)
	require.NoError(t, err)
	require.Less(t, len(subjects), limit, "the run is not full, so every account's turns are in it")

	now := f.clk.Now()
	turns := map[accounts.AccountID]int{}
	lastTurn := 0
	where := map[string]int{}
	for i, s := range subjects {
		turns[s.AccountID]++
		assert.LessOrEqual(t, turns[s.AccountID], perAccount, "one account holds at most %d places", perAccount)
		assert.GreaterOrEqual(t, turns[s.AccountID], lastTurn, "every account's turn before anyone's next")
		lastTurn = turns[s.AccountID]
		where[s.ID()] = i
		switch s.Kind {
		case SubjectSwap:
			assert.True(t, s.Due.After(now), "a swap past its quote is never taken")
			assert.Contains(t, []string{string(SwapAdmitted), string(SwapApproved)}, s.Status)
		case SubjectWithdrawal:
			assert.False(t, s.Due.After(now), "a withdrawal still cooling is never taken")
			assert.Equal(t, string(WithdrawalApproved), s.Status)
		default:
			t.Fatalf("subject of kind %q", s.Kind)
		}
	}
	for i, sw := range many {
		_, taken := where[sw.ID.String()]
		assert.Equal(t, i < perAccount, taken, "a's swap %d", i+1)
	}
	require.Contains(t, where, victim.ID.String())
	assert.Less(t, where[victim.ID.String()], where[many[1].ID.String()], "b's first swap is served before a's second")
	assert.Less(t, where[many[0].ID.String()], where[many[1].ID.String()])
	assert.Contains(t, where, dueW.ID.String())
	assert.NotContains(t, where, coolingW.ID.String())
	assert.NotContains(t, where, stale.ID.String())

	// The selection decides nothing: the victim's swap executes, the cooling
	// withdrawal is untouched.
	res, err := f.svc.ExecuteSwap(f.ctx, victim.ID)
	require.NoError(t, err)
	assert.Equal(t, SwapSettled, res.Swap.Status)
	assert.Equal(t, WithdrawalApproved, f.withdrawal(coolingW.ID).Status)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM delegated_signing_decisions WHERE withdrawal_id = $1`, coolingW.ID))
}

// --- A venue's house book is its own ---------------------------------------------------

// TestIntegration_AVenueNeedsAHouseBookOfItsOwn: a venue is never registered
// on a treasury book or on another venue's house book -- the service refuses
// and so does the database (DG008), even for the migration owner -- and a
// venue's house book never becomes a treasury book.
func TestIntegration_AVenueNeedsAHouseBookOfItsOwn(t *testing.T) {
	f := newFixture(t)
	owner := ownerConn(t)
	register := func(name string, house econ.HouseAccountID) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.RegisterVenue(ctx, tx, VenueSpec{Name: name, Chain: devnethost.ChainSolana,
				Address: ukey("venue-" + name), HouseAccountID: house, SpreadBPS: 50}, operator)
			return err
		})
	}
	rawVenue := func(name string, house econ.HouseAccountID) error {
		return rawTx(f.ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(f.ctx, `INSERT INTO delegation_venues (id, name, chain_id, address, house_account_id, spread_bps,
				created_by_actor_type, created_by_actor_id) VALUES ($1, $2, $3, $4, $5, 50, 'OPERATOR', 'itest')`,
				uuid.New(), name, string(devnethost.ChainSolana), ukey("raw-venue-"+name).String(), house)
			return err
		})
	}
	treasuryBook := func(house econ.HouseAccountID, class string) error {
		return rawTx(f.ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(f.ctx, `INSERT INTO treasury_books (house_account_id, class, valuation_asset_id, registered_by_actor_type,
				registered_by_actor_id, registered_at) VALUES ($1, $2, $3, 'OPERATOR', 'itest', now())`, house, class, f.usd.ID)
			return err
		})
	}
	newHouse := func(name string, strategy econ.HouseStrategy) econ.HouseAccount {
		var h econ.HouseAccount
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			h, err = econ.NewRegistry().CreateHouseAccount(ctx, tx, name+"-"+f.tag, strategy, operator)
			return err
		}))
		return h
	}

	// A treasury book.
	treasury := newHouse("dg-treasury", econ.HouseTreasuryConversion)
	require.NoError(t, treasuryBook(treasury.ID, "TREASURY"))
	err := register("dg-tv-"+f.tag, treasury.ID)
	requireCode(t, err, errs.CodeConflict)
	requireReason(t, err, ReasonVenueHouseNotDedicated)
	requireSQLState(t, rawVenue("dg-tr-"+f.tag, treasury.ID), "DG008")

	// Another venue's house book.
	err = register("dg-sv-"+f.tag, f.house.ID)
	requireCode(t, err, errs.CodeConflict)
	requireReason(t, err, ReasonVenueHouseNotDedicated)
	requireSQLState(t, rawVenue("dg-sr-"+f.tag, f.house.ID), "DG008")

	// A venue's house book never becomes a treasury book (f.house is a
	// LIQUIDITY_PROVISION book, so a PRINCIPAL one by its strategy).
	requireSQLState(t, treasuryBook(f.house.ID, "PRINCIPAL"), "DG008")
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM treasury_books WHERE house_account_id = $1`, f.house.ID))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM delegation_venues WHERE house_account_id = $1`, treasury.ID))

	// A house book of its own is fine.
	own := newHouse("dg-own", econ.HouseLiquidityProvision)
	require.NoError(t, register("dg-own-"+f.tag, own.ID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegation_venues WHERE house_account_id = $1`, own.ID))
}
