package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracer-ai/tracer-cli/pkg/engine"
)

func TestValidateFlags(t *testing.T) {
	origConsole := console
	origSilent := silent
	origDebug := debug
	origLogFile := logFile
	defer func() {
		console = origConsole
		silent = origSilent
		debug = origDebug
		logFile = origLogFile
	}()

	t.Run("console and silent are mutually exclusive", func(t *testing.T) {
		console = true
		silent = true
		debug = false
		logFile = false

		err := validateFlags()
		if err == nil {
			t.Fatal("expected error when both console and silent are set")
		}
	})

	t.Run("debug requires console or log", func(t *testing.T) {
		console = false
		silent = false
		debug = true
		logFile = false

		err := validateFlags()
		if err == nil {
			t.Fatal("expected error when debug is enabled without console/log")
		}
	})

	t.Run("valid combinations pass", func(t *testing.T) {
		console = true
		silent = false
		debug = true
		logFile = false

		if err := validateFlags(); err != nil {
			t.Fatalf("expected valid flag combination, got error: %v", err)
		}
	})
}

func TestAcquireWatchLockSingleInstance(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "watch.lock")

	firstLock, err := acquireWatchLock(lockPath)
	if err != nil {
		t.Fatalf("acquireWatchLock() first lock error = %v", err)
	}

	if _, err := acquireWatchLock(lockPath); err == nil {
		t.Fatal("acquireWatchLock() second lock should fail while first lock is held")
	}

	releaseWatchLock(firstLock)

	secondLock, err := acquireWatchLock(lockPath)
	if err != nil {
		t.Fatalf("acquireWatchLock() after release error = %v", err)
	}
	releaseWatchLock(secondLock)

	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("watch lock file should be removed after release, stat error = %v", err)
	}
}

func TestSyncProgressTracker_RenderLines(t *testing.T) {
	tracker := newSyncProgressTracker([]string{"claude", "codex"}, false, os.Stderr)

	tracker.mu.Lock()
	lines := tracker.renderLinesLocked()
	tracker.mu.Unlock()
	initial := strings.Join(lines, "\n")

	if !strings.Contains(initial, "claude: pending...") {
		t.Fatalf("initial progress missing pending line for claude: %s", initial)
	}
	if !strings.Contains(initial, "codex: pending...") {
		t.Fatalf("initial progress missing pending line for codex: %s", initial)
	}

	tracker.onProviderScanStart("claude")
	tracker.onProviderScanComplete("claude", 2, nil)
	tracker.onSessionProcessed("claude", engine.OutcomeCreated, 1, 2)
	tracker.onSessionProcessed("claude", engine.OutcomeSkipped, 2, 2)
	tracker.onProviderScanComplete("codex", 0, os.ErrPermission)

	tracker.mu.Lock()
	lines = tracker.renderLinesLocked()
	tracker.mu.Unlock()
	updated := strings.Join(lines, "\n")

	if !strings.Contains(updated, "claude: 2/2 processed (c:1 u:0 s:1 e:0)") {
		t.Fatalf("updated progress missing claude counters: %s", updated)
	}
	if !strings.Contains(updated, "codex: scan failed") {
		t.Fatalf("updated progress missing codex scan failure: %s", updated)
	}
	if !strings.Contains(updated, "overall: 2/2 processed (c:1 u:0 s:1 e:0)") {
		t.Fatalf("updated progress missing overall counters: %s", updated)
	}
}

func TestSyncProgressTracker_OnSessionProcessedOutcomeCounts(t *testing.T) {
	tracker := newSyncProgressTracker([]string{"claude"}, false, os.Stderr)
	tracker.onProviderScanComplete("claude", 4, nil)
	tracker.onSessionProcessed("claude", engine.OutcomeCreated, 1, 4)
	tracker.onSessionProcessed("claude", engine.OutcomeUpdated, 2, 4)
	tracker.onSessionProcessed("claude", engine.OutcomeSkipped, 3, 4)
	tracker.onSessionProcessed("claude", engine.OutcomeError, 4, 4)

	tracker.mu.Lock()
	state := tracker.providers["claude"]
	tracker.mu.Unlock()

	if state == nil {
		t.Fatal("missing claude progress state")
	}
	if state.Created != 1 || state.Updated != 1 || state.Skipped != 1 || state.Errors != 1 {
		t.Fatalf("unexpected outcome counters: %+v", *state)
	}
	if state.Processed != 4 || state.Total != 4 {
		t.Fatalf("unexpected processed/total counters: %+v", *state)
	}
}
