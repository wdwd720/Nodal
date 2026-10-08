package devnethost

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnet"
)

// TestProp_ReplayIsDeterministic: for any sequence of submissions, block
// productions, faults and reorgs, a fresh host replaying the history reaches
// the same StateHash and reports the same status for every transaction.
func TestProp_ReplayIsDeterministic(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		store := newMemStore()
		h, err := Open(ctx, store, testConfig(config.EnvTest), testClock())
		require.NoError(rt, err)
		payer, issuer, usd := testKey("payer"), testKey("issuer"), testAddr("usd")
		for _, c := range Chains() {
			require.NoError(rt, h.Airdrop(ctx, c, payer.PublicKey(), 10_000_000))
		}
		require.NoError(rt, h.EnsureMint(ctx, ChainSolana, devnet.MintConfig{Address: usd, Decimals: 6, Authority: issuer.PublicKey()}))

		var ids []devnet.TxID
		n := 0
		sign := func(chain devnet.ChainID, ixs []devnet.Instruction, signers ...devnet.Keypair) []byte {
			bh, err := h.LatestBlockhash(ctx, chain)
			require.NoError(rt, err)
			tx, err := devnet.Sign(devnet.Message{ChainID: chain, RecentBlockhash: bh, FeePayer: payer.PublicKey(), Instructions: ixs}, signers...)
			require.NoError(rt, err)
			raw, err := tx.Encode()
			require.NoError(rt, err)
			return raw
		}
		steps := rapid.IntRange(1, 40).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			chain := rapid.SampledFrom(Chains()).Draw(rt, "chain")
			switch rapid.IntRange(0, 9).Draw(rt, "op") {
			case 0, 1:
				n++
				id, err := h.Submit(ctx, chain, sign(chain, []devnet.Instruction{devnet.Memo{Data: []byte(fmt.Sprintf("m%d", n))}}, payer))
				if err == nil || devnet.ReasonOf(err) == devnet.CodeSubmissionTimeout {
					ids = append(ids, id)
				}
			case 2:
				n++
				owner := testKey(fmt.Sprintf("o%d", rapid.IntRange(0, 3).Draw(rt, "owner"))).PublicKey()
				amount := rapid.Uint64Range(1, 1_000).Draw(rt, "amount")
				id, err := h.Submit(ctx, ChainSolana, sign(ChainSolana, []devnet.Instruction{
					devnet.TokenMintTo{Mint: usd, ToOwner: owner, Amount: amount, Authority: issuer.PublicKey()},
					devnet.Memo{Data: []byte(fmt.Sprintf("m%d", n))},
				}, payer, issuer))
				if err == nil || devnet.ReasonOf(err) == devnet.CodeSubmissionTimeout {
					ids = append(ids, id)
				}
			case 3, 4:
				_, err := h.ProduceBlocks(ctx, chain, rapid.Uint64Range(1, 4).Draw(rt, "blocks"))
				require.NoError(rt, err)
			case 5:
				require.NoError(rt, h.DropNext(ctx, chain, rapid.Uint64Range(1, 2).Draw(rt, "drops")))
			case 6:
				if rapid.Bool().Draw(rt, "land") {
					require.NoError(rt, h.TimeoutButLand(ctx, chain))
				} else {
					require.NoError(rt, h.TimeoutAndDrop(ctx, chain))
				}
			case 7:
				require.NoError(rt, h.DelayInclusion(ctx, chain, rapid.Uint64Range(0, 2).Draw(rt, "delay")))
			case 8:
				require.NoError(rt, h.ClearFaults(ctx, chain))
			case 9:
				mode := rapid.SampledFrom([]devnet.ReorgMode{devnet.ReorgRequeue, devnet.ReorgDrop}).Draw(rt, "mode")
				_, err := h.Reorg(ctx, chain, rapid.Uint64Range(1, 3).Draw(rt, "depth"), mode)
				require.NoError(rt, err)
			}
		}

		replayed, err := Open(ctx, store, testConfig(config.EnvTest), testClock())
		require.NoError(rt, err)
		a, err := h.StateHash(ctx)
		require.NoError(rt, err)
		b, err := replayed.StateHash(ctx)
		require.NoError(rt, err)
		require.Equal(rt, a, b)
		for _, id := range ids {
			for _, chain := range Chains() {
				x, err := h.Status(ctx, chain, id)
				require.NoError(rt, err)
				y, err := replayed.Status(ctx, chain, id)
				require.NoError(rt, err)
				require.Equal(rt, x, y)
			}
		}
	})
}

// FuzzReplayEvent: replaying any stored event -- arguments, bytes and outcome
// as a hostile or corrupted row could carry them -- never panics; it either
// applies or refuses.
func FuzzReplayEvent(f *testing.F) {
	f.Add(uint8(1), []byte(`{"address":"x","decimals":6,"authority":"y","frozen":false}`), []byte{}, uint8(0))
	f.Add(uint8(2), []byte(`{"to":"11111111111111111111111111111111","lamports":"5"}`), []byte{}, uint8(0))
	f.Add(uint8(4), []byte(`{"tx_id":"abc"}`), []byte("ndvt\x01"), uint8(1))
	f.Add(uint8(5), []byte(`{"blocks":"1","tip_height":"1","tip_hash":"x"}`), []byte{}, uint8(0))
	f.Add(uint8(11), []byte(`{"depth":"1","mode":"DROP","removed":"0"}`), []byte{}, uint8(0))
	f.Fuzz(func(t *testing.T, kind uint8, args, raw []byte, outcome uint8) {
		cfg := testConfig(config.EnvTest)
		net, err := cfg.newNetwork()
		if err != nil {
			t.Fatal(err)
		}
		kinds := AllEventKinds()
		ev := Event{
			Kind:    kinds[int(kind)%len(kinds)],
			Chain:   ChainSolana,
			Args:    json.RawMessage(args),
			RawTx:   raw,
			TxID:    "abc",
			Outcome: AllOutcomes()[int(outcome)%len(AllOutcomes())],
		}
		_ = replayEvent(net, ev)
	})
}
