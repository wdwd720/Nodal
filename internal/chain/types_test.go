package chain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/money"
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func TestCommitment_ParseAndRank(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want chain.Commitment
		ok   bool
	}{
		{"processed", chain.CommitmentProcessed, true},
		{" Confirmed ", chain.CommitmentConfirmed, true},
		{"FINALIZED", chain.CommitmentFinalized, true},
		{"recent", "", false},
		{"", "", false},
	} {
		got, err := chain.ParseCommitment(tc.in)
		if !tc.ok {
			require.Error(t, err, tc.in)
			continue
		}
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
	require.Less(t, chain.CommitmentProcessed.Rank(), chain.CommitmentConfirmed.Rank())
	require.Less(t, chain.CommitmentConfirmed.Rank(), chain.CommitmentFinalized.Rank())
	require.Equal(t, -1, chain.Commitment("x").Rank())
	require.False(t, chain.Commitment("x").Valid())
}

func TestFinality_MappingAndOrdering(t *testing.T) {
	t.Parallel()
	require.Equal(t, chain.FinalityObserved, chain.FinalityFromCommitment(chain.CommitmentProcessed))
	require.Equal(t, chain.FinalityConfirmed, chain.FinalityFromCommitment(chain.CommitmentConfirmed))
	require.Equal(t, chain.FinalityFinalized, chain.FinalityFromCommitment(chain.CommitmentFinalized))
	require.Equal(t, chain.FinalitySubmitted, chain.FinalityFromCommitment(""))

	levels := []chain.Finality{chain.FinalitySubmitted, chain.FinalityObserved, chain.FinalityConfirmed, chain.FinalityFinalized}
	for i := 1; i < len(levels); i++ {
		require.True(t, levels[i].AtLeast(levels[i-1]))
		require.False(t, levels[i-1].AtLeast(levels[i]))
		require.Equal(t, levels[i-1], chain.MinFinality(levels[i], levels[i-1]))
		require.Equal(t, levels[i-1], chain.MinFinality(levels[i-1], levels[i]))
	}
	require.False(t, chain.Finality("BOGUS").Valid())
	require.Equal(t, chain.FinalitySubmitted, chain.TxObservation{Found: false, Commitment: chain.CommitmentFinalized}.Finality())
	require.Equal(t, chain.FinalityFinalized, chain.TxObservation{Found: true, Commitment: chain.CommitmentFinalized}.Finality())
}

func TestTxObservation_Helpers(t *testing.T) {
	t.Parallel()
	obs := chain.TxObservation{
		Found:       true,
		AccountKeys: []string{"W1", "P1"},
		TokenBalanceDeltas: []chain.TokenDelta{
			{Owner: "W2", Mint: "M", TokenAccount: "T2", Pre: q(5), Post: q(7)},
			{Owner: "W1", Mint: "M", TokenAccount: "T1", Pre: q(10), Post: q(4)},
		},
		LamportDeltas: []chain.LamportDelta{{Account: "W3", Pre: q(3), Post: q(1)}},
	}
	require.True(t, obs.Touches("W1"))
	require.True(t, obs.Touches("W2"))
	require.True(t, obs.Touches("W3"))
	require.False(t, obs.Touches("W4"))
	require.True(t, obs.Succeeded())
	obs.Err = "BlockhashNotFound"
	require.False(t, obs.Succeeded())

	ds := obs.DeltasFor("W1")
	require.Len(t, ds, 1)
	require.Equal(t, "-6", ds[0].Delta().String())
	require.Equal(t, "-2", obs.LamportDeltas[0].Delta().String())
	require.Equal(t, "-2", chain.SumLamportDeltas(obs.LamportDeltas).String())

	all := append([]chain.TokenDelta(nil), obs.TokenBalanceDeltas...)
	chain.SortTokenDeltas(all)
	require.Equal(t, "T1", all[0].TokenAccount)
}

func TestSequenceOf(t *testing.T) {
	t.Parallel()
	require.Equal(t, uint64(5)<<20|7, chain.SequenceOf(5, 7))
	require.Equal(t, uint64(5)<<20|0xFFFFF, chain.SequenceOf(5, 0xFFFFFFFF), "index is masked to 20 bits")
	require.Less(t, chain.SequenceOf(5, 0xFFFFF), chain.SequenceOf(6, 0), "sequence is monotonic across slots")
}

func TestSortBalances(t *testing.T) {
	t.Parallel()
	bs := []chain.BalanceObservation{
		{Mint: "B", TokenAccount: "2"}, {Mint: "A", TokenAccount: "9"}, {Mint: "B", TokenAccount: "1"},
	}
	chain.SortBalances(bs)
	require.Equal(t, []string{"A/9", "B/1", "B/2"}, []string{bs[0].Mint + "/" + bs[0].TokenAccount, bs[1].Mint + "/" + bs[1].TokenAccount, bs[2].Mint + "/" + bs[2].TokenAccount})
	_ = time.Now
}
