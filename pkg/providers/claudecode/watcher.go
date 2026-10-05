package claudecode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/tracer-ai/tracer-cli/pkg/spi"
)

// newFSWatcher creates the watchers' fsnotify watchers.
// Why a variable: tests replace it to drop events and inject errors, which a
// real event queue overflow does but cannot be made to do deterministically.
var newFSWatcher = fsnotify.NewWatcher

func ensureClaudeDirWatch(claudeDir string, projectsDir string, addWatch func(string) error, watchProjectsRoot func()) {
	if err := addWatch(claudeDir); err != nil {
		slog.Warn("watchClaudeProjects: failed to watch Claude directory", "directory", claudeDir, "error", err)
		return
	}
	if info, err := os.Stat(projectsDir); err == nil && info.IsDir() {
		watchProjectsRoot()
	}
}

// catchUp, when set, runs once after every existing project directory is
// watched and before the first event is handled (see spi.CatchUpWatcher).
func watchClaudeProjects(ctx context.Context, debugRaw bool, catchUp func(), sessionCallback func(*spi.AgentChatSession)) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home directory: %w", err)
	}

	claudeDir := filepath.Join(homeDir, ".claude")
	projectsDir := filepath.Join(claudeDir, "projects")

	watcher, err := newFSWatcher()
	if err != nil {
		return fmt.Errorf("failed to create Claude watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	// Keys are session files, or project directories that appeared and need a
	// full scan.
	throttle := newClaudeThrottle(func(key string) {
		if strings.HasSuffix(key, ".jsonl") {
			emitClaudeSessions(filepath.Dir(key), debugRaw, sessionCallback, key)
			return
		}
		emitClaudeSessions(key, debugRaw, sessionCallback)
	})
	defer throttle.Stop()

	watches := spi.NewDirWatches(watcher)
	addWatch := func(dir string) error {
		_, err := watches.Add(dir)
		return err
	}

	watchProjectDir := func(projectDir string) {
		if err := addWatch(projectDir); err != nil {
			slog.Warn("watchClaudeProjects: failed to watch project directory", "directory", projectDir, "error", err)
		}
	}

	watchProjectsRoot := func() {
		if err := addWatch(projectsDir); err != nil {
			slog.Warn("watchClaudeProjects: failed to watch Claude projects root", "directory", projectsDir, "error", err)
			return
		}

		projectDirs, err := ListClaudeCodeProjectDirs()
		if err != nil {
			slog.Warn("watchClaudeProjects: failed to list Claude project directories", "error", err)
			return
		}
		for _, projectDir := range projectDirs {
			watchProjectDir(projectDir)
		}
	}

	if err := addWatch(homeDir); err != nil {
		return fmt.Errorf("failed to watch home directory: %w", err)
	}
	// Already watched directories are skipped, so this also picks up
	// directories whose creation event was lost.
	watchExisting := func() {
		if info, err := os.Stat(claudeDir); err == nil && info.IsDir() {
			ensureClaudeDirWatch(claudeDir, projectsDir, addWatch, watchProjectsRoot)
		}
		if info, err := os.Stat(projectsDir); err == nil && info.IsDir() {
			watchProjectsRoot()
		}
	}
	watchExisting()
	// Recovery from an overflow adds every directory again, because any of
	// them may have been replaced unseen.
	rewatchAll := func() {
		watches.Reset()
		if err := addWatch(homeDir); err != nil {
			slog.Warn("watchClaudeProjects: failed to watch home directory", "directory", homeDir, "error", err)
		}
		watchExisting()
	}
	if catchUp != nil {
		catchUp()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			if event.Has(fsnotify.Create) {
				if strings.EqualFold(event.Name, claudeDir) {
					ensureClaudeDirWatch(claudeDir, projectsDir, addWatch, watchProjectsRoot)
					continue
				}
				if strings.EqualFold(event.Name, projectsDir) {
					watchProjectsRoot()
					continue
				}
				if filepath.Dir(event.Name) == projectsDir {
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						watchProjectDir(event.Name)
						throttle.Trigger(event.Name)
						continue
					}
				}
			}

			if strings.HasSuffix(event.Name, ".jsonl") && (event.Has(fsnotify.Create) || event.Has(fsnotify.Write)) {
				throttle.Trigger(event.Name)
			}
			if strings.HasSuffix(event.Name, ".jsonl") && (event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename)) {
				claudeTails.forget(event.Name)
			}
			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				watches.Forget(event.Name)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) && catchUp != nil {
				spi.RecoverFromOverflow("claude", throttle, rewatchAll, catchUp)
				continue
			}
			slog.Warn("watchClaudeProjects: watcher error", "error", err)
		}
	}
}

func watchClaudeProject(ctx context.Context, claudeProjectDir string, debugRaw bool, catchUp func(), sessionCallback func(*spi.AgentChatSession)) error {
	watcher, err := newFSWatcher()
	if err != nil {
		return fmt.Errorf("failed to create Claude project watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	throttle := newClaudeThrottle(func(path string) {
		emitClaudeSessions(claudeProjectDir, debugRaw, sessionCallback, path)
	})
	defer throttle.Stop()

	parentDir := filepath.Dir(claudeProjectDir)
	projectDirWatched := false

	if info, err := os.Stat(claudeProjectDir); err == nil && info.IsDir() {
		if err := watcher.Add(claudeProjectDir); err != nil {
			return fmt.Errorf("failed to watch Claude project directory: %w", err)
		}
		projectDirWatched = true
	} else {
		if err := watcher.Add(parentDir); err != nil {
			return fmt.Errorf("failed to watch Claude project parent directory: %w", err)
		}
	}
	if catchUp != nil {
		catchUp()
	}
	watchProjectDir := func() bool {
		if info, err := os.Stat(claudeProjectDir); err != nil || !info.IsDir() {
			return false
		}
		if err := watcher.Add(claudeProjectDir); err != nil {
			slog.Warn("watchClaudeProject: failed to watch project directory", "directory", claudeProjectDir, "error", err)
			return false
		}
		projectDirWatched = true
		return true
	}
	// rewatch watches the project directory if it exists, or else its parent,
	// so that its creation produces an event. It runs when the directory is
	// removed or renamed, and after an overflow, when it may have been
	// created, or replaced at the same path, while the events were lost.
	// Adding a directory already watched does nothing (see spi.DirWatches.Reset).
	rewatch := func() {
		if watchProjectDir() {
			return
		}
		projectDirWatched = false
		if err := watcher.Add(parentDir); err != nil {
			slog.Warn("watchClaudeProject: failed to watch project parent directory", "directory", parentDir, "error", err)
			return
		}
		// Checked again because the directory may have been created before
		// its parent was watched, which produced no event.
		watchProjectDir()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			if !projectDirWatched && event.Has(fsnotify.Create) && strings.EqualFold(event.Name, claudeProjectDir) {
				if err := watcher.Add(claudeProjectDir); err != nil {
					return fmt.Errorf("failed to watch created Claude project directory: %w", err)
				}
				projectDirWatched = true
				continue
			}
			if strings.EqualFold(event.Name, claudeProjectDir) && (event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename)) {
				rewatch()
				continue
			}

			if strings.HasSuffix(event.Name, ".jsonl") && (event.Has(fsnotify.Create) || event.Has(fsnotify.Write)) {
				throttle.Trigger(event.Name)
			}
			if strings.HasSuffix(event.Name, ".jsonl") && (event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename)) {
				claudeTails.forget(event.Name)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) && catchUp != nil {
				spi.RecoverFromOverflow("claude", throttle, rewatch, catchUp)
				continue
			}
			slog.Warn("watchClaudeProject: watcher error", "error", err)
		}
	}
}

// newClaudeThrottle serializes Claude parses. One session can span several
// files and every parse reads all of them, so two concurrent runs triggered by
// different files of the same session could deliver an older snapshot after a
// newer one. Running one at a time, with callbacks delivered synchronously,
// keeps snapshots in the order their files were read.
func newClaudeThrottle(run func(key string)) *spi.FileThrottle {
	return spi.NewSessionFileThrottle(1, run)
}

// emitClaudeSessions parses Claude sessions and calls sessionCallback for each.
// With a changedFile, only the session that file belongs to is emitted, and it
// is rebuilt incrementally from cached records (see claudeTailCache).
// Without one, every session in the project directory is parsed from scratch.
func emitClaudeSessions(claudeProjectDir string, debugRaw bool, sessionCallback func(*spi.AgentChatSession), changedFile ...string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("emitClaudeSessions: PANIC recovered", "panic", r)
		}
	}()

	var agentSessions []*spi.AgentChatSession
	if len(changedFile) > 0 && changedFile[0] != "" {
		sessions, err := claudeTails.agentSessionsForFile(claudeProjectDir, changedFile[0], debugRaw)
		if err != nil {
			slog.Warn("emitClaudeSessions: failed to parse changed session", "file", changedFile[0], "error", err)
			return
		}
		agentSessions = sessions
	} else {
		parser := NewJSONLParser()
		if err := parser.ParseProjectSessions(claudeProjectDir, true); err != nil {
			slog.Warn("emitClaudeSessions: failed to parse project sessions", "directory", claudeProjectDir, "error", err)
			return
		}
		for _, session := range parser.Sessions {
			if agentSession := convertToAgentChatSession(session, "", debugRaw); agentSession != nil {
				agentSessions = append(agentSessions, agentSession)
			}
		}
	}

	for _, agentSession := range agentSessions {
		// Delivered synchronously so snapshots reach the engine in the order
		// they were parsed; the callback only queues the update.
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("emitClaudeSessions: callback panicked", "panic", r)
				}
			}()
			sessionCallback(agentSession)
		}()
	}
}

func convertToAgentChatSession(session Session, workspaceRoot string, debugRaw bool) *spi.AgentChatSession {
	if len(session.Records) == 0 {
		return nil
	}

	if debugRaw {
		writeDebugRawFiles(session)
	}

	filteredRecords := filterWarmupMessages(session.Records)
	if len(filteredRecords) == 0 {
		slog.Debug("Skipping warmup-only session", "sessionId", session.SessionUuid)
		return nil
	}

	filteredSession := Session{
		SessionUuid: session.SessionUuid,
		Records:     filteredRecords,
	}

	rootRecord := filteredSession.Records[0]
	timestamp, ok := rootRecord.Data["timestamp"].(string)
	if !ok {
		slog.Warn("convertToAgentChatSession: No timestamp found in root record")
		return nil
	}

	slug := FileSlugFromRootRecord(filteredSession)

	sessionData, err := GenerateAgentSession(filteredSession, workspaceRoot)
	if err != nil {
		slog.Error("Failed to generate SessionData", "sessionId", session.SessionUuid, "error", err)
		return nil
	}

	return &spi.AgentChatSession{
		SessionID:   session.SessionUuid,
		CreatedAt:   timestamp,
		Slug:        slug,
		SessionData: sessionData,
	}
}
