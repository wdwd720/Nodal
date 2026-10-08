package main

import (
	"context"
	"fmt"

	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/scripts/internal/sandboxworld"
)

func init() {
	register("I", "Failure: a chain submission whose outcome is unknown is reconciled, never retried; one that never lands expires and releases its funds", scenarioFailure)
}

func scenarioFailure(ctx context.Context, w *sandboxworld.World, rec *recorder) error {
	usd := func(n int64) money.Quantity { return sandboxworld.Units(n, w.USD.Decimals) }
	show := func(q money.Quantity) string { return q.ToDecimalString(w.USD.Decimals) }
	operator, err := w.OperatorActor(ctx, "omar-failure-operator")
	if err != nil {
		return err
	}
	pusd, err := platformStableAsset(ctx, w, operator)
	if err != nil {
		return fmt.Errorf("stable asset: %w", err)
	}
	if _, err := w.Mark(ctx, pusd.AssetID, "1.000000", "demo-oracle"); err != nil {
		return err
	}
	xena, err := w.Customer(ctx, "xena-failure")
	if err != nil {
		return err
	}
	if _, err := w.Fund(ctx, xena, w.USD.ID, usd(1_000), "demo-i-fund"); err != nil {
		return err
	}
	facts, err := stableFacts(ctx, w, xena)
	if err != nil {
		return err
	}
	actor := econ.Actor{Type: security.ActorUser, ID: xena.User.String()}
	asXena := security.WithPrincipal(ctx, xena.P)
	mint := func(n int64, key string) (stableasset.MintResult, error) {
		return w.Stable.Mint(asXena, stableasset.MintRequest{
			StableAssetID: pusd.ID, AccountID: xena.Account, Quantity: usd(n), ReserveAssetID: w.USD.ID,
			IdempotencyKey: key, Mode: gates.ModeSandbox, Actor: actor, Facts: facts,
		})
	}
	submissions := func(tx devnet.TxID) (int, error) {
		var n int
		err := w.DB.QueryRow(ctx, `SELECT count(*) FROM devnet_events WHERE network = $1 AND kind = 'SUBMIT' AND tx_id = $2`,
			pusd.Network, string(tx)).Scan(&n)
		return n, err
	}
	supplyBefore, err := w.Host.Supply(ctx, pusd.Chain, pusd.Mint)
	if err != nil {
		return err
	}

	// Fault one: the chain accepts the transaction, but the submission times
	// out, so the platform cannot know whether it landed.
	if err := w.Host.TimeoutButLand(ctx, pusd.Chain); err != nil {
		return err
	}
	res, mintErr := mint(100, "demo-i-mint-unknown")
	m := res.Request
	effect, err := w.Effects.Get(ctx, w.DB, m.EffectID)
	if err != nil {
		return err
	}
	rec.step("fault injected: the next submission times out although the chain accepts it; Xena mints 100 SBX-PUSD",
		"mint_request", m.ID.String(), "status", string(m.Status), "effect_status", string(effect.Status), "tx", string(m.TxID))
	rec.check("the outcome is recorded as unknown, not as success or failure",
		errs.CodeOf(mintErr) == errs.CodeSubmissionStateUnknown && m.Status == stableasset.MintSubmissionUnknown && effect.Status == econ.EffectUnknown,
		"error %v, request %s, effect %s", errs.CodeOf(mintErr), m.Status, effect.Status)
	_, blockedErr := mint(10, "demo-i-mint-blocked")
	rec.check("while a settlement is unknown, the account takes no new risk", errs.CodeOf(blockedErr) == errs.CodeRiskRejected,
		"%v", blockedErr)

	wait, err := w.Stable.ReconcileUnknown(ctx, 50)
	if err != nil {
		return fmt.Errorf("reconcile unknown: %w", err)
	}
	rec.check("reconciliation waits while the transaction is in the mempool", wait.Waiting == 1, "%+v", wait)
	if _, err := w.Host.ProduceBlocks(ctx, pusd.Chain, 1+devnet.DefaultFinalityDepth); err != nil {
		return err
	}
	done, err := w.Stable.ReconcileUnknown(ctx, 50)
	if err != nil {
		return fmt.Errorf("reconcile unknown: %w", err)
	}
	m, err = w.Stable.GetMint(ctx, w.DB, m.ID)
	if err != nil {
		return err
	}
	in, err := w.Intents.Get(ctx, w.DB, m.IntentID)
	if err != nil {
		return err
	}
	n, err := submissions(m.TxID)
	if err != nil {
		return err
	}
	rec.step("blocks are produced, the transaction finalizes, and reconciliation observes it on chain",
		"issued", fmt.Sprint(done.Issued), "status", string(m.Status), "intent", string(in.Status))
	rec.check("the unknown mint is issued by observation, exactly once, and its intent marked RECONCILED",
		done.Issued == 1 && m.Status == stableasset.MintIssued && in.Status == econ.StatusReconciled && n == 1,
		"issued %d, status %s, intent %s, submissions %d", done.Issued, m.Status, in.Status, n)

	// Fault two: the submission times out and the chain drops it. NOT_FOUND
	// proves nothing while the blockhash could still land, so reconciliation
	// waits for the blockhash to expire below finality before failing it.
	if err := w.Host.TimeoutAndDrop(ctx, pusd.Chain); err != nil {
		return err
	}
	available := func() (money.Quantity, error) {
		av, err := w.Capital.Availability(ctx, w.DB, xena.Account.String(), w.USD.ID, w.Clock.Now())
		return av.Available, err
	}
	before, err := available()
	if err != nil {
		return err
	}
	res, mintErr = mint(50, "demo-i-mint-dropped")
	dropped := res.Request
	held, err := available()
	if err != nil {
		return err
	}
	rec.step("fault injected: the next submission times out and is dropped; Xena mints 50 SBX-PUSD",
		"mint_request", dropped.ID.String(), "status", string(dropped.Status), "reserved_while_unknown", show(before.Sub(held)))
	rec.check("the reserve payment stays reserved while the outcome is unknown",
		errs.CodeOf(mintErr) == errs.CodeSubmissionStateUnknown && before.Sub(held).Equal(usd(50)),
		"error %v, reserved %s", errs.CodeOf(mintErr), show(before.Sub(held)))
	wait, err = w.Stable.ReconcileUnknown(ctx, 50)
	if err != nil {
		return err
	}
	rec.check("not found alone proves nothing: reconciliation waits while the blockhash can still land", wait.Waiting == 1, "%+v", wait)
	if _, err := w.Host.ProduceBlocks(ctx, pusd.Chain, devnet.DefaultBlockhashWindow+devnet.DefaultFinalityDepth+1); err != nil {
		return err
	}
	done, err = w.Stable.ReconcileUnknown(ctx, 50)
	if err != nil {
		return err
	}
	dropped, err = w.Stable.GetMint(ctx, w.DB, dropped.ID)
	if err != nil {
		return err
	}
	after, err := available()
	if err != nil {
		return err
	}
	n, err = submissions(dropped.TxID)
	if err != nil {
		return err
	}
	rec.step("the blockhash expires below finality; reconciliation fails the request and releases the payment",
		"failed", fmt.Sprint(done.Failed), "failure_code", dropped.FailureCode)
	rec.check("the dropped mint fails as SUBMISSION_EXPIRED, was never resubmitted, and its funds are released",
		done.Failed == 1 && dropped.Status == stableasset.MintFailed && dropped.FailureCode == "SUBMISSION_EXPIRED" && n == 1 && after.Equal(before),
		"failed %d, status %s %s, submissions %d, available %s (was %s)", done.Failed, dropped.Status, dropped.FailureCode, n, show(after), show(before))

	supplyAfter, err := w.Host.Supply(ctx, pusd.Chain, pusd.Mint)
	if err != nil {
		return err
	}
	rec.check("on chain, exactly the one mint that landed added supply", supplyAfter-supplyBefore == 100_000_000,
		"supply grew by %d base units", supplyAfter-supplyBefore)
	return w.Host.ClearFaults(ctx, pusd.Chain)
}
