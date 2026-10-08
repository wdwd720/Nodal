package delegation

import (
	"crypto/sha256"
	"strconv"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// tb is what the unit helpers need of a test: *testing.T, *testing.F and
// *rapid.T all provide it (testing.TB cannot be implemented outside the
// testing package).
type tb interface {
	Helper()
	Errorf(format string, args ...any)
	FailNow()
}

func useed(label string) [32]byte { return sha256.Sum256([]byte("delegation-test/seed/" + label)) }

func ukey(label string) devnet.PublicKey { return devnet.NewKeypairFromSeed(useed(label)).PublicKey() }

func uhash(label string) devnet.Hash {
	return devnet.Hash(sha256.Sum256([]byte("delegation-test/hash/" + label)))
}

var unitNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// unitCapability is a valid, hashed capability granted at unitNow.
func unitCapability(t tb) Capability {
	t.Helper()
	c := Capability{
		ID: id.New[capabilityKind](), GrantedAt: unitNow, Nonce: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		IdempotencyKey: "grant-1", Status: CapabilityActive,
		GrantedBy: Actor{Type: security.ActorUser, ID: id.New[id.Any]().String()},
		Terms: CapabilityTerms{
			AccountID: accounts.AccountID(id.New[id.Any]()), AgentID: id.New[id.Any]().String(), StrategyID: id.New[id.Any]().String(),
			Environment: config.EnvTest, Mode: gates.ModeSandbox,
			InstrumentClasses: []InstrumentClass{ClassStableValue, ClassSpotToken},
			ActionClasses:     []ActionClass{ActionAcquireExposure, ActionReduceExposure, ActionProposeWithdrawal},
			MaxNotional:       money.USDFromMinor(1_000_000), MaxDailyNotional: money.USDFromMinor(500_000),
			MaxSingleTrade: money.USDFromMinor(100_000), DestinationAllowlist: []devnet.PublicKey{ukey("venue"), ukey("dest")},
			ChainAllowlist: []devnet.ChainID{devnethost.ChainSolana}, ExpiresAt: unitNow.Add(24 * time.Hour),
		},
	}
	c.Terms = c.Terms.Normalize()
	h, err := c.ComputeContentHash()
	require.NoError(t, err)
	c.ContentHash = h
	return c
}

// unitUse is a use the unit capability covers.
func unitUse(c Capability) Use {
	return Use{AccountID: c.Terms.AccountID, AgentID: c.Terms.AgentID, StrategyID: c.Terms.StrategyID, Action: ActionAcquireExposure,
		Classes: []InstrumentClass{ClassSpotToken, ClassStableValue}, Chain: devnethost.ChainSolana, Destination: ukey("venue"),
		Notional: money.USDFromMinor(10_000), Environment: config.EnvTest, Mode: gates.ModeSandbox, Now: unitNow.Add(time.Hour)}
}

// unitSwapPlan is a valid swap plan: pay 2_000_000 base units of a 6-decimal
// stable at par for a 9-decimal token at 100.
func unitSwapPlan(t tb) (SwapPlan, [32]byte) {
	t.Helper()
	p := SwapPlan{
		Version: swapPlanVersion, SwapID: id.New[id.Any]().String(), AccountID: id.New[id.Any]().String(),
		AgentID: id.New[id.Any]().String(), CapabilityID: id.New[id.Any]().String(), CapabilityHash: "00",
		IntentID: id.New[id.Any]().String(), RequestNonce: "1", ActionClass: string(ActionAcquireExposure), Mode: "SANDBOX",
		Chain: string(devnethost.ChainSolana), Wallet: ukey("wallet").String(), VenueID: id.New[id.Any]().String(),
		VenueAddress: ukey("venue").String(), PayAssetID: id.New[id.Any]().String(), PayMint: ukey("mint-usd").String(),
		PayDecimals: "6", ReceiveAssetID: id.New[id.Any]().String(), ReceiveMint: ukey("mint-tok").String(), ReceiveDecimals: "9",
		AmountIn: "2000000", AmountOut: "19900000", MinAmountOut: "19800000", SpreadBPS: "50", MaxSlippageBPS: "100",
		NotionalUSD: "2.00", FeePerTx: "10", RequestedAt: unitNow, QuoteExpiresAt: unitNow.Add(2 * time.Minute),
		PayPrice:     PlanPrice{Mantissa: "1", Scale: "0", Source: "registry-usd-peg", ObservedAt: unitNow, Par: true},
		ReceivePrice: PlanPrice{Mantissa: "100", Scale: "0", Source: "itest", ObservedAt: unitNow},
	}
	h, err := p.Hash()
	require.NoError(t, err)
	return p, h
}

func unitSwapExpectation(t tb) SwapExpectation {
	p, h := unitSwapPlan(t)
	return SwapExpectation{Plan: p, PlanHash: h, Blockhash: uhash("tip"), Wallet: ukey("wallet"),
		AllowedChains: []devnet.ChainID{devnethost.ChainSolana}, AllowedDestinations: []devnet.PublicKey{ukey("venue")},
		ChainFee: 10, MaxFee: 100_000}
}

func unitWithdrawalPlan(t tb) (WithdrawalPlan, [32]byte) {
	t.Helper()
	p := WithdrawalPlan{
		Version: withdrawalPlanVersion, WithdrawalID: id.New[id.Any]().String(), AccountID: id.New[id.Any]().String(),
		AgentID: id.New[id.Any]().String(), CapabilityID: id.New[id.Any]().String(), CapabilityHash: "00",
		IntentID: id.New[id.Any]().String(), ProposalNonce: "1", Mode: "SANDBOX", Chain: string(devnethost.ChainSolana),
		Wallet: ukey("wallet").String(), DestinationID: id.New[id.Any]().String(), DestinationAddress: ukey("dest").String(),
		AssetID: id.New[id.Any]().String(), Mint: ukey("mint-usd").String(), Decimals: "6", Amount: "5000000",
		NotionalUSD: "5.00", Price: PlanPrice{Mantissa: "1", Scale: "0", Source: "registry-usd-peg", ObservedAt: unitNow, Par: true},
		PolicyID: id.New[id.Any]().String(), PolicyVersion: "1", PolicyHash: "00", EvaluationOutcome: string(OutcomeAutoApprove),
		RiskScore: "0", FeePerTx: "10", ProposedAt: unitNow, ExecutableAt: unitNow, ApprovalExpiresAt: unitNow.Add(24 * time.Hour),
	}
	h, err := p.Hash()
	require.NoError(t, err)
	return p, h
}

func unitWithdrawalExpectation(t tb) WithdrawalExpectation {
	p, h := unitWithdrawalPlan(t)
	return WithdrawalExpectation{Plan: p, PlanHash: h, Blockhash: uhash("tip"), Wallet: ukey("wallet"),
		AllowedChains: []devnet.ChainID{devnethost.ChainSolana}, AllowedDestinations: []devnet.PublicKey{ukey("dest")},
		CustomerDestination: ukey("dest"), ChainFee: 10, MaxFee: 100_000}
}

func usdAsset(decimals uint8) assets.Asset {
	return assets.Asset{Kind: assets.KindSimulated, Chain: string(devnethost.ChainSolana), IsStablecoin: true, PegCurrency: "USD",
		Decimals: decimals, RiskClass: assets.RiskSettlement, Status: assets.StatusActive}
}

// unitQuote is the quote unit every unit-test price is in.
const unitQuote = "01920000-0000-7000-8000-5342582d5553"

func price(mantissa int64, scale int32) money.Price {
	p, err := money.NewPrice(money.QuantityFromInt64(mantissa), scale, unitQuote, "unit", unitNow)
	if err != nil {
		panic(err)
	}
	return p
}

func itoa(n int) string { return strconv.Itoa(n) }
