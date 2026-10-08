package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/transfers"
	"github.com/nodal/controlplane/scripts/internal/sandboxworld"
)

func init() {
	register("0", "Sandbox funding and a customer-to-customer transfer", scenarioTransfer)
}

// send runs one transfer in its own transaction. A refusal the service
// records (REJECTED) is committed -- that is what makes it a record -- and
// still returned as the error.
func send(ctx context.Context, w *sandboxworld.World, p security.Principal, req transfers.SendRequest) (transfers.Result, error) {
	var res transfers.Result
	var sendErr error
	err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		res, sendErr = w.Transfers.Send(ctx, tx, p, req)
		if sendErr != nil && res.Transfer.Status != transfers.StatusRejected {
			return sendErr
		}
		return nil
	})
	if err != nil {
		return transfers.Result{}, err
	}
	return res, sendErr
}

func scenarioTransfer(ctx context.Context, w *sandboxworld.World, rec *recorder) error {
	alice, err := w.Customer(ctx, "alice")
	if err != nil {
		return err
	}
	bob, err := w.Customer(ctx, "bob")
	if err != nil {
		return err
	}
	rec.step("two synthetic customers, each with a VERIFIED sandbox compliance profile",
		"alice_account", alice.Account.String(), "bob_account", bob.Account.String())

	units := func(n int64) money.Quantity { return sandboxworld.Units(n, w.USD.Decimals) }
	g, err := w.Fund(ctx, alice, w.USD.ID, units(1_000), "demo-0-fund-alice")
	if err != nil {
		return fmt.Errorf("faucet: %w", err)
	}
	rec.step("the faucet grants Alice 1,000 SBX-USD: a SANDBOX_FUNDING journal, an exactly-once effect and archived evidence",
		"grant", g.ID.String(), "journal", g.JournalTransactionID.String(), "effect", g.EffectID.String(),
		"evidence", g.EvidenceRef+" sha256="+hex.EncodeToString(g.EvidenceSHA256))

	req := transfers.SendRequest{
		SenderAccountID: alice.Account, RecipientAccountID: bob.Account, AssetID: w.USD.ID, Quantity: units(250),
		Mode: gates.ModeSandbox, Memo: "synthetic demo transfer", IdempotencyKey: "demo-0-send-1", CorrelationID: "demo-0",
	}
	res, err := send(ctx, w, alice.P, req)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	tr := res.Transfer
	rec.step("Alice sends Bob 250 SBX-USD: intent, route, capability gate, Risk Kernel V2, reservation, P2P_TRANSFER journal, effect, evidence — one transaction",
		"transfer", tr.ID.String(), "intent", tr.IntentID.String(), "risk_decision", ptrString(tr.RiskDecisionID),
		"effect", ptrString(tr.EffectID), "journal", ptrString(tr.JournalTransactionID),
		"evidence", tr.EvidenceRef+" sha256="+hex.EncodeToString(tr.EvidenceSHA256))
	rec.check("the transfer completed", tr.Status == transfers.StatusCompleted, "status %s", tr.Status)

	again, err := send(ctx, w, alice.P, req)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	rec.check("replaying the same request returns the same transfer and writes nothing",
		again.Replayed && again.Transfer.ID == tr.ID, "replayed=%t same_id=%t", again.Replayed, again.Transfer.ID == tr.ID)

	over := req
	over.Quantity, over.IdempotencyKey = units(10_000), "demo-0-send-overdraft"
	overRes, err := send(ctx, w, alice.P, over)
	rec.check("an overdraft is refused with a stable code and recorded as REJECTED",
		errs.CodeOf(err) == errs.CodeInsufficientBuyingPower && overRes.Transfer.Status == transfers.StatusRejected,
		"code %s, status %s", errs.CodeOf(err), overRes.Transfer.Status)

	agent := security.AgentPrincipal("demo-agent", alice.Account.String())
	byAgent := req
	byAgent.IdempotencyKey = "demo-0-send-agent"
	_, err = send(ctx, w, agent, byAgent)
	rec.check("an agent principal cannot move value between customers", err != nil, "refused with %s", errs.CodeOf(err))

	aliceBal, err := w.Wallet(ctx, alice, w.USD.ID)
	if err != nil {
		return err
	}
	bobBal, err := w.Wallet(ctx, bob, w.USD.ID)
	if err != nil {
		return err
	}
	rec.check("balances moved exactly once", aliceBal.Equal(units(750)) && bobBal.Equal(units(250)),
		"alice %s, bob %s SBX-USD", aliceBal.ToDecimalString(w.USD.Decimals), bobBal.ToDecimalString(w.USD.Decimals))
	rec.check("value is conserved", aliceBal.Add(bobBal).Equal(units(1_000)), "Σ wallets %s",
		aliceBal.Add(bobBal).ToDecimalString(w.USD.Decimals))

	rep, err := w.Transfers.Reconcile(ctx, w.DB, w.USD.ID)
	if err != nil {
		return err
	}
	rec.step("the transfers reconciliation runs over the database alone", "transfers_checked", fmt.Sprint(rep.Transfers))
	rec.check("reconciliation finds no discrepancy", rep.OK(), "%d discrepancies", len(rep.Discrepancies))
	return nil
}

// ptrString renders an optional identifier.
func ptrString[T fmt.Stringer](p *T) string {
	if p == nil {
		return "none"
	}
	return (*p).String()
}
