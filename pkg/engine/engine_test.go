package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/specstoryai/getspecstory/specstory-cli/pkg/spi"
	"github.com/specstoryai/getspecstory/specstory-cli/pkg/spi/schema"
)

type testProvider struct {
	name     string
	sessions map[string]spi.AgentChatSession
}

func newTestProvider(name string) *testProvider {
	return &testProvider{
		name:     name,
		sessions: make(map[string]spi.AgentChatSession),
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
	return true
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
	opts := Options{
		HistoryDir:     filepath.Join(tempDir, "history"),
		StatisticsPath: filepath.Join(tempDir, "statistics.json"),
		StateDBPath:    filepath.Join(tempDir, "state.db"),
		UseUTC:         true,
		Debounce:       25 * time.Millisecond,
	}

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
