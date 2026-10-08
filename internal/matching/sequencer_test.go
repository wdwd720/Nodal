package matching

import (
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// requireCancelRaceConsistent checks the outcome of a Cancel and an
// aggressive IOC order racing for the same resting sell order "m" of 10 lots,
// in whichever order they were sequenced.
func requireCancelRaceConsistent(t *testing.T, cancelRes, takerRes Result, takerQty int64) (cancelFirst bool) {
	t.Helper()
	trades := eventsOf(takerRes.Events, EventTrade)
	if cancelRes.CommandSeq < takerRes.CommandSeq {
		// The cancel removed the whole order; the taker cannot have matched it.
		require.Equal(t, []EventType{EventCancelled, EventBookDelta}, typesOf(cancelRes.Events))
		c := cancelRes.Events[0]
		require.Equal(t, []int64{10, 0, 0}, []int64{c.QtyLots, c.FilledLots, c.RemainingLots})
		require.Empty(t, trades, "the taker matched a cancelled order")
		return true
	}
	// The taker matched first; the cancel finds the order filled or partially filled.
	require.Len(t, trades, 1)
	require.Equal(t, "m", trades[0].MakerOrderID)
	require.Equal(t, takerQty, trades[0].QtyLots)
	if takerQty == 10 {
		requireRejected(t, cancelRes, RejectOrderNotOpen)
		return false
	}
	require.Equal(t, []EventType{EventCancelled, EventBookDelta}, typesOf(cancelRes.Events))
	c := cancelRes.Events[0]
	require.Equal(t, []int64{10 - takerQty, takerQty, 0}, []int64{c.QtyLots, c.FilledLots, c.RemainingLots})
	return false
}

func TestCancelRaceBothOrderings(t *testing.T) {
	for _, takerQty := range []int64{10, 4} {
		for _, cancelFirst := range []bool{true, false} {
			b := newTestBook(t)
			_, err := b.Apply(lim("m", "maker", SideSell, 100, 10))
			require.NoError(t, err)
			cancel := Cancel{OrderID: "m", OwnerID: "maker"}
			taker := ioc(lim("t", "taker", SideBuy, 100, takerQty))
			var cancelRes, takerRes Result
			if cancelFirst {
				cancelRes, _ = b.Apply(cancel)
				takerRes, _ = b.Apply(taker)
			} else {
				takerRes, _ = b.Apply(taker)
				cancelRes, _ = b.Apply(cancel)
			}
			assert.Equal(t, cancelFirst, requireCancelRaceConsistent(t, cancelRes, takerRes, takerQty))
			require.NoError(t, b.verify())
		}
	}
}

// TestSequencer_CancelRace submits a cancel and an aggressive order for the
// same resting order from two goroutines at once, many times. Whatever order
// the sequencer assigns, the outcome is consistent, and replaying the journal
// reproduces exactly the results returned live.
func TestSequencer_CancelRace(t *testing.T) {
	const rounds = 400
	var cancelFirst, takerFirst int
	for i := 0; i < rounds; i++ {
		takerQty := int64(10)
		if i%2 == 1 {
			takerQty = 4
		}
		seq, err := NewSequencer(newTestBook(t))
		require.NoError(t, err)
		makerRes, err := seq.Submit(lim("m", "maker", SideSell, 100, 10))
		require.NoError(t, err)

		var (
			wg                  sync.WaitGroup
			start               = make(chan struct{})
			cancelRes, takerRes Result
			cancelErr, takerErr error
		)
		submitCancel := func() {
			defer wg.Done()
			<-start
			cancelRes, cancelErr = seq.Submit(Cancel{Timestamp: 2, OrderID: "m", OwnerID: "maker"})
		}
		submitTaker := func() {
			defer wg.Done()
			<-start
			takerRes, takerErr = seq.Submit(ioc(lim("t", "taker", SideBuy, 100, takerQty)))
		}
		wg.Add(2)
		if (i/2)%2 == 0 { // alternate the launch order so both sequencings occur
			go submitCancel()
			go submitTaker()
		} else {
			go submitTaker()
			go submitCancel()
		}
		close(start)
		wg.Wait()
		require.NoError(t, cancelErr)
		require.NoError(t, takerErr)

		if requireCancelRaceConsistent(t, cancelRes, takerRes, takerQty) {
			cancelFirst++
		} else {
			takerFirst++
		}

		live := []Result{makerRes, cancelRes, takerRes}
		if takerRes.CommandSeq < cancelRes.CommandSeq {
			live[1], live[2] = takerRes, cancelRes
		}
		journal := seq.Journal()
		replayed, results, err := Replay(testCfg, journal)
		require.NoError(t, err)
		requireSameResults(t, live, results, "replay must reproduce what was returned live")
		var liveHash [32]byte
		seq.Read(func(r Reader) { liveHash = r.StateHash() })
		require.Equal(t, liveHash, replayed.StateHash())
	}
	t.Logf("%d rounds: cancel sequenced first %d times, taker first %d times", rounds, cancelFirst, takerFirst)
}

// TestSequencer_ConcurrentSubmitDeterminism has many goroutines submit
// interleaved streams (with a shared owner so they also self-trade-prevent
// against each other). The journal must hold every command in the order the
// sequencer assigned, and replaying it must reproduce every live result and
// the live state hash.
func TestSequencer_ConcurrentSubmitDeterminism(t *testing.T) {
	const workers, perWorker = 8, 500
	seq, err := NewSequencer(newTestBook(t))
	require.NoError(t, err)

	var mu sync.Mutex
	live := make(map[int64]Result, workers*perWorker)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			name := "w" + strconv.Itoa(w)
			gen := newStreamGen(uint64(1000+w), name+"-", []string{name, name + "b", "shared"})
			for i := 0; i < perWorker; i++ {
				res, err := seq.Submit(gen.nextCommand())
				if err != nil {
					t.Errorf("worker %d: %v", w, err)
					return
				}
				mu.Lock()
				live[res.CommandSeq] = res
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	journal := seq.Journal()
	require.Len(t, journal, workers*perWorker)
	ordered := make([]Result, len(journal))
	for i, e := range journal {
		require.Equal(t, int64(i+1), e.Seq)
		res, ok := live[e.Seq]
		require.True(t, ok)
		ordered[i] = res
	}
	replayed, results, err := Replay(testCfg, journal)
	require.NoError(t, err)
	requireSameResults(t, ordered, results, "replayed journal")

	var liveHash [32]byte
	var liveSnapshot []byte
	seq.Read(func(r Reader) {
		liveHash = r.StateHash()
		liveSnapshot = r.Snapshot()
	})
	require.Equal(t, liveHash, replayed.StateHash())
	requireSameBytes(t, liveSnapshot, replayed.Snapshot(), "replayed snapshot")
	require.NoError(t, replayed.verify())

	trades := 0
	for _, res := range ordered {
		trades += len(eventsOf(res.Events, EventTrade))
	}
	t.Logf("%d commands from %d goroutines, %d trades, state hash %x", len(journal), workers, trades, liveHash[:8])
}

func TestSequencer_Basics(t *testing.T) {
	_, err := NewSequencer(nil)
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	seq, err := NewSequencer(newTestBook(t))
	require.NoError(t, err)
	_, err = seq.Submit(nil)
	require.Error(t, err)
	_, err = seq.Submit(&Cancel{OrderID: "x", OwnerID: "y"})
	require.Error(t, err)
	assert.Empty(t, seq.Journal(), "commands refused with an error are not journaled")

	r1, err := seq.Submit(lim("a", "alice", SideSell, 100, 5))
	require.NoError(t, err)
	r2, err := seq.Submit(Cancel{OrderID: "nope", OwnerID: "alice"})
	require.NoError(t, err)
	_, rejected := r2.Rejected()
	assert.True(t, rejected)
	assert.Equal(t, []int64{1, 2}, []int64{r1.CommandSeq, r2.CommandSeq})
	assert.Equal(t, []JournalEntry{
		{Seq: 1, Command: lim("a", "alice", SideSell, 100, 5)},
		{Seq: 2, Command: Cancel{OrderID: "nope", OwnerID: "alice"}},
	}, seq.Journal(), "rejected commands are journaled: they consume sequence numbers")
	assert.Len(t, seq.JournalSince(1), 1)

	// Checkpoint, restore elsewhere, keep going there, and compare.
	var snap []byte
	var at int64
	seq.Read(func(r Reader) {
		snap, at = r.Snapshot(), r.CommandSeq()
		ask, ok := r.BestAsk()
		assert.True(t, ok)
		assert.Equal(t, int64(100), ask.PriceTicks)
	})
	restored, err := Restore(snap)
	require.NoError(t, err)
	follower, err := NewSequencer(restored)
	require.NoError(t, err)
	for _, cmd := range []Command{lim("b", "bob", SideBuy, 100, 2), Replace{OrderID: "a", OwnerID: "alice", NewPriceTicks: 101, NewQtyLots: 9}} {
		want, err := seq.Submit(cmd)
		require.NoError(t, err)
		got, err := follower.Submit(cmd)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	assert.Equal(t, seq.JournalSince(at), follower.Journal(), "a follower's journal continues the leader's sequence")
	_, full, err := Replay(testCfg, seq.Journal())
	require.NoError(t, err)
	assert.Len(t, full, 4)
}

func TestJournal(t *testing.T) {
	var j Journal
	require.Error(t, j.Append(JournalEntry{Seq: 1}), "no command")
	require.Error(t, j.Append(JournalEntry{Seq: 0, Command: Cancel{}}), "seq starts at 1")
	require.NoError(t, j.Append(JournalEntry{Seq: 5, Command: Cancel{OrderID: "a"}}))
	err := j.Append(JournalEntry{Seq: 7, Command: Cancel{OrderID: "b"}})
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	require.NoError(t, j.Append(JournalEntry{Seq: 6, Command: Cancel{OrderID: "b"}}))
	assert.Equal(t, 2, j.Len())
	assert.Len(t, j.Since(4), 2)
	assert.Len(t, j.Since(5), 1)
	assert.Empty(t, j.Since(6))

	// A journal that does not start where the book is cannot be replayed.
	_, _, err = Replay(testCfg, j.Entries())
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	b := newTestBook(t)
	_, err = b.Replay([]JournalEntry{{Seq: 1, Command: Cancel{OrderID: "a", OwnerID: "x"}}, {Seq: 3, Command: Cancel{}}})
	require.Error(t, err)
	assert.Equal(t, int64(1), b.CommandSeq(), "entries before the gap stay applied")
	_, err = b.Replay([]JournalEntry{{Seq: 2, Command: nil}})
	require.Error(t, err)
	_, _, err = Replay(Config{}, nil)
	require.Error(t, err)
}
