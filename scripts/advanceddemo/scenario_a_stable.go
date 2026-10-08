package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/scripts/internal/sandboxworld"
)

func init() {
	register("A", "Stable asset: a fully reserved sandbox stable asset minted on chain finality, signed reserve snapshots, exact redemption, and a peg breaker", scenarioStable)
}

// stableFacts are the identity facts of a customer as a deployment resolves
// them, never as a request states them: the production identity reader (a
// verified email address, D-055) and the verification level that implies.
func stableFacts(ctx context.Context, w *sandboxworld.World, c sandboxworld.Customer) (stableasset.AccountFacts, error) {
	verified, err := w.Identity.IdentityVerified(ctx, w.DB, c.Account)
	if err != nil {
		return stableasset.AccountFacts{}, err
	}
	return stableasset.AccountFacts{Verification: valuedomain.VerificationNodalIdentity, IdentityVerified: verified}, nil
}

// platformStableAsset registers the demo's platform stable asset: SBX-PUSD,
// pegged at 1.000000 SBX-USD and fully reserved in the sandbox dollar itself.
// Registering the identical definition again returns the existing asset, so
// every scenario that needs it calls this.
func platformStableAsset(ctx context.Context, w *sandboxworld.World, operator econ.Actor) (stableasset.PlatformStableAsset, error) {
	return w.Stable.Register(ctx, stableasset.Definition{
		Name: "platform-sandbox-dollar", Symbol: "SBX-PUSD", PegCurrency: "USD", Decimals: 6,
		IssuanceCap: sandboxworld.Units(1_000_000, 6), RedemptionMode: stableasset.RedemptionInstant,
		PegQuoteAssetID: w.USD.ID, PegMantissa: money.QuantityFromInt64(1_000_000), PegScale: 6,
		OracleSource: "demo-oracle", MaxPriceAge: time.Hour, PegDeviationThreshold: 50, Attestable: true,
		Reserves: []stableasset.ReserveSpec{
			{AssetID: w.USD.ID, Kind: stableasset.ReserveCashEquivalent, FactorBPS: 10_000, AcceptedForMint: true},
		},
	}, operator)
}

func scenarioStable(ctx context.Context, w *sandboxworld.World, rec *recorder) error {
	usd := func(n int64) money.Quantity { return sandboxworld.Units(n, w.USD.Decimals) }
	show := func(q money.Quantity) string { return q.ToDecimalString(w.USD.Decimals) }
	operator, err := w.OperatorActor(ctx, "una-stable-operator")
	if err != nil {
		return err
	}
	a, err := platformStableAsset(ctx, w, operator)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	if _, err := w.Mark(ctx, a.AssetID, "1.000000", "demo-oracle"); err != nil {
		return err
	}
	rec.step("an operator registers SBX-PUSD: a SIMULATED stable asset on sbx-solana, pegged at 1.000000 SBX-USD, fully reserved in SBX-USD, mint authority held only by the platform issuer signer",
		"stable_asset", a.ID.String(), "asset", a.AssetID.String(), "chain", string(a.Chain), "mint", a.Mint.String(),
		"mint_authority", a.MintAuthority.String(), "facility", a.FacilityID.String())

	vera, err := w.Customer(ctx, "vera-stable")
	if err != nil {
		return err
	}
	if _, err := w.Fund(ctx, vera, w.USD.ID, usd(1_000), "demo-a-fund"); err != nil {
		return err
	}
	facts, err := stableFacts(ctx, w, vera)
	if err != nil {
		return err
	}
	actor := econ.Actor{Type: security.ActorUser, ID: vera.User.String()}
	asVera := security.WithPrincipal(ctx, vera.P)

	minted, err := w.Stable.Mint(asVera, stableasset.MintRequest{
		StableAssetID: a.ID, AccountID: vera.Account, Quantity: usd(600), ReserveAssetID: w.USD.ID,
		IdempotencyKey: "demo-a-mint", Mode: gates.ModeSandbox, Actor: actor, Facts: facts,
	})
	if err != nil {
		return fmt.Errorf("mint: %w", err)
	}
	m := minted.Request
	rec.step("Vera deposits 600 SBX-USD and mints 600 SBX-PUSD: admission against the reserve, Risk Kernel V2, a hashed mint plan inspected and signed by the issuer, one submission, and the STABLE_ISSUANCE posting only after finality",
		"mint_request", m.ID.String(), "intent", m.IntentID.String(), "effect", m.EffectID.String(), "tx", string(m.TxID),
		"plan_hash", fmt.Sprintf("%x", m.PlanHash), "journal", ptrString(m.JournalTransactionID))
	rec.check("the mint is ISSUED", m.Status == stableasset.MintIssued, "status %s %s", m.Status, m.FailureCode)
	_, inspectErr := stableasset.InspectMintTransaction(m.Plan, m.SignedTx)
	rec.check("the stored transaction is exactly its plan", inspectErr == nil, "%v", inspectErr)
	var submits int
	if err := w.DB.QueryRow(ctx, `SELECT count(*) FROM devnet_events WHERE network = $1 AND kind = 'SUBMIT' AND tx_id = $2`,
		a.Network, string(m.TxID)).Scan(&submits); err != nil {
		return err
	}
	rec.check("the transaction was submitted exactly once", submits == 1, "%d submissions", submits)

	holder := w.Custody.HolderAddress(vera.Account)
	chainBal, err := w.Host.TokenBalance(ctx, a.Chain, holder, a.Mint)
	if err != nil {
		return err
	}
	supply, err := w.Host.Supply(ctx, a.Chain, a.Mint)
	if err != nil {
		return err
	}
	reserve := ledger.AccountRef{OwnerType: ledger.OwnerFacility, OwnerID: a.FacilityID.String(), Code: ledger.CodeStableReserve, AssetID: w.USD.ID}
	reserveBal, err := w.Balance(ctx, reserve)
	if err != nil {
		return err
	}
	cash, _ := w.Wallet(ctx, vera, w.USD.ID)
	held, _ := w.Wallet(ctx, vera, a.AssetID)
	rec.check("ledger, wallet, reserve and chain agree: 600 issued against 600 in reserve",
		held.Equal(usd(600)) && cash.Equal(usd(400)) && reserveBal.Equal(usd(600)) && chainBal == 600_000_000 && supply == 600_000_000,
		"wallet %s SBX-PUSD and %s SBX-USD, reserve %s, chain balance %d, chain supply %d", show(held), show(cash), show(reserveBal), chainBal, supply)

	snap := func() (stableasset.SignedSnapshot, error) {
		var sn stableasset.SignedSnapshot
		err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			sn, err = w.Stable.TakeSnapshot(ctx, tx, a.ID)
			return err
		})
		return sn, err
	}
	first, err := snap()
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	rec.step("a signed reserve snapshot is published: ledger supply, chain supply, reserve composition and coverage, hash-linked to the previous one",
		"seq", first.Snapshot.Seq, "hash", fmt.Sprintf("%x", first.Hash), "coverage_bps", first.Snapshot.CoverageBPS,
		"evidence", first.EvidenceRef)
	rec.check("the snapshot states 600 on both the ledger and the chain at full coverage",
		first.Snapshot.LedgerSupply == "600000000" && first.Snapshot.ChainSupply == "600000000" && first.Snapshot.CoverageBPS == "10000",
		"ledger %s, chain %s, coverage %s bps", first.Snapshot.LedgerSupply, first.Snapshot.ChainSupply, first.Snapshot.CoverageBPS)

	var supplyRec stableasset.SupplyReconciliation
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		supplyRec, err = w.Stable.ReconcileSupply(ctx, tx, a.ID, reconciliation.ModePeriodic, operator)
		return err
	}); err != nil {
		return fmt.Errorf("reconcile supply: %w", err)
	}
	rec.check("supply reconciliation matches books, wallets, chain and reserve", supplyRec.Matched(), "%+v", supplyRec.Matched())

	burned, err := w.Stable.Burn(asVera, stableasset.BurnRequest{
		StableAssetID: a.ID, AccountID: vera.Account, Quantity: usd(200), IdempotencyKey: "demo-a-burn",
		Mode: gates.ModeSandbox, Actor: actor, Facts: facts,
	})
	if err != nil {
		return fmt.Errorf("redeem: %w", err)
	}
	rec.step("Vera redeems 200 SBX-PUSD: Vera's custodied key burns them on chain, and after finality STABLE_REDEMPTION pays 200 SBX-USD from the reserve",
		"burn_request", burned.Request.ID.String(), "status", string(burned.Request.Status), "journal", ptrString(burned.Request.JournalTransactionID))
	cash, _ = w.Wallet(ctx, vera, w.USD.ID)
	held, _ = w.Wallet(ctx, vera, a.AssetID)
	reserveBal, _ = w.Balance(ctx, reserve)
	supply, _ = w.Host.Supply(ctx, a.Chain, a.Mint)
	rec.check("the redemption is the exact inverse of issuance",
		burned.Request.Status == stableasset.BurnRedeemed && held.Equal(usd(400)) && cash.Equal(usd(600)) && reserveBal.Equal(usd(400)) && supply == 400_000_000,
		"status %s; wallet %s SBX-PUSD and %s SBX-USD; reserve %s; chain supply %d", burned.Request.Status, show(held), show(cash), show(reserveBal), supply)
	rec.check("SBX-USD is conserved between Vera and the reserve", cash.Add(reserveBal).Equal(usd(1_000)), "%s + %s", show(cash), show(reserveBal))

	w.Clock.Set(w.Clock.Now().Add(time.Minute))
	second, err := snap()
	if err != nil {
		return err
	}
	stored, err := w.Stable.Snapshots(ctx, w.DB, a.ID)
	if err != nil {
		return err
	}
	verifyErr := stableasset.VerifySnapshotChain(stored, w.Snapshots.PublicKey())
	rec.check("every published snapshot verifies against the snapshot signer and links to the one before it",
		verifyErr == nil && len(stored) >= 2 && second.Snapshot.LedgerSupply == "400000000", "%d snapshots: %v", len(stored), verifyErr)

	// The market drifts off peg: the monitor sees 200 bps against a 50 bps
	// threshold and pauses issuance.
	if _, err := w.Mark(ctx, a.AssetID, "0.980000", "demo-oracle"); err != nil {
		return err
	}
	var report stableasset.MonitorReport
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		report, err = w.Stable.RunMonitor(ctx, tx, a.ID, operator)
		return err
	}); err != nil {
		return fmt.Errorf("monitor: %w", err)
	}
	alerts := make([]string, 0, len(report.Alerts))
	for _, al := range report.Alerts {
		alerts = append(alerts, string(al))
	}
	rec.step("the sandbox market prices SBX-PUSD at 0.98 and the peg monitor runs",
		"deviation_bps", fmt.Sprint(int64(report.DeviationBPS)), "alerts", strings.Join(alerts, ","), "status", string(report.Status))
	rec.check("a 200 bps deviation trips the breaker and pauses the asset",
		report.DeviationBPS == 200 && report.BreakerTripped && report.Status == stableasset.StatusPaused,
		"deviation %d bps, tripped %t, status %s", int64(report.DeviationBPS), report.BreakerTripped, report.Status)
	_, pausedErr := w.Stable.Mint(asVera, stableasset.MintRequest{
		StableAssetID: a.ID, AccountID: vera.Account, Quantity: usd(10), ReserveAssetID: w.USD.ID,
		IdempotencyKey: "demo-a-mint-paused", Mode: gates.ModeSandbox, Actor: actor, Facts: facts,
	})
	rec.check("a mint while paused is refused", pausedErr != nil, "%v", pausedErr)

	// The market returns to the peg and a person, not the monitor, resumes
	// issuance.
	w.Clock.Set(w.Clock.Now().Add(time.Minute))
	if _, err := w.Mark(ctx, a.AssetID, "1.000000", "demo-oracle"); err != nil {
		return err
	}
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.Stable.Transition(ctx, tx, a.ID, stableasset.StatusActive, operator, "the market price is back at the peg")
		return err
	}); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	again, err := w.Stable.Mint(asVera, stableasset.MintRequest{
		StableAssetID: a.ID, AccountID: vera.Account, Quantity: usd(10), ReserveAssetID: w.USD.ID,
		IdempotencyKey: "demo-a-mint-resumed", Mode: gates.ModeSandbox, Actor: actor, Facts: facts,
	})
	if err != nil {
		return fmt.Errorf("mint after resume: %w", err)
	}
	rec.step("the price returns to 1.00 and an operator resumes issuance; Vera mints 10 more", "mint_request", again.Request.ID.String())
	rec.check("issuance resumes only by a person's decision, and then works", again.Request.Status == stableasset.MintIssued,
		"status %s", again.Request.Status)
	return nil
}
