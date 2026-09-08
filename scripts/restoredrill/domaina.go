package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// seedDomainA writes a live internal economy into the source database, through
// the REAL services rather than by hand.
//
// It exists because the drill's original fixture was users, accounts, one asset
// and twenty-five journal transactions. Every Domain A table restored EMPTY,
// so "row counts match" compared zero with zero and the readiness report had to
// record the backup drill as not covering the new tables. A backup that has
// only ever been proven on tables nobody uses is not a proven backup.
//
// Going through the services rather than writing rows is the point. The Domain
// A tables carry deferred balance triggers, the CR004 lot-event binding, the
// AU001 state-change binding, the IC001 order-balance trigger and the
// terms-frozen guard. Hand-written rows would have to satisfy all of them, and
// rows that satisfied them by construction would prove less: what the restore
// has to survive is data shaped the way the application actually makes it.
func seedDomainA(ctx context.Context, appDSN string) error {
	pool, err := db.Open(ctx, db.Config{URL: appDSN, AppName: "restoredrill", MaxConns: 4, MinConns: 1})
	if err != nil {
		return err
	}
	defer pool.Close()

	// A fixed epoch, not the wall clock: the ledger includes effective_at in a
	// posting's content hash, so a moving timestamp under a fixed idempotency
	// key would be refused on the second run.
	epoch, err := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
	if err != nil {
		return err
	}
	clk := clock.NewFake(epoch)
	ctx = security.WithPrincipal(ctx, security.Principal{
		SubjectID: "restoredrill", ActorType: security.ActorSystem, AuthTime: epoch,
	})

	led := ledger.NewService(clk, "restoredrill")
	led.SetCapabilityResolver(drillCaps{
		valuedomain.CapNativeMarketTrading: true,
		commerce.CapMarketplace:            true,
	})
	credits := credit.NewService(led, clk)
	assetSvc := nativeasset.NewService(clk, nil)
	marketSvc := nativemarket.NewService(led, credits,
		valuation.NewPriceStore(clk), audit.NewWriter(), instruments.NewRepository(),
		nativemarket.NewRiskGate(risk.NewStore(), clk), clk)
	commerceSvc := commerce.NewService(led, credits, audit.NewWriter(), clk)
	commerceSvc.SetCapabilityResolver(drillCaps{commerce.CapMarketplace: true})

	repo := accounts.NewRepository()
	buyer, err := drillAccount(ctx, pool, repo, "drill-buyer")
	if err != nil {
		return err
	}
	seller, err := drillAccount(ctx, pool, repo, "drill-seller")
	if err != nil {
		return err
	}

	creditAsset, err := assets.NewRepository().Create(ctx, pool, assets.Asset{
		Chain: assets.InternalChain, Kind: assets.KindCredit, ValueDomain: valuedomain.InternalCredit,
		Symbol: "CREDIT", Name: "Nodal Credit", Decimals: 6,
		RiskClass: assets.RiskUnsupported, Status: assets.StatusActive,
	})
	if err != nil {
		return fmt.Errorf("credit asset: %w", err)
	}

	return pool.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		// The GLOBAL risk policy. A native trade is evaluated against it and
		// fails closed without one, so the drill records what
		// `go run ./scripts/riskpolicy` gives a fresh deployment -- and
		// risk_policies and risk_decisions then carry rows for the restore to
		// prove something about, which was the whole point of this file.
		if _, err := risk.NewStore().RecordPolicy(ctx, tx, risk.PolicyRecord{
			Scope:       risk.ScopeGlobal,
			Version:     "restore-drill-global",
			Rules:       json.RawMessage(risk.DefaultGlobalPolicyJSON),
			EffectiveAt: epoch.Add(-time.Hour),
			ActorType:   security.ActorSystem,
			ActorID:     "restoredrill",
			Reason:      "the compiled-in default limits, as a fresh deployment gets them",
		}); err != nil {
			return fmt.Errorf("risk policy: %w", err)
		}

		// Credits, held in provenance lots.
		for _, a := range []accounts.AccountID{buyer, seller} {
			if _, err := credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: a, Quantity: money.QuantityFromInt64(50_000_000_000),
				Origin: valuedomain.OriginPromotional, Finality: valuedomain.FinalityUnfunded,
				Reference:      credit.Reference{Type: "restore_drill", ID: a.String()},
				IdempotencyKey: "drill-issue-" + a.String(),
				Reason:         "restore drill fixture", EffectiveAt: epoch,
			}); err != nil {
				return fmt.Errorf("issue credits: %w", err)
			}
		}

		// A native asset, its market, and a trade — so native_assets,
		// native_markets, native_market_state, native_market_fills,
		// asset_prices and the instrument row all carry data.
		asset, _, err := assetSvc.CreateDraft(ctx, tx, nativeasset.CreateRequest{
			CreatorAccountID: seller,
			Name:             "Restore Drill Coin", Symbol: "DRILL",
			Description: "a fixture asset for the backup drill",
			Supply: nativeasset.SupplyModel{
				MaxSupply:         money.QuantityFromInt64(1_000_000_000_000_000),
				CreatorAllocation: money.QuantityFromInt64(100_000_000_000_000),
			},
		})
		if err != nil {
			return fmt.Errorf("create asset: %w", err)
		}
		if _, err := assetSvc.SetStatus(ctx, tx, asset.AssetID, nativeasset.StatusPendingReview, "drill"); err != nil {
			return err
		}
		if _, err := assetSvc.SetModeration(ctx, tx, asset.AssetID, nativeasset.ModerationApproved, "drill"); err != nil {
			return err
		}
		if _, err := assetSvc.Activate(ctx, tx, asset.AssetID, "drill"); err != nil {
			return err
		}
		market, err := marketSvc.Create(ctx, tx, nativemarket.CreateRequest{
			AssetID: asset.AssetID, CreditAssetID: creditAsset.ID, CreatorID: seller,
			PoolSupply: asset.Supply.PoolSupply(), CreatorAllocation: asset.Supply.CreatorAllocation,
			VirtualCreditReserve: money.QuantityFromInt64(30_000_000_000),
			Fees:                 nativemarket.Fees{PlatformBPS: 100, CreatorBPS: 50},
			IdempotencyKey:       "drill-mint", EffectiveAt: epoch,
		})
		if err != nil {
			return fmt.Errorf("create market: %w", err)
		}
		if _, err := marketSvc.SetStatus(ctx, tx, market.ID, nativemarket.StatusActive, "drill"); err != nil {
			return err
		}
		if _, err := marketSvc.Execute(ctx, tx, nativemarket.ExecuteRequest{
			MarketID: market.ID, AccountID: buyer, Side: nativemarket.Buy,
			Amount: money.QuantityFromInt64(2_000_000_000), MinOutput: money.QuantityFromInt64(1),
			IdempotencyKey: "drill-buy", EffectiveAt: epoch,
		}); err != nil {
			return fmt.Errorf("trade: %w", err)
		}

		// A marketplace sale — internal_sellers, internal_products and
		// internal_commerce_orders, with the earning provenance it mints.
		if _, err := commerceSvc.RegisterSeller(ctx, tx, commerce.Seller{
			AccountID: seller, DisplayName: "Restore drill seller",
		}); err != nil {
			return fmt.Errorf("register seller: %w", err)
		}
		product, err := commerceSvc.CreateProduct(ctx, tx, commerce.Product{
			SellerAccountID: seller, Kind: commerce.KindResearch,
			Title: "Restore drill research note", Description: "a fixture product",
			Price: money.QuantityFromInt64(1_000_000_000), PlatformFeeBPS: 500,
		})
		if err != nil {
			return fmt.Errorf("create product: %w", err)
		}
		if _, err := commerceSvc.Publish(ctx, tx, product.ID); err != nil {
			return err
		}
		if _, err := commerceSvc.Purchase(ctx, tx, commerce.PurchaseRequest{
			ProductID: product.ID, BuyerAccountID: buyer,
			ExpectedPrice: product.Price, IdempotencyKey: "drill-purchase",
			EffectiveAt: epoch,
		}); err != nil {
			return fmt.Errorf("purchase: %w", err)
		}
		return nil
	})
}

// drillCaps is a fixed capability answer. The drill is proving that data
// survives a restore, not what a deployment permits.
type drillCaps map[valuedomain.CapabilityKey]bool

func (c drillCaps) ActiveConversionCapabilities(context.Context, db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	return c, nil
}

func (c drillCaps) ActiveCapabilities(context.Context, db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	return c, nil
}

func drillAccount(ctx context.Context, q *db.DB, repo *accounts.Repository, subject string) (accounts.AccountID, error) {
	u, err := repo.CreateUser(ctx, q, "restoredrill", subject, nil)
	if err != nil {
		return accounts.AccountID{}, err
	}
	a, err := repo.CreateAccount(ctx, q, u.ID, accounts.KindCustomer)
	if err != nil {
		return accounts.AccountID{}, err
	}
	return a.ID, nil
}
