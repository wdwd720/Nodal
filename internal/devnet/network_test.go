package devnet

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

func TestNetwork_Registry(t *testing.T) {
	n := NewNetwork()
	eth, err := n.AddChain(testConfig("sbx-ethereum"))
	require.NoError(t, err)
	sol, err := n.AddChain(testConfig("sbx-solana"))
	require.NoError(t, err)
	require.Equal(t, []ChainID{"sbx-ethereum", "sbx-solana"}, n.ChainIDs())

	got, err := n.Chain("sbx-solana")
	require.NoError(t, err)
	require.Same(t, sol, got)
	require.Equal(t, ChainID("sbx-ethereum"), eth.ID())

	_, err = n.AddChain(testConfig("sbx-solana"))
	requireReason(t, err, errs.CodeConflict, CodeChainExists)
	_, err = n.Chain("sbx-bitcoin")
	requireReason(t, err, errs.CodeNotFound, CodeChainNotFound)
	_, err = n.AddChain(testConfig("Bad"))
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidChainID)
	require.Len(t, n.ChainIDs(), 2)
}

func TestNetwork_StateHashCoversEveryChain(t *testing.T) {
	build := func() (*Network, *Chain) {
		n := NewNetwork()
		_, err := n.AddChain(testConfig("sbx-ethereum"))
		require.NoError(t, err)
		sol, err := n.AddChain(testConfig("sbx-solana"))
		require.NoError(t, err)
		return n, sol
	}
	n1, sol1 := build()
	n2, _ := build()
	require.Equal(t, n1.StateHash(), n2.StateHash())
	require.NoError(t, sol1.Airdrop(testKey("x").PublicKey(), 1))
	require.NotEqual(t, n1.StateHash(), n2.StateHash())
}

func TestChainConfig_Validate(t *testing.T) {
	ok := testConfig("sbx-solana")
	require.NoError(t, ok.Validate())
	require.NoError(t, DefaultChainConfig("sbx-solana").Validate())

	cases := map[string]func(*ChainConfig){
		"zero window":           func(c *ChainConfig) { c.BlockhashWindow = 0 },
		"zero depth":            func(c *ChainConfig) { c.FinalityDepth = 0 },
		"zero fee collector":    func(c *ChainConfig) { c.FeeCollector = PublicKey{} },
		"zero bridge authority": func(c *ChainConfig) { c.Bridge = &BridgeConfig{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := ok
			mutate(&cfg)
			requireReason(t, cfg.Validate(), errs.CodeValidationFailed, CodeInvalidConfig)
			_, err := NewChain(cfg)
			requireReason(t, err, errs.CodeValidationFailed, CodeInvalidConfig)
		})
	}
}

func TestChain_ConfigIsImmutableFromOutside(t *testing.T) {
	cfg := testConfig("sbx-solana")
	cfg.Bridge = &BridgeConfig{Authority: testKey("attester").PublicKey()}
	c, err := NewChain(cfg)
	require.NoError(t, err)
	cfg.Bridge.Authority = testKey("someone else").PublicKey() // the caller's copy
	got := c.Config()
	require.Equal(t, testKey("attester").PublicKey(), got.Bridge.Authority)
	got.Bridge.Authority = PublicKey{} // the returned copy
	require.Equal(t, testKey("attester").PublicKey(), c.Config().Bridge.Authority)
}

func TestTxState_Names(t *testing.T) {
	names := map[TxState]string{
		TxNotFound: "NOT_FOUND", TxPending: "PENDING", TxConfirmed: "CONFIRMED",
		TxFinalized: "FINALIZED", TxFailed: "FAILED", TxState(99): "UNKNOWN",
	}
	for s, n := range names {
		require.Equal(t, n, s.String())
	}
}
