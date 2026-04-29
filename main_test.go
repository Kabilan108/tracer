package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracer-ai/tracer-cli/pkg/config"
	"github.com/tracer-ai/tracer-cli/pkg/engine"
	"github.com/tracer-ai/tracer-cli/pkg/spi"
	"github.com/tracer-ai/tracer-cli/pkg/spi/schema"
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

type getTestProvider struct {
	name    string
	session *spi.AgentChatSession
	queries int
}

func (p *getTestProvider) Name() string {
	return p.name
}

func (p *getTestProvider) Check(customCommand string) spi.CheckResult {
	return spi.CheckResult{Success: true}
}

func (p *getTestProvider) DetectAgent(projectPath string, helpOutput bool) bool {
	return true
}

func (p *getTestProvider) GetAgentChatSessions(projectPath string, debugRaw bool, progress spi.ProgressCallback) ([]spi.AgentChatSession, error) {
	if p.session == nil {
		return nil, nil
	}
	return []spi.AgentChatSession{*p.session}, nil
}

func (p *getTestProvider) GetAgentChatSession(projectPath string, sessionID string, debugRaw bool) (*spi.AgentChatSession, error) {
	p.queries++
	if p.session == nil || p.session.SessionID != sessionID {
		return nil, nil
	}
	sessionCopy := *p.session
	return &sessionCopy, nil
}

func (p *getTestProvider) ListAgentChatSessions(projectPath string) ([]spi.SessionMetadata, error) {
	return nil, nil
}

func (p *getTestProvider) WatchAgent(ctx context.Context, projectPath string, debugRaw bool, sessionCallback func(*spi.AgentChatSession)) error {
	return nil
}

func newGetTestSession(providerID string, sessionID string, workspaceRoot string) *spi.AgentChatSession {
	now := "2026-03-04T00:00:00Z"
	return &spi.AgentChatSession{
		SessionID: sessionID,
		CreatedAt: now,
		Slug:      "test-session",
		SessionData: &schema.SessionData{
			SchemaVersion: "1.0",
			Provider: schema.ProviderInfo{
				ID:      providerID,
				Name:    providerID,
				Version: "test",
			},
			SessionID:     sessionID,
			CreatedAt:     now,
			UpdatedAt:     now,
			Slug:          "test-session",
			WorkspaceRoot: workspaceRoot,
			Exchanges: []schema.Exchange{
				{
					ExchangeID: sessionID + ":0",
					StartTime:  now,
					EndTime:    now,
					Messages: []schema.Message{
						{
							ID:        "msg-1",
							Role:      "user",
							Timestamp: now,
							Content: []schema.ContentPart{
								{Type: "text", Text: "hello from get"},
							},
						},
					},
				},
			},
		},
	}
}

func captureStdout(t *testing.T, run func() error) (string, error) {
	t.Helper()

	origStdout := os.Stdout
	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	os.Stdout = writeFile

	runErr := run()
	if closeErr := writeFile.Close(); closeErr != nil {
		t.Fatalf("stdout pipe close error = %v", closeErr)
	}
	os.Stdout = origStdout

	data, readErr := io.ReadAll(readFile)
	if readErr != nil {
		t.Fatalf("stdout pipe read error = %v", readErr)
	}
	if closeErr := readFile.Close(); closeErr != nil {
		t.Fatalf("stdout pipe read close error = %v", closeErr)
	}
	return string(data), runErr
}

func writeCodexGetSessionFile(t *testing.T, homeDir string, sessionID string, workspaceRoot string) {
	t.Helper()

	sessionDir := filepath.Join(homeDir, ".codex", "sessions", "2026", "03", "04")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("failed to create codex session dir: %v", err)
	}

	records := []map[string]interface{}{
		{
			"type":      "session_meta",
			"timestamp": "2026-03-04T00:00:00Z",
			"payload": map[string]interface{}{
				"id":        sessionID,
				"timestamp": "2026-03-04T00:00:00Z",
				"cwd":       workspaceRoot,
			},
		},
		{
			"type":      "message",
			"timestamp": "2026-03-04T00:00:01Z",
			"payload": map[string]interface{}{
				"role":    "user",
				"content": "hello from command get",
			},
		},
	}

	var content strings.Builder
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("failed to marshal codex record: %v", err)
		}
		content.Write(data)
		content.WriteString("\n")
	}

	sessionPath := filepath.Join(sessionDir, sessionID+".jsonl")
	if err := os.WriteFile(sessionPath, []byte(content.String()), 0o644); err != nil {
		t.Fatalf("failed to write codex session file: %v", err)
	}
}

func withGetCommandGlobals(t *testing.T, archiveRoot string, debugRoot string, cfg *config.Config) {
	t.Helper()

	origOutputDir := outputDir
	origDebugDir := debugDir
	origLoadedConfig := loadedConfig
	origLocalTimeZone := localTimeZone
	t.Cleanup(func() {
		outputDir = origOutputDir
		debugDir = origDebugDir
		loadedConfig = origLoadedConfig
		localTimeZone = origLocalTimeZone
	})

	outputDir = archiveRoot
	debugDir = debugRoot
	loadedConfig = cfg
	localTimeZone = false
}

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

func TestGetCommandRequiresSessionID(t *testing.T) {
	cmd := createGetCommand()
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected missing session-id to fail")
	}
}

func TestGetCommandRejectsEmptySessionID(t *testing.T) {
	cmd := createGetCommand()
	cmd.SetArgs([]string{"   "})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected empty session-id to fail")
	}
}

func TestFindGetMatchesDuplicateMatches(t *testing.T) {
	sessionID := "session-1"
	workspaceRoot := filepath.Join(t.TempDir(), "project")
	providers := map[string]spi.Provider{
		"claude": &getTestProvider{name: "claude", session: newGetTestSession("claude", sessionID, workspaceRoot)},
		"codex":  &getTestProvider{name: "codex", session: newGetTestSession("codex", sessionID, workspaceRoot)},
	}
	pathBuilder := func(providerID string, session *spi.AgentChatSession) string {
		return filepath.Join("/archive", providerID, "project", session.SessionID+".md")
	}

	matches, err := findGetMatches(providers, sessionID, pathBuilder)
	if err != nil {
		t.Fatalf("findGetMatches() error = %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("findGetMatches() returned %d matches, want 2", len(matches))
	}

	err = formatGetAmbiguityError(sessionID, matches)
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "claude") || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("ambiguity error missing providers: %v", err)
	}
}

func TestFindGetMatchesProviderFilter(t *testing.T) {
	sessionID := "session-1"
	workspaceRoot := filepath.Join(t.TempDir(), "project")
	claudeProvider := &getTestProvider{name: "claude", session: newGetTestSession("claude", sessionID, workspaceRoot)}
	codexProvider := &getTestProvider{name: "codex", session: newGetTestSession("codex", sessionID, workspaceRoot)}
	providers := map[string]spi.Provider{"codex": codexProvider}
	pathBuilder := func(providerID string, session *spi.AgentChatSession) string {
		return filepath.Join("/archive", providerID, "project", session.SessionID+".md")
	}

	matches, err := findGetMatches(providers, sessionID, pathBuilder)
	if err != nil {
		t.Fatalf("findGetMatches() error = %v", err)
	}
	if len(matches) != 1 || matches[0].ProviderID != "codex" {
		t.Fatalf("unexpected matches: %+v", matches)
	}
	if claudeProvider.queries != 0 {
		t.Fatalf("filtered provider should not be queried, got %d queries", claudeProvider.queries)
	}
	if codexProvider.queries != 1 {
		t.Fatalf("codex provider queries = %d, want 1", codexProvider.queries)
	}
}

func TestWriteGetSessionMarkdownPathAndContent(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "session-1"
	session := newGetTestSession("codex", sessionID, filepath.Join(tempDir, "project"))
	opts := engine.Options{
		HistoryDir:     filepath.Join(tempDir, "history"),
		StatisticsPath: filepath.Join(tempDir, "stats.json"),
		StateDBPath:    filepath.Join(tempDir, "state.db"),
		UseUTC:         true,
		PathBuilder: func(providerID string, session *spi.AgentChatSession) string {
			return filepath.Join(tempDir, "history", providerID, filepath.Base(session.SessionData.WorkspaceRoot), session.SessionID+".md")
		},
		Debounce: 1,
	}

	if err := writeGetSessionMarkdown("codex", session, opts); err != nil {
		t.Fatalf("writeGetSessionMarkdown() error = %v", err)
	}

	wantPath := filepath.Join(tempDir, "history", "codex", "project", sessionID+".md")
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("failed to read markdown path %s: %v", wantPath, err)
	}
	content := string(data)
	if !strings.Contains(content, "hello from get") {
		t.Fatalf("markdown missing session content: %s", content)
	}
}

func TestWriteGetSessionMarkdownExcludedSession(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "session-1"
	session := newGetTestSession("codex", sessionID, filepath.Join(tempDir, "excluded-project"))
	opts := engine.Options{
		HistoryDir:     filepath.Join(tempDir, "history"),
		StatisticsPath: filepath.Join(tempDir, "stats.json"),
		StateDBPath:    filepath.Join(tempDir, "state.db"),
		UseUTC:         true,
		PathBuilder: func(providerID string, session *spi.AgentChatSession) string {
			return filepath.Join(tempDir, "history", providerID, filepath.Base(session.SessionData.WorkspaceRoot), session.SessionID+".md")
		},
		ShouldProcessSession: func(providerID string, session *spi.AgentChatSession) bool {
			return false
		},
		Debounce: 1,
	}

	err := writeGetSessionMarkdown("codex", session, opts)
	if err == nil {
		t.Fatal("writeGetSessionMarkdown() expected exclusion error")
	}
	if !strings.Contains(err.Error(), "excluded by configuration") || !strings.Contains(err.Error(), "excluded-project") {
		t.Fatalf("writeGetSessionMarkdown() error = %v, want exclusion with workspace root", err)
	}
}

func TestGetCommandMarkdownOutput(t *testing.T) {
	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	workspaceRoot := filepath.Join(tempDir, "workspace", "get-project")
	sessionID := "get-markdown-session"
	t.Setenv("HOME", homeDir)
	writeCodexGetSessionFile(t, homeDir, sessionID, workspaceRoot)
	withGetCommandGlobals(t, filepath.Join(tempDir, "archive"), filepath.Join(tempDir, "debug"), &config.Config{
		Ingest: config.IngestConfig{EnabledProviders: []string{"codex"}},
	})

	cmd := createGetCommand()
	cmd.SetArgs([]string{sessionID})

	stdout, err := captureStdout(t, cmd.Execute)
	if err != nil {
		t.Fatalf("get command error = %v", err)
	}
	if !strings.Contains(stdout, "<!-- Generated by Tracer") {
		t.Fatalf("get command stdout missing markdown header: %s", stdout)
	}
	if !strings.Contains(stdout, "Session "+sessionID) {
		t.Fatalf("get command stdout missing session ID: %s", stdout)
	}
}

func TestGetCommandPathOutput(t *testing.T) {
	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	workspaceRoot := filepath.Join(tempDir, "workspace", "get-project")
	sessionID := "get-path-session"
	t.Setenv("HOME", homeDir)
	writeCodexGetSessionFile(t, homeDir, sessionID, workspaceRoot)
	archiveRoot := filepath.Join(tempDir, "archive")
	withGetCommandGlobals(t, archiveRoot, filepath.Join(tempDir, "debug"), &config.Config{
		Ingest: config.IngestConfig{EnabledProviders: []string{"codex"}},
	})

	cmd := createGetCommand()
	cmd.SetArgs([]string{sessionID, "-P"})

	stdout, err := captureStdout(t, cmd.Execute)
	if err != nil {
		t.Fatalf("get command error = %v", err)
	}

	wantPath := filepath.Join(archiveRoot, "codex", "get-project", sessionID+".md")
	if strings.TrimSpace(stdout) != wantPath {
		t.Fatalf("get command path stdout = %q, want %q", strings.TrimSpace(stdout), wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected archived markdown at %s: %v", wantPath, err)
	}
}
