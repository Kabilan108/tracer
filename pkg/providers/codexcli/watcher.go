package codexcli

import (
	"context"
	"fmt"
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

// WatchForCodexSessions watches for Codex CLI sessions that match the given project path.
// If resumeSessionID is provided, it finds and watches the directory containing that session.
// Otherwise, watches hierarchically for new sessions, handling date changes across days/months/years.
func WatchForCodexSessions(ctx context.Context, projectPath string, resumeSessionID string, debugRaw bool, sessionCallback func(*spi.AgentChatSession)) error {
	slog.Info("WatchForCodexSessions: Starting Codex session watcher",
		"projectPath", projectPath,
		"resumeSessionID", resumeSessionID)

	homeDir, err := osUserHomeDir()
	if err != nil {
		slog.Error("WatchForCodexSessions: Failed to get home directory", "error", err)
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	sessionsRoot := codexSessionsRoot(homeDir)
	slog.Info("WatchForCodexSessions: Sessions root", "path", sessionsRoot)

	var initialDayDir string

	if resumeSessionID != "" {
		// Find the directory containing the resumed session
		slog.Info("WatchForCodexSessions: Finding directory for resumed session", "sessionID", resumeSessionID)

		// Use findCodexSessions to locate the specific session (will short-circuit when found)
		sessions, err := findCodexSessions(projectPath, resumeSessionID, false)
		if err != nil {
			slog.Error("WatchForCodexSessions: Failed to find resumed session", "error", err)
			return fmt.Errorf("failed to find resumed session: %w", err)
		}

		// Check if session was found
		if len(sessions) == 0 {
			slog.Error("WatchForCodexSessions: Resumed session not found", "sessionID", resumeSessionID)
			return fmt.Errorf("resumed session %s not found", resumeSessionID)
		}

		// Get the directory containing the session file
		initialDayDir = filepath.Dir(sessions[0].SessionPath)
		slog.Info("WatchForCodexSessions: Found resumed session directory", "path", initialDayDir)
	} else {
		// Calculate today's directory (will be watched along with hierarchical watching)
		now := time.Now()
		initialDayDir = filepath.Join(sessionsRoot, fmt.Sprintf("%04d", now.Year()), fmt.Sprintf("%02d", now.Month()), fmt.Sprintf("%02d", now.Day()))
		slog.Info("WatchForCodexSessions: Initial day directory", "path", initialDayDir)
	}

	return startCodexSessionWatcher(ctx, projectPath, sessionsRoot, initialDayDir, debugRaw, sessionCallback)
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
// The initialDayDir is scanned immediately if it exists.
func startCodexSessionWatcher(ctx context.Context, projectPath string, sessionsRoot string, initialDayDir string, debugRaw bool, sessionCallback func(*spi.AgentChatSession)) error {
	slog.Info("startCodexSessionWatcher: Creating hierarchical watcher",
		"sessionsRoot", sessionsRoot,
		"initialDayDir", initialDayDir)

	// Create a new watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %v", err)
	}
	defer func() {
		if err := watcher.Close(); err != nil {
			slog.Debug("startCodexSessionWatcher: Error closing watcher", "error", err)
		}
	}()

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

	scanDayDir := func(dayDir string) {
		if _, err := os.Stat(dayDir); err == nil {
			slog.Info("startCodexSessionWatcher: Scanning day directory", "directory", dayDir)
			ScanCodexSessions(projectPath, dayDir, nil, debugRaw, sessionCallback)
		}
	}

	watchDayDir := func(dayDir string) {
		if err := addWatch(dayDir); err != nil {
			slog.Error("startCodexSessionWatcher: Failed to watch day directory",
				"directory", dayDir,
				"error", err)
			return
		}
		scanDayDir(dayDir)
	}

	watchMonthDir := func(monthDir string) {
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
				watchDayDir(dayDir)
			}
		}
	}

	watchYearDir := func(yearDir string) {
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
				watchMonthDir(monthDir)
			}
		}
	}

	if err := addWatch(sessionsRoot); err != nil {
		log.UserError("Error watching sessions root: %v", err)
		slog.Error("startCodexSessionWatcher: Failed to watch sessions root", "error", err)
		return err
	}

	entries, err := os.ReadDir(sessionsRoot)
	if err != nil {
		slog.Warn("startCodexSessionWatcher: Cannot read sessions root",
			"directory", sessionsRoot,
			"error", err)
	} else {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if len(entry.Name()) == 4 {
				yearDir := filepath.Join(sessionsRoot, entry.Name())
				watchYearDir(yearDir)
			}
		}
	}

	scanDayDir(initialDayDir)

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

			if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) && !event.Has(fsnotify.Remove) {
				continue
			}

			eventPath := event.Name
			parentDir := filepath.Dir(eventPath)

			if strings.HasSuffix(eventPath, ".jsonl") {
				switch {
				case event.Has(fsnotify.Create), event.Has(fsnotify.Write):
					slog.Info("startCodexSessionWatcher: JSONL file event",
						"operation", event.Op.String(),
						"file", eventPath)
					ScanCodexSessions(projectPath, parentDir, &eventPath, debugRaw, sessionCallback)
				case event.Has(fsnotify.Remove):
					slog.Info("startCodexSessionWatcher: JSONL file removed", "file", eventPath)
					ScanCodexSessions(projectPath, parentDir, nil, debugRaw, sessionCallback)
				}
				continue
			}

			if event.Has(fsnotify.Create) {
				switch dirType(eventPath, sessionsRoot) {
				case "year":
					slog.Info("startCodexSessionWatcher: New year directory created", "directory", eventPath)
					watchYearDir(eventPath)
				case "month":
					slog.Info("startCodexSessionWatcher: New month directory created", "directory", eventPath)
					watchMonthDir(eventPath)
				case "day":
					slog.Info("startCodexSessionWatcher: New day directory created", "directory", eventPath)
					watchDayDir(eventPath)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				slog.Info("startCodexSessionWatcher: Watcher errors channel closed")
				return nil
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
// if the session matches the project path.
func processCodexSessionFile(sessionPath string, projectPath string, normalizedProjectPath string, debugRaw bool, callback func(*spi.AgentChatSession)) error {
	// Load session metadata
	meta, err := loadCodexSessionMeta(sessionPath)
	if err != nil {
		return fmt.Errorf("failed to load session meta: %w", err)
	}

	// Check if session matches project path
	sessionID := strings.TrimSpace(meta.Payload.ID)
	normalizedCWD := normalizeCodexPath(meta.Payload.CWD)
	if normalizedCWD == "" {
		slog.Debug("processCodexSessionFile: Session meta missing cwd", "sessionID", sessionID, "path", sessionPath)
		return fmt.Errorf("session meta missing cwd")
	}

	// Empty projectPath means global mode: include every session.
	matched := strings.TrimSpace(projectPath) == ""
	if !matched {
		if normalizedProjectPath != "" {
			matched = normalizedCWD == normalizedProjectPath || strings.EqualFold(normalizedCWD, normalizedProjectPath)
		} else {
			matched = normalizedCWD == projectPath || strings.EqualFold(normalizedCWD, projectPath)
		}
	}

	if !matched {
		slog.Debug("processCodexSessionFile: Session does not match project path",
			"sessionID", sessionID,
			"sessionCWD", normalizedCWD,
			"projectPath", normalizedProjectPath)
		return nil // Not an error, just doesn't match
	}

	slog.Info("processCodexSessionFile: Session matched project",
		"sessionID", sessionID,
		"sessionPath", sessionPath)

	// Create session info
	sessionInfo := &codexSessionInfo{
		SessionID:   sessionID,
		SessionPath: sessionPath,
		Meta:        meta,
	}

	// Process the session
	agentSession, err := processSessionToAgentChat(sessionInfo, projectPath, debugRaw)
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
	// Call the callback in a goroutine to avoid blocking
	go func(s *spi.AgentChatSession) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("processCodexSessionFile: Callback panicked", "panic", r)
			}
		}()
		callback(s)
	}(agentSession)

	return nil
}
