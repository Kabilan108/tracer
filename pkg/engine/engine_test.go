package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tracer-ai/tracer-cli/pkg/spi"
	"github.com/tracer-ai/tracer-cli/pkg/spi/schema"
)

type testProvider struct {
	name     string
	sessions map[string]spi.AgentChatSession
	detect   bool
	watchFn  func(ctx context.Context, projectPath string, debugRaw bool, sessionCallback func(*spi.AgentChatSession)) error
}

func newTestProvider(name string) *testProvider {
	return &testProvider{
		name:     name,
		sessions: make(map[string]spi.AgentChatSession),
		detect:   true,
	}
}

func (p *testProvider) setSession(session spi.AgentChatSession) {
	p.sessions[session.SessionID] = session
}

func (p *testProvider) Name() string {
	return p.name
}

func (p *testProvider) Check(customCommand string) spi.CheckResult {
	return spi.CheckResult{Success: true}
}

func (p *testProvider) DetectAgent(projectPath string, helpOutput bool) bool {
	return p.detect
}

func (p *testProvider) GetAgentChatSession(projectPath string, sessionID string, debugRaw bool) (*spi.AgentChatSession, error) {
	session, ok := p.sessions[sessionID]
	if !ok {
		return nil, nil
	}
	copy := session
	return &copy, nil
}

func (p *testProvider) GetAgentChatSessions(projectPath string, debugRaw bool, progress spi.ProgressCallback) ([]spi.AgentChatSession, error) {
	result := make([]spi.AgentChatSession, 0, len(p.sessions))
	for _, session := range p.sessions {
		result = append(result, session)
	}
	return result, nil
}

func (p *testProvider) ListAgentChatSessions(projectPath string) ([]spi.SessionMetadata, error) {
	return nil, nil
}

func (p *testProvider) ExecAgentAndWatch(projectPath string, customCommand string, resumeSessionID string, debugRaw bool, sessionCallback func(*spi.AgentChatSession)) error {
	return nil
}

func (p *testProvider) WatchAgent(ctx context.Context, projectPath string, debugRaw bool, sessionCallback func(*spi.AgentChatSession)) error {
	if p.watchFn != nil {
		return p.watchFn(ctx, projectPath, debugRaw, sessionCallback)
	}
	return nil
}

func newSession(providerID, providerName, sessionID, slug, userText string) spi.AgentChatSession {
	now := "2026-03-04T00:00:00Z"
	return spi.AgentChatSession{
		SessionID: sessionID,
		CreatedAt: now,
		Slug:      slug,
		RawData:   "{}",
		SessionData: &schema.SessionData{
			SchemaVersion: "1.0",
			Provider: schema.ProviderInfo{
				ID:      providerID,
				Name:    providerName,
				Version: "test",
			},
			SessionID:     sessionID,
			CreatedAt:     now,
			UpdatedAt:     now,
			Slug:          slug,
			WorkspaceRoot: "/tmp/workspace",
			Exchanges: []schema.Exchange{
				{
					ExchangeID: "ex-1",
					StartTime:  now,
					EndTime:    now,
					Messages: []schema.Message{
						{
							ID:        "u-1",
							Timestamp: now,
							Role:      schema.RoleUser,
							Content: []schema.ContentPart{
								{Type: schema.ContentTypeText, Text: userText},
							},
						},
						{
							ID:        "a-1",
							Timestamp: now,
							Role:      schema.RoleAgent,
							Model:     "test-model",
							Content: []schema.ContentPart{
								{Type: schema.ContentTypeText, Text: "ack"},
							},
						},
					},
				},
			},
		},
	}
}

func sessionWithAddedUserMessages(session spi.AgentChatSession, texts ...string) spi.AgentChatSession {
	if session.SessionData == nil || len(texts) == 0 || len(session.SessionData.Exchanges) == 0 {
		return session
	}

	sessionCopy := session
	dataCopy := *session.SessionData
	exchanges := append([]schema.Exchange(nil), session.SessionData.Exchanges...)
	firstExchange := exchanges[0]
	messages := append([]schema.Message(nil), firstExchange.Messages...)
	for _, text := range texts {
		messages = append(messages, schema.Message{
			ID:        "extra-user",
			Timestamp: dataCopy.UpdatedAt,
			Role:      schema.RoleUser,
			Content: []schema.ContentPart{
				{Type: schema.ContentTypeText, Text: text},
			},
		})
	}
	firstExchange.Messages = messages
	exchanges[0] = firstExchange
	dataCopy.Exchanges = exchanges
	sessionCopy.SessionData = &dataCopy
	return sessionCopy
}

func newEngineForTest(t *testing.T, dir string) *Engine {
	t.Helper()
	engine, err := New(Options{
		HistoryDir:     filepath.Join(dir, "history"),
		StatisticsPath: filepath.Join(dir, "statistics.json"),
		StateDBPath:    filepath.Join(dir, "state.db"),
		UseUTC:         true,
		Debounce:       40 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return engine
}

func newRunModeOptions(dir string, debounce time.Duration) Options {
	historyDir := filepath.Join(dir, "history")
	return Options{
		HistoryDir:     historyDir,
		StatisticsPath: filepath.Join(dir, "statistics.json"),
		StateDBPath:    filepath.Join(dir, "state.db"),
		UseUTC:         true,
		Debounce:       debounce,
		PathBuilder: func(providerID string, session *spi.AgentChatSession) string {
			return filepath.Join(historyDir, providerID, session.SessionID+".md")
		},
	}
}

func TestIngestProviders_IdempotentWithStateDB(t *testing.T) {
	tempDir := t.TempDir()
	provider := newTestProvider("Claude Code")
	session := newSession("claude", "Claude Code", "session-1", "phase3-test", "hello")
	provider.setSession(session)

	engine1 := newEngineForTest(t, tempDir)
	summary1, err := engine1.IngestProviders(context.Background(), "/tmp/workspace", map[string]spi.Provider{
		"claude": provider,
	}, false)
	if err != nil {
		t.Fatalf("IngestProviders() error = %v", err)
	}
	if err := engine1.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if summary1.Created != 1 || summary1.Updated != 0 || summary1.Skipped != 0 || summary1.Errors != 0 {
		t.Fatalf("first ingest summary = %+v", summary1)
	}

	engine2 := newEngineForTest(t, tempDir)
	summary2, err := engine2.IngestProviders(context.Background(), "/tmp/workspace", map[string]spi.Provider{
		"claude": provider,
	}, false)
	if err != nil {
		t.Fatalf("IngestProviders() second pass error = %v", err)
	}
	if err := engine2.Close(); err != nil {
		t.Fatalf("Close() second pass error = %v", err)
	}

	if summary2.Skipped != 1 || summary2.Created != 0 || summary2.Updated != 0 || summary2.Errors != 0 {
		t.Fatalf("second ingest summary = %+v", summary2)
	}
}

func TestQueueSessionUpdate_DebouncesByProviderAndSession(t *testing.T) {
	tempDir := t.TempDir()
	engine := newEngineForTest(t, tempDir)

	first := newSession("codex", "Codex CLI", "session-2", "debounce-test", "first")
	second := newSession("codex", "Codex CLI", "session-2", "debounce-test", "second")

	engine.QueueSessionUpdate("codex", &first)
	time.Sleep(10 * time.Millisecond)
	engine.QueueSessionUpdate("codex", &second)
	time.Sleep(120 * time.Millisecond)

	if err := engine.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	summary := engine.SnapshotSummary()
	if summary.Created != 1 || summary.Updated != 0 || summary.Skipped != 0 || summary.Errors != 0 {
		t.Fatalf("debounce summary = %+v", summary)
	}

	expectedPath := filepath.Join(tempDir, "history", "codex", "2026-03-04_00-00-00Z-debounce-test.md")
	content, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	if string(content) == "" || !strings.Contains(string(content), "second") {
		t.Fatalf("output markdown does not contain latest update")
	}
	if strings.Contains(string(content), "first") {
		t.Fatalf("output markdown should not contain superseded content")
	}
}

func TestRunModes_ShareStateAndProcessingPath(t *testing.T) {
	tempDir := t.TempDir()
	opts := newRunModeOptions(tempDir, 25*time.Millisecond)

	provider := newTestProvider("Claude Code")
	provider.setSession(newSession("claude", "Claude Code", "session-3", "shared-path", "same-content"))

	ingestSummary, err := RunIngest(context.Background(), opts, "/tmp/workspace", map[string]spi.Provider{
		"claude": provider,
	}, false)
	if err != nil {
		t.Fatalf("RunIngest() error = %v", err)
	}
	if ingestSummary.Created != 1 || ingestSummary.Skipped != 0 {
		t.Fatalf("RunIngest summary = %+v", ingestSummary)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonSummary, err := RunDaemon(ctx, opts, "/tmp/workspace", map[string]spi.Provider{
		"claude": provider,
	}, false)
	if err != nil {
		t.Fatalf("RunDaemon() error = %v", err)
	}
	if daemonSummary.Skipped < 1 {
		t.Fatalf("RunDaemon summary expected skipped >= 1, got %+v", daemonSummary)
	}
}

func TestRunDaemon_PerformsHistoricalBackfill(t *testing.T) {
	tempDir := t.TempDir()
	opts := newRunModeOptions(tempDir, 25*time.Millisecond)

	provider := newTestProvider("Claude Code")
	provider.setSession(newSession("claude", "Claude Code", "session-backfill", "daemon-backfill", "historical"))

	summary, err := RunDaemon(context.Background(), opts, "/tmp/workspace", map[string]spi.Provider{
		"claude": provider,
	}, false)
	if err != nil {
		t.Fatalf("RunDaemon() error = %v", err)
	}
	if summary.Created != 1 || summary.Updated != 0 || summary.Errors != 0 {
		t.Fatalf("RunDaemon summary = %+v", summary)
	}

	outputPath := filepath.Join(tempDir, "history", "claude", "session-backfill.md")
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	if !strings.Contains(string(content), "historical") {
		t.Fatalf("historical content missing from output markdown")
	}
}

func TestRunDaemon_DebouncesLiveUpdates(t *testing.T) {
	tempDir := t.TempDir()
	opts := newRunModeOptions(tempDir, 200*time.Millisecond)

	provider := newTestProvider("Codex CLI")
	firstUpdate := sessionWithAddedUserMessages(
		newSession("codex", "Codex CLI", "session-live", "daemon-live", "FIRST_ONLY"),
		"first-extra",
	)
	secondUpdate := sessionWithAddedUserMessages(
		newSession("codex", "Codex CLI", "session-live", "daemon-live", "SECOND_ONLY"),
		"second-extra-1",
		"second-extra-2",
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider.watchFn = func(ctx context.Context, projectPath string, debugRaw bool, sessionCallback func(*spi.AgentChatSession)) error {
		sessionCallback(&firstUpdate)
		sessionCallback(&secondUpdate)
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}

	summary, err := RunDaemon(ctx, opts, "/tmp/workspace", map[string]spi.Provider{
		"codex": provider,
	}, false)
	if err != nil {
		t.Fatalf("RunDaemon() error = %v", err)
	}
	if summary.Created != 1 || summary.Updated != 0 || summary.Errors != 0 {
		t.Fatalf("RunDaemon summary = %+v", summary)
	}

	outputPath := filepath.Join(tempDir, "history", "codex", "session-live.md")
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	if !strings.Contains(string(content), "SECOND_ONLY") {
		t.Fatalf("output markdown missing latest debounced content")
	}
	if strings.Contains(string(content), "FIRST_ONLY") {
		t.Fatalf("output markdown includes superseded content after debounce")
	}
}

func TestIngestProviders_ProgressCallbacks(t *testing.T) {
	tempDir := t.TempDir()

	claudeProvider := newTestProvider("Claude Code")
	claudeProvider.setSession(newSession("claude", "Claude Code", "progress-claude", "progress-claude", "hello from claude"))

	codexProvider := newTestProvider("Codex CLI")
	codexProvider.setSession(newSession("codex", "Codex CLI", "progress-codex", "progress-codex", "hello from codex"))

	var mu sync.Mutex
	started := make(map[string]int)
	completedTotals := make(map[string]int)
	completedErrors := make(map[string]int)
	processed := make(map[string]int)

	opts := newRunModeOptions(tempDir, 10*time.Millisecond)
	opts.OnProviderScanStart = func(providerID string) {
		mu.Lock()
		defer mu.Unlock()
		started[providerID]++
	}
	opts.OnProviderScanComplete = func(providerID string, totalSessions int, err error) {
		mu.Lock()
		defer mu.Unlock()
		completedTotals[providerID] = totalSessions
		if err != nil {
			completedErrors[providerID]++
		}
	}
	opts.OnSessionProcessed = func(providerID string, outcome ProcessOutcome, processedCount int, total int) {
		mu.Lock()
		defer mu.Unlock()
		processed[providerID] = processedCount
	}

	summary, err := RunIngest(context.Background(), opts, "/tmp/workspace", map[string]spi.Provider{
		"claude": claudeProvider,
		"codex":  codexProvider,
	}, false)
	if err != nil {
		t.Fatalf("RunIngest() error = %v", err)
	}
	if summary.Created != 2 || summary.Errors != 0 {
		t.Fatalf("RunIngest summary = %+v", summary)
	}

	mu.Lock()
	defer mu.Unlock()

	for _, providerID := range []string{"claude", "codex"} {
		if started[providerID] != 1 {
			t.Fatalf("provider %s scan started count = %d, want 1", providerID, started[providerID])
		}
		if completedTotals[providerID] != 1 {
			t.Fatalf("provider %s completed total = %d, want 1", providerID, completedTotals[providerID])
		}
		if completedErrors[providerID] != 0 {
			t.Fatalf("provider %s completed with unexpected errors count = %d", providerID, completedErrors[providerID])
		}
		if processed[providerID] != 1 {
			t.Fatalf("provider %s processed count = %d, want 1", providerID, processed[providerID])
		}
	}
}

type testProviderWithListError struct {
	name string
	err  error
}

func (p *testProviderWithListError) Name() string {
	return p.name
}

func (p *testProviderWithListError) Check(customCommand string) spi.CheckResult {
	return spi.CheckResult{Success: true}
}

func (p *testProviderWithListError) DetectAgent(projectPath string, helpOutput bool) bool {
	return true
}

func (p *testProviderWithListError) GetAgentChatSession(projectPath string, sessionID string, debugRaw bool) (*spi.AgentChatSession, error) {
	return nil, nil
}

func (p *testProviderWithListError) GetAgentChatSessions(projectPath string, debugRaw bool, progress spi.ProgressCallback) ([]spi.AgentChatSession, error) {
	return nil, p.err
}

func (p *testProviderWithListError) ListAgentChatSessions(projectPath string) ([]spi.SessionMetadata, error) {
	return nil, nil
}

func (p *testProviderWithListError) ExecAgentAndWatch(projectPath string, customCommand string, resumeSessionID string, debugRaw bool, sessionCallback func(*spi.AgentChatSession)) error {
	return nil
}

func (p *testProviderWithListError) WatchAgent(ctx context.Context, projectPath string, debugRaw bool, sessionCallback func(*spi.AgentChatSession)) error {
	return nil
}

func TestIngestProviders_ProgressCallbacksWithScanError(t *testing.T) {
	tempDir := t.TempDir()
	providerErr := fmt.Errorf("boom")
	provider := &testProviderWithListError{name: "ErrProvider", err: providerErr}

	var mu sync.Mutex
	started := 0
	completed := 0
	completedTotal := -1
	completedHadErr := false
	processedCalls := 0

	opts := newRunModeOptions(tempDir, 10*time.Millisecond)
	opts.OnProviderScanStart = func(providerID string) {
		mu.Lock()
		defer mu.Unlock()
		started++
	}
	opts.OnProviderScanComplete = func(providerID string, totalSessions int, err error) {
		mu.Lock()
		defer mu.Unlock()
		completed++
		completedTotal = totalSessions
		completedHadErr = err != nil
	}
	opts.OnSessionProcessed = func(providerID string, outcome ProcessOutcome, processedCount int, total int) {
		mu.Lock()
		defer mu.Unlock()
		processedCalls++
	}

	summary, err := RunIngest(context.Background(), opts, "/tmp/workspace", map[string]spi.Provider{
		"error-provider": provider,
	}, false)
	if err != nil {
		t.Fatalf("RunIngest() error = %v", err)
	}
	if summary.Errors != 1 {
		t.Fatalf("expected summary errors = 1, got %+v", summary)
	}

	mu.Lock()
	defer mu.Unlock()

	if started != 1 {
		t.Fatalf("scan started count = %d, want 1", started)
	}
	if completed != 1 {
		t.Fatalf("scan completed count = %d, want 1", completed)
	}
	if completedTotal != 0 {
		t.Fatalf("scan completed total = %d, want 0", completedTotal)
	}
	if !completedHadErr {
		t.Fatal("expected scan completion callback to receive error")
	}
	if processedCalls != 0 {
		t.Fatalf("session processed callbacks = %d, want 0", processedCalls)
	}
}
