package devnet

import (
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestChain_ConcurrentUse submits from several goroutines while another
// produces blocks and a third reads. Run under -race it proves the chain's
// locking; the totals prove nothing was lost or applied twice.
func TestChain_ConcurrentUse(t *testing.T) {
	const workers, perWorker, maxBlocks = 4, 25, 200
	w := newWorld(t, func(cfg *ChainConfig) { cfg.BlockhashWindow = 2 * maxBlocks })
	raws := make([][][]byte, workers)
	for g := range raws {
		for i := 0; i < perWorker; i++ {
			raws[g] = append(raws[g], signed(t, w.msg(
				SystemTransfer{From: w.alice.PublicKey(), To: w.bob.PublicKey(), Lamports: 1},
				memo(strconv.Itoa(g)+"/"+strconv.Itoa(i)),
			), w.payer, w.alice))
		}
	}

	var wg sync.WaitGroup
	done := make(chan struct{})
	ids := make([][]TxID, workers)
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for _, raw := range raws[g] {
				id, err := w.c.Submit(raw)
				if err != nil {
					t.Errorf("submit: %v", err)
					return
				}
				ids[g] = append(ids[g], id)
			}
		}(g)
	}
	// The producer and the reader are bounded so the producer can never age
	// the submitters' blockhash out of the window, however the scheduler
	// interleaves them.
	var side sync.WaitGroup
	side.Add(2)
	go func() {
		defer side.Done()
		for i := 0; i < maxBlocks; i++ {
			select {
			case <-done:
				return
			default:
				w.c.ProduceBlock()
				runtime.Gosched()
			}
		}
	}()
	go func() {
		defer side.Done()
		for i := 0; i < maxBlocks; i++ {
			select {
			case <-done:
				return
			default:
				_ = w.c.StateHash()
				_ = w.c.Balance(w.bob.PublicKey())
				_ = w.c.Tip()
				runtime.Gosched()
			}
		}
	}()
	wg.Wait()
	close(done)
	side.Wait()

	w.c.ProduceBlocks(1 + testDepth)
	for g := range ids {
		require.Len(t, ids[g], perWorker)
		for _, id := range ids[g] {
			require.Equal(t, TxFinalized, w.c.Status(id).State)
		}
	}
	total := uint64(workers * perWorker)
	require.Equal(t, testFunds+total, w.nativeOf(w.bob))
	require.Equal(t, testFunds-total, w.nativeOf(w.alice))
	require.Equal(t, testFunds-total*testFee, w.nativeOf(w.payer))
	require.NoError(t, w.c.audit())
}
