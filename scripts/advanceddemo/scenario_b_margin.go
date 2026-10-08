package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/scripts/internal/sandboxworld"
)

func init() {
	register("B", "Leveraged trade: collateral, a paper leveraged buy, a falling mark, liquidation, and books that reconcile", scenarioMargin)
}

func scenarioMargin(ctx context.Context, w *sandboxworld.World, rec *recorder) error {
	units := func(n int64) money.Quantity { return sandboxworld.Units(n, w.USD.Decimals) }
	sol, err := w.SimAsset(ctx, "SBX-SOL", "Sandbox SOL (simulated)", 9, assets.RiskMajor, false)
	if err != nil {
		return err
	}
	var facs margin.Facilities
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		facs, err = w.Margin.SeedFacilities(ctx, tx, margin.SeedRequest{Asset: w.USD.ID, PoolAmount: units(1_000_000),
			FundAmount: units(100_000), Key: "demo-b-seed", Actor: w.System})
		return err
	}); err != nil {
		return fmt.Errorf("seed facilities: %w", err)
	}
	rec.step("the sandbox lending pool (1,000,000 SBX-USD) and insurance fund (100,000) are funded",
		"lending_pool", facs.Pool.ID.String(), "insurance_fund", facs.Fund.ID.String())

	carol, err := w.Customer(ctx, "carol-margin")
	if err != nil {
		return err
	}
	if _, err := w.Fund(ctx, carol, w.USD.ID, units(1_000), "demo-b-fund"); err != nil {
		return err
	}
	req := margin.Requester{Type: security.ActorUser, ID: carol.User.String()}
	var acct margin.Account
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		acct, err = w.Margin.OpenAccount(ctx, tx, margin.OpenRequest{AccountID: carol.Account, MarginMode: margin.ModeCross,
			Mode: gates.ModePaper, NumeraireAssetID: w.USD.ID, Actor: econ.Actor{Type: security.ActorUser, ID: carol.User.String()}})
		return err
	}); err != nil {
		return fmt.Errorf("open margin account: %w", err)
	}
	if _, err := w.Mark(ctx, sol.ID, "100", "demo-oracle"); err != nil {
		return err
	}
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.Margin.PostCollateral(ctx, tx, margin.CollateralRequest{AccountID: carol.Account, AssetID: w.USD.ID,
			Quantity: units(1_000), IdempotencyKey: "demo-b-post", Requester: req})
		return err
	}); err != nil {
		return fmt.Errorf("post collateral: %w", err)
	}
	rec.step("Carol opens a CROSS paper margin account and posts 1,000 SBX-USD as collateral; SBX-SOL marks at 100",
		"margin_account", acct.ID.String())

	var buy margin.SpotResult
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		buy, err = w.Margin.OpenLeveragedSpot(ctx, tx, margin.SpotRequest{AccountID: carol.Account, AssetID: sol.ID,
			Quantity: sandboxworld.Units(20, sol.Decimals), IdempotencyKey: "demo-b-buy", Requester: req})
		return err
	}); err != nil {
		return fmt.Errorf("leveraged buy: %w", err)
	}
	rec.step("a paper leveraged buy of 20 SBX-SOL, the whole cost borrowed from the pool at the disclosed slippage",
		"cost", buy.Cost.ToDecimalString(w.USD.Decimals)+" SBX-USD", "disclosed_slippage", buy.SlippageBPS.String(),
		"borrow_journal", buy.BorrowJournal.String(), "trade_journal", buy.TradeJournal.String(),
		"equity", buy.Evaluation.Equity.ToDecimalString(w.USD.Decimals), "maintenance", buy.Evaluation.MaintenanceMargin.ToDecimalString(w.USD.Decimals))
	rec.check("the position opened in good standing", buy.Status == margin.StatusNormal, "status %s", buy.Status)

	// The mark falls in steps; each evaluation is recorded.
	var statuses []string
	called := false
	for _, mark := range []string{"90", "80", "72", "64"} {
		w.Clock.Advance(time.Minute)
		if _, err := w.Mark(ctx, sol.ID, mark, "demo-oracle"); err != nil {
			return err
		}
		var ev margin.EvaluateResult
		if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			ev, err = w.Margin.Evaluate(ctx, tx, carol.Account)
			return err
		}); err != nil {
			return fmt.Errorf("evaluate at %s: %w", mark, err)
		}
		statuses = append(statuses, fmt.Sprintf("%s→%s (equity %s, maintenance %s)", mark, ev.Status,
			ev.Evaluation.Equity.ToDecimalString(w.USD.Decimals), ev.Evaluation.MaintenanceMargin.ToDecimalString(w.USD.Decimals)))
		if ev.Status == margin.StatusMarginCall {
			called = true
		}
	}
	rec.step("the mark falls 100 → 90 → 80 → 72 → 64; the engine re-evaluates the account each time", "evaluations", strings.Join(statuses, "; "))
	rec.check("a falling mark shrinks margin until a margin call opens", called, "%s", strings.Join(statuses, "; "))

	// A crash before anything is sold, then liquidation until the case closes.
	w.Clock.Advance(time.Minute)
	if _, err := w.Mark(ctx, sol.ID, "40", "demo-oracle"); err != nil {
		return err
	}
	var steps []string
	var last margin.LiquidationResult
	for i := 0; i < 10; i++ {
		var r margin.LiquidationResult
		if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			r, err = w.Margin.Liquidate(ctx, tx, margin.LiquidateRequest{AccountID: carol.Account})
			return err
		}); err != nil {
			return fmt.Errorf("liquidate: %w", err)
		}
		last = r
		desc := string(r.Decision)
		if r.Step != nil {
			dec, sym := w.USD.Decimals, "SBX-USD"
			if r.Step.AssetID == sol.ID {
				dec, sym = sol.Decimals, "SBX-SOL"
			}
			desc += fmt.Sprintf(" (%s %s, proceeds %s, fee %s, slippage %d of %d bps)", r.Step.Quantity.ToDecimalString(dec), sym,
				r.Step.Proceeds.ToDecimalString(w.USD.Decimals), r.Step.Fee.ToDecimalString(w.USD.Decimals),
				int64(r.Step.SlippageBPS), int64(r.Step.SlippageCapBPS))
		}
		steps = append(steps, desc)
		if r.Case == nil || r.Case.Status != margin.CaseInProgress {
			break
		}
	}
	rec.step("the mark crashes to 40 and liquidation runs step by step, each step capped in slippage and recorded",
		"steps", strings.Join(steps, "; "), "account_status", string(last.AccountStatus))
	rec.check("liquidation reached a terminal case", last.Case != nil && last.Case.Status != margin.CaseInProgress,
		"case %v", caseStatus(last.Case))

	if last.Case != nil && last.Case.Status == margin.CaseBankrupt {
		var cover, again margin.DeficitResult
		for i, out := range []*margin.DeficitResult{&cover, &again} {
			if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				*out, err = w.Margin.CoverDeficit(ctx, tx, last.Case.ID)
				return err
			}); err != nil {
				return fmt.Errorf("cover deficit (call %d): %w", i+1, err)
			}
		}
		rec.step("the account ends bankrupt and the insurance fund covers the deficit, which the customer now owes the fund",
			"deficit", cover.Deficit.ToDecimalString(w.USD.Decimals)+" SBX-USD", "journal", cover.JournalTransactionID.String())
		rec.check("a deficit is covered exactly once", again.Replayed && again.JournalTransactionID == cover.JournalTransactionID,
			"second call replayed=%t", again.Replayed)
		loan, err := w.Balance(ctx, ledger.CustomerAccount(carol.Account, ledger.CodeMarginLoan, w.USD.ID))
		if err != nil {
			return err
		}
		claim, err := w.Balance(ctx, facs.Fund.Account(ledger.CodeDeficitReceivable, w.USD.ID))
		if err != nil {
			return err
		}
		rec.check("the loan is closed and the fund's claim equals the deficit", loan.IsZero() && claim.Equal(cover.Deficit),
			"loan %s, fund claim %s", loan.ToDecimalString(w.USD.Decimals), claim.ToDecimalString(w.USD.Decimals))
	}

	receivable, err := w.Balance(ctx, facs.Pool.Account(ledger.CodeLoanReceivable, w.USD.ID))
	if err != nil {
		return err
	}
	interest, err := w.Balance(ctx, facs.Pool.Account(ledger.CodeInterestIncome, w.USD.ID))
	if err != nil {
		return err
	}
	poolCash, err := w.Balance(ctx, facs.Pool.Account(ledger.CodePoolCash, w.USD.ID))
	if err != nil {
		return err
	}
	rec.check("the lending pool is whole: no receivable left, cash = capital + interest earned",
		receivable.IsZero() && poolCash.Equal(units(1_000_000).Add(interest)),
		"receivable %s, cash %s, interest %s", receivable.ToDecimalString(w.USD.Decimals),
		poolCash.ToDecimalString(w.USD.Decimals), interest.ToDecimalString(w.USD.Decimals))

	var notApplied, effects int
	if err := w.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status <> 'APPLIED' OR journal_transaction_id IS NULL
		OR evidence_ref IS NULL OR evidence_sha256 IS NULL), count(*) FROM economic_effects WHERE subsystem = 'MARGIN' AND account_id = $1`,
		carol.Account).Scan(&notApplied, &effects); err != nil {
		return err
	}
	rec.check("every margin effect is APPLIED, backed by its journal and by archived evidence", notApplied == 0 && effects > 0,
		"%d effects, %d incomplete", effects, notApplied)
	return nil
}

func caseStatus(c *margin.Case) string {
	if c == nil {
		return "none"
	}
	return string(c.Status)
}
