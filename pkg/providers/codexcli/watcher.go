package codexcli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/tracer-ai/tracer-cli/pkg/log"
	"github.com/tracer-ai/tracer-cli/pkg/spi"
)

// newFSWatcher creates the watcher's fsnotify watcher.
// Why a variable: tests replace it to drop events and inject errors, which a
// real event queue overflow does but cannot be made to do deterministically.
var newFSWatcher = fsnotify.NewWatcher

// WatchForCodexSessions watches for Codex CLI sessions that match the given project path.
// It watches hierarchically for new sessions, handling date changes across days/months/years.
// catchUp, when set, runs once after every existing directory is watched and
// before the first event is handled (see spi.CatchUpWatcher).
func WatchForCodexSessions(ctx context.Context, projectPath string, debugRaw bool, catchUp func(), sessionCallback func(*spi.AgentChatSession)) error {
	slog.Info("WatchForCodexSessions: Starting Codex session watcher", "projectPath", projectPath)

	homeDir, err := osUserHomeDir()
	if err != nil {
		slog.Error("WatchForCodexSessions: Failed to get home directory", "error", err)
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	sessionsRoot := codexSessionsRoot(homeDir)
	slog.Info("WatchForCodexSessions: Sessions root", "path", sessionsRoot)

	return startCodexSessionWatcher(ctx, projectPath, sessionsRoot, debugRaw, catchUp, sessionCallback)
}

// dirType determines the type of directory relative to sessionsRoot based on the
// YYYY/MM/DD structure. Returns "year", "month", "day", or "" if not a recognized type.
func dirType(path string, sessionsRoot string) string {
	rel, err := filepath.Rel(sessionsRoot, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "" // Path is not under sessionsRoot
	}

	parts := strings.Split(filepath.ToSlash(rel), "/")
	switch len(parts) {
	case 1: // YYYY
		if len(parts[0]) == 4 {
			return "year"
		}
	case 2: // YYYY/MM
		if len(parts[0]) == 4 && len(parts[1]) == 2 {
			return "month"
		}
	case 3: // YYYY/MM/DD
		if len(parts[0]) == 4 && len(parts[1]) == 2 && len(parts[2]) == 2 {
			return "day"
		}
	}
	return ""
}

// startCodexSessionWatcher starts watching hierarchically for Codex sessions.
// Watches sessionsRoot/YYYY/MM/DD/ structure to handle date changes across days, months, and years.
func startCodexSessionWatcher(ctx context.Context, projectPath string, sessionsRoot string, debugRaw bool, catchUp func(), sessionCallback func(*spi.AgentChatSession)) error {
	slog.Info("startCodexSessionWatcher: Creating hierarchical watcher", "sessionsRoot", sessionsRoot)

	// Create a new watcher
	watcher, err := newFSWatcher()
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %v", err)
	}
	defer func() {
		if err := watcher.Close(); err != nil {
			slog.Debug("startCodexSessionWatcher: Error closing watcher", "error", err)
		}
	}()

	// Each Codex rollout file is exactly one session, so parses of different
	// files never race over the same archive entry and can run two at a time.
	throttle := spi.NewSessionFileThrottle(2, func(path string) {
		ScanCodexSessions(projectPath, filepath.Dir(path), &path, debugRaw, sessionCallback)
	})
	// Stop runs before the watcher closes so no parse outlives this function.
	defer throttle.Stop()

	watchedDirs := make(map[string]bool)
	var watchedDirsMutex sync.Mutex

	addWatch := func(dir string) error {
		watchedDirsMutex.Lock()
		defer watchedDirsMutex.Unlock()

		if watchedDirs[dir] {
			return nil
		}

		if err := watcher.Add(dir); err != nil {
			return err
		}
		watchedDirs[dir] = true
		slog.Info("startCodexSessionWatcher: Added watch", "directory", dir)
		return nil
	}

	// Day directories that appear while watching are fed through the throttle
	// file by file: a parse of one of their files may already be scheduled or
	// in flight, and one file must never be parsed by two runs at once, or an
	// older snapshot could land after a newer one.
	scanDayDir := func(dayDir string) {
		entries, err := os.ReadDir(dayDir)
		if err != nil {
			slog.Warn("startCodexSessionWatcher: Cannot read day directory", "directory", dayDir, "error", err)
			return
		}
		slog.Info("startCodexSessionWatcher: Scanning day directory", "directory", dayDir)
		for _, entry := range entries {
			if !entry.IsDir() && filepath.Ext(entry.Name()) == ".jsonl" {
				throttle.Trigger(filepath.Join(dayDir, entry.Name()))
			}
		}
	}

	// The scan flag is false while registering the existing tree at startup:
	// those files are covered by catchUp, which parses only the ones that
	// changed since historical ingest. Directories created later are scanned
	// because files can appear in them before their watch is added.
	watchDayDir := func(dayDir string, scan bool) {
		if err := addWatch(dayDir); err != nil {
			slog.Error("startCodexSessionWatcher: Failed to watch day directory",
				"directory", dayDir,
				"error", err)
			return
		}
		if scan {
			scanDayDir(dayDir)
		}
	}

	watchMonthDir := func(monthDir string, scan bool) {
		if err := addWatch(monthDir); err != nil {
			slog.Error("startCodexSessionWatcher: Failed to watch month directory",
				"directory", monthDir,
				"error", err)
			return
		}

		entries, err := os.ReadDir(monthDir)
		if err != nil {
			slog.Debug("startCodexSessionWatcher: Cannot read month directory",
				"directory", monthDir,
				"error", err)
			return
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if len(entry.Name()) == 2 {
				dayDir := filepath.Join(monthDir, entry.Name())
				watchDayDir(dayDir, scan)
			}
		}
	}

	watchYearDir := func(yearDir string, scan bool) {
		if err := addWatch(yearDir); err != nil {
			slog.Error("startCodexSessionWatcher: Failed to watch year directory",
				"directory", yearDir,
				"error", err)
			return
		}

		entries, err := os.ReadDir(yearDir)
		if err != nil {
			slog.Debug("startCodexSessionWatcher: Cannot read year directory",
				"directory", yearDir,
				"error", err)
			return
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if len(entry.Name()) == 2 {
				monthDir := filepath.Join(yearDir, entry.Name())
				watchMonthDir(monthDir, scan)
			}
		}
	}

	if err := addWatch(sessionsRoot); err != nil {
		log.UserError("Error watching sessions root: %v", err)
		slog.Error("startCodexSessionWatcher: Failed to watch sessions root", "error", err)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %w", spi.ErrNothingToWatch, err)
		}
		return err
	}

	// Registers every existing directory without scanning its files, which
	// catchUp covers. Already watched directories are skipped.
	watchExistingTree := func() {
		entries, err := os.ReadDir(sessionsRoot)
		if err != nil {
			slog.Warn("startCodexSessionWatcher: Cannot read sessions root",
				"directory", sessionsRoot,
				"error", err)
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if len(entry.Name()) == 4 {
				yearDir := filepath.Join(sessionsRoot, entry.Name())
				watchYearDir(yearDir, false)
			}
		}
	}
	watchExistingTree()

	// Why before the event loop: writes made between historical ingest and the
	// watches above produced no event. Events that arrive while catchUp runs
	// wait in the watcher's queue, so their parses land after catchUp's.
	if catchUp != nil {
		catchUp()
	}

	slog.Info("startCodexSessionWatcher: Now watching for file and directory events")
	for {
		select {
		case <-ctx.Done():
			slog.Info("startCodexSessionWatcher: Context cancelled, stopping watcher")
			return ctx.Err()
		case event, ok := <-watcher.Events:
			if !ok {
				slog.Info("startCodexSessionWatcher: Watcher events channel closed")
				return nil
			}

			if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) && !event.Has(fsnotify.Remove) && !event.Has(fsnotify.Rename) {
				continue
			}

			eventPath := event.Name

			if strings.HasSuffix(eventPath, ".jsonl") {
				switch {
				case event.Has(fsnotify.Create), event.Has(fsnotify.Write):
					slog.Info("startCodexSessionWatcher: JSONL file event",
						"operation", event.Op.String(),
						"file", eventPath)
					throttle.Trigger(eventPath)
				case event.Has(fsnotify.Remove), event.Has(fsnotify.Rename):
					// Each rollout is its own session, so removing one changes no
					// other session's archive and needs no rescan; only its cached
					// read state goes. The removed session's archive is kept.
					slog.Info("startCodexSessionWatcher: JSONL file removed", "file", eventPath)
					codexTails.forget(eventPath)
				}
				continue
			}

			if event.Has(fsnotify.Create) {
				switch dirType(eventPath, sessionsRoot) {
				case "year":
					slog.Info("startCodexSessionWatcher: New year directory created", "directory", eventPath)
					watchYearDir(eventPath, true)
				case "month":
					slog.Info("startCodexSessionWatcher: New month directory created", "directory", eventPath)
					watchMonthDir(eventPath, true)
				case "day":
					slog.Info("startCodexSessionWatcher: New day directory created", "directory", eventPath)
					watchDayDir(eventPath, true)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				slog.Info("startCodexSessionWatcher: Watcher errors channel closed")
				return nil
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) && catchUp != nil {
				spi.RecoverFromOverflow("codex", throttle, watchExistingTree, catchUp)
				continue
			}
			log.UserWarn("Watcher error: %v", err)
			slog.Error("startCodexSessionWatcher: Watcher error", "error", err)
			return fmt.Errorf("codex watcher error: %w", err)
		}
	}
}

// ScanCodexSessions scans JSONL files in the session directory and processes sessions
// that match the project path. If changedFile is nil, scans all JSONL files in the directory.
// If changedFile is non-nil, only processes that specific file.
func ScanCodexSessions(projectPath string, sessionDir string, changedFile *string, debugRaw bool, callback func(*spi.AgentChatSession)) {
	// Ensure logs are flushed even if we panic
	defer func() {
		if r := recover(); r != nil {
			log.UserWarn("PANIC in ScanCodexSessions: %v", r)
			slog.Error("ScanCodexSessions: PANIC recovered", "panic", r)
		}
	}()

	slog.Info("ScanCodexSessions: === START SCAN ===", "timestamp", time.Now().Format(time.RFC3339))
	if changedFile != nil {
		slog.Info("ScanCodexSessions: Scanning with changed file",
			"projectPath", projectPath,
			"sessionDir", sessionDir,
			"changedFile", *changedFile)
	} else {
		slog.Info("ScanCodexSessions: Scanning all sessions",
			"projectPath", projectPath,
			"sessionDir", sessionDir)
	}

	if callback == nil {
		slog.Error("ScanCodexSessions: No callback provided - this should not happen")
		return
	}

	// Normalize project path for comparison
	normalizedProjectPath := normalizeCodexPath(projectPath)
	if normalizedProjectPath == "" {
		slog.Debug("ScanCodexSessions: Unable to normalize project path", "projectPath", projectPath)
	}

	// If we have a specific changed file, only process that file
	if changedFile != nil {
		if err := processCodexSessionFile(*changedFile, projectPath, normalizedProjectPath, debugRaw, callback); err != nil {
			slog.Debug("ScanCodexSessions: Failed to process changed file",
				"file", *changedFile,
				"error", err)
		}
		slog.Info("ScanCodexSessions: === END SCAN ===", "timestamp", time.Now().Format(time.RFC3339))
		return
	}

	// Otherwise, scan all JSONL files in the session directory
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		slog.Error("ScanCodexSessions: Failed to read session directory",
			"directory", sessionDir,
			"error", err)
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}

		sessionPath := filepath.Join(sessionDir, entry.Name())
		if err := processCodexSessionFile(sessionPath, projectPath, normalizedProjectPath, debugRaw, callback); err != nil {
			slog.Debug("ScanCodexSessions: Failed to process session file",
				"file", sessionPath,
				"error", err)
			// Continue processing other files even if one fails
		}
	}

	slog.Info("ScanCodexSessions: Processing complete")
	slog.Info("ScanCodexSessions: === END SCAN ===", "timestamp", time.Now().Format(time.RFC3339))
}

// processCodexSessionFile processes a single Codex session file and calls the callback
// if the session matches the project path. Only lines appended since the previous
// call for the same file are decoded (see codexTailCache).
func processCodexSessionFile(sessionPath string, projectPath string, normalizedProjectPath string, debugRaw bool, callback func(*spi.AgentChatSession)) error {
	return processCodexSessionFileWith(codexTails, sessionPath, projectPath, normalizedProjectPath, debugRaw, callback)
}

func processCodexSessionFileWith(tails *codexTailCache, sessionPath string, projectPath string, normalizedProjectPath string, debugRaw bool, callback func(*spi.AgentChatSession)) error {
	meta, records, err := tails.read(sessionPath, func(meta *codexSessionMeta) bool {
		return codexSessionMatches(meta, projectPath, normalizedProjectPath)
	})
	if err != nil {
		return fmt.Errorf("failed to read session: %w", err)
	}
	if meta == nil {
		slog.Debug("processCodexSessionFile: No matching session in file",
			"sessionPath", sessionPath,
			"projectPath", normalizedProjectPath)
		return nil // Not an error, just doesn't match
	}

	sessionID := strings.TrimSpace(meta.Payload.ID)
	slog.Info("processCodexSessionFile: Session matched project",
		"sessionID", sessionID,
		"sessionPath", sessionPath)

	sessionInfo := &codexSessionInfo{
		SessionID:   sessionID,
		SessionPath: sessionPath,
		Meta:        meta,
	}

	agentSession, err := agentChatFromRecords(sessionInfo, records, projectPath, debugRaw)
	if err != nil {
		return fmt.Errorf("failed to process session: %w", err)
	}

	// Skip empty sessions
	if agentSession == nil {
		slog.Debug("processCodexSessionFile: Skipping empty session", "sessionID", sessionID)
		return nil
	}

	// Skip sessions without user message (empty slug)
	if agentSession.Slug == "" {
		slog.Debug("processCodexSessionFile: Skipping session without user message",
			"sessionID", agentSession.SessionID)
		return nil
	}

	slog.Info("processCodexSessionFile: Calling callback for session", "sessionID", agentSession.SessionID)
	// Delivered synchronously so a file's snapshots reach the engine in the
	// order they were parsed; the callback only queues the update.
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("processCodexSessionFile: Callback panicked", "panic", r)
			}
		}()
		callback(agentSession)
	}()

	return nil
}
