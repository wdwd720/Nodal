//go:build integration

package margin

import (
	"context"
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
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_AManualReviewCaseResumesOnlyWithTwoOperators (F1): a case
// that spent its trading budget with something left to sell stops in
// MANUAL_REVIEW, and nothing automated moves it (Liquidate, the queue,
// CoverDeficit, CloseAccount). One operator cannot move it either: the
// proposer's approval is refused. A second operator's approval returns it to
// IN_PROGRESS with a fresh budget window, the queue takes it up again, and the
// case runs to its end.
func TestIntegration_AManualReviewCaseResumesOnlyWithTwoOperators(t *testing.T) {
	f := newFixture(t)
	f.rebuild(func(cfg *Config, _ *Dependencies) { cfg.Policy.MaxLiquidationSteps = 1 })
	f.seed(usd(1_000_000), usd(100_000))
	acct, a, review := f.reviewAccount()
	cid := review.Case.ID
	assert.Equal(t, StatusLiquidating, review.AccountStatus)

	again, err := f.liquidate(acct, 0)
	require.NoError(t, err)
	assert.Equal(t, DecisionManualReview, again.Decision, "automation never moves a case a person holds")
	assert.Nil(t, again.Step)
	q, err := f.svc.ProcessLiquidationQueue(f.ctx, testDB, 100)
	require.NoError(t, err)
	for _, r := range q.Results {
		assert.NotEqual(t, a.ID, r.MarginAccountID, "the queue leaves a case in review to its person")
	}
	_, err = f.coverDeficit(cid)
	require.Error(t, err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.CloseAccount(ctx, tx, acct, f.actor)
		return err
	})
	require.Error(t, err)

	alice, bob := operator("alice"), operator("bob")
	_, err = f.propose(cid, ResolveResume, econ.Actor{Type: security.ActorService, ID: "a-service"})
	require.Error(t, err, "a resolution is an operator's")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	res, err := f.propose(cid, ResolveResume, alice)
	require.NoError(t, err)
	assert.Nil(t, res.Decision)

	_, err = f.decide(res.ID, ResolutionApproved, alice)
	require.Error(t, err, "the proposer cannot approve")
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	still, err := f.svc.st.caseByID(f.ctx, testDB, cid, false)
	require.NoError(t, err)
	assert.Equal(t, CaseManualReview, still.Status)

	out, err := f.decide(res.ID, ResolutionApproved, bob)
	require.NoError(t, err)
	assert.Equal(t, CaseInProgress, out.Case.Status)
	assert.Nil(t, out.Case.ClosedAt, "reopened for automation")
	assert.Equal(t, 1, out.Case.StepsExecuted)
	assert.Equal(t, out.Case.StepsExecuted, out.Case.BudgetFromStep, "a fresh budget window")
	assert.Equal(t, StatusLiquidating, out.AccountStatus)
	require.NotNil(t, out.Resolution.Decision)
	assert.Equal(t, ResolutionApproved, *out.Resolution.Decision)
	assert.Equal(t, bob.ID, out.Resolution.DecidedBy.ID)
	assert.Equal(t, alice.ID, out.Resolution.ProposedBy.ID)
	var actorType, actorID, resolution string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT actor_type, actor_id, resolution_id::text FROM margin_liquidation_case_transitions
		WHERE case_id = $1 AND from_status = 'MANUAL_REVIEW'`, cid).Scan(&actorType, &actorID, &resolution))
	assert.Equal(t, "OPERATOR", actorType)
	assert.Equal(t, bob.ID, actorID, "the approver writes the exit")
	assert.Equal(t, res.ID.String(), resolution, "the exit names its resolution")

	replay, err := f.decide(res.ID, ResolutionApproved, bob)
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	_, err = f.decide(res.ID, ResolutionRejected, alice)
	require.Error(t, err, "a decision is final")
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	// The queue sees the case again and the fresh window sells the rest.
	f.mark("10")
	q, err = f.svc.ProcessLiquidationQueue(f.ctx, testDB, 100)
	require.NoError(t, err)
	require.Empty(t, q.Failures)
	require.Len(t, q.Results, 1)
	require.NotNil(t, q.Results[0].Step)
	assert.Equal(t, ActionSell, q.Results[0].Step.Action)
	assert.Equal(t, 2, q.Results[0].Step.StepNo)
	requireQ(t, money.Quantity{}, f.customer(acct, ledger.CodeMarginCollateral, f.sol), "the SOL is sold")

	// The fresh window is a budget too: the database refuses a second sale in it.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO margin_liquidation_steps (id, case_id, step_no, action, asset_id, quantity, mark_mantissa,
			mark_scale, mark_observed_at, execution_mantissa, execution_scale, slippage_bps, slippage_cap_bps, proceeds, fee, repaid,
			excess, trade_journal_transaction_id, equity_before, equity_after, plan_hash, executed_at)
			VALUES ($1,$2,3,'SELL',$3,1,10,0,now(),99750,4,25,500,1,1,0,0,
			        (SELECT trade_journal_transaction_id FROM margin_liquidation_steps WHERE case_id = $2 AND step_no = 2),0,0,'h',now())`,
			uuid.New(), cid, f.sol)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MG004", db.SQLState(err), "%v", err)
	assert.Contains(t, err.Error(), "MARGIN_STEP_BUDGET")

	end, err := f.liquidate(acct, 0)
	require.NoError(t, err)
	assert.Equal(t, DecisionBankrupt, end.Decision, "nothing is left to sell, apply or sweep")
	_, err = f.coverDeficit(cid)
	require.NoError(t, err, "a resolved case reaches the insurance fund")
}

// TestIntegration_AManualReviewCaseIsDeclaredBankruptOnlyWhenNothingIsLeft
// (F1): a case in review because derivative exposure remains is declared
// BANKRUPT by two operators only once nothing is left: not while the exposure
// is open, not while the customer holds free cash the loans must take first.
// The case that still had cash is resumed instead and the sweep takes it; a
// bankruptcy proposal for a review that has ended can never apply. The case
// with nothing left is declared bankrupt with exactly the loans as its
// deficit, and covered.
func TestIntegration_AManualReviewCaseIsDeclaredBankruptOnlyWhenNothingIsLeft(t *testing.T) {
	f := newFixture(t)
	book := &switchExposure{}
	book.on.Store(true)
	f.rebuild(func(_ *Config, deps *Dependencies) { deps.Derivatives = book })
	f.seed(usd(1_000_000), usd(100_000))
	open := func() (accounts.AccountID, Account) {
		acct := f.account()
		f.fund(acct, f.sol, sol(20))
		a := f.open(acct, ModeCross)
		f.post(acct, f.sol, sol(20))
		_, err := f.borrow(acct, usd(1_000), "deriv-borrow-"+uuid.NewString())
		require.NoError(t, err)
		f.withdraw(acct, f.usd, usd(1_000))
		return acct, a
	}
	f.mark("100")
	cashy, _ := open()
	broke, brokeAcct := open()
	f.clk.Advance(time.Minute)
	f.mark("5")
	inReview := func(acct accounts.AccountID) Case {
		sold, err := f.liquidate(acct, 0)
		require.NoError(t, err)
		require.Equal(t, DecisionSellAll, sold.Decision)
		r, err := f.liquidate(acct, 0)
		require.NoError(t, err)
		require.Equal(t, DecisionManualReview, r.Decision, "%s", r.Reason)
		return *r.Case
	}
	cashyCase, brokeCase := inReview(cashy), inReview(broke)

	alice, bob := operator("alice"), operator("bob")
	declare, err := f.propose(cashyCase.ID, ResolveDeclareBankrupt, alice)
	require.NoError(t, err)
	_, err = f.decide(declare.ID, ResolutionApproved, bob)
	require.Error(t, err, "derivative exposure remains")
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	book.on.Store(false) // the derivatives lane closed the positions
	f.fund(cashy, f.usd, usd(30))
	_, err = f.decide(declare.ID, ResolutionApproved, bob)
	require.Error(t, err, "the customer's free cash repays the loans before any bankruptcy")
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	resume, err := f.propose(cashyCase.ID, ResolveResume, bob)
	require.NoError(t, err)
	_, err = f.decide(resume.ID, ResolutionApproved, alice)
	require.NoError(t, err)
	_, err = f.decide(declare.ID, ResolutionApproved, bob)
	require.Error(t, err, "a proposal for a review that has ended never applies")
	swept, err := f.liquidate(cashy, 0)
	require.NoError(t, err)
	require.Equal(t, DecisionSweepWallet, swept.Decision)
	requireQ(t, usd(30), swept.Step.Quantity, "the free cash")
	ended, err := f.liquidate(cashy, 0)
	require.NoError(t, err)
	assert.Equal(t, DecisionBankrupt, ended.Decision)

	loans := f.customer(broke, ledger.CodeMarginLoan, f.usd)
	d, err := f.propose(brokeCase.ID, ResolveDeclareBankrupt, alice)
	require.NoError(t, err)
	out, err := f.decide(d.ID, ResolutionApproved, bob)
	require.NoError(t, err)
	assert.Equal(t, CaseBankrupt, out.Case.Status)
	assert.Equal(t, StatusBankrupt, out.AccountStatus)
	require.NotNil(t, out.Case.DeficitUnits)
	requireQ(t, loans, *out.Case.DeficitUnits, "the deficit is the loans the books carry: the review charged no interest")
	for _, w := range out.Waived {
		assert.True(t, w.Waived)
		assert.True(t, w.Amount.IsZero())
	}
	cover, err := f.coverDeficit(brokeCase.ID)
	require.NoError(t, err)
	requireQ(t, loans, cover.Deficit, "covered")
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.CloseAccount(ctx, tx, broke, f.actor)
		return err
	}), "a declared bankruptcy closes like any other")
	_ = brokeAcct
}

// TestIntegration_TheDatabaseRefusesAManualReviewExitWithoutTwoOperators (F1)
// goes around Go: leaving MANUAL_REVIEW without a resolution, on a resolution
// its own proposer approved, on an approved resolution of another outcome or
// written by someone other than the approver, moving the budget window
// outside a resume, and declaring BANKRUPT while collateral remains are all
// refused by the database.
func TestIntegration_TheDatabaseRefusesAManualReviewExitWithoutTwoOperators(t *testing.T) {
	f := newFixture(t)
	f.rebuild(func(cfg *Config, _ *Dependencies) { cfg.Policy.MaxLiquidationSteps = 1 })
	f.seed(usd(1_000_000), usd(100_000))
	_, _, review := f.reviewAccount()
	cid := review.Case.ID
	var rev string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT id::text FROM margin_liquidation_case_transitions
		WHERE case_id = $1 AND to_status = 'MANUAL_REVIEW'`, cid).Scan(&rev))

	exec := func(stmts ...func(ctx context.Context, tx pgx.Tx) error) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			for _, s := range stmts {
				if err := s(ctx, tx); err != nil {
					return err
				}
			}
			return nil
		})
	}
	sql := func(q string, args ...any) func(ctx context.Context, tx pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, args...)
			return err
		}
	}
	propose := func(rid uuid.UUID, outcome, by string) func(ctx context.Context, tx pgx.Tx) error {
		return sql(`INSERT INTO margin_case_resolutions (id, case_id, review_transition_id, outcome, reason, proposed_by_actor_type,
			proposed_by_actor_id, proposed_at) VALUES ($1,$2,$3::uuid,$4,'bypass','OPERATOR',$5,now())`, rid, cid, rev, outcome, by)
	}
	approve := func(rid uuid.UUID, by string) func(ctx context.Context, tx pgx.Tx) error {
		return sql(`INSERT INTO margin_case_resolution_decisions (id, resolution_id, decision, reason, decided_by_actor_type,
			decided_by_actor_id, decided_at) VALUES ($1,$2,'APPROVED','bypass','OPERATOR',$3,now())`, uuid.New(), rid, by)
	}
	exit := func(to string, rid any, actor string) func(ctx context.Context, tx pgx.Tx) error {
		return sql(`INSERT INTO margin_liquidation_case_transitions (id, case_id, from_status, to_status, reason, actor_type, actor_id,
			occurred_at, resolution_id) VALUES ($1,$2,'MANUAL_REVIEW',$3,'bypass','OPERATOR',$4,now(),$5)`, uuid.New(), cid, to, actor, rid)
	}

	err := exec(exit("IN_PROGRESS", nil, "mallory"))
	require.Error(t, err, "leaving review is always a resolution")
	assert.Contains(t, err.Error(), "margin_liquidation_case_transitions_review_exit_resolved")

	self := uuid.New()
	err = exec(propose(self, "RESUME", "mallory"), approve(self, "mallory"))
	require.Error(t, err)
	assert.Equal(t, "MG009", db.SQLState(err), "one operator never approves their own resolution: %v", err)

	unapproved := uuid.New()
	err = exec(propose(unapproved, "RESUME", "alice"), exit("IN_PROGRESS", unapproved, "bob"))
	require.Error(t, err)
	assert.Equal(t, "MG009", db.SQLState(err), "a proposal alone licenses nothing: %v", err)

	other := uuid.New()
	err = exec(propose(other, "RESUME", "alice"), approve(other, "bob"), exit("BANKRUPT", other, "bob"))
	require.Error(t, err)
	assert.Equal(t, "MG009", db.SQLState(err), "a resume does not license a bankruptcy: %v", err)

	impostor := uuid.New()
	err = exec(propose(impostor, "RESUME", "alice"), approve(impostor, "bob"), exit("IN_PROGRESS", impostor, "carol"))
	require.Error(t, err)
	assert.Equal(t, "MG009", db.SQLState(err), "the approver writes the exit: %v", err)

	err = exec(sql(`UPDATE margin_liquidation_cases SET budget_from_step = steps_executed WHERE id = $1`, cid))
	require.Error(t, err)
	assert.Equal(t, "MG002", db.SQLState(err), "the budget window moves only when the case resumes: %v", err)

	held := uuid.New()
	err = exec(propose(held, "DECLARE_BANKRUPT", "alice"), approve(held, "bob"), exit("BANKRUPT", held, "bob"),
		sql(`UPDATE margin_liquidation_cases SET status = 'BANKRUPT', deficit_units = 1, closed_at = now() WHERE id = $1`, cid))
	require.Error(t, err)
	assert.Equal(t, "MG010", db.SQLState(err), "a case is not bankrupt while SOL is left to sell: %v", err)

	// Another case's transition: a partial liquidation that completes.
	healthy, _ := f.fixedAccountOnBooks()
	f.mark("75")
	done, err := f.liquidate(healthy, 0)
	require.NoError(t, err)
	require.Equal(t, CaseCompleted, done.Case.Status)
	elsewhere := uuid.New()
	var foreign string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT id::text FROM margin_liquidation_case_transitions WHERE case_id = $1 LIMIT 1`,
		done.Case.ID).Scan(&foreign))
	err = exec(sql(`INSERT INTO margin_case_resolutions (id, case_id, review_transition_id, outcome, reason, proposed_by_actor_type,
		proposed_by_actor_id, proposed_at) VALUES ($1,$2,$3::uuid,'RESUME','bypass','OPERATOR','alice',now())`, elsewhere, cid, foreign))
	require.Error(t, err)
	assert.Equal(t, "MG009", db.SQLState(err), "a resolution answers this case's own review: %v", err)

	c, err := f.svc.st.caseByID(f.ctx, testDB, cid, false)
	require.NoError(t, err)
	assert.Equal(t, CaseManualReview, c.Status, "nothing moved")
}

// TestIntegration_NoInterestAccruesWhileAPersonHoldsTheCase (F1): interest is
// charged up to the moment a case enters review and not after: the daily run
// charges nothing, the database refuses a charge while the case is in review
// or the account is bankrupt (MG007), and the resolution records the review
// as waived (zero accruals) so it can never be charged later. Once resumed,
// the loan accrues again from the resolution on.
func TestIntegration_NoInterestAccruesWhileAPersonHoldsTheCase(t *testing.T) {
	f := newFixture(t)
	f.rebuild(func(cfg *Config, _ *Dependencies) { cfg.Policy.MaxLiquidationSteps = 1 })
	f.seed(usd(1_000_000), usd(100_000))
	acct, a, review := f.reviewAccount()
	loan := f.customer(acct, ledger.CodeMarginLoan, f.usd)
	accrue := func() []Accrual {
		var out []Accrual
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = f.svc.AccrueInterest(ctx, tx, acct)
			return err
		}))
		return out
	}
	f.clk.Advance(48 * time.Hour)
	assert.Empty(t, accrue(), "no interest runs while a person holds the case")
	requireQ(t, loan, f.customer(acct, ledger.CodeMarginLoan, f.usd), "the loan does not grow")

	lots, err := f.svc.st.openLots(f.ctx, testDB, a.ID, false)
	require.NoError(t, err)
	require.NotEmpty(t, lots)
	l := lots[0]
	charge := func(lot Lot, mid AccountID) error {
		_, err := testDB.Exec(f.ctx, `INSERT INTO margin_interest_accruals (id, lot_id, margin_account_id, period_start, period_end,
			principal_basis, rate_bps, amount, journal_transaction_id, effect_id)
			VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,1,(SELECT borrow_journal_transaction_id FROM margin_borrow_lots WHERE id = $2),
			        (SELECT effect_id FROM margin_borrow_lots WHERE id = $2))`,
			uuid.New(), lot.ID, mid, lot.AccruedThrough, lot.AccruedThrough.Add(time.Hour), lot.OutstandingPrincipal.String(), int64(lot.RateBPS))
		return err
	}
	err = charge(l, a.ID)
	require.Error(t, err)
	assert.Equal(t, "MG007", db.SQLState(err), "the database refuses interest on a case in review: %v", err)

	reviewedFrom := l.AccruedThrough
	alice, bob := operator("alice"), operator("bob")
	res, err := f.propose(review.Case.ID, ResolveResume, alice)
	require.NoError(t, err)
	out, err := f.decide(res.ID, ResolutionApproved, bob)
	require.NoError(t, err)
	require.NotEmpty(t, out.Waived, "the review is recorded as waived")
	for _, w := range out.Waived {
		assert.True(t, w.Waived)
		assert.True(t, w.Amount.IsZero())
		assert.Nil(t, w.JournalTransactionID)
	}
	assert.Equal(t, reviewedFrom, out.Waived[0].Period.Start, "from the moment the case entered review")
	assert.Equal(t, dbTime(f.clk.Now()), out.Waived[len(out.Waived)-1].Period.End, "to the resolution")
	requireQ(t, loan, f.customer(acct, ledger.CodeMarginLoan, f.usd), "waived, not charged")

	f.clk.Advance(48 * time.Hour)
	resumed := accrue()
	require.NotEmpty(t, resumed)
	assert.Equal(t, dbTime(f.clk.Now().Add(-48*time.Hour)), resumed[0].Period.Start, "interest runs again from the resolution")
	assert.True(t, f.customer(acct, ledger.CodeMarginLoan, f.usd).Cmp(loan) > 0)

	// A bankrupt account accrues nothing either.
	f2 := newFixture(t)
	f2.seed(usd(1_000_000), usd(100_000))
	_, b, _ := f2.bankruptAccount()
	blots, err := f2.svc.st.openLots(f2.ctx, testDB, b.ID, false)
	require.NoError(t, err)
	require.NotEmpty(t, blots)
	err = charge(blots[0], b.ID)
	require.Error(t, err)
	assert.Equal(t, "MG007", db.SQLState(err), "%v", err)
}
