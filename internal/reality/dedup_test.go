package reality_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/reality"
)

func TestDedupID_Strategies(t *testing.T) {
	t.Parallel()
	got, err := reality.DedupID(reality.DedupProviderID, "sig/wallet", "src", "t", nil)
	require.NoError(t, err)
	require.Equal(t, "sig/wallet", got)

	_, err = reality.DedupID(reality.DedupProviderID, "", "src", "t", nil)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "provider id strategy without an id fails closed")

	a, err := reality.DedupID(reality.DedupCompositeHash, "ignored", "src", "t", map[string]string{"signature": "s", "wallet": "w"})
	require.NoError(t, err)
	b, err := reality.DedupID(reality.DedupCompositeHash, "other", "src", "t", map[string]string{"wallet": "w", "signature": "s"})
	require.NoError(t, err)
	require.Equal(t, a, b, "composite hash ignores the provider id and map order")
	require.Len(t, a, 64)

	c, err := reality.DedupID(reality.DedupCompositeHash, "", "src2", "t", map[string]string{"signature": "s", "wallet": "w"})
	require.NoError(t, err)
	require.NotEqual(t, a, c, "source is part of the hash")

	_, err = reality.DedupID(reality.DedupCompositeHash, "", "src", "t", nil)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = reality.DedupID("MAGIC", "x", "src", "t", nil)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestCompositeHash_NoFieldBoundaryCollisions(t *testing.T) {
	t.Parallel()
	x := reality.CompositeHash("s", "t", map[string]string{"a": "1", "b": "2"})
	y := reality.CompositeHash("s", "t", map[string]string{"a": "1b", "b": ""})
	z := reality.CompositeHash("s", "t", map[string]string{"ab": "12"})
	require.NotEqual(t, x, y)
	require.NotEqual(t, x, z)
	require.NotEqual(t, reality.CompositeHash("st", "", map[string]string{"a": "1"}), reality.CompositeHash("s", "t", map[string]string{"a": "1"}))
}

// PART 199 / POINT_IN_TIME.md §11: N duplicates of an event collapse to one.
func TestProp_DedupCollapsesDuplicatesToOne(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		distinct := rapid.IntRange(1, 20).Draw(rt, "distinct")
		var events []reality.NormalizedEvent
		var order []string
		for i := 0; i < distinct; i++ {
			dedup := fmt.Sprintf("sig-%d/wallet", i)
			order = append(order, dedup)
			copies := rapid.IntRange(1, 25).Draw(rt, fmt.Sprintf("copies_%d", i))
			for c := 0; c < copies; c++ {
				events = append(events, reality.NormalizedEvent{
					EventID: reality.NewEventID().String(), DedupID: dedup, Source: "src", EventType: reality.EventTypeWalletTransaction,
					Payload: json.RawMessage(fmt.Sprintf(`{"copy":"%d"}`, c)),
				})
			}
		}
		// Shuffle deterministically under rapid (Fisher–Yates with drawn indexes).
		for i := len(events) - 1; i > 0; i-- {
			j := rapid.IntRange(0, i).Draw(rt, fmt.Sprintf("swap_%d", i))
			events[i], events[j] = events[j], events[i]
		}
		out := reality.Dedup(events)
		require.Len(rt, out, distinct)
		seen := map[string]bool{}
		for _, e := range out {
			require.False(rt, seen[e.DedupID])
			seen[e.DedupID] = true
		}
		for _, d := range order {
			require.True(rt, seen[d])
		}
		// Different sources with the same dedup id are different events.
		other := append([]reality.NormalizedEvent(nil), out...)
		for i := range other {
			other[i].Source = "other"
		}
		require.Len(rt, reality.Dedup(append(out, other...)), 2*distinct)
	})
}
