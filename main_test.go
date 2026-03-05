package main

import (
	"os"
	"path/filepath"
	"testing"
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
