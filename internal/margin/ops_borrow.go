package margin

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/settlement"
)

// maxPeriodsPerLot bounds the accrual work of one call per lot; a lot
// further behind catches up over several calls.
const maxPeriodsPerLot = 366

// LoanRequest borrows or repays an amount of the account's numeraire, in its
// base units.
type LoanRequest struct {
	AccountID      accounts.AccountID
	Amount         money.Quantity
	IdempotencyKey string
	Requester      Requester
	CorrelationID  string
}

func (r LoanRequest) validate() error {
	if r.AccountID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "margin: account is required")
	}
	if !r.Amount.IsPositive() {
		return errs.New(errs.CodeValidationFailed, "margin: the amount must be positive")
	}
	if err := validKey(r.IdempotencyKey); err != nil {
		return err
	}
	return r.Requester.validate()
}

// LoanResult is the outcome of a borrow or a repayment.
type LoanResult struct {
	MarginAccountID      AccountID
	IntentID             econ.IntentID
	LotID                *LotID
	JournalTransactionID ledger.TransactionID
	EffectID             econ.EffectID
	Allocations          []LoanAllocation
	Replayed             bool
	Status               AccountStatus
	Evaluation           Evaluation
}

// Borrow lends the account's numeraire from the lending pool into the
// customer's wallet (MARGIN_BORROW) and opens a borrow lot at the policy
// rate. It is the economic intent BORROW, risk-increasing: Risk Kernel V2
// refuses it unless equity after the borrow covers initial margin, leverage
// stays within the mode's cap, every mark is fresh (a stale mark blocks new
// leverage) and the pool has the cash. A CASH account never borrows.
func (s *Service) Borrow(ctx context.Context, tx pgx.Tx, r LoanRequest) (LoanResult, error) {
	if err := r.validate(); err != nil {
		return LoanResult{}, err
	}
	a, err := s.lockAccount(ctx, tx, r.AccountID)
	if err != nil {
		return LoanResult{}, err
	}
	if a.MarginMode == ModeCash {
		return LoanResult{}, errs.New(errs.CodeUnsupported, "margin: a CASH margin account does not borrow")
	}
	sp := intentSpec{
		kind: econ.KindBorrow, capability: gates.Leverage, account: a, requester: r.Requester, key: r.IdempotencyKey,
		asset: a.NumeraireAssetID, quantity: r.Amount, corr: r.CorrelationID,
		terms:   map[string]string{"margin_account_id": a.ID.String(), "lending_pool_id": a.LendingPoolID.String()},
		subject: settlement.Subject{Type: settlement.SubjectFacility, ID: a.LendingPoolID.String(), AssetID: a.NumeraireAssetID.String()},
	}
	in, existing, err := s.submitIntent(ctx, tx, sp)
	if err != nil {
		return LoanResult{}, err
	}
	key := "margin:borrow:" + in.ID.String()
	if existing {
		if err := replayIntent(in); err != nil {
			return LoanResult{}, err
		}
		e, found, err := s.d.Effects.GetByKey(ctx, tx, key)
		if err != nil || !found {
			return LoanResult{}, errs.New(errs.CodeInternal, "margin: a completed borrow has no effect")
		}
		lot, err := s.st.lotByEffect(ctx, tx, e.ID)
		if err != nil {
			return LoanResult{}, err
		}
		return LoanResult{MarginAccountID: a.ID, IntentID: in.ID, LotID: &lot.ID, JournalTransactionID: *e.JournalTransactionID,
			EffectID: e.ID, Replayed: true, Status: a.Status}, nil
	}
	res, err := s.borrow(ctx, tx, a, &in, sp, key, r)
	if err != nil {
		return LoanResult{}, s.abandon(ctx, tx, &in, err)
	}
	return res, nil
}

func (s *Service) borrow(ctx context.Context, tx pgx.Tx, a Account, in *econ.Intent, sp intentSpec, key string, r LoanRequest) (LoanResult, error) {
	if err := s.route(ctx, tx, in, sp); err != nil {
		return LoanResult{}, err
	}
	if !a.Status.AllowsNewRisk() {
		return LoanResult{}, errs.Newf(errs.CodeInvalidStateTransition, "margin: a %s account takes no new leverage", a.Status)
	}
	if err := s.authorize(ctx, tx, a.Mode, false, gates.Leverage, gates.Margin); err != nil {
		return LoanResult{}, err
	}
	pool, _, err := s.accountFacilities(ctx, tx, a)
	if err != nil {
		return LoanResult{}, err
	}
	if err := pool.RequireActive(); err != nil {
		return LoanResult{}, err
	}
	now := s.now()
	state, _, err := s.loadState(ctx, tx, a, now)
	if err != nil {
		return LoanResult{}, err
	}
	lotID := newLotID()
	after, err := WithBorrow(state, lotID.String(), r.Amount, s.policy.BorrowRateBPS)
	if err != nil {
		return LoanResult{}, err
	}
	evAfter, err := Evaluate(s.policy, after)
	if err != nil {
		return LoanResult{}, err
	}
	sections, err := RiskSectionsFor(evAfter)
	if err != nil {
		return LoanResult{}, err
	}
	poolCash, err := s.d.Ledger.Balance(ctx, tx, pool.Account(ledger.CodePoolCash, a.NumeraireAssetID))
	if err != nil {
		return LoanResult{}, err
	}
	notionalUSD, err := unitsToUSD(r.Amount, state.NumeraireDecimals, money.RoundCeil)
	if err != nil {
		return LoanResult{}, err
	}
	poolUSD, err := unitsToUSD(poolCash, state.NumeraireDecimals, money.RoundFloor)
	if err != nil {
		return LoanResult{}, err
	}
	ai := risk.AdvancedIntent{Kind: string(econ.KindBorrow), RiskIncreasing: true, NotionalUSD: notionalUSD}
	if _, err := s.decide(ctx, tx, in, a, ai, func(input *risk.AdvancedInput) {
		input.Margin, input.Oracle, input.Concentration = sections.Margin, sections.Oracle, sections.Concentration
		input.Liquidity = &risk.LiquiditySnapshot{AvailableUSD: poolUSD}
	}); err != nil {
		return LoanResult{}, err
	}
	if poolCash.Cmp(r.Amount) < 0 {
		return LoanResult{}, errs.New(errs.CodeInsufficientBuyingPower, "margin: the lending pool does not hold that much cash").
			WithField("pool_cash", poolCash.String()).WithField("requested", r.Amount.String())
	}
	if err := s.moveIntent(ctx, tx, in, econ.StatusExecuting, "borrowing", ""); err != nil {
		return LoanResult{}, err
	}
	m := s.meta("margin_borrow_lot", lotID.String(), "margin borrow")
	p, err := BorrowPosting(m, a.AccountID, pool, a.NumeraireAssetID, r.Amount)
	if err != nil {
		return LoanResult{}, err
	}
	req := map[string]string{"margin_account_id": a.ID.String(), "intent_id": in.ID.String(), "amount": r.Amount.String(),
		"asset_id": a.NumeraireAssetID.String(), "lending_pool_id": pool.ID.String()}
	iid := in.ID
	out, err := s.postOnce(ctx, tx, postSpec{key: key, kind: "BORROW", intent: &iid, account: a.AccountID, request: req, posting: p,
		evidence: map[string]any{"request": req, "evaluation_after": evAfter}, corr: r.CorrelationID})
	if err != nil {
		return LoanResult{}, err
	}
	if err := s.st.insertLot(ctx, tx, Lot{ID: lotID, MarginAccountID: a.ID, AssetID: a.NumeraireAssetID, Principal: r.Amount,
		RateBPS: s.policy.BorrowRateBPS, OpenedAt: now, BorrowJournalID: out.Journal, EffectID: out.Effect.ID}); err != nil {
		return LoanResult{}, err
	}
	if err := s.moveIntent(ctx, tx, in, econ.StatusCompleted, "borrowed", ""); err != nil {
		return LoanResult{}, err
	}
	ev, _, err := s.refreshHealth(ctx, tx, &a, "borrowed")
	if err != nil {
		return LoanResult{}, err
	}
	return LoanResult{MarginAccountID: a.ID, IntentID: in.ID, LotID: &lotID, JournalTransactionID: out.Journal, EffectID: out.Effect.ID,
		Status: a.Status, Evaluation: ev}, nil
}

// Repay repays loans from the customer's wallet (MARGIN_REPAY): interest
// first, oldest lot first. Interest is first accrued to the instant on the
// principal outstanding before the repayment, so a repayment can never
// shorten a period's charge. The wallet amount is reserved through capital.
// Repaying reduces risk: it is allowed in a margin call or a liquidation and
// never stopped by a kill switch.
func (s *Service) Repay(ctx context.Context, tx pgx.Tx, r LoanRequest) (LoanResult, error) {
	if err := r.validate(); err != nil {
		return LoanResult{}, err
	}
	a, err := s.lockAccount(ctx, tx, r.AccountID)
	if err != nil {
		return LoanResult{}, err
	}
	sp := intentSpec{
		kind: econ.KindRepay, capability: gates.Margin, account: a, requester: r.Requester, key: r.IdempotencyKey,
		asset: a.NumeraireAssetID, quantity: r.Amount, corr: r.CorrelationID,
		terms:   map[string]string{"margin_account_id": a.ID.String(), "lending_pool_id": a.LendingPoolID.String()},
		subject: settlement.Subject{Type: settlement.SubjectFacility, ID: a.LendingPoolID.String(), AssetID: a.NumeraireAssetID.String()},
	}
	in, existing, err := s.submitIntent(ctx, tx, sp)
	if err != nil {
		return LoanResult{}, err
	}
	key := "margin:repay:" + in.ID.String()
	if existing {
		if err := replayIntent(in); err != nil {
			return LoanResult{}, err
		}
		e, found, err := s.d.Effects.GetByKey(ctx, tx, key)
		if err != nil || !found {
			return LoanResult{}, errs.New(errs.CodeInternal, "margin: a completed repayment has no effect")
		}
		return LoanResult{MarginAccountID: a.ID, IntentID: in.ID, JournalTransactionID: *e.JournalTransactionID, EffectID: e.ID,
			Replayed: true, Status: a.Status}, nil
	}
	res, err := s.repay(ctx, tx, a, &in, sp, key, r)
	if err != nil {
		return LoanResult{}, s.abandon(ctx, tx, &in, err)
	}
	return res, nil
}

func (s *Service) repay(ctx context.Context, tx pgx.Tx, a Account, in *econ.Intent, sp intentSpec, key string, r LoanRequest) (LoanResult, error) {
	if err := s.route(ctx, tx, in, sp); err != nil {
		return LoanResult{}, err
	}
	if !a.Status.AllowsRiskReduction() {
		return LoanResult{}, errs.Newf(errs.CodeInvalidStateTransition, "margin: a %s account takes no repayment", a.Status)
	}
	if err := s.authorize(ctx, tx, a.Mode, true, gates.Margin); err != nil {
		return LoanResult{}, err
	}
	pool, _, err := s.accountFacilities(ctx, tx, a)
	if err != nil {
		return LoanResult{}, err
	}
	now := s.now()
	if _, err := s.accrue(ctx, tx, a, pool, now, true); err != nil {
		return LoanResult{}, err
	}
	state, lots, err := s.loadState(ctx, tx, a, now)
	if err != nil {
		return LoanResult{}, err
	}
	_, allocs, err := WithRepay(state, r.Amount)
	if err != nil {
		return LoanResult{}, err
	}
	avail, err := s.d.Capital.Availability(ctx, tx, a.AccountID.String(), a.NumeraireAssetID, s.d.Clock.Now())
	if err != nil {
		return LoanResult{}, err
	}
	availUSD, err := unitsToUSD(avail.Available, state.NumeraireDecimals, money.RoundFloor)
	if err != nil {
		return LoanResult{}, err
	}
	amountUSD, err := unitsToUSD(r.Amount, state.NumeraireDecimals, money.RoundCeil)
	if err != nil {
		return LoanResult{}, err
	}
	ai := risk.AdvancedIntent{Kind: string(econ.KindRepay), RiskIncreasing: false, NotionalUSD: amountUSD}
	if _, err := s.decide(ctx, tx, in, a, ai, func(input *risk.AdvancedInput) {
		input.Balance = &risk.BalanceSnapshot{Available: availUSD, Required: amountUSD}
	}); err != nil {
		return LoanResult{}, err
	}
	res, err := s.d.Capital.Reserve(ctx, tx, capital.ReserveRequest{
		AccountID: a.AccountID.String(), AssetID: a.NumeraireAssetID, Quantity: r.Amount,
		ActorType: r.Requester.Type, ActorID: r.Requester.ID, IdempotencyKey: key + ":reserve", TTL: reservationTTL,
		Reason: "margin repayment",
	})
	if err != nil {
		return LoanResult{}, err
	}
	if err := s.moveIntent(ctx, tx, in, econ.StatusReserved, "repayment reserved", ""); err != nil {
		return LoanResult{}, err
	}
	if err := s.moveIntent(ctx, tx, in, econ.StatusExecuting, "repaying", ""); err != nil {
		return LoanResult{}, err
	}
	m := s.meta("margin_repayment", in.ID.String(), "margin repayment")
	p, err := RepayPosting(m, a.AccountID, pool, a.NumeraireAssetID, r.Amount)
	if err != nil {
		return LoanResult{}, err
	}
	req := map[string]string{"margin_account_id": a.ID.String(), "intent_id": in.ID.String(), "amount": r.Amount.String(),
		"asset_id": a.NumeraireAssetID.String()}
	iid := in.ID
	out, err := s.postOnce(ctx, tx, postSpec{key: key, kind: "REPAY", intent: &iid, account: a.AccountID, request: req, posting: p,
		evidence: map[string]any{"request": req, "allocations": allocs}, reserve: res.ID.String(), corr: r.CorrelationID})
	if err != nil {
		return LoanResult{}, err
	}
	if _, err := s.d.Capital.ConsumeFinal(ctx, tx, res.ID, r.Amount, 0, ""); err != nil {
		return LoanResult{}, err
	}
	if err := s.applyAllocations(ctx, tx, lots, allocs, now); err != nil {
		return LoanResult{}, err
	}
	if err := s.moveIntent(ctx, tx, in, econ.StatusCompleted, "repaid", ""); err != nil {
		return LoanResult{}, err
	}
	ev, _, err := s.refreshHealth(ctx, tx, &a, "repaid")
	if err != nil {
		return LoanResult{}, err
	}
	return LoanResult{MarginAccountID: a.ID, IntentID: in.ID, JournalTransactionID: out.Journal, EffectID: out.Effect.ID,
		Allocations: allocs, Status: a.Status, Evaluation: ev}, nil
}

// applyAllocations writes a repayment's split onto the lots and closes the
// ones that owe nothing.
func (s *Service) applyAllocations(ctx context.Context, tx pgx.Tx, lots []Lot, allocs []LoanAllocation, at time.Time) error {
	byID := map[string]Lot{}
	for _, l := range lots {
		byID[l.ID.String()] = l
	}
	for _, al := range allocs {
		l, ok := byID[al.LotID]
		if !ok {
			return errs.New(errs.CodeInternal, "margin: a repayment allocation names a lot that is not open").WithField("lot_id", al.LotID)
		}
		if err := s.st.repayLot(ctx, tx, l.ID, al, CloseRepaid, at); err != nil {
			return err
		}
	}
	return nil
}

// Accrual is one interest period charged to a lot.
type Accrual struct {
	LotID                LotID
	Period               Period
	Amount               money.Quantity
	JournalTransactionID *ledger.TransactionID
}

// AccrueInterest charges every open lot of the account the interest of each
// complete UTC day since it was last accrued (MARGIN_INTEREST, capitalized
// into the loan). Each lot and period is charged exactly once: the period is
// part of the effect key, the database refuses a second accrual of the same
// period and refuses one that does not start where the last ended, so a
// replay charges nothing. A bankrupt account's loss is crystallized and
// accrues no more.
func (s *Service) AccrueInterest(ctx context.Context, tx pgx.Tx, acct accounts.AccountID) ([]Accrual, error) {
	a, err := s.lockAccount(ctx, tx, acct)
	if err != nil {
		return nil, err
	}
	if a.Status == StatusBankrupt || a.Status == StatusClosed {
		return []Accrual{}, nil
	}
	if err := s.authorize(ctx, tx, a.Mode, true, gates.Margin); err != nil {
		return nil, err
	}
	pool, _, err := s.accountFacilities(ctx, tx, a)
	if err != nil {
		return nil, err
	}
	out, err := s.accrue(ctx, tx, a, pool, s.now(), false)
	if err != nil {
		return nil, err
	}
	if _, _, err := s.refreshHealth(ctx, tx, &a, "interest accrued"); err != nil {
		return nil, err
	}
	return out, nil
}

// accrue charges the open lots of an account up to until: complete UTC days
// only, or with stub also the partial day up to the instant.
func (s *Service) accrue(ctx context.Context, tx pgx.Tx, a Account, pool econ.Facility, until time.Time, stub bool) ([]Accrual, error) {
	lots, err := s.st.openLots(ctx, tx, a.ID, true)
	if err != nil {
		return nil, err
	}
	out := []Accrual{}
	for _, l := range lots {
		for _, period := range DayPeriods(l.AccruedThrough, until, stub, maxPeriodsPerLot) {
			amount, err := InterestFor(l.OutstandingPrincipal, l.RateBPS, period.Start, period.End)
			if err != nil {
				return nil, err
			}
			acc := Accrual{LotID: l.ID, Period: period, Amount: amount}
			var journal *ledger.TransactionID
			var effect *econ.EffectID
			if amount.IsPositive() {
				key := "margin:interest:" + l.ID.String() + ":" + strconv.FormatInt(period.Start.UnixMicro(), 10)
				m := s.meta("margin_interest_accrual", l.ID.String(), "margin interest")
				m.At = period.End
				p, err := InterestPosting(m, a.AccountID, pool, l.AssetID, amount)
				if err != nil {
					return nil, err
				}
				req := map[string]string{"lot_id": l.ID.String(), "period_start": period.Start.Format(time.RFC3339Nano),
					"period_end": period.End.Format(time.RFC3339Nano), "principal": l.OutstandingPrincipal.String(),
					"rate_bps": strconv.FormatInt(int64(l.RateBPS), 10), "amount": amount.String()}
				res, err := s.postOnce(ctx, tx, postSpec{key: key, kind: "INTEREST", account: a.AccountID, request: req, posting: p, evidence: req})
				if err != nil {
					return nil, err
				}
				j, e := res.Journal, res.Effect.ID
				journal, effect = &j, &e
				acc.JournalTransactionID = journal
			}
			if err := s.st.insertAccrual(ctx, tx, l, period, amount, journal, effect); err != nil {
				return nil, err
			}
			l.AccruedThrough = period.End
			l.OutstandingInterest = l.OutstandingInterest.Add(amount)
			out = append(out, acc)
		}
	}
	return out, nil
}
