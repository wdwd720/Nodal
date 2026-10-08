package devnet

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkSubmitProduceBlock measures end-to-end throughput: each iteration
// submits a block's worth of signed token transfers (decode, rule checks and
// two Ed25519 verifications each) and produces the block that executes them.
// Signing happens off the clock.
func BenchmarkSubmitProduceBlock(b *testing.B) {
	const perBlock = 64
	cfg := DefaultChainConfig("sbx-bench")
	c, err := NewChain(cfg)
	require.NoError(b, err)
	payer, alice, bob, issuer := testKey("bench-payer"), testKey("bench-alice"), testKey("bench-bob"), testKey("bench-issuer")
	usd := testAddr("bench-usd")
	require.NoError(b, c.Airdrop(payer.PublicKey(), 1<<62))
	require.NoError(b, c.CreateMint(MintConfig{Address: usd, Decimals: 6, Authority: issuer.PublicKey()}))
	issue := Message{
		ChainID: cfg.ID, RecentBlockhash: c.LatestBlockhash(), FeePayer: payer.PublicKey(),
		Instructions: []Instruction{TokenMintTo{Mint: usd, ToOwner: alice.PublicKey(), Amount: 1 << 62, Authority: issuer.PublicKey()}},
	}
	_, err = c.Submit(signed(b, issue, payer, issuer))
	require.NoError(b, err)
	c.ProduceBlock()

	n := 0
	batch := make([][]byte, perBlock)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		bh := c.LatestBlockhash()
		for j := range batch {
			n++
			m := Message{
				ChainID: cfg.ID, RecentBlockhash: bh, FeePayer: payer.PublicKey(),
				Instructions: []Instruction{
					TokenTransfer{Mint: usd, FromOwner: alice.PublicKey(), ToOwner: bob.PublicKey(), Amount: 1},
					Memo{Data: []byte(strconv.Itoa(n))},
				},
			}
			batch[j] = signed(b, m, payer, alice)
		}
		b.StartTimer()
		for _, raw := range batch {
			if _, err := c.Submit(raw); err != nil {
				b.Fatal(err)
			}
		}
		if got := len(c.ProduceBlock().TxIDs); got != perBlock {
			b.Fatalf("block included %d of %d transactions", got, perBlock)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*perBlock), "ns/tx")
	if got := c.TokenBalance(bob.PublicKey(), usd); got != uint64(n) {
		b.Fatalf("bob holds %d, want %d", got, n)
	}
}
