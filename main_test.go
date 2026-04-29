package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracer-ai/tracer-cli/pkg/engine"
)

type fileInfoStub struct {
	mode os.FileMode
}

func (f fileInfoStub) Name() string       { return "stub" }
func (f fileInfoStub) Size() int64        { return 0 }
func (f fileInfoStub) Mode() os.FileMode  { return f.mode }
func (f fileInfoStub) ModTime() time.Time { return time.Time{} }
func (f fileInfoStub) IsDir() bool        { return false }
func (f fileInfoStub) Sys() any           { return nil }

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

func TestRenderListOutput(t *testing.T) {
	projects := []projectSummary{
		{
			ProviderID:   "codex",
			Project:      "tracer-cli",
			ProjectPath:  "/workspace/tracer-cli",
			SessionCount: 2,
		},
	}
	sessions := []sessionRow{
		{
			ProviderID: "codex",
			Project:    "tracer-cli",
			SessionID:  "session-1",
			CreatedAt:  "2026-04-29T10:20:30Z",
			Slug:       "list-pager",
		},
	}

	t.Run("projects only", func(t *testing.T) {
		var out strings.Builder
		if err := renderListOutput(&out, projects, sessions, false); err != nil {
			t.Fatalf("renderListOutput() returned error: %v", err)
		}
		output := out.String()

		for _, want := range []string{"PROVIDER", "tracer-cli", "/workspace/tracer-cli"} {
			if !strings.Contains(output, want) {
				t.Errorf("renderListOutput() missing %q in %q", want, output)
			}
		}
		if strings.Contains(output, "session-1") {
			t.Errorf("renderListOutput() included sessions without includeSessions: %q", output)
		}
	})

	t.Run("includes sessions", func(t *testing.T) {
		var out strings.Builder
		if err := renderListOutput(&out, projects, sessions, true); err != nil {
			t.Fatalf("renderListOutput() returned error: %v", err)
		}
		output := out.String()

		for _, want := range []string{"SESSION ID", "session-1", "2026-04-29T10:20:30", "list-pager"} {
			if !strings.Contains(output, want) {
				t.Errorf("renderListOutput() missing %q in %q", want, output)
			}
		}
	})
}

func TestShouldPageListOutput(t *testing.T) {
	origStdinStat := stdinStat
	origStdoutStat := stdoutStat
	t.Cleanup(func() {
		stdinStat = origStdinStat
		stdoutStat = origStdoutStat
	})

	stdinStat = func() (os.FileInfo, error) {
		return fileInfoStub{mode: os.ModeCharDevice}, nil
	}
	stdoutStat = func() (os.FileInfo, error) {
		return fileInfoStub{mode: os.ModeCharDevice}, nil
	}

	t.Run("interactive terminal", func(t *testing.T) {
		t.Setenv("TERM", "xterm-256color")

		if !shouldPageListOutput(listFlags{}) {
			t.Fatal("shouldPageListOutput() should page for an interactive terminal")
		}
	})

	t.Run("json bypasses pager", func(t *testing.T) {
		t.Setenv("TERM", "xterm-256color")

		if shouldPageListOutput(listFlags{json: true}) {
			t.Fatal("shouldPageListOutput() should not page JSON output")
		}
	})

	t.Run("no pager flag bypasses pager", func(t *testing.T) {
		t.Setenv("TERM", "xterm-256color")

		if shouldPageListOutput(listFlags{noPager: true}) {
			t.Fatal("shouldPageListOutput() should not page with --no-pager")
		}
	})

	t.Run("dumb terminal bypasses pager", func(t *testing.T) {
		t.Setenv("TERM", "dumb")

		if shouldPageListOutput(listFlags{}) {
			t.Fatal("shouldPageListOutput() should not page dumb terminals")
		}
	})

	t.Run("piped stdout bypasses pager", func(t *testing.T) {
		t.Setenv("TERM", "xterm-256color")
		stdoutStat = func() (os.FileInfo, error) {
			return fileInfoStub{mode: 0}, nil
		}
		t.Cleanup(func() {
			stdoutStat = func() (os.FileInfo, error) {
				return fileInfoStub{mode: os.ModeCharDevice}, nil
			}
		})

		if shouldPageListOutput(listFlags{}) {
			t.Fatal("shouldPageListOutput() should not page non-tty stdout")
		}
	})
}

func TestResolvePagerCommand(t *testing.T) {
	origLookupPath := lookupPath
	t.Cleanup(func() {
		lookupPath = origLookupPath
	})

	t.Run("default less", func(t *testing.T) {
		t.Setenv("PAGER", "")
		lookupPath = func(file string) (string, error) {
			if file != "less" {
				t.Fatalf("lookupPath() file = %q, want less", file)
			}
			return "/bin/less", nil
		}

		pager, args, ok := resolvePagerCommand()
		if !ok || pager != "/bin/less" || strings.Join(args, " ") != "-FRSX" {
			t.Fatalf("resolvePagerCommand() = %q %q %v, want /bin/less -FRSX true", pager, args, ok)
		}
	})

	t.Run("missing default less disables pager", func(t *testing.T) {
		t.Setenv("PAGER", "")
		lookupPath = func(file string) (string, error) {
			return "", errors.New("not found")
		}

		if _, _, ok := resolvePagerCommand(); ok {
			t.Fatal("resolvePagerCommand() should disable paging when default less is missing")
		}
	})

	t.Run("custom pager parses executable and args without shell", func(t *testing.T) {
		t.Setenv("PAGER", "bat --paging=always")
		lookupPath = func(file string) (string, error) {
			if file != "bat" {
				t.Fatalf("lookupPath() file = %q, want bat", file)
			}
			return "/bin/bat", nil
		}

		pager, args, ok := resolvePagerCommand()
		if !ok || pager != "/bin/bat" || strings.Join(args, " ") != "--paging=always" {
			t.Fatalf("resolvePagerCommand() = %q %q %v, want /bin/bat --paging=always true", pager, args, ok)
		}
	})
}
