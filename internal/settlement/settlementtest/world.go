package settlementtest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/eligibility"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuation"
)

// Now is the fixed instant every World starts at.
var Now = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// World is a consistent set of fakes and fixtures for executor tests: a
// SOL/USDC spot instrument listed on JUPITER, one account with a wallet
// holding USDC and SOL, eligible and risk-allowed, healthy providers.
type World struct {
	Clock *clock.Fake

	USDC, SOL  assets.Asset
	Instrument instruments.Instrument
	Venue      instruments.Venue
	Listing    instruments.VenueListing

	AccountID     accounts.AccountID
	WalletID      string
	WalletAddress string
	WalletChain   string
	IntentID      string

	Chain     *Chain
	Adapter   *Adapter
	Observer  *Observer
	Recoverer *Recoverer
	Signer    *Signer
	Inspector *Inspector
	Archive   *Archive

	DB           *MemDB
	Plans        *MemPlans
	Orders       *MemOrders
	Attempts     *MemAttempts
	Quotes       *MemQuotes
	Intents      *MemIntents
	Capital      *MemCapital
	Ledger       *MemLedger
	Positions    *MemPositions
	KillSwitches *KillSwitches
	Risk         *RiskFinal
	Audit        *MemAudit

	Planner *settlement.V1Planner
}

// NewWorld builds a fresh world.
func NewWorld() *World {
	clk := clock.NewFake(Now)
	w := &World{Clock: clk}
	w.USDC = assets.Asset{
		ID: assets.NewAssetID(), Chain: "solana-devnet", MintAddress: "USDC-mint", Kind: assets.KindSPLToken, Symbol: "USDC", Name: "USD Coin",
		Decimals: 6, IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive, MetadataVersion: 1,
	}
	w.SOL = assets.Asset{
		ID: assets.NewAssetID(), Chain: "solana-devnet", MintAddress: "native", Kind: assets.KindNative, Symbol: "SOL", Name: "Solana",
		Decimals: 9, RiskClass: assets.RiskMajor, Status: assets.StatusActive, MetadataVersion: 1,
	}
	solID, usdcID := w.SOL.ID, w.USDC.ID
	w.Instrument = instruments.Instrument{
		ID: instruments.NewInstrumentID(), Type: instruments.TypeSpotPair, CanonicalName: "SOL/USDC", ExposureID: newExposureID(),
		BaseAssetID: &solID, QuoteAssetID: &usdcID, SettlementAssetID: usdcID, RiskClass: assets.RiskMajor, Status: assets.StatusActive,
		ActiveFrom: Now.Add(-time.Hour), MetadataVersion: 1,
	}
	w.Venue = instruments.Venue{ID: newVenueID(), Code: "JUPITER", Name: "Jupiter", Kind: instruments.VenueDEXAggregator, Chain: "solana-devnet", Status: instruments.VenueActive}
	w.Listing = instruments.VenueListing{
		ID: newListingID(), VenueID: w.Venue.ID, InstrumentID: w.Instrument.ID, VenueNativeID: "SOL-USDC",
		Network: "solana-devnet", BaseMint: w.SOL.MintAddress, QuoteMint: w.USDC.MintAddress, BasePrecision: 9, QuotePrecision: 6,
		MinNotionalQuote: money.QuantityFromInt64(1_000_000), Status: instruments.VenueActive,
	}
	w.AccountID = accounts.NewAccountID()
	w.WalletID = id.New[id.Any]().String()
	w.WalletAddress = "wallet-" + w.AccountID.String()[:8]
	w.WalletChain = "solana-devnet"
	w.IntentID = id.New[id.Any]().String()

	w.Chain = NewChain(clk)
	w.Chain.SetBalance(w.WalletAddress, w.USDC.MintAddress, money.QuantityFromInt64(1_000_000_000)) // 1,000 USDC
	w.Chain.SetBalance(w.WalletAddress, w.SOL.MintAddress, money.QuantityFromInt64(5_000_000_000))  // 5 SOL
	w.Adapter = NewAdapter(w.Chain, clk)
	w.Adapter.NetworkFeeAsset = w.SOL.ID
	w.Observer = NewObserver(w.Chain)
	w.Recoverer = NewRecoverer(w.Chain)
	w.Signer = &Signer{}
	w.Inspector = &Inspector{}
	w.Archive = NewArchive()

	w.DB = &MemDB{}
	w.Plans = NewMemPlans(clk)
	w.Orders = NewMemOrders(clk)
	w.Attempts = NewMemAttempts(clk, w.Orders)
	w.Quotes = &MemQuotes{}
	w.Intents = NewMemIntents()
	w.Capital = NewMemCapital(clk)
	w.Ledger = NewMemLedger()
	w.Positions = &MemPositions{}
	w.KillSwitches = &KillSwitches{}
	w.Risk = &RiskFinal{Constraints: risk.ResultingConstraints{MaxNotionalUSD: money.USDFromMinor(100_000), MaxSlippageBPS: 100, MaxFeeBPS: 50, MaxPriceImpactBPS: 100, MaxQuoteAgeMS: 3000}}
	w.Audit = NewMemAudit()
	w.Planner = settlement.NewPlanner(settlement.DefaultOptions())
	return w
}

// The instruments package does not export constructors for venue, listing
// and exposure ids; fresh UUIDv7 values are parsed into the alias types.
func newVenueID() instruments.VenueID {
	var v instruments.VenueID
	if err := v.UnmarshalText([]byte(id.New[id.Any]().String())); err != nil {
		panic(err)
	}
	return v
}

func newListingID() instruments.ListingID {
	var v instruments.ListingID
	if err := v.UnmarshalText([]byte(id.New[id.Any]().String())); err != nil {
		panic(err)
	}
	return v
}

func newExposureID() instruments.ExposureID {
	var v instruments.ExposureID
	if err := v.UnmarshalText([]byte(id.New[id.Any]().String())); err != nil {
		panic(err)
	}
	return v
}

// Deps wires the world into executor dependencies. Sleep is a no-op so
// OBSERVE_FINALITY polls without waiting.
func (w *World) Deps() settlement.Deps {
	return settlement.Deps{
		DB: w.DB, Plans: w.Plans, Orders: w.Orders, Attempts: w.Attempts, Quotes: w.Quotes, Intents: w.Intents,
		Adapter: w.Adapter, Observer: w.Observer, Capital: w.Capital, Ledger: w.Ledger, Positions: w.Positions,
		KillSwitches: w.KillSwitches, Risk: w.Risk, Inspector: w.Inspector, Recoverer: w.Recoverer, Audit: w.Audit, Archive: w.Archive,
		Clock: w.Clock, Sleep: func(context.Context, time.Duration) error { return nil },
	}
}

// IntentRef is the executor's view of the world's intent.
func (w *World) IntentRef() settlement.IntentRef {
	return settlement.IntentRef{
		ID: w.IntentID, AccountID: w.AccountID.String(), ActorType: security.ActorUser, ActorID: "user-1", InstrumentID: w.Instrument.ID.String(),
		Mode: execution.ModeLive, CorrelationID: "corr-" + w.IntentID[:8], WalletID: w.WalletID, WalletAddress: w.WalletAddress, WalletChain: w.WalletChain, WalletProvider: "privy",
	}
}

// Input returns a complete planner input for the world's intent.
func (w *World) Input() settlement.PlannerInput {
	usdc, sol := w.USDC, w.SOL
	notional := money.USDFromMinor(10_000) // 100.00
	return settlement.PlannerInput{
		Intent: settlement.IntentSnapshot{
			ID: w.IntentID, AccountID: w.AccountID.String(), ActorType: security.ActorUser, ActorID: "user-1", Action: settlement.ActionAcquireNotional,
			InstrumentID: w.Instrument.ID.String(), NotionalUSD: &notional,
			Constraints: settlement.IntentConstraints{MaxSlippageBPS: 50, QuoteFreshness: 5 * time.Second},
			Deadline:    Now.Add(10 * time.Minute), RequestedAt: Now, IdempotencyKey: "idem-" + w.IntentID[:8], CorrelationID: "corr-" + w.IntentID[:8], Mode: execution.ModeLive,
		},
		Account: settlement.AccountState{
			ID: w.AccountID.String(), Status: accounts.StatusActive, Mode: execution.ModeLive, WalletID: w.WalletID, WalletAddress: w.WalletAddress,
			WalletChain: w.WalletChain, CostBasisMethod: "FIFO",
		},
		BuyingPower: capital.BuyingPower{
			PortfolioValue: money.USDFromMinor(175_000), BuyingPower: money.USDFromMinor(160_000), AvailableNow: money.USDFromMinor(100_000),
			PolicyVersion: "bp-v1", AsOf: Now, Purpose: capital.PurposeTrade,
		},
		Instrument: w.Instrument,
		Listings:   []instruments.VenueListing{w.Listing},
		Venues:     []settlement.VenueCandidate{{Venue: w.Venue, Provider: "jupiter", FeeBPS: 0, ProgramIDs: []string{"JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"}}},
		Assets:     map[assets.AssetID]assets.Asset{usdc.ID: usdc, sol.ID: sol},
		AssetPolicies: map[assets.AssetID]valuation.AssetPolicy{
			usdc.ID: {AssetID: usdc.ID, Status: assets.StatusActive, CollateralFactor: 10_000, StablecoinStatus: valuation.StablecoinNormal, MaxPriceAge: time.Minute, PolicyVersion: "usdc-v1"},
			sol.ID:  {AssetID: sol.ID, Status: assets.StatusActive, CollateralFactor: 8_000, MaxPriceAge: time.Minute, PolicyVersion: "sol-v1"},
		},
		Prices: map[assets.AssetID]money.Price{
			sol.ID:  {Mantissa: money.QuantityFromInt64(150), Scale: 0, QuoteAsset: settlement.USDQuoteAsset, Source: "pyth", At: Now.Add(-time.Second)},
			usdc.ID: {Mantissa: money.QuantityFromInt64(1), Scale: 0, QuoteAsset: settlement.USDQuoteAsset, Source: "pyth", At: Now.Add(-time.Second)},
		},
		Holdings: []positions.Holding{{AssetID: sol.ID, Symbol: "SOL", Decimals: 9, Quantity: money.QuantityFromInt64(2_000_000_000), CostBasis: money.USDFromMinor(28_000), LotCount: 1}},
		Health: settlement.SystemHealth{
			Providers: map[string]provider.Health{"jupiter": provider.Healthy},
			Chains: map[string]settlement.ChainStatus{"solana-devnet": {
				Health: provider.Healthy, Height: 1000, EstimatedNetworkFee: money.QuantityFromInt64(5000), NetworkFeeAsset: sol.ID, ObservedAt: Now,
			}},
			ActiveKillSwitches: []killswitch.Switch{},
		},
		Eligibility: eligibility.Decision{Eligible: true, PolicyVersion: "elig-v1", PolicyHash: "elig-hash", ReasonCodes: []string{}, EvaluatedAt: Now, ContextHash: "ctx", Hash: "elig-decision-hash"},
		Risk: risk.Decision{
			Verdict: risk.Allow, Stage: risk.StagePreTrade, ActionClass: risk.ClassNewRisk, EffectiveNotionalUSD: notional, PolicyVersion: "risk-v1", PolicyHash: "risk-hash",
			EvaluatorVersion: risk.EvaluatorVersion, ReasonCodes: []string{}, MatchedKillSwitches: []risk.KillSwitch{},
			Constraints: risk.ResultingConstraints{MaxNotionalUSD: money.USDFromMinor(100_000), MaxSlippageBPS: 100, MaxFeeBPS: 50, MaxPriceImpactBPS: 100, MaxQuoteAgeMS: 3000},
			InputHash:   "input-hash", EvaluatedAt: Now, Hash: "risk-decision-hash",
		},
		FeePolicy:      fees.Policy{Version: "fees-v1", PlatformFeeBPS: 10, FeeAsset: usdc.ID, Rounding: money.RoundHalfEven},
		FinalityPolicy: execution.DefaultFinalityPolicy(),
		Now:            Now,
		PlannerVersion: settlement.PlannerVersion,
		Version:        1,
	}
}

// SellInput returns a REDUCE_NOTIONAL input selling 100 USD of SOL.
func (w *World) SellInput() settlement.PlannerInput {
	in := w.Input()
	in.Intent.Action = settlement.ActionReduceNotional
	in.Risk.ActionClass = risk.ClassReduceRisk
	return in
}

// PlanAndApprove plans in, stores the plan as APPROVED and registers the
// intent, returning the stored plan. The signer and the adapter are left
// untouched.
func (w *World) PlanAndApprove(t testing.TB, in settlement.PlannerInput) settlement.Plan {
	t.Helper()
	plan, err := w.Planner.Plan(in)
	require.NoError(t, err)
	plan.AccountID = in.Intent.AccountID
	plan.Status = settlement.PlanApproved
	at := w.Clock.Now()
	plan.ApprovedAt = &at
	w.Plans.Put(plan)
	w.Intents.Put(w.IntentRef())
	return plan
}

// PlanDryRun plans in as a dry run and stores it APPROVED.
func (w *World) PlanDryRun(t testing.TB, in settlement.PlannerInput) settlement.Plan {
	t.Helper()
	in.DryRun = true
	return w.PlanAndApprove(t, in)
}

// Plan reads the current stored plan.
func (w *World) Plan(t testing.TB, planID settlement.PlanID) settlement.Plan {
	t.Helper()
	p, err := w.Plans.Get(context.Background(), nil, planID)
	require.NoError(t, err)
	return p
}

// Order returns the order of a plan, if any.
func (w *World) Order(planID settlement.PlanID) (execution.Order, bool) {
	o, err := w.Orders.GetByPlan(context.Background(), nil, planID.String())
	if err != nil {
		return execution.Order{}, false
	}
	return o, true
}
