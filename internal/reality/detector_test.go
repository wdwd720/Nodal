package reality_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/reality"
)

func u64(v uint64) *uint64 { return &v }

func activeCheckpoint(lastSeq uint64) reality.Checkpoint {
	return reality.Checkpoint{
		ID: "cp-1", DataSourceID: "ds-1", DataSource: "src", Stream: "s", Partition: "0", Consumer: "c",
		LastSequence: u64(lastSeq), LastSourceEventAt: fixedNow.Add(-2 * time.Second), LastPlatformReceivedAt: fixedNow.Add(-time.Second),
		Status: reality.CheckpointActive, ConnectedAt: fixedNow.Add(-time.Minute), Version: 3, Found: true,
	}
}

// Table test for every kind in POINT_IN_TIME.md §4.
func TestDetector_EveryGapKind(t *testing.T) {
	t.Parallel()
	det, err := reality.NewDetector(reality.DetectorOptions{ContiguousSequence: true, HeartbeatTimeout: 30 * time.Second, OrderingTolerance: 500 * time.Millisecond})
	require.NoError(t, err)
	event := func(seq uint64, sourceAt time.Time) reality.Observation {
		return reality.Observation{Kind: reality.ObservationEvent, Sequence: u64(seq), SourceEventAt: sourceAt, PlatformReceivedAt: fixedNow, RawObjectID: "obj"}
	}
	cases := []struct {
		name       string
		cp         reality.Checkpoint
		obs        reality.Observation
		wantKinds  []string
		wantOpen   bool
		wantFlag   bool
		checkStart time.Time
		checkEnd   time.Time
	}{
		{
			name: "GAP: observed sequence > expected + 1", cp: activeCheckpoint(10), obs: event(13, fixedNow.Add(-time.Second)),
			wantKinds: []string{reality.GapKindGap}, wantOpen: true, checkStart: fixedNow.Add(-time.Second), checkEnd: fixedNow,
		},
		{name: "contiguous next sequence is clean", cp: activeCheckpoint(10), obs: event(11, fixedNow.Add(-time.Second))},
		{name: "same sequence (redelivery) is not an anomaly", cp: activeCheckpoint(10), obs: event(10, fixedNow.Add(-2*time.Second))},
		{
			name: "ORDERING_ANOMALY: sequence < last without replay marker", cp: activeCheckpoint(10), obs: event(7, fixedNow.Add(-time.Second)),
			wantKinds: []string{reality.GapKindOrderingAnomaly}, wantFlag: true,
		},
		{
			name: "ORDERING_ANOMALY: source_event_at decreasing beyond tolerance", cp: activeCheckpoint(10), obs: event(11, fixedNow.Add(-5*time.Second)),
			wantKinds: []string{reality.GapKindOrderingAnomaly}, wantFlag: true,
		},
		{name: "source_event_at decreasing within tolerance is fine", cp: activeCheckpoint(10), obs: event(11, fixedNow.Add(-2*time.Second-400*time.Millisecond))},
		{
			name: "RECONNECT is always recorded and acknowledged", cp: activeCheckpoint(10),
			obs:       reality.Observation{Kind: reality.ObservationReconnect, PlatformReceivedAt: fixedNow, Detail: "reconnected after 2 attempts"},
			wantKinds: []string{reality.GapKindReconnect}, checkStart: fixedNow.Add(-time.Second), checkEnd: fixedNow,
		},
		{
			name: "RECONNECT on first connection", cp: reality.Checkpoint{DataSourceID: "ds-1", Stream: "s", Partition: "0", Status: reality.CheckpointStopped},
			obs:       reality.Observation{Kind: reality.ObservationReconnect, PlatformReceivedAt: fixedNow},
			wantKinds: []string{reality.GapKindReconnect}, checkStart: fixedNow, checkEnd: fixedNow,
		},
		{
			name: "REPLAY marker suppresses ordering anomalies", cp: func() reality.Checkpoint {
				cp := activeCheckpoint(10)
				cp.Status = reality.CheckpointReplaying
				return cp
			}(), obs: event(3, fixedNow.Add(-time.Hour)),
		},
		{name: "no sequences: nothing to compare", cp: reality.Checkpoint{Found: true, Status: reality.CheckpointActive}, obs: reality.Observation{Kind: reality.ObservationEvent, PlatformReceivedAt: fixedNow}},
		{
			name: "gap and ordering anomaly can coincide", cp: activeCheckpoint(10), obs: event(20, fixedNow.Add(-time.Minute)),
			wantKinds: []string{reality.GapKindGap, reality.GapKindOrderingAnomaly}, wantOpen: true, wantFlag: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := det.Inspect(tc.cp, tc.obs)
			kinds := make([]string, 0, len(got))
			open, flag := false, false
			for _, f := range got {
				kinds = append(kinds, f.Kind)
				open = open || f.Resolution == reality.ResolutionOpen
				flag = flag || f.FlagEvent
				require.False(t, f.GapStartAt.IsZero(), "every finding has a start")
				if f.Kind == reality.GapKindGap {
					require.Equal(t, uint64(11), *f.ExpectedSequence)
					require.Equal(t, uint64(11), *f.GapStartSequence)
					require.Equal(t, *tc.obs.Sequence-1, *f.GapEndSequence)
				}
				if !tc.checkStart.IsZero() {
					require.Equal(t, tc.checkStart, f.GapStartAt)
					require.Equal(t, tc.checkEnd, f.GapEndAt)
				}
			}
			require.Equal(t, tc.wantKinds, nilIfEmpty(kinds))
			require.Equal(t, tc.wantOpen, open, "GAP rows open; RECONNECT/ORDERING rows acknowledged")
			require.Equal(t, tc.wantFlag, flag, "affected events are flagged, not dropped")
		})
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func TestDetector_NonContiguousSourcesDoNotReportSequenceGaps(t *testing.T) {
	t.Parallel()
	det, err := reality.NewDetector(reality.DetectorOptions{ContiguousSequence: false, HeartbeatTimeout: time.Minute})
	require.NoError(t, err)
	got := det.Inspect(activeCheckpoint(10), reality.Observation{Kind: reality.ObservationEvent, Sequence: u64(1_000_000), PlatformReceivedAt: fixedNow})
	require.Empty(t, got, "slot-keyed streams find gaps through reconnect + backfill, not arithmetic")
	got = det.Inspect(activeCheckpoint(10), reality.Observation{Kind: reality.ObservationEvent, Sequence: u64(2), PlatformReceivedAt: fixedNow})
	require.Len(t, got, 1)
	require.Equal(t, reality.GapKindOrderingAnomaly, got[0].Kind, "going backwards is still an anomaly")
}

func TestDetector_Silence(t *testing.T) {
	t.Parallel()
	det, err := reality.NewDetector(reality.DetectorOptions{HeartbeatTimeout: 30 * time.Second})
	require.NoError(t, err)
	cp := activeCheckpoint(1)
	_, ok := det.Silence(cp, cp.LastPlatformReceivedAt.Add(30*time.Second))
	require.False(t, ok, "exactly at the timeout is not silence")
	f, ok := det.Silence(cp, cp.LastPlatformReceivedAt.Add(31*time.Second))
	require.True(t, ok)
	require.Equal(t, reality.GapKindSilence, f.Kind)
	require.Equal(t, reality.ResolutionOpen, f.Resolution)
	require.Equal(t, cp.LastPlatformReceivedAt, f.GapStartAt)
	require.True(t, f.GapEndAt.IsZero(), "silence stays open until the next event")

	disconnected := cp
	disconnected.Status = reality.CheckpointDisconnected
	_, ok = det.Silence(disconnected, fixedNow.Add(time.Hour))
	require.False(t, ok, "a disconnected stream is not silent, it is disconnected")

	fresh := reality.Checkpoint{Found: true, Status: reality.CheckpointActive, ConnectedAt: fixedNow}
	_, ok = det.Silence(fresh, fixedNow.Add(time.Minute))
	require.True(t, ok, "a connected stream that never delivered is silent after the timeout since connect")
	_, ok = det.Silence(reality.Checkpoint{}, fixedNow.Add(time.Hour))
	require.False(t, ok, "an unknown position cannot be silent")
}

func TestDetectorOptions_Validate(t *testing.T) {
	t.Parallel()
	_, err := reality.NewDetector(reality.DetectorOptions{})
	require.Error(t, err)
	_, err = reality.NewDetector(reality.DetectorOptions{HeartbeatTimeout: time.Second, OrderingTolerance: -1})
	require.Error(t, err)
}
