package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/tracer-ai/tracer-cli/pkg/spi"
)

func ensureClaudeDirWatch(claudeDir string, projectsDir string, addWatch func(string) error, watchProjectsRoot func()) {
	if err := addWatch(claudeDir); err != nil {
		slog.Warn("watchClaudeProjects: failed to watch Claude directory", "directory", claudeDir, "error", err)
		return
	}
	if info, err := os.Stat(projectsDir); err == nil && info.IsDir() {
		watchProjectsRoot()
	}
}

func watchClaudeProjects(ctx context.Context, debugRaw bool, sessionCallback func(*spi.AgentChatSession)) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home directory: %w", err)
	}

	claudeDir := filepath.Join(homeDir, ".claude")
	projectsDir := filepath.Join(claudeDir, "projects")

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create Claude watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	watchedDirs := make(map[string]bool)
	addWatch := func(dir string) error {
		if watchedDirs[dir] {
			return nil
		}
		if err := watcher.Add(dir); err != nil {
			return err
		}
		watchedDirs[dir] = true
		return nil
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
	if info, err := os.Stat(claudeDir); err == nil && info.IsDir() {
		ensureClaudeDirWatch(claudeDir, projectsDir, addWatch, watchProjectsRoot)
	}
	if info, err := os.Stat(projectsDir); err == nil && info.IsDir() {
		watchProjectsRoot()
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
						emitClaudeSessions(event.Name, debugRaw, sessionCallback)
						continue
					}
				}
			}

			if strings.HasSuffix(event.Name, ".jsonl") && (event.Has(fsnotify.Create) || event.Has(fsnotify.Write)) {
				emitClaudeSessions(filepath.Dir(event.Name), debugRaw, sessionCallback, event.Name)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			slog.Warn("watchClaudeProjects: watcher error", "error", err)
		}
	}
}

func watchClaudeProject(ctx context.Context, claudeProjectDir string, debugRaw bool, sessionCallback func(*spi.AgentChatSession)) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create Claude project watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

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

			if strings.HasSuffix(event.Name, ".jsonl") && (event.Has(fsnotify.Create) || event.Has(fsnotify.Write)) {
				emitClaudeSessions(claudeProjectDir, debugRaw, sessionCallback, event.Name)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			slog.Warn("watchClaudeProject: watcher error", "error", err)
		}
	}
}

func emitClaudeSessions(claudeProjectDir string, debugRaw bool, sessionCallback func(*spi.AgentChatSession), changedFile ...string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("emitClaudeSessions: PANIC recovered", "panic", r)
		}
	}()

	parser := NewJSONLParser()
	targetSessionUUID := ""

	if len(changedFile) > 0 && changedFile[0] != "" {
		sessionID, err := extractSessionIDFromFile(changedFile[0])
		if err != nil || sessionID == "" {
			return
		}
		targetSessionUUID = sessionID
	}

	var parseErr error
	if targetSessionUUID != "" {
		parseErr = parser.ParseProjectSessionsForSession(claudeProjectDir, true, targetSessionUUID)
	} else {
		parseErr = parser.ParseProjectSessions(claudeProjectDir, true)
	}
	if parseErr != nil {
		slog.Warn("emitClaudeSessions: failed to parse project sessions", "directory", claudeProjectDir, "error", parseErr)
		return
	}

	for _, session := range parser.Sessions {
		if len(session.Records) == 0 {
			continue
		}
		if targetSessionUUID != "" && session.SessionUuid != targetSessionUUID {
			continue
		}

		agentSession := convertToAgentChatSession(session, "", debugRaw)
		if agentSession == nil {
			continue
		}

		sessionCopy := *agentSession
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("emitClaudeSessions: callback panicked", "panic", r)
				}
			}()
			sessionCallback(&sessionCopy)
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

	var rawDataBuilder strings.Builder
	for _, record := range session.Records {
		jsonBytes, _ := json.Marshal(record.Data)
		rawDataBuilder.Write(jsonBytes)
		rawDataBuilder.WriteString("\n")
	}

	return &spi.AgentChatSession{
		SessionID:   session.SessionUuid,
		CreatedAt:   timestamp,
		Slug:        slug,
		SessionData: sessionData,
		RawData:     rawDataBuilder.String(),
	}
}
