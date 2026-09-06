package provider

import (
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func mustTracker(t *testing.T, th Thresholds) *Tracker {
	t.Helper()
	tr, err := NewTracker("jupiter", th, t0)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestHealth_Semantics(t *testing.T) {
	if !Healthy.AllowsNewActions() || !Degraded.AllowsNewActions() {
		t.Fatal("healthy/degraded must allow new actions")
	}
	if Unhealthy.AllowsNewActions() || Disabled.AllowsNewActions() {
		t.Fatal("unhealthy/disabled must block new actions")
	}
	for _, h := range []Health{Healthy, Degraded, Unhealthy} {
		if !h.AllowsObservation() {
			t.Fatalf("%s must still allow observation", h)
		}
	}
	if Disabled.AllowsObservation() {
		t.Fatal("disabled blocks observation only by operator decision")
	}
	if Worst(Healthy, Unhealthy) != Unhealthy || Worst(Disabled, Degraded) != Disabled || Worst("weird", Healthy) != "weird" {
		t.Fatal("worst-of composition")
	}
	if !SafeRetry.MayRetryBlindly() || !IdempotentWrite.MayRetryBlindly() || UnknownEffectWrite.MayRetryBlindly() {
		t.Fatal("retry classes")
	}
}

func TestThresholds_Validate(t *testing.T) {
	if err := DefaultThresholds().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := DefaultThresholds()
	bad.DegradedErrBPS = 6000
	if bad.Validate() == nil {
		t.Fatal("degraded > unhealthy must fail")
	}
	bad = DefaultThresholds()
	bad.RecoverStreak = 0
	if bad.Validate() == nil {
		t.Fatal("recover streak 0 must fail")
	}
	if _, err := NewTracker("", DefaultThresholds(), t0); err == nil {
		t.Fatal("empty name must fail")
	}
}

func TestTracker_DegradesImmediatelyRecoversWithHysteresis(t *testing.T) {
	th := DefaultThresholds()
	th.MinSamples = 4
	th.RecoverStreak = 3
	th.MaxStaleness = 0
	tr := mustTracker(t, th)
	now := t0
	// 4 samples: 2 failures → 50% → UNHEALTHY immediately.
	for i, ok := range []bool{true, false, true, false} {
		now = now.Add(time.Second)
		h := tr.Observe(now, ok, 100*time.Millisecond)
		if i == 3 && h != Unhealthy {
			t.Fatalf("expected UNHEALTHY at 50%% errors, got %s", h)
		}
	}
	// Successes: recovery needs a streak, steps one level at a time, and the
	// window statistics must justify the better state. The two failures stay
	// in the 60 s window, so HEALTHY (< 10% errors) is only reachable once the
	// window holds more than 20 samples: 4 initial + 17 successes, i.e. the
	// 17th success (index 16) is the earliest legal HEALTHY.
	var seen []Health
	for i := 0; i < 30; i++ {
		now = now.Add(time.Second)
		seen = append(seen, tr.Observe(now, true, 100*time.Millisecond))
	}
	if seen[0] != Unhealthy || seen[1] != Unhealthy {
		t.Fatalf("recovery too fast: %v", seen)
	}
	if seen[2] != Degraded {
		t.Fatalf("expected DEGRADED after streak, got %v", seen)
	}
	firstHealthy := -1
	for i, h := range seen {
		if h == Healthy {
			firstHealthy = i
			break
		}
	}
	if firstHealthy < 16 {
		t.Fatalf("HEALTHY before the window error rate fell below 10%%: index %d in %v", firstHealthy, seen)
	}
	if tr.Health() != Healthy {
		t.Fatalf("final state %s (%v)", tr.Health(), seen)
	}
}

func TestTracker_LatencyAndStaleness(t *testing.T) {
	th := DefaultThresholds()
	th.MinSamples = 2
	th.DegradedP95 = time.Second
	th.UnhealthyP95 = 3 * time.Second
	th.MaxStaleness = 10 * time.Second
	tr := mustTracker(t, th)
	now := t0.Add(time.Second)
	tr.Observe(now, true, 1500*time.Millisecond)
	now = now.Add(time.Second)
	if h := tr.Observe(now, true, 1500*time.Millisecond); h != Degraded {
		t.Fatalf("slow p95 should degrade, got %s", h)
	}
	// No samples for > MaxStaleness → UNHEALTHY on tick.
	if h := tr.Tick(now.Add(20 * time.Second)); h != Unhealthy {
		t.Fatalf("stale should be unhealthy, got %s", h)
	}
	snap := tr.Snapshot(now.Add(20 * time.Second))
	if snap.Health != Unhealthy || snap.LastSuccessAt != now {
		t.Fatalf("snapshot %+v", snap)
	}
}

func TestTracker_InsufficientEvidenceHolds(t *testing.T) {
	th := DefaultThresholds()
	th.MinSamples = 10
	th.MaxStaleness = 0
	tr := mustTracker(t, th)
	// A single failure is not evidence at MinSamples=10.
	if h := tr.Observe(t0.Add(time.Second), false, 0); h != Healthy {
		t.Fatalf("one failure below min samples must hold HEALTHY, got %s", h)
	}
}

func TestTracker_DisableOverridesObservation(t *testing.T) {
	tr := mustTracker(t, DefaultThresholds())
	tr.Disable(t0, "kill switch PROVIDER_DISABLE_NEW_ACTIONS")
	if tr.Health() != Disabled {
		t.Fatal("disabled")
	}
	if tr.Observe(t0.Add(time.Second), true, 0) != Disabled {
		t.Fatal("observation cannot lift an operator disable")
	}
	snap := tr.Snapshot(t0.Add(time.Second))
	if !snap.Disabled || snap.Observed != Healthy || snap.DisableReason == "" {
		t.Fatalf("snapshot %+v", snap)
	}
	tr.Enable(t0.Add(2 * time.Second))
	if tr.Health() != Healthy {
		t.Fatal("enable restores observed health")
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	a := mustTracker(t, DefaultThresholds())
	if err := r.Register(a); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(a); err == nil {
		t.Fatal("duplicate must fail")
	}
	b, _ := NewTracker("helius", DefaultThresholds(), t0)
	_ = r.Register(b)
	if got := r.Names(); len(got) != 2 || got[0] != "helius" || got[1] != "jupiter" {
		t.Fatalf("names %v", got)
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("unknown")
	}
	if m := r.HealthMap(); m["jupiter"] != Healthy || m["helius"] != Healthy {
		t.Fatalf("map %v", m)
	}
}

func TestTracker_Concurrent(t *testing.T) {
	tr := mustTracker(t, DefaultThresholds())
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				tr.Observe(t0.Add(time.Duration(i)*time.Millisecond), (i+g)%7 != 0, time.Duration(i)*time.Millisecond)
				_ = tr.Health()
				_ = tr.Snapshot(t0.Add(time.Duration(i) * time.Millisecond))
			}
		}(g)
	}
	wg.Wait()
	if h := tr.Health(); h != Healthy && h != Degraded && h != Unhealthy {
		t.Fatalf("unexpected %s", h)
	}
}

// TestProp_NeverImprovesWithoutSuccessStreak: for any sample sequence, the
// state only steps to a better level after at least RecoverStreak
// consecutive successes.
func TestProp_NeverImprovesWithoutSuccessStreak(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		th := DefaultThresholds()
		th.MinSamples = rapid.IntRange(1, 6).Draw(rt, "min")
		th.RecoverStreak = rapid.IntRange(1, 6).Draw(rt, "streak")
		th.MaxStaleness = 0
		tr, err := NewTracker("p", th, t0)
		if err != nil {
			rt.Fatal(err)
		}
		prev := tr.Health()
		run := 0
		now := t0
		n := rapid.IntRange(1, 200).Draw(rt, "n")
		for i := 0; i < n; i++ {
			ok := rapid.Bool().Draw(rt, "ok")
			now = now.Add(time.Second)
			h := tr.Observe(now, ok, 10*time.Millisecond)
			if ok {
				run++
			} else {
				run = 0
			}
			if h.Rank() < prev.Rank() && run < th.RecoverStreak {
				rt.Fatalf("improved %s→%s with success run %d < streak %d", prev, h, run, th.RecoverStreak)
			}
			if h.Rank() < prev.Rank() {
				run = 0 // the tracker resets its streak on a step up
			}
			prev = h
		}
	})
}
