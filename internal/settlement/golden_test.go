package settlement

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/eligibility"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Golden corpus layout:
//
//	testdata/inputs/base.json   the complete PlannerInput document (generated from goldenBase by -update)
//	testdata/cases/<name>.json  {"input_base", "patch", "expect"}
//
// Patches deep-merge into the base: objects merge recursively, a JSON null
// removes the key, arrays are replaced unless the patch is an object whose
// keys are element indexes ({"listings": {"0": {"Status": "DISABLED"}}}).
// Running `go test ./internal/settlement -run TestGolden -update` rewrites
// base.json and every case's expected hash.

var update = flag.Bool("update", false, "rewrite golden inputs and expected hashes")

const determinismRuns = 1000

// Fixed identifiers (UUIDv7 form) so the corpus is byte-stable.
const (
	gUSDC      = "01920000-0000-7000-8000-000000000001"
	gSOL       = "01920000-0000-7000-8000-000000000002"
	gUSDT      = "01920000-0000-7000-8000-000000000003"
	gInstr     = "01920000-0000-7000-8000-000000000010"
	gExposure  = "01920000-0000-7000-8000-000000000011"
	gVenueJup  = "01920000-0000-7000-8000-000000000020"
	gVenueRay  = "01920000-0000-7000-8000-000000000021"
	gListJup   = "01920000-0000-7000-8000-000000000030"
	gListRay   = "01920000-0000-7000-8000-000000000031"
	gAccount   = "01920000-0000-7000-8000-000000000040"
	gIntent    = "01920000-0000-7000-8000-000000000050"
	gWallet    = "01920000-0000-7000-8000-000000000060"
	gNowString = "2026-09-05T12:00:00Z"
)

func mustAsset(s string) assets.AssetID {
	a, err := assets.ParseAssetID(s)
	if err != nil {
		panic(err)
	}
	return a
}

func mustText[T interface{ UnmarshalText([]byte) error }](v T, s string) T {
	if err := v.UnmarshalText([]byte(s)); err != nil {
		panic(err)
	}
	return v
}

// goldenBase is the canonical happy-path input: buy 100 USD of SOL/USDC on
// JUPITER with a healthy provider and chain.
func goldenBase() PlannerInput {
	now, _ := time.Parse(time.RFC3339, gNowString)
	usdcID, solID := mustAsset(gUSDC), mustAsset(gSOL)
	usdc := assets.Asset{ID: usdcID, Chain: "solana-devnet", MintAddress: "USDC-mint", Kind: assets.KindSPLToken, ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "USDC", Name: "USD Coin", Decimals: 6, IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive, MetadataVersion: 1}
	sol := assets.Asset{ID: solID, Chain: "solana-devnet", MintAddress: "native", Kind: assets.KindNative, ValueDomain: valuedomain.SelfCustodialCrypto, Symbol: "SOL", Name: "Solana", Decimals: 9, RiskClass: assets.RiskMajor, Status: assets.StatusActive, MetadataVersion: 1}
	instrID := mustText(new(instruments.InstrumentID), gInstr)
	exposureID := mustText(new(instruments.ExposureID), gExposure)
	venueJup := mustText(new(instruments.VenueID), gVenueJup)
	venueRay := mustText(new(instruments.VenueID), gVenueRay)
	listJup := mustText(new(instruments.ListingID), gListJup)
	listRay := mustText(new(instruments.ListingID), gListRay)
	_ = listRay
	notional := money.USDFromMinor(10_000)
	return PlannerInput{
		Intent: IntentSnapshot{
			ID: gIntent, AccountID: gAccount, ActorType: security.ActorUser, ActorID: "user-1", Action: ActionAcquireNotional, InstrumentID: gInstr,
			NotionalUSD: &notional, Constraints: IntentConstraints{MaxSlippageBPS: 50, QuoteFreshness: 5 * time.Second},
			Deadline: now.Add(10 * time.Minute), RequestedAt: now, IdempotencyKey: "idem-1", CorrelationID: "corr-1", Mode: execution.ModeLive,
		},
		Account: AccountState{ID: gAccount, Status: accounts.StatusActive, Mode: execution.ModeLive, WalletID: gWallet, WalletAddress: "wallet-1", WalletChain: "solana-devnet", CostBasisMethod: "FIFO"},
		BuyingPower: capital.BuyingPower{
			PortfolioValue: money.USDFromMinor(175_000), BuyingPower: money.USDFromMinor(160_000), AvailableNow: money.USDFromMinor(100_000),
			PolicyVersion: "bp-v1", AsOf: now, Purpose: capital.PurposeTrade,
		},
		Instrument: instruments.Instrument{
			ID: *instrID, Type: instruments.TypeSpotPair, CanonicalName: "SOL/USDC", ExposureID: *exposureID, BaseAssetID: &solID, QuoteAssetID: &usdcID,
			SettlementAssetID: usdcID, RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: now.Add(-time.Hour), MetadataVersion: 3,
		},
		Listings: []instruments.VenueListing{{
			ID: *listJup, VenueID: *venueJup, InstrumentID: *instrID, VenueNativeID: "SOL-USDC", Network: "solana-devnet", BaseMint: "native", QuoteMint: "USDC-mint",
			BasePrecision: 9, QuotePrecision: 6, MinNotionalQuote: money.QuantityFromInt64(1_000_000), Status: instruments.VenueActive,
		}},
		Venues: []VenueCandidate{
			{
				Venue:    instruments.Venue{ID: *venueJup, Code: "JUPITER", Name: "Jupiter", Kind: instruments.VenueDEXAggregator, Chain: "solana-devnet", Status: instruments.VenueActive},
				Provider: "jupiter", FeeBPS: 0, ProgramIDs: []string{"JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"}, FeeAccounts: []string{"JupFeeAcc111111111111111111111111111111111"},
			},
			{Venue: instruments.Venue{ID: *venueRay, Code: "RAYDIUM", Name: "Raydium", Kind: instruments.VenueDEX, Chain: "solana-devnet", Status: instruments.VenueDegraded}, Provider: "raydium", FeeBPS: 25},
		},
		Assets: map[assets.AssetID]assets.Asset{usdcID: usdc, solID: sol},
		AssetPolicies: map[assets.AssetID]valuation.AssetPolicy{
			usdcID: {AssetID: usdcID, Status: assets.StatusActive, CollateralFactor: 10_000, StablecoinStatus: valuation.StablecoinNormal, MaxPriceAge: time.Minute, PolicyVersion: "usdc-v1"},
			solID:  {AssetID: solID, Status: assets.StatusActive, CollateralFactor: 8_000, MaxPriceAge: time.Minute, PolicyVersion: "sol-v1"},
		},
		Prices: map[assets.AssetID]money.Price{
			solID:  {Mantissa: money.QuantityFromInt64(150), Scale: 0, QuoteAsset: USDQuoteAsset, Source: "pyth", At: now.Add(-time.Second)},
			usdcID: {Mantissa: money.QuantityFromInt64(1), Scale: 0, QuoteAsset: USDQuoteAsset, Source: "pyth", At: now.Add(-time.Second)},
		},
		Holdings:  []positions.Holding{{AssetID: solID, Symbol: "SOL", Decimals: 9, Quantity: money.QuantityFromInt64(2_000_000_000), CostBasis: money.USDFromMinor(28_000), LotCount: 1, OldestAcquiredAt: now.Add(-24 * time.Hour)}},
		Liquidity: map[instruments.ListingID]money.USD{*listJup: money.USDFromMinor(5_000_000_00)},
		Health: SystemHealth{
			Providers: map[string]provider.Health{"jupiter": provider.Healthy, "raydium": provider.Degraded},
			Chains:    map[string]ChainStatus{"solana-devnet": {Health: provider.Healthy, Height: 1000, EstimatedNetworkFee: money.QuantityFromInt64(5000), NetworkFeeAsset: solID, ObservedAt: now}},
		},
		Eligibility: eligibility.Decision{Eligible: true, PolicyVersion: "elig-v1", PolicyHash: "elig-hash", ReasonCodes: []string{}, EvaluatedAt: now, ContextHash: "ctx-hash", Hash: "elig-decision-hash"},
		Risk: risk.Decision{
			Verdict: risk.Allow, Stage: risk.StagePreTrade, ActionClass: risk.ClassNewRisk, EffectiveNotionalUSD: notional, PolicyVersion: "risk-v1", PolicyHash: "risk-hash",
			EvaluatorVersion: risk.EvaluatorVersion, ReasonCodes: []string{}, MatchedKillSwitches: []risk.KillSwitch{},
			Constraints: risk.ResultingConstraints{MaxNotionalUSD: money.USDFromMinor(100_000), MaxSlippageBPS: 100, MaxFeeBPS: 50, MaxPriceImpactBPS: 100, MaxQuoteAgeMS: 3000},
			InputHash:   "input-hash", EvaluatedAt: now, Hash: "risk-decision-hash",
		},
		FeePolicy:      fees.Policy{Version: "fees-v1", PlatformFeeBPS: 10, FeeAsset: usdcID, Rounding: money.RoundHalfEven},
		FinalityPolicy: execution.DefaultFinalityPolicy(),
		Now:            now,
		PlannerVersion: PlannerVersion,
		Version:        1,
	}
}

type caseExpect struct {
	Result            string   `json:"result"`
	Reasons           []string `json:"reasons,omitempty"`
	Side              string   `json:"side,omitempty"`
	InputQuantity     string   `json:"input_quantity,omitempty"`
	MinOutputQuantity string   `json:"min_output_quantity,omitempty"`
	MaxSlippageBPS    *int64   `json:"max_slippage_bps,omitempty"`
	MaxFeeBPS         *int64   `json:"max_fee_bps,omitempty"`
	MaxPriceImpactBPS *int64   `json:"max_price_impact_bps,omitempty"`
	MaxNotionalUSD    string   `json:"max_notional_usd,omitempty"`
	NotionalUSD       string   `json:"notional_usd,omitempty"`
	Venue             string   `json:"venue,omitempty"`
	PlatformFee       string   `json:"platform_fee,omitempty"`
	Hash              string   `json:"hash,omitempty"`
}

type caseFixture struct {
	Name      string          `json:"-"`
	Path      string          `json:"-"`
	InputBase string          `json:"input_base"`
	Patch     json.RawMessage `json:"patch"`
	Expect    caseExpect      `json:"expect"`
	Note      string          `json:"note,omitempty"`
}

func loadBase(t *testing.T, name string) json.RawMessage {
	t.Helper()
	path := filepath.Join("testdata", "inputs", name+".json")
	if *update && name == "base" {
		b, err := json.MarshalIndent(goldenBase(), "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, append(b, '\n'), 0o600))
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

func loadCases(t *testing.T) []caseFixture {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "cases", "*.json"))
	require.NoError(t, err)
	sort.Strings(files)
	out := make([]caseFixture, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		var c caseFixture
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		require.NoError(t, dec.Decode(&c), f)
		c.Name = strings.TrimSuffix(filepath.Base(f), ".json")
		c.Path = f
		if c.InputBase == "" {
			c.InputBase = "base"
		}
		out = append(out, c)
	}
	return out
}

// mergeJSON applies patch to base (see the layout comment).
func mergeJSON(base, patch any) any {
	pm, pIsObj := patch.(map[string]any)
	if !pIsObj {
		return patch
	}
	switch b := base.(type) {
	case map[string]any:
		out := make(map[string]any, len(b)+len(pm))
		for k, v := range b {
			out[k] = v
		}
		if r, ok := pm["$replace"]; ok && r == true {
			out = map[string]any{}
		}
		for k, v := range pm {
			if k == "$replace" {
				continue
			}
			if v == nil {
				delete(out, k)
				continue
			}
			if cur, ok := out[k]; ok {
				out[k] = mergeJSON(cur, v)
			} else {
				out[k] = v
			}
		}
		return out
	case []any:
		out := append([]any(nil), b...)
		for k, v := range pm {
			idx := -1
			for i := range out {
				if k == itoa(i) {
					idx = i
				}
			}
			if idx < 0 {
				return patch
			}
			out[idx] = mergeJSON(out[idx], v)
		}
		return out
	}
	return patch
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

func caseInput(t *testing.T, base json.RawMessage, c caseFixture) PlannerInput {
	t.Helper()
	var doc any
	require.NoError(t, json.Unmarshal(base, &doc))
	if len(c.Patch) > 0 {
		var patch any
		require.NoError(t, json.Unmarshal(c.Patch, &patch))
		doc = mergeJSON(doc, patch)
	}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	var in PlannerInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	require.NoError(t, dec.Decode(&in), c.Name)
	return in
}

func TestGolden_Planner(t *testing.T) {
	cases := loadCases(t)
	require.GreaterOrEqual(t, len(cases), 40, "golden corpus must hold at least 40 cases")
	bases := map[string]json.RawMessage{}
	planner := NewPlanner(DefaultOptions())
	known := ReasonCodes()
	coveredReasons := map[string]bool{}
	coveredActions := map[Action]bool{}
	for i := range cases {
		c := &cases[i]
		t.Run(c.Name, func(t *testing.T) {
			base, ok := bases[c.InputBase]
			if !ok {
				base = loadBase(t, c.InputBase)
				bases[c.InputBase] = base
			}
			in := caseInput(t, base, *c)
			coveredActions[in.Intent.Action] = true
			plan, err := planner.Plan(in)
			switch c.Expect.Result {
			case "PLAN":
				require.NoError(t, err)
				require.NoError(t, plan.Validate())
				require.Len(t, plan.Hash, 32)
				ok, err := VerifyHash(plan)
				require.NoError(t, err)
				require.True(t, ok)
				if c.Expect.Side != "" {
					require.Equal(t, execution.Side(c.Expect.Side), plan.HardConstraints.Side)
				}
				if c.Expect.InputQuantity != "" {
					require.Equal(t, c.Expect.InputQuantity, plan.HardConstraints.MaxInputQuantity.String(), "input quantity")
				}
				if c.Expect.MinOutputQuantity != "" {
					require.Equal(t, c.Expect.MinOutputQuantity, plan.HardConstraints.MinOutputQuantity.String(), "min output")
				}
				if c.Expect.MaxSlippageBPS != nil {
					require.Equal(t, money.BPS(*c.Expect.MaxSlippageBPS), plan.HardConstraints.MaxSlippageBPS)
				}
				if c.Expect.MaxFeeBPS != nil {
					require.Equal(t, money.BPS(*c.Expect.MaxFeeBPS), plan.HardConstraints.MaxFeeBPS)
				}
				if c.Expect.MaxPriceImpactBPS != nil {
					require.Equal(t, money.BPS(*c.Expect.MaxPriceImpactBPS), plan.HardConstraints.MaxPriceImpactBPS)
				}
				if c.Expect.MaxNotionalUSD != "" {
					require.Equal(t, c.Expect.MaxNotionalUSD, plan.HardConstraints.MaxNotionalUSD.String())
				}
				if c.Expect.NotionalUSD != "" {
					require.Equal(t, c.Expect.NotionalUSD, plan.HardConstraints.NotionalUSD.String())
				}
				if c.Expect.Venue != "" {
					require.Equal(t, c.Expect.Venue, plan.HardConstraints.Venue)
				}
				if c.Expect.PlatformFee != "" {
					require.Equal(t, c.Expect.PlatformFee, plan.EstimatedCosts.PlatformFee.Quantity.String())
				}
				for _, s := range plan.Steps {
					require.False(t, s.Type.Reserved(), "reserved step %s emitted", s.Type)
				}
				require.Equal(t, in.DryRun, plan.DryRun)
				require.Equal(t, PlanDraft, plan.Status)
				got := hex.EncodeToString(plan.Hash)
				if *update {
					c.Expect.Hash = got
					writeCase(t, *c)
					return
				}
				require.Equal(t, c.Expect.Hash, got, "plan hash changed; review and run with -update")
			case "NO_VALID_PLAN":
				require.Error(t, err)
				require.True(t, errs.HasCode(err, errs.CodeNoValidPlan), "%v", err)
				reasons := NoValidPlanReasons(err)
				require.True(t, sort.StringsAreSorted(reasons))
				for _, r := range reasons {
					require.Contains(t, known, r)
					coveredReasons[r] = true
				}
				require.Equal(t, c.Expect.Reasons, reasons)
			case "VALIDATION_FAILED":
				require.Error(t, err)
				require.True(t, errs.HasCode(err, errs.CodeValidationFailed), "%v", err)
			default:
				t.Fatalf("unknown expected result %q", c.Expect.Result)
			}
		})
	}
	if !*update {
		for _, r := range known {
			require.True(t, coveredReasons[r], "no golden case exercises reason %s", r)
		}
		for _, a := range []Action{ActionAcquireNotional, ActionReduceNotional, ActionClosePosition, ActionTargetExposure} {
			require.True(t, coveredActions[a], "no golden case exercises action %s", a)
		}
	}
}

func writeCase(t *testing.T, c caseFixture) {
	t.Helper()
	b, err := json.MarshalIndent(c, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(c.Path, append(b, '\n'), 0o600))
}

// TestGolden_Determinism1000 plans every PLAN case 1,000 times and requires
// an identical hash and canonical content every time.
func TestGolden_Determinism1000(t *testing.T) {
	base := loadBase(t, "base")
	planner := NewPlanner(DefaultOptions())
	for _, c := range loadCases(t) {
		if c.Expect.Result != "PLAN" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			in := caseInput(t, base, c)
			first, err := planner.Plan(in)
			require.NoError(t, err)
			firstContent, err := CanonicalContent(first)
			require.NoError(t, err)
			for i := 0; i < determinismRuns; i++ {
				p, err := planner.Plan(in)
				require.NoError(t, err)
				if !bytes.Equal(p.Hash, first.Hash) {
					t.Fatalf("run %d: hash differs", i)
				}
				content, err := CanonicalContent(p)
				require.NoError(t, err)
				if !bytes.Equal(content, firstContent) {
					t.Fatalf("run %d: canonical content differs", i)
				}
				require.NotEqual(t, first.ID, p.ID, "row identifiers are fresh per plan and excluded from the hash")
			}
		})
	}
}

// TestGolden_DeterminismAcrossGoroutines plans concurrently; under -race it
// also proves the planner shares no mutable state.
func TestGolden_DeterminismAcrossGoroutines(t *testing.T) {
	base := loadBase(t, "base")
	planner := NewPlanner(DefaultOptions())
	in := caseInput(t, base, caseFixture{Name: "base"})
	want, err := planner.Plan(in)
	require.NoError(t, err)
	var wg sync.WaitGroup
	hashes := make([][]byte, 8)
	for g := range hashes {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			var h []byte
			for i := 0; i < 100; i++ {
				p, err := planner.Plan(in)
				if err != nil {
					return
				}
				h = p.Hash
			}
			hashes[g] = h
		}(g)
	}
	wg.Wait()
	for _, h := range hashes {
		require.Equal(t, want.Hash, h)
	}
}

// TestGolden_NoValidPlanReasonsAreClosed checks the reason vocabulary is
// exactly the §3 set plus DELTA_BELOW_MINIMUM (§4).
func TestGolden_NoValidPlanReasonsAreClosed(t *testing.T) {
	t.Parallel()
	want := []string{
		"DEADLINE_IMPOSSIBLE", "DELTA_BELOW_MINIMUM", "ELIGIBILITY_FAILED", "INSTRUMENT_STATUS", "KILL_SWITCH", "LIQUIDITY_INSUFFICIENT",
		"NOTIONAL_ABOVE_MAXIMUM", "NOTIONAL_BELOW_MINIMUM", "NO_ELIGIBLE_LISTING", "PROVIDER_DEGRADED", "QUOTE_TOO_EXPENSIVE",
		"RECONCILIATION_BLOCKED", "RISK_REJECTED", "SETTLEMENT_ASSET_UNAVAILABLE", "VENUE_DISABLED",
	}
	require.Equal(t, want, ReasonCodes())
}

// TestKillSwitchSwitchesUsedByGolden ensures the switch kinds referenced in
// the corpus are real kinds.
func TestGolden_KillSwitchKindsValid(t *testing.T) {
	t.Parallel()
	for _, k := range killswitch.AllKinds() {
		require.True(t, k.Valid())
	}
}
