package spi

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the deadline passes. The throttle runs
// on timers, so tests assert eventual state rather than exact timing.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestFileThrottle_CoalescesBursts(t *testing.T) {
	tests := []struct {
		name     string
		triggers int
		keys     []string
		wantRuns map[string]int32
	}{
		{name: "single trigger runs once", triggers: 1, keys: []string{"a"}, wantRuns: map[string]int32{"a": 1}},
		{name: "burst on one key runs once", triggers: 50, keys: []string{"a"}, wantRuns: map[string]int32{"a": 1}},
		{name: "bursts on two keys run once each", triggers: 20, keys: []string{"a", "b"}, wantRuns: map[string]int32{"a": 1, "b": 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			runs := map[string]int32{}
			throttle := NewFileThrottle(20*time.Millisecond, 0, 4, func(key string) {
				mu.Lock()
				runs[key]++
				mu.Unlock()
			})

			for range tt.triggers {
				for _, key := range tt.keys {
					throttle.Trigger(key)
				}
			}
			// Long enough for every scheduled run and any spurious follow-up.
			time.Sleep(150 * time.Millisecond)
			throttle.Stop()

			mu.Lock()
			defer mu.Unlock()
			for key, want := range tt.wantRuns {
				if runs[key] != want {
					t.Errorf("runs[%q] = %d, want %d", key, runs[key], want)
				}
			}
		})
	}
}

func TestFileThrottle_TriggerDuringRunSchedulesOneFollowUp(t *testing.T) {
	var runs atomic.Int32
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	throttle := NewFileThrottle(5*time.Millisecond, 0, 1, func(string) {
		if runs.Add(1) == 1 {
			started <- struct{}{}
			<-release
		}
	})
	defer throttle.Stop()

	throttle.Trigger("a")
	<-started
	for range 10 {
		throttle.Trigger("a")
	}
	close(release)

	waitFor(t, time.Second, func() bool { return runs.Load() == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := runs.Load(); got != 2 {
		t.Errorf("runs = %d, want 2 (initial run plus one follow-up)", got)
	}
}

func TestFileThrottle_DelayScalesWithRunCost(t *testing.T) {
	const runCost = 30 * time.Millisecond
	const costFactor = 4

	var mu sync.Mutex
	var finishes, starts []time.Time
	throttle := NewFileThrottle(time.Millisecond, costFactor, 1, func(string) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		time.Sleep(runCost)
		mu.Lock()
		finishes = append(finishes, time.Now())
		mu.Unlock()
	})
	defer throttle.Stop()

	throttle.Trigger("a")
	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(finishes) == 1
	})
	throttle.Trigger("a")
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(starts) == 2
	})

	mu.Lock()
	defer mu.Unlock()
	gap := starts[1].Sub(finishes[0])
	if gap < costFactor*runCost {
		t.Errorf("second run started %s after the first finished, want at least %s", gap, costFactor*runCost)
	}
}

func TestFileThrottle_LimitsConcurrentRuns(t *testing.T) {
	var active, peak atomic.Int32
	var done atomic.Int32
	throttle := NewFileThrottle(time.Millisecond, 0, 2, func(string) {
		now := active.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		done.Add(1)
	})
	defer throttle.Stop()

	for _, key := range []string{"a", "b", "c", "d", "e"} {
		throttle.Trigger(key)
	}
	waitFor(t, 2*time.Second, func() bool { return done.Load() == 5 })

	if got := peak.Load(); got > 2 {
		t.Errorf("peak concurrent runs = %d, want at most 2", got)
	}
}

func TestFileThrottle_StopWaitsForRunningAndIgnoresLaterTriggers(t *testing.T) {
	var scheduledRan, runningFinished atomic.Bool
	started := make(chan struct{})
	throttle := NewFileThrottle(time.Millisecond, 0, 2, func(key string) {
		if key == "running" {
			close(started)
			time.Sleep(50 * time.Millisecond)
			runningFinished.Store(true)
			return
		}
		scheduledRan.Store(true)
	})

	throttle.Trigger("running")
	<-started
	throttle.Stop()
	// A watcher's event loop can still deliver one event after shutdown begins.
	throttle.Trigger("scheduled")

	if !runningFinished.Load() {
		t.Error("Stop returned before the in-flight run finished")
	}
	time.Sleep(20 * time.Millisecond)
	if scheduledRan.Load() {
		t.Error("run started after Stop")
	}
}

func TestFileThrottle_StopCancelsPendingTimer(t *testing.T) {
	var ran atomic.Bool
	throttle := NewFileThrottle(100*time.Millisecond, 0, 1, func(string) { ran.Store(true) })

	throttle.Trigger("a")
	throttle.Stop()
	time.Sleep(150 * time.Millisecond)

	if ran.Load() {
		t.Error("scheduled run executed after Stop")
	}
}

func TestFileThrottle_RecoversFromPanickingRun(t *testing.T) {
	var runs atomic.Int32
	throttle := NewFileThrottle(time.Millisecond, 0, 1, func(string) {
		if runs.Add(1) == 1 {
			panic("boom")
		}
	})
	defer throttle.Stop()

	throttle.Trigger("a")
	waitFor(t, time.Second, func() bool { return runs.Load() == 1 })
	// The key must not stay marked as running after the panic.
	waitFor(t, time.Second, func() bool {
		throttle.Trigger("a")
		return runs.Load() == 2
	})
}
