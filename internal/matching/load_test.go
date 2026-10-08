package matching

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLoad_100kOrders drives 120,000 commands from a seeded synthetic stream
// (mostly limit orders around the touch, plus markets, IOC, FOK, post-only,
// cancels and replaces over 16 owners) through one book, reports throughput,
// and then checks the final invariants, every event against the independent
// model, and that a full replay reproduces the identical events and state.
func TestLoad_100kOrders(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic load test skipped in -short mode")
	}
	const commands = 120_000
	owners := make([]string, 16)
	for i := range owners {
		owners[i] = "owner-" + strconv.Itoa(i)
	}
	gen := newStreamGen(0x5eed, "L", owners)
	cmds := make([]Command, commands)
	newOrders := 0
	for i := range cmds {
		cmds[i] = gen.nextCommand()
		if _, ok := cmds[i].(NewOrder); ok {
			newOrders++
		}
	}

	b := newTestBook(t)
	results := make([]Result, 0, commands)
	journal := make([]JournalEntry, 0, commands)
	start := time.Now()
	for _, cmd := range cmds {
		res, err := b.Apply(cmd)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, res)
		journal = append(journal, JournalEntry{Seq: res.CommandSeq, Command: cmd})
	}
	elapsed := time.Since(start)

	var events, trades, rejected int
	for _, res := range results {
		events += len(res.Events)
		trades += len(eventsOf(res.Events, EventTrade))
		if _, ok := res.Rejected(); ok {
			rejected++
		}
	}
	open := 0
	for _, l := range []*ladder{&b.bids, &b.asks} {
		for _, lvl := range l.levels {
			open += lvl.count
		}
	}
	perSec := int64(commands) * int64(time.Second) / max(int64(elapsed), 1)
	t.Logf("%d commands (%d new orders) in %s: %d commands/sec; %d events, %d trades, %d rejections; "+
		"%d orders resting on %d bid / %d ask levels",
		commands, newOrders, elapsed.Round(time.Millisecond), perSec, events, trades, rejected,
		open, len(b.bids.levels), len(b.asks.levels))

	require.NoError(t, b.verify())
	requireLevelsFIFOAndAggregates(t, b)

	// Re-run the stream under the independent model, which checks every
	// event and cross-checks the book after every command.
	replica := newTestBook(t)
	model := newShadow()
	for i, cmd := range cmds {
		res, err := replica.Apply(cmd)
		require.NoError(t, err)
		require.Equal(t, results[i], res)
		if err := model.observe(cmd, res, replica); err != nil {
			t.Fatalf("command %d: %v", i+1, err)
		}
	}
	require.NoError(t, model.checkAll(b))
	require.Equal(t, b.StateHash(), replica.StateHash())

	// Snapshot at the midpoint, restore, replay the rest.
	requireRestoreMidpoint(t, journal, results, b.Snapshot(), commands/2)
}
