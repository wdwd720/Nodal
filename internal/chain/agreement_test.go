package chain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/money"
)

var allFinalities = []chain.Finality{chain.FinalitySubmitted, chain.FinalityObserved, chain.FinalityConfirmed, chain.FinalityFinalized}

func baseObs(source string, c chain.Commitment) chain.TxObservation {
	bt := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	return chain.TxObservation{
		Signature: "sig1", Found: true, Slot: 1000, BlockTime: &bt, Commitment: c, Version: chain.VersionV0,
		AccountKeys: []string{"W", "ATA1", "ATA2", "JUP"},
		TokenBalanceDeltas: []chain.TokenDelta{
			{Owner: "W", Mint: "USDC", TokenAccount: "ATA1", Pre: q(1_000_000), Post: q(0), Decimals: 6},
			{Owner: "W", Mint: "BONK", TokenAccount: "ATA2", Pre: q(0), Post: q(123_456_789), Decimals: 5},
		},
		LamportDeltas: []chain.LamportDelta{{Account: "W", Pre: q(10_000_000), Post: q(9_995_000)}},
		Fee:           q(5000),
		Source:        source,
	}
}

func notFound(source string) chain.TxObservation {
	return chain.TxObservation{Signature: "sig1", Source: source}
}

func TestAgreementPolicy_Validate(t *testing.T) {
	t.Parallel()
	require.NoError(t, chain.DefaultPolicy().Validate())
	bad := chain.DefaultPolicy()
	bad.RequireBothFor = chain.FinalityObserved
	require.Error(t, bad.Validate(), "a single observer may never assert more than CONFIRMED")
	bad = chain.DefaultPolicy()
	bad.PreferChainRPCFor = "x"
	require.Error(t, bad.Validate())
	bad = chain.DefaultPolicy()
	bad.OnDisagreement = "IGNORE"
	require.Error(t, bad.Validate())
	require.Error(t, chain.AgreementPolicy{}.Validate())
}

// TestAgreementPolicy_Matrix walks every (primary state, secondary state)
// pair against every required finality.
func TestAgreementPolicy_Matrix(t *testing.T) {
	t.Parallel()
	pol := chain.DefaultPolicy()
	type side struct {
		name string
		obs  chain.TxObservation
	}
	wrong := baseObs("helius", chain.CommitmentFinalized)
	wrong.TokenBalanceDeltas[1].Post = q(123_456_788)
	sides := func(src string) []side {
		return []side{
			{"absent", notFound(src)},
			{"confirmed", baseObs(src, chain.CommitmentConfirmed)},
			{"finalized", baseObs(src, chain.CommitmentFinalized)},
		}
	}
	for _, p := range sides("helius") {
		for _, s := range sides("rpc-fallback") {
			for _, req := range allFinalities {
				res := pol.Resolve(p.obs, s.obs, req)
				name := p.name + "/" + s.name + "/" + string(req)
				switch {
				case p.name == "absent" && s.name == "absent":
					require.Equal(t, chain.NotFound, res.State, name)
					require.Equal(t, chain.FinalitySubmitted, res.Finality, name)
					require.False(t, res.BlockDependent, name)
					require.False(t, res.Satisfied, name)
					require.Nil(t, res.Observation, name)
				case p.name == "absent" || s.name == "absent":
					require.Equal(t, chain.Disagreed, res.State, name)
					require.True(t, res.BlockDependent, name)
					require.Equal(t, chain.FinalitySubmitted, res.Finality, name)
					require.Equal(t, []string{"found"}, res.Differences, name)
					require.False(t, res.Satisfied, name)
				default:
					require.Equal(t, chain.Agreed, res.State, name)
					require.False(t, res.BlockDependent, name)
					want := chain.MinFinality(p.obs.Finality(), s.obs.Finality())
					require.Equal(t, want, res.Finality, name)
					require.Equal(t, want.AtLeast(req), res.Satisfied, name)
					require.NotNil(t, res.Observation, name)
					if p.name == "finalized" && s.name == "finalized" {
						require.Equal(t, chain.FinalityFinalized, res.Finality, name)
					} else {
						require.NotEqual(t, chain.FinalityFinalized, res.Finality, "FINALIZED requires both: "+name)
					}
					if p.name != s.name {
						require.Equal(t, "rpc-fallback", res.Observation.Source, "commitment conflict reports the chain RPC: "+name)
					}
				}
				require.NotEmpty(t, res.Detail, name)
				require.NotNil(t, res.Primary, name)
				require.NotNil(t, res.Secondary, name)
			}
		}
	}
	// Wrong deltas on one side: DISAGREED regardless of commitment.
	for _, req := range allFinalities {
		res := pol.Resolve(wrong, baseObs("rpc-fallback", chain.CommitmentFinalized), req)
		require.Equal(t, chain.Disagreed, res.State)
		require.True(t, res.BlockDependent)
		require.Equal(t, []string{"token_balance_deltas"}, res.Differences)
		require.Contains(t, res.Detail, string(chain.BlockDependentActivity))
	}
}

func TestAgreementPolicy_PreferPrimaryOnConflict(t *testing.T) {
	t.Parallel()
	pol := chain.DefaultPolicy()
	pol.PreferChainRPCFor = chain.PreferPrimary
	res := pol.Resolve(baseObs("helius", chain.CommitmentFinalized), baseObs("rpc-fallback", chain.CommitmentConfirmed), chain.FinalityConfirmed)
	require.Equal(t, chain.Agreed, res.State)
	require.Equal(t, "helius", res.Observation.Source)
	require.Equal(t, chain.FinalityConfirmed, res.Finality, "preference never raises finality")
}

func TestAgreementPolicy_DifferentSignatures(t *testing.T) {
	t.Parallel()
	a := baseObs("helius", chain.CommitmentFinalized)
	b := baseObs("rpc-fallback", chain.CommitmentFinalized)
	b.Signature = "sig2"
	res := chain.DefaultPolicy().Resolve(a, b, chain.FinalityConfirmed)
	require.Equal(t, chain.Disagreed, res.State)
	require.Equal(t, []string{"signature"}, res.Differences)
}

func TestAgreementPolicy_ResolveSingle(t *testing.T) {
	t.Parallel()
	pol := chain.DefaultPolicy()
	for _, side := range []chain.Side{chain.SidePrimary, chain.SideSecondary} {
		for _, req := range allFinalities {
			res := pol.ResolveSingle(baseObs("helius", chain.CommitmentFinalized), side, req, "rpc-fallback is DISABLED")
			require.True(t, res.Degraded)
			require.Equal(t, chain.FinalityConfirmed, res.Finality, "single observer is capped at CONFIRMED")
			if side == chain.SidePrimary {
				require.Equal(t, chain.PrimaryOnly, res.State)
				require.NotNil(t, res.Primary)
				require.Nil(t, res.Secondary)
			} else {
				require.Equal(t, chain.SecondaryOnly, res.State)
				require.NotNil(t, res.Secondary)
				require.Nil(t, res.Primary)
			}
			require.Equal(t, req != chain.FinalityFinalized, res.Satisfied)
			require.Equal(t, req == chain.FinalityFinalized, res.BlockDependent, "a FINALIZED requirement blocks in degraded mode")
			require.Contains(t, res.Detail, "degraded")
			require.Contains(t, res.Detail, "DISABLED")
		}
		res := pol.ResolveSingle(notFound("helius"), side, chain.FinalityConfirmed, "x unavailable")
		require.Equal(t, chain.NotFound, res.State)
		require.True(t, res.BlockDependent, "absence is never proven by one observer")
		require.True(t, res.Degraded)
		require.Nil(t, res.Observation)
	}
	// A processed-only observation stays OBSERVED.
	res := pol.ResolveSingle(baseObs("helius", chain.CommitmentProcessed), chain.SidePrimary, chain.FinalityObserved, "r")
	require.Equal(t, chain.FinalityObserved, res.Finality)
	require.True(t, res.Satisfied)
}

func TestEconomicDifferences(t *testing.T) {
	t.Parallel()
	a := baseObs("a", chain.CommitmentConfirmed)
	b := baseObs("b", chain.CommitmentFinalized)
	b.RawRef, b.ObservedAt, b.ReceivedAt = "other", time.Now(), time.Now()
	require.Empty(t, chain.EconomicDifferences(a, b), "commitment, source, timestamps and raw refs are not economic")

	// Reordered deltas and keys are the same economics.
	b.TokenBalanceDeltas[0], b.TokenBalanceDeltas[1] = b.TokenBalanceDeltas[1], b.TokenBalanceDeltas[0]
	b.AccountKeys = []string{"JUP", "ATA2", "ATA1", "W"}
	require.Empty(t, chain.EconomicDifferences(a, b))

	// Missing block time on one side is tolerated; different block times are not.
	b.BlockTime = nil
	require.Empty(t, chain.EconomicDifferences(a, b))
	other := a.BlockTime.Add(time.Second)
	b.BlockTime = &other
	require.Equal(t, []string{"block_time"}, chain.EconomicDifferences(a, b))
	b.BlockTime = a.BlockTime

	b.Slot++
	b.Err = "x"
	b.Fee = q(1)
	b.Version = chain.VersionLegacy
	b.LamportDeltas = nil
	b.AccountKeys = append(b.AccountKeys, "EXTRA")
	b.TokenBalanceDeltas = b.TokenBalanceDeltas[:1]
	require.Equal(t, []string{"account_keys", "err", "fee", "lamport_deltas", "slot", "token_balance_deltas", "version"}, chain.EconomicDifferences(a, b))
}

// TestProp_NeverAgreedWhenAnyFieldDiffers mutates one economic field of an
// otherwise identical pair and asserts the policy never says AGREED.
func TestProp_NeverAgreedWhenAnyFieldDiffers(t *testing.T) {
	t.Parallel()
	pol := chain.DefaultPolicy()
	rapid.Check(t, func(rt *rapid.T) {
		p := baseObs("helius", rapid.SampledFrom([]chain.Commitment{chain.CommitmentConfirmed, chain.CommitmentFinalized}).Draw(rt, "pc"))
		s := baseObs("rpc-fallback", rapid.SampledFrom([]chain.Commitment{chain.CommitmentConfirmed, chain.CommitmentFinalized}).Draw(rt, "sc"))
		s.TokenBalanceDeltas = append([]chain.TokenDelta(nil), s.TokenBalanceDeltas...)
		s.LamportDeltas = append([]chain.LamportDelta(nil), s.LamportDeltas...)
		req := rapid.SampledFrom(allFinalities).Draw(rt, "req")
		delta := money.QuantityFromInt64(rapid.Int64Range(1, 1_000_000_000).Draw(rt, "delta"))
		if rapid.Bool().Draw(rt, "neg") {
			delta = delta.Neg()
		}
		switch mutation := rapid.IntRange(0, 8).Draw(rt, "mutation"); mutation {
		case 0:
			s.Found = false
		case 1:
			s.Slot += uint64(rapid.IntRange(1, 100).Draw(rt, "slot"))
		case 2:
			s.Err = rapid.SampledFrom([]string{"BlockhashNotFound", `{"InstructionError":[0,{"Custom":1}]}`}).Draw(rt, "err")
		case 3:
			s.Fee = s.Fee.Add(delta)
		case 4:
			i := rapid.IntRange(0, len(s.TokenBalanceDeltas)-1).Draw(rt, "i")
			s.TokenBalanceDeltas[i].Post = s.TokenBalanceDeltas[i].Post.Add(delta)
		case 5:
			i := rapid.IntRange(0, len(s.TokenBalanceDeltas)-1).Draw(rt, "i")
			s.TokenBalanceDeltas[i].Pre = s.TokenBalanceDeltas[i].Pre.Add(delta)
		case 6:
			s.LamportDeltas[0].Post = s.LamportDeltas[0].Post.Add(delta)
		case 7:
			s.TokenBalanceDeltas = append(s.TokenBalanceDeltas, chain.TokenDelta{Owner: "X", Mint: "Y", TokenAccount: "Z", Pre: q(0), Post: delta})
		case 8:
			s.AccountKeys = append([]string{"EXTRA"}, s.AccountKeys...)
		}
		res := pol.Resolve(p, s, req)
		if res.State == chain.Agreed {
			rt.Fatalf("AGREED despite a mutation: %+v", res)
		}
		if res.State != chain.Disagreed {
			rt.Fatalf("expected DISAGREED, got %s", res.State)
		}
		if !res.BlockDependent || res.Satisfied || res.Finality != chain.FinalitySubmitted {
			rt.Fatalf("disagreement must block and never satisfy: %+v", res)
		}
	})
}

// TestProp_FinalityNeverExceedsEitherObserver: the resolved finality is
// never stronger than the weaker observation, and FINALIZED needs both.
func TestProp_FinalityNeverExceedsEitherObserver(t *testing.T) {
	t.Parallel()
	pol := chain.DefaultPolicy()
	comms := []chain.Commitment{"", chain.CommitmentProcessed, chain.CommitmentConfirmed, chain.CommitmentFinalized}
	rapid.Check(t, func(rt *rapid.T) {
		p := baseObs("helius", rapid.SampledFrom(comms).Draw(rt, "pc"))
		s := baseObs("rpc-fallback", rapid.SampledFrom(comms).Draw(rt, "sc"))
		p.Found = rapid.Bool().Draw(rt, "pf")
		s.Found = rapid.Bool().Draw(rt, "sf")
		req := rapid.SampledFrom(allFinalities).Draw(rt, "req")
		res := pol.Resolve(p, s, req)
		weakest := chain.MinFinality(p.Finality(), s.Finality())
		if res.Finality.Rank() > weakest.Rank() {
			rt.Fatalf("finality %s exceeds weakest observation %s", res.Finality, weakest)
		}
		if res.Finality == chain.FinalityFinalized && (p.Commitment != chain.CommitmentFinalized || s.Commitment != chain.CommitmentFinalized) {
			rt.Fatalf("FINALIZED without both finalized")
		}
		if res.Satisfied && !res.Finality.AtLeast(req) {
			rt.Fatalf("satisfied below requirement")
		}
		single := pol.ResolveSingle(p, chain.SidePrimary, req, "r")
		if single.Finality.Rank() > chain.FinalityConfirmed.Rank() {
			rt.Fatalf("single observer exceeded CONFIRMED: %s", single.Finality)
		}
		if single.Finality.Rank() > p.Finality().Rank() {
			rt.Fatalf("single finality %s exceeds observation %s", single.Finality, p.Finality())
		}
	})
}

func TestAgreementPolicy_ResolveBalances(t *testing.T) {
	t.Parallel()
	pol := chain.DefaultPolicy()
	p := []chain.BalanceObservation{
		{Owner: "W", Mint: "USDC", TokenAccount: "A1", Amount: q(100), Slot: 50, Source: "helius"},
		{Owner: "W", Mint: "SOL", TokenAccount: "W", Amount: q(5), Slot: 50, Source: "helius"},
	}
	s := []chain.BalanceObservation{
		{Owner: "W", Mint: "SOL", TokenAccount: "W", Amount: q(5), Slot: 47, Source: "rpc"},
		{Owner: "W", Mint: "USDC", TokenAccount: "A1", Amount: q(100), Slot: 47, Source: "rpc"},
	}
	res := pol.ResolveBalances(p, s)
	require.Equal(t, chain.Agreed, res.State)
	require.False(t, res.BlockDependent)
	require.Equal(t, uint64(3), res.SlotSkew)
	require.Len(t, res.Balances, 2)
	require.Equal(t, "SOL", res.Balances[0].Mint, "canonical order")

	s[1].Amount = q(99)
	res = pol.ResolveBalances(p, s)
	require.Equal(t, chain.Disagreed, res.State)
	require.True(t, res.BlockDependent)
	require.Equal(t, []string{"USDC/A1"}, res.Differences)

	res = pol.ResolveBalances(p, s[:1])
	require.Equal(t, chain.Disagreed, res.State, "a balance missing on one side is a disagreement")
	res = pol.ResolveBalances(p[:1], p)
	require.Equal(t, chain.Disagreed, res.State)

	single := pol.ResolveBalancesSingle(p, chain.SideSecondary, "helius is DISABLED")
	require.Equal(t, chain.SecondaryOnly, single.State)
	require.True(t, single.Degraded)
	require.Len(t, single.Balances, 2)
}
