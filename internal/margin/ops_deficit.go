package margin

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/settlement"
)

// DeficitRepaymentRequest repays part or all of the covered deficit of an
// account's bankrupt margin case to the insurance fund that covered it, in
// base units of the account's numeraire.
type DeficitRepaymentRequest struct {
	AccountID      accounts.AccountID
	Amount         money.Quantity
	IdempotencyKey string
	Requester      Requester
	CorrelationID  string
}

func (r DeficitRepaymentRequest) validate() error {
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

// DeficitRepaymentResult is the outcome of a deficit repayment.
type DeficitRepaymentResult struct {
	MarginAccountID      AccountID
	CaseID               CaseID
	RepaymentID          RepaymentID
	IntentID             econ.IntentID
	JournalTransactionID ledger.TransactionID
	EffectID             econ.EffectID
	Repaid               money.Quantity
	// Outstanding is what the case still owes the fund after this
	// repayment.
	Outstanding money.Quantity
	Replayed    bool
}

// RepayDeficit pays a covered deficit back to the insurance fund
// (DeficitRepaymentPosting: C: Dr DEFICIT / Cr WALLET; F(insurance): Dr
// INSURANCE_FUND_CASH / Cr DEFICIT_RECEIVABLE). It is the economic intent
// REPAY: routed, authorized, decided by Risk Kernel V2 (balance), reserved
// through capital and posted as one exactly-once effect per intent. The
// customer account is RESTRICTED once the deficit is covered, which still
// lets it reduce what it owes: a repayment is not new risk.
//
// A case's deficit is repaid at most once in total: the repayments never
// exceed it, in Go and in the database (margin_deficit_repayments, MG011),
// and the ledger's DEFICIT and DEFICIT_RECEIVABLE cannot go below zero
// besides.
func (s *Service) RepayDeficit(ctx context.Context, tx pgx.Tx, r DeficitRepaymentRequest) (DeficitRepaymentResult, error) {
	if err := r.validate(); err != nil {
		return DeficitRepaymentResult{}, err
	}
	a, err := s.lockAccount(ctx, tx, r.AccountID)
	if err != nil {
		return DeficitRepaymentResult{}, err
	}
	c, found, err := s.st.latestCase(ctx, tx, a.ID, true)
	if err != nil {
		return DeficitRepaymentResult{}, err
	}
	if !found || c.Status != CaseBankrupt || c.DeficitUnits == nil || c.CoverageJournalID == nil {
		return DeficitRepaymentResult{}, errs.New(errs.CodeInvalidStateTransition, "margin: the account has no covered deficit to repay")
	}
	sp := intentSpec{
		kind: econ.KindRepay, capability: gates.Margin, account: a, requester: r.Requester, key: r.IdempotencyKey,
		asset: a.NumeraireAssetID, quantity: r.Amount, corr: r.CorrelationID,
		terms: map[string]string{"margin_account_id": a.ID.String(), "case_id": c.ID.String(),
			"insurance_fund_id": a.InsuranceFundID.String(), "debt": "DEFICIT"},
		subject: settlement.Subject{Type: settlement.SubjectFacility, ID: a.InsuranceFundID.String(), AssetID: a.NumeraireAssetID.String()},
	}
	in, existing, err := s.submitIntent(ctx, tx, sp)
	if err != nil {
		return DeficitRepaymentResult{}, err
	}
	key := "margin:deficit-repay:" + in.ID.String()
	if existing {
		if err := replayIntent(in); err != nil {
			return DeficitRepaymentResult{}, err
		}
		e, found, err := s.d.Effects.GetByKey(ctx, tx, key)
		if err != nil || !found {
			return DeficitRepaymentResult{}, errs.New(errs.CodeInternal, "margin: a completed deficit repayment has no effect")
		}
		rep, err := s.st.repaymentByEffect(ctx, tx, e.ID)
		if err != nil {
			return DeficitRepaymentResult{}, err
		}
		repaid, err := s.st.repaidDeficit(ctx, tx, c.ID)
		if err != nil {
			return DeficitRepaymentResult{}, err
		}
		return DeficitRepaymentResult{MarginAccountID: a.ID, CaseID: c.ID, RepaymentID: rep.id, IntentID: in.ID,
			JournalTransactionID: *e.JournalTransactionID, EffectID: e.ID, Repaid: rep.amount,
			Outstanding: c.DeficitUnits.Sub(repaid), Replayed: true}, nil
	}
	res, err := s.repayDeficit(ctx, tx, a, c, &in, sp, key, r)
	if err != nil {
		return DeficitRepaymentResult{}, s.abandon(ctx, tx, &in, err)
	}
	return res, nil
}

func (s *Service) repayDeficit(ctx context.Context, tx pgx.Tx, a Account, c Case, in *econ.Intent, sp intentSpec, key string,
	r DeficitRepaymentRequest) (DeficitRepaymentResult, error) {
	if err := s.route(ctx, tx, in, sp); err != nil {
		return DeficitRepaymentResult{}, err
	}
	if err := s.authorize(ctx, tx, a.Mode, true, gates.Margin); err != nil {
		return DeficitRepaymentResult{}, err
	}
	_, fund, err := s.accountFacilities(ctx, tx, a)
	if err != nil {
		return DeficitRepaymentResult{}, err
	}
	if err := s.lockFacilities(ctx, tx, a, fundBooks); err != nil {
		return DeficitRepaymentResult{}, err
	}
	repaid, err := s.st.repaidDeficit(ctx, tx, c.ID)
	if err != nil {
		return DeficitRepaymentResult{}, err
	}
	outstanding := c.DeficitUnits.Sub(repaid)
	if r.Amount.Cmp(outstanding) > 0 {
		return DeficitRepaymentResult{}, errs.New(errs.CodeValidationFailed, "margin: the repayment exceeds the deficit still owed").
			WithField("outstanding", outstanding.String()).WithField("repayment", r.Amount.String())
	}
	num, err := s.assets.Get(ctx, tx, a.NumeraireAssetID)
	if err != nil {
		return DeficitRepaymentResult{}, err
	}
	avail, err := s.d.Capital.Availability(ctx, tx, a.AccountID.String(), a.NumeraireAssetID, s.d.Clock.Now())
	if err != nil {
		return DeficitRepaymentResult{}, err
	}
	// Both sides round down, so repaying exactly what is available is never
	// refused by a rounding; the quantity itself is enforced exactly by the
	// capital reservation below.
	availUSD, err := unitsToUSD(avail.Available, num.Decimals, money.RoundFloor)
	if err != nil {
		return DeficitRepaymentResult{}, err
	}
	amountUSD, err := unitsToUSD(r.Amount, num.Decimals, money.RoundFloor)
	if err != nil {
		return DeficitRepaymentResult{}, err
	}
	ai := risk.AdvancedIntent{Kind: string(econ.KindRepay), RiskIncreasing: false, NotionalUSD: amountUSD}
	if _, err := s.decide(ctx, tx, in, a, ai, func(input *risk.AdvancedInput) {
		input.Balance = &risk.BalanceSnapshot{Available: availUSD, Required: amountUSD}
	}); err != nil {
		return DeficitRepaymentResult{}, err
	}
	res, err := s.d.Capital.Reserve(ctx, tx, capital.ReserveRequest{
		AccountID: a.AccountID.String(), AssetID: a.NumeraireAssetID, Quantity: r.Amount, EconomicIntentID: in.ID.String(),
		ActorType: r.Requester.Type, ActorID: r.Requester.ID, IdempotencyKey: key + ":reserve", TTL: reservationTTL,
		Reason: "margin deficit repayment",
	})
	if err != nil {
		return DeficitRepaymentResult{}, err
	}
	if err := s.moveIntent(ctx, tx, in, econ.StatusReserved, "deficit repayment reserved", ""); err != nil {
		return DeficitRepaymentResult{}, err
	}
	if err := s.moveIntent(ctx, tx, in, econ.StatusExecuting, "repaying the deficit", ""); err != nil {
		return DeficitRepaymentResult{}, err
	}
	now := s.now()
	rid := newRepaymentID()
	m := s.meta(refDeficitRepayment, rid.String(), "covered margin deficit repaid to the insurance fund")
	p, err := DeficitRepaymentPosting(m, a.AccountID, fund, a.NumeraireAssetID, r.Amount)
	if err != nil {
		return DeficitRepaymentResult{}, err
	}
	req := map[string]string{"margin_account_id": a.ID.String(), "case_id": c.ID.String(), "intent_id": in.ID.String(),
		"amount": r.Amount.String(), "asset_id": a.NumeraireAssetID.String(), "insurance_fund_id": fund.ID.String()}
	iid := in.ID
	out, err := s.postOnce(ctx, tx, postSpec{key: key, kind: "DEFICIT_REPAY", intent: &iid, account: a.AccountID, request: req,
		posting: p, evidence: map[string]any{"request": req, "deficit": c.DeficitUnits.String(), "repaid_before": repaid.String()},
		reserve: res.ID.String(), corr: r.CorrelationID})
	if err != nil {
		return DeficitRepaymentResult{}, err
	}
	if _, err := s.d.Capital.ConsumeFinal(ctx, tx, res.ID, r.Amount, 0, ""); err != nil {
		return DeficitRepaymentResult{}, err
	}
	if err := s.st.insertRepayment(ctx, tx, repayment{id: rid, caseID: c.ID, marginAccountID: a.ID, amount: r.Amount,
		journal: out.Journal, effect: out.Effect.ID, intent: iid, at: now}); err != nil {
		return DeficitRepaymentResult{}, err
	}
	if err := s.moveIntent(ctx, tx, in, econ.StatusCompleted, "deficit repaid", ""); err != nil {
		return DeficitRepaymentResult{}, err
	}
	return DeficitRepaymentResult{MarginAccountID: a.ID, CaseID: c.ID, RepaymentID: rid, IntentID: iid, JournalTransactionID: out.Journal,
		EffectID: out.Effect.ID, Repaid: r.Amount, Outstanding: outstanding.Sub(r.Amount)}, nil
}

// --- SQL ---------------------------------------------------------------------

// repayment is a margin_deficit_repayments row.
type repayment struct {
	id              RepaymentID
	caseID          CaseID
	marginAccountID AccountID
	amount          money.Quantity
	journal         ledger.TransactionID
	effect          econ.EffectID
	intent          econ.IntentID
	at              time.Time
}

func (st *store) insertRepayment(ctx context.Context, tx pgx.Tx, r repayment) error {
	_, err := tx.Exec(ctx, `INSERT INTO margin_deficit_repayments (id, case_id, margin_account_id, amount, journal_transaction_id,
		effect_id, intent_id, repaid_at) VALUES ($1,$2,$3,$4::numeric,$5,$6,$7,$8)`,
		r.id, r.caseID, r.marginAccountID, r.amount.String(), r.journal, r.effect, r.intent, r.at)
	return dbErr("record deficit repayment", err)
}

// repaidDeficit is what has been repaid of a case's covered deficit.
func (st *store) repaidDeficit(ctx context.Context, q db.Querier, cid CaseID) (money.Quantity, error) {
	var s string
	if err := q.QueryRow(ctx, `SELECT coalesce(sum(amount), 0)::text FROM margin_deficit_repayments WHERE case_id = $1`, cid).Scan(&s); err != nil {
		return money.Quantity{}, dbErr("sum deficit repayments", err)
	}
	q2, err := money.ScanQuantity(s)
	if err != nil {
		return money.Quantity{}, dbErr("scan deficit repayments", err)
	}
	return q2, nil
}

func (st *store) repaymentByEffect(ctx context.Context, q db.Querier, eid econ.EffectID) (repayment, error) {
	var r repayment
	var amount string
	err := q.QueryRow(ctx, `SELECT id, case_id, margin_account_id, amount::text, journal_transaction_id, effect_id, intent_id, repaid_at
		FROM margin_deficit_repayments WHERE effect_id = $1`, eid).
		Scan(&r.id, &r.caseID, &r.marginAccountID, &amount, &r.journal, &r.effect, &r.intent, &r.at)
	if errors.Is(err, pgx.ErrNoRows) {
		return repayment{}, errs.New(errs.CodeNotFound, "deficit repayment not found").WithField("effect_id", eid.String())
	}
	if err != nil {
		return repayment{}, dbErr("load deficit repayment", err)
	}
	if r.amount, err = money.ScanQuantity(amount); err != nil {
		return repayment{}, dbErr("scan deficit repayment", err)
	}
	r.at = r.at.UTC()
	return r, nil
}
