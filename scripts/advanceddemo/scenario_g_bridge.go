package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/scripts/internal/sandboxworld"
)

func init() {
	register("G", "Cross-chain capital: SBX-PUSD bridged from sbx-solana to sbx-ethereum through a lock, an attestation and a release, reconciled on both chains", scenarioBridge)
}

func scenarioBridge(ctx context.Context, w *sandboxworld.World, rec *recorder) error {
	usd := func(n int64) money.Quantity { return sandboxworld.Units(n, w.USD.Decimals) }
	show := func(q money.Quantity) string { return q.ToDecimalString(w.USD.Decimals) }
	operator, err := w.OperatorActor(ctx, "ines-bridge-operator")
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
	wes, err := w.Customer(ctx, "wes-bridge")
	if err != nil {
		return err
	}
	if _, err := w.Fund(ctx, wes, w.USD.ID, usd(500), "demo-g-fund"); err != nil {
		return err
	}
	facts, err := stableFacts(ctx, w, wes)
	if err != nil {
		return err
	}
	actor := econ.Actor{Type: security.ActorUser, ID: wes.User.String()}
	asWes := security.WithPrincipal(ctx, wes.P)
	minted, err := w.Stable.Mint(asWes, stableasset.MintRequest{
		StableAssetID: pusd.ID, AccountID: wes.Account, Quantity: usd(300), ReserveAssetID: w.USD.ID,
		IdempotencyKey: "demo-g-mint", Mode: gates.ModeSandbox, Actor: actor, Facts: facts,
	})
	if err != nil || minted.Request.Status != stableasset.MintIssued {
		return fmt.Errorf("mint: %v (status %s)", err, minted.Request.Status)
	}
	rec.step("Wes mints 300 SBX-PUSD on sbx-solana", "mint_request", minted.Request.ID.String())

	unit, err := money.NewPrice(money.QuantityFromInt64(1), 0, "USD", "ROUTE_UNIT_PRICE", w.Clock.Now())
	if err != nil {
		return err
	}
	route, err := w.Bridge.CreateRoute(ctx, bridge.RouteDefinition{
		Name: "pusd-solana-to-ethereum", SourceAssetID: pusd.AssetID, DestinationChain: devnethost.ChainEthereum,
		DestinationSymbol: "SBX-PUSD.E", UnitPrice: unit, MaxReleaseAttempts: 3,
	}, operator)
	if err != nil {
		return fmt.Errorf("create route: %w", err)
	}
	rec.step("an operator opens a route sbx-solana -> sbx-ethereum: a BRIDGE facility, an escrow on the source and a bridged twin minted only by the attested release",
		"route", route.ID.String(), "facility", route.FacilityID.String(), "source_mint", route.SourceMint.String(),
		"destination_asset", route.DestinationAssetID.String(), "destination_mint", route.DestinationMint.String(),
		"attester", route.Attester.String())

	req := bridge.BridgeRequest{
		RouteID: route.ID, AccountID: wes.Account, Quantity: usd(100), IdempotencyKey: "demo-g-bridge",
		Mode: gates.ModeSandbox, Actor: actor,
		Facts: bridge.AccountFacts{Verification: facts.Verification, IdentityVerified: facts.IdentityVerified},
	}
	res, err := w.Bridge.Bridge(asWes, req)
	if err != nil {
		return fmt.Errorf("bridge: %w", err)
	}
	leg := res.Leg
	rec.step("Wes bridges 100 SBX-PUSD: Wes's custodied key locks them in the source escrow, the lock finalizes, the attester signs the release and the twin is minted on sbx-ethereum",
		"leg", leg.ID.String(), "status", string(leg.Status), "lock_tx", string(leg.LockTxID),
		"source_effect", leg.SourceEffectID.String(), "destination_effect", leg.DestinationEffectID.String())
	rec.check("the leg ran to RECONCILED", leg.Status == bridge.StateReconciled, "status %s %s", leg.Status, leg.FailureCode)

	var path []string
	rows, err := w.DB.Query(ctx, `SELECT to_status FROM bridge_leg_transitions WHERE leg_id = $1 ORDER BY occurred_at, id`, leg.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return err
		}
		path = append(path, s)
	}
	rows.Close()
	rec.check("every state change of the leg is a bound transition",
		strings.Join(path, ">") == "AVAILABLE>RESERVED>BRIDGE_REQUESTED>SOURCE_FINALIZED>MESSAGE_IN_FLIGHT>DESTINATION_FINALIZED>RECONCILED",
		"%s", strings.Join(path, " > "))

	src, _ := w.Wallet(ctx, wes, pusd.AssetID)
	dst, _ := w.Wallet(ctx, wes, route.DestinationAssetID)
	locked, err := w.Balance(ctx, ledger.AccountRef{OwnerType: ledger.OwnerFacility, OwnerID: route.FacilityID.String(), Code: ledger.CodeBridgeLocked, AssetID: pusd.AssetID})
	if err != nil {
		return err
	}
	outstanding, err := w.Balance(ctx, ledger.AccountRef{OwnerType: ledger.OwnerFacility, OwnerID: route.FacilityID.String(), Code: ledger.CodeBridgeOutstanding, AssetID: route.DestinationAssetID})
	if err != nil {
		return err
	}
	rec.check("the ledger moved 100 from Wes's source wallet into the bridge and 100 twins into Wes's destination wallet",
		src.Equal(usd(200)) && dst.Equal(usd(100)) && locked.Equal(usd(100)) && outstanding.Equal(usd(100)),
		"source wallet %s, destination wallet %s, locked %s, outstanding %s", show(src), show(dst), show(locked), show(outstanding))

	holder := w.Custody.HolderAddress(wes.Account)
	srcChain, err := w.Host.TokenBalance(ctx, route.SourceChain, holder, route.SourceMint)
	if err != nil {
		return err
	}
	escrow, err := w.Host.TokenBalance(ctx, route.SourceChain, devnet.BridgeEscrowAddress(route.SourceChain), route.SourceMint)
	if err != nil {
		return err
	}
	dstChain, err := w.Host.TokenBalance(ctx, route.DestinationChain, holder, route.DestinationMint)
	if err != nil {
		return err
	}
	dstSupply, err := w.Host.Supply(ctx, route.DestinationChain, route.DestinationMint)
	if err != nil {
		return err
	}
	rec.check("both chains agree with the ledger: escrow on the source equals the twin supply on the destination",
		srcChain == 200_000_000 && escrow == 100_000_000 && dstChain == 100_000_000 && dstSupply == 100_000_000,
		"source holder %d, escrow %d, destination holder %d, destination supply %d", srcChain, escrow, dstChain, dstSupply)

	var routeRec bridge.RouteReconciliation
	var supplyRec stableasset.SupplyReconciliation
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if routeRec, err = w.Bridge.ReconcileRoute(ctx, tx, route.ID, reconciliation.ModePeriodic, operator); err != nil {
			return err
		}
		supplyRec, err = w.Stable.ReconcileSupply(ctx, tx, pusd.ID, reconciliation.ModePeriodic, operator)
		return err
	}); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	rec.check("the route reconciles: locked on the source, outstanding on the destination and both chains match", routeRec.Matched(), "%+v", routeRec.Matched())
	rec.check("the stable asset still reconciles with 100 of its units locked in the bridge", supplyRec.Matched(),
		"ledger outstanding %s, customer wallets %s, chain supply %s", show(supplyRec.LedgerOutstanding),
		show(supplyRec.CustomerWalletSum), show(supplyRec.ChainSupply))

	again, err := w.Bridge.Bridge(asWes, req)
	if err != nil {
		return fmt.Errorf("bridge again: %w", err)
	}
	var legs int
	if err := w.DB.QueryRow(ctx, `SELECT count(*) FROM bridge_legs WHERE account_id = $1`, wes.Account).Scan(&legs); err != nil {
		return err
	}
	rec.check("the same request again is a replay: one leg, nothing moved twice", again.Replay && again.Leg.ID == leg.ID && legs == 1,
		"replay %t, %d legs", again.Replay, legs)

	bp, err := w.Bridge.BuyingPower(ctx, w.DB, wes.Account, bridge.DefaultHaircutPolicy())
	if err != nil {
		return fmt.Errorf("cross-chain buying power: %w", err)
	}
	rec.check("the bridge's part of buying power counts nothing in flight once the leg is final", len(bp.Legs) == 0 && len(bp.Holdings) == 0,
		"%d legs, %d in-flight holdings", len(bp.Legs), len(bp.Holdings))

	// Universal buying power V2 values every final holding on every chain at
	// a fresh mark in the sandbox dollar: the twin on sbx-ethereum counts as
	// fully as the original on sbx-solana.
	if _, err := w.Mark(ctx, route.DestinationAssetID, "1.000000", "demo-oracle"); err != nil {
		return err
	}
	loader, err := buyingpower.NewV2Loader(buyingpower.V2LoaderConfig{
		Clock: w.Clock, Prices: w.Prices, QuoteAssetID: w.USD.ID, MaxPriceAge: 5 * time.Minute,
		Margin: buyingpower.CombinedMarginReader{w.Margin.BuyingPowerReader(), w.Derivatives.MarginReader()},
	})
	if err != nil {
		return err
	}
	v2, err := loader.Compute(ctx, w.DB, "CUSTOMER", wes.Account.String())
	if err != nil {
		return fmt.Errorf("buying power v2: %w", err)
	}
	rec.step("buying power V2 values Wes's holdings on both chains: SBX-USD, SBX-PUSD on sbx-solana and SBX-PUSD.E on sbx-ethereum",
		"gross_equity", v2.GrossEquity.String(), "trade_buying_power", v2.TradeBuyingPower.String(),
		"policy", v2.PolicyVersion)
	rec.check("gross equity is the three holdings at par: 200 + 200 + 100 = 500 dollars", v2.GrossEquity.Equal(money.USDFromMinor(500_00)),
		"gross equity %s", v2.GrossEquity)
	return nil
}
