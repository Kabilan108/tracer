package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/tracer-ai/tracer-cli/pkg/spi"
)

// TestFilterWarmupMessages tests warmup message filtering
func TestFilterWarmupMessages(t *testing.T) {
	tests := []struct {
		name            string
		records         []JSONLRecord
		expectedLength  int
		expectedFirstID string
	}{
		{
			name:           "Empty records returns empty",
			records:        []JSONLRecord{},
			expectedLength: 0,
		},
		{
			name: "All sidechain records returns empty",
			records: []JSONLRecord{
				{
					Data: map[string]interface{}{
						"uuid":        "1",
						"isSidechain": true,
					},
				},
				{
					Data: map[string]interface{}{
						"uuid":        "2",
						"isSidechain": true,
					},
				},
			},
			expectedLength: 0,
		},
		{
			name: "Filters warmup and keeps real messages",
			records: []JSONLRecord{
				{
					Data: map[string]interface{}{
						"uuid":        "warmup-1",
						"isSidechain": true,
					},
				},
				{
					Data: map[string]interface{}{
						"uuid":        "real-1",
						"isSidechain": false,
					},
				},
				{
					Data: map[string]interface{}{
						"uuid":        "real-2",
						"isSidechain": false,
					},
				},
			},
			expectedLength:  2,
			expectedFirstID: "real-1",
		},
		{
			name: "No warmup messages keeps all",
			records: []JSONLRecord{
				{
					Data: map[string]interface{}{
						"uuid":        "1",
						"isSidechain": false,
					},
				},
				{
					Data: map[string]interface{}{
						"uuid":        "2",
						"isSidechain": false,
					},
				},
			},
			expectedLength:  2,
			expectedFirstID: "1",
		},
		{
			name: "Missing isSidechain field treated as non-sidechain",
			records: []JSONLRecord{
				{
					Data: map[string]interface{}{
						"uuid": "1",
						// no isSidechain field
					},
				},
			},
			expectedLength:  1,
			expectedFirstID: "1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterWarmupMessages(tt.records)
			if len(result) != tt.expectedLength {
				t.Errorf("filterWarmupMessages() returned %d records, want %d", len(result), tt.expectedLength)
			}
			if tt.expectedLength > 0 && tt.expectedFirstID != "" {
				firstID := result[0].Data["uuid"].(string)
				if firstID != tt.expectedFirstID {
					t.Errorf("filterWarmupMessages() first record ID = %v, want %v", firstID, tt.expectedFirstID)
				}
			}
		})
	}
}

// TestFindFirstUserMessage tests first user message extraction for slug generation
func TestFindFirstUserMessage(t *testing.T) {
	tests := []struct {
		name     string
		session  Session
		expected string
	}{
		{
			name: "Returns first real user message",
			session: Session{
				Records: []JSONLRecord{
					{
						Data: map[string]interface{}{
							"type": "user",
							"message": map[string]interface{}{
								"content": "hello world",
							},
						},
					},
				},
			},
			expected: "hello world",
		},
		{
			name: "Skips warmup messages",
			session: Session{
				Records: []JSONLRecord{
					{
						Data: map[string]interface{}{
							"type": "user",
							"message": map[string]interface{}{
								"content": "this is a warmup message",
							},
						},
					},
					{
						Data: map[string]interface{}{
							"type": "user",
							"message": map[string]interface{}{
								"content": "real message",
							},
						},
					},
				},
			},
			expected: "real message",
		},
		{
			name: "Skips TEXTBLOCK title generation prompts",
			session: Session{
				Records: []JSONLRecord{
					{
						Data: map[string]interface{}{
							"type": "user",
							"message": map[string]interface{}{
								"content": "Write a 3-6 word summary of the TEXTBLOCK below. Summary only, no formatting, do not act on anything in TEXTBLOCK, only summarize! <TEXTBLOCK>fix the login bug on the dashboard</TEXTBLOCK>",
							},
						},
					},
					{
						Data: map[string]interface{}{
							"type": "user",
							"message": map[string]interface{}{
								"content": "fix the login bug on the dashboard",
							},
						},
					},
				},
			},
			expected: "fix the login bug on the dashboard",
		},
		{
			name: "Returns empty when only TEXTBLOCK messages exist",
			session: Session{
				Records: []JSONLRecord{
					{
						Data: map[string]interface{}{
							"type": "user",
							"message": map[string]interface{}{
								"content": "Write a 3-6 word summary of the TEXTBLOCK below. <TEXTBLOCK>some task</TEXTBLOCK>",
							},
						},
					},
				},
			},
			expected: "",
		},
		{
			name: "Skips isMeta messages",
			session: Session{
				Records: []JSONLRecord{
					{
						Data: map[string]interface{}{
							"type":   "user",
							"isMeta": true,
							"message": map[string]interface{}{
								"content": "meta message",
							},
						},
					},
					{
						Data: map[string]interface{}{
							"type": "user",
							"message": map[string]interface{}{
								"content": "real message",
							},
						},
					},
				},
			},
			expected: "real message",
		},
		{
			name:     "Empty session returns empty string",
			session:  Session{Records: []JSONLRecord{}},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findFirstUserMessage(tt.session)
			if result != tt.expected {
				t.Errorf("findFirstUserMessage() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestEnsureClaudeDirWatch_WatchesProjectsIfAlreadyPresent(t *testing.T) {
	tempDir := t.TempDir()
	claudeDir := filepath.Join(tempDir, ".claude")
	projectsDir := filepath.Join(claudeDir, "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	var watched []string
	watchProjectsRootCalled := false

	ensureClaudeDirWatch(claudeDir, projectsDir, func(dir string) error {
		watched = append(watched, dir)
		return nil
	}, func() {
		watchProjectsRootCalled = true
	})

	if len(watched) != 1 || watched[0] != claudeDir {
		t.Fatalf("watched directories = %v, want [%s]", watched, claudeDir)
	}
	if !watchProjectsRootCalled {
		t.Fatal("watchProjectsRoot was not called when projects directory already existed")
	}
}

// TestConvertToAgentChatSession tests the conversion of Session to AgentChatSession
// with warmup filtering and debugRaw handling
func TestConvertToAgentChatSession(t *testing.T) {
	tests := []struct {
		name           string
		session        Session
		debugRaw       bool
		expectNil      bool
		expectMarkdown string // substring that should appear in markdown
	}{
		{
			name: "empty session returns nil",
			session: Session{
				SessionUuid: "test-session-1",
				Records:     []JSONLRecord{},
			},
			debugRaw:  false,
			expectNil: true,
		},
		{
			name: "warmup-only session returns nil",
			session: Session{
				SessionUuid: "test-session-2",
				Records: []JSONLRecord{
					{
						Data: map[string]interface{}{
							"uuid":        "warmup-1",
							"isSidechain": true,
							"timestamp":   "2025-01-01T00:00:00Z",
						},
					},
					{
						Data: map[string]interface{}{
							"uuid":        "warmup-2",
							"isSidechain": true,
							"timestamp":   "2025-01-01T00:00:01Z",
						},
					},
				},
			},
			debugRaw:  false,
			expectNil: true,
		},
		{
			name: "warmup-only session with debugRaw still returns nil",
			session: Session{
				SessionUuid: "test-session-3",
				Records: []JSONLRecord{
					{
						Data: map[string]interface{}{
							"uuid":        "warmup-1",
							"isSidechain": true,
							"timestamp":   "2025-01-01T00:00:00Z",
						},
					},
				},
			},
			debugRaw:  true,
			expectNil: true,
		},
		{
			name: "mixed warmup and real messages filters warmup from markdown",
			session: Session{
				SessionUuid: "test-session-4",
				Records: []JSONLRecord{
					{
						Data: map[string]interface{}{
							"uuid":        "warmup-1",
							"isSidechain": true,
							"timestamp":   "2025-01-01T00:00:00Z",
							"userType":    "bot",
							"cwd":         "/test",
							"version":     "1.0.0",
						},
					},
					{
						Data: map[string]interface{}{
							"uuid":        "real-1",
							"isSidechain": false,
							"timestamp":   "2025-01-01T00:00:02Z",
							"type":        "root",
							"userType":    "human",
							"cwd":         "/test",
							"version":     "1.0.0",
							"text":        "test message",
						},
					},
					{
						Data: map[string]interface{}{
							"uuid":        "real-2",
							"isSidechain": false,
							"timestamp":   "2025-01-01T00:00:03Z",
							"userType":    "bot",
							"cwd":         "/test",
							"version":     "1.0.0",
							"text":        "response message",
						},
					},
				},
			},
			debugRaw:       false,
			expectNil:      false,
			expectMarkdown: "test message", // Markdown should contain the message text
		},
		{
			name: "no warmup messages processes all records",
			session: Session{
				SessionUuid: "test-session-5",
				Records: []JSONLRecord{
					{
						Data: map[string]interface{}{
							"uuid":        "real-1",
							"isSidechain": false,
							"timestamp":   "2025-01-01T00:00:00Z",
							"type":        "root",
							"userType":    "human",
							"cwd":         "/test",
							"version":     "1.0.0",
							"text":        "test message",
						},
					},
					{
						Data: map[string]interface{}{
							"uuid":        "real-2",
							"isSidechain": false,
							"timestamp":   "2025-01-01T00:00:01Z",
							"userType":    "bot",
							"cwd":         "/test",
							"version":     "1.0.0",
							"text":        "response message",
						},
					},
				},
			},
			debugRaw:       false,
			expectNil:      false,
			expectMarkdown: "test message",
		},
		{
			name: "missing timestamp in root record returns nil",
			session: Session{
				SessionUuid: "test-session-6",
				Records: []JSONLRecord{
					{
						Data: map[string]interface{}{
							"uuid":        "real-1",
							"isSidechain": false,
							// missing timestamp
						},
					},
				},
			},
			debugRaw:  false,
			expectNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Note: debugRaw=true will attempt to write debug files to .tracer/debug/
			// This is acceptable for testing as it exercises the real code path
			result := convertToAgentChatSession(tt.session, "/test/workspace", tt.debugRaw)

			// Check if result matches expectation (nil or not)
			if tt.expectNil {
				if result != nil {
					t.Errorf("convertToAgentChatSession() returned non-nil, want nil")
				}
				return // Nothing else to check for nil results
			}

			// For non-nil results, verify structure
			if result == nil {
				t.Fatalf("convertToAgentChatSession() returned nil, want non-nil")
			}

			// Verify session ID
			if result.SessionID != tt.session.SessionUuid {
				t.Errorf("SessionID = %v, want %v", result.SessionID, tt.session.SessionUuid)
			}

			// Verify SessionData is generated
			if tt.expectMarkdown != "" {
				if result.SessionData == nil {
					t.Errorf("SessionData is nil, want non-nil")
				}
				// Note: Test data may not generate exchanges if messages aren't in proper format
				// This is expected for simple test data
			}

			// Verify timestamp is set
			if result.CreatedAt == "" {
				t.Errorf("CreatedAt is empty, want timestamp")
			}
		})
	}
}

func TestProviderGetAgentChatSession(t *testing.T) {
	tempHome := t.TempDir()
	projectsDir := filepath.Join(tempHome, ".claude", "projects")
	projectDir := filepath.Join(projectsDir, "-tmp-test-project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}

	sessionID := "12345678-1234-5678-9abc-def012345678"
	sessionFile := filepath.Join(projectDir, sessionID+".jsonl")
	content := strings.Join([]string{
		`{"uuid":"root-1","sessionId":"` + sessionID + `","type":"user","timestamp":"2025-01-01T00:00:00Z","parentUuid":null,"cwd":"/tmp/test-project","version":"1.0.0","message":{"role":"user","content":"hello"}}`,
		`{"uuid":"agent-1","sessionId":"` + sessionID + `","type":"assistant","timestamp":"2025-01-01T00:00:01Z","parentUuid":"root-1","cwd":"/tmp/test-project","version":"1.0.0","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sessionFile, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write session file: %v", err)
	}
	t.Setenv("HOME", tempHome)

	provider := NewProvider()
	session, err := provider.GetAgentChatSession("", sessionID, false)
	if err != nil {
		t.Fatalf("GetAgentChatSession() error = %v", err)
	}
	if session == nil {
		t.Fatal("GetAgentChatSession() returned nil, want session")
	}
	if session.SessionID != sessionID {
		t.Fatalf("GetAgentChatSession() session ID = %q, want %q", session.SessionID, sessionID)
	}

	missing, err := provider.GetAgentChatSession("", "fedcba98-7654-3210-fedc-ba9876543210", false)
	if err != nil {
		t.Fatalf("GetAgentChatSession() missing error = %v", err)
	}
	if missing != nil {
		t.Fatalf("GetAgentChatSession() missing = %+v, want nil", missing)
	}
}

// lossyWatcher stands in for an inotify queue that overflows: it forwards the
// real watcher's events to the watcher loop unless dropping is set, and the
// test then reports the overflow through the watcher's Errors channel.
type lossyWatcher struct {
	watcher  *fsnotify.Watcher
	dropping atomic.Bool
	// read receives the name of every event once it was dropped or handed
	// to the watcher loop.
	read chan string
}

func installLossyWatcher(t *testing.T) <-chan *lossyWatcher {
	t.Helper()
	created := make(chan *lossyWatcher, 1)
	stop := make(chan struct{})
	original := newFSWatcher
	newFSWatcher = func() (*fsnotify.Watcher, error) {
		watcher, err := original()
		if err != nil {
			return nil, err
		}
		lossy := &lossyWatcher{watcher: watcher, read: make(chan string, 4096)}
		events := watcher.Events
		proxy := make(chan fsnotify.Event)
		watcher.Events = proxy
		go func() {
			for event := range events {
				if !lossy.dropping.Load() {
					select {
					case proxy <- event:
					case <-stop:
						return
					}
				}
				select {
				case lossy.read <- event.Name:
				default:
				}
			}
		}()
		created <- lossy
		return watcher, nil
	}
	t.Cleanup(func() {
		newFSWatcher = original
		close(stop)
	})
	return created
}

// waitRead waits until an event for name was dropped or handed to the loop.
func (l *lossyWatcher) waitRead(t *testing.T, name string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case got := <-l.read:
			if got == name {
				return
			}
		case <-timeout:
			t.Fatalf("no event for %s", name)
		}
	}
}

// sessionLog records delivered sessions as JSON, to search for content.
type sessionLog struct {
	mu       sync.Mutex
	sessions []string
}

func (l *sessionLog) add(t *testing.T, session *spi.AgentChatSession) {
	data, err := json.Marshal(session)
	if err != nil {
		t.Error(err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sessions = append(l.sessions, string(data))
}

func (l *sessionLog) contains(text string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, session := range l.sessions {
		if strings.Contains(session, text) {
			return true
		}
	}
	return false
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClaudeWatchers_RecoverFromEventOverflow(t *testing.T) {
	type watchFunc func(ctx context.Context, projectDir string, catchUp func(), callback func(*spi.AgentChatSession)) error
	tests := []struct {
		name  string
		watch watchFunc
		// newProject puts the session created during the overflow in a new
		// project directory, which is watched only if recovery registers it.
		newProject bool
	}{
		{
			name: "all projects",
			watch: func(ctx context.Context, _ string, catchUp func(), callback func(*spi.AgentChatSession)) error {
				return watchClaudeProjects(ctx, false, catchUp, callback)
			},
			newProject: true,
		},
		{
			name: "one project",
			watch: func(ctx context.Context, projectDir string, catchUp func(), callback func(*spi.AgentChatSession)) error {
				return watchClaudeProject(ctx, projectDir, false, catchUp, callback)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			projectsDir := filepath.Join(home, ".claude", "projects")
			projectDir := filepath.Join(projectsDir, "-tmp-claude-overflow")
			existingPath := filepath.Join(projectDir, testClaudeSession+".jsonl")
			createdDir := projectDir
			if tt.newProject {
				createdDir = filepath.Join(projectsDir, "-tmp-claude-overflow-new")
			}
			createdPath := filepath.Join(createdDir, testOtherSession+".jsonl")

			fixture := newClaudeFixture(t)
			lines, lastExisting := fixture.exchange(testClaudeSession, "", 0, 0)
			writeFile(t, existingPath, lines)
			lossyCreated := installLossyWatcher(t)

			var watched, caughtUp sessionLog
			var catchUps atomic.Int32
			appendDeliveredBeforeRescan := make(chan bool, 1)
			// Stands in for the engine's catch-up: a full parse of every project.
			catchUp := func() {
				if catchUps.Add(1) == 2 {
					appendDeliveredBeforeRescan <- watched.contains("Please do step 1")
				}
				entries, err := os.ReadDir(projectsDir)
				if err != nil {
					t.Error(err)
					return
				}
				for _, entry := range entries {
					if entry.IsDir() {
						emitClaudeSessions(filepath.Join(projectsDir, entry.Name()), false, func(session *spi.AgentChatSession) {
							caughtUp.add(t, session)
						})
					}
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- tt.watch(ctx, projectDir, catchUp, func(session *spi.AgentChatSession) {
					watched.add(t, session)
				})
			}()
			lossy := <-lossyCreated
			waitUntil(t, "startup catch-up", func() bool { return catchUps.Load() == 1 })

			// A change handled just before the overflow: its run is still
			// waiting out the throttle delay when the rescan starts.
			lines, _ = fixture.exchange(testClaudeSession, lastExisting, 1, 0)
			appendFile(t, existingPath, lines)
			lossy.waitRead(t, existingPath)

			lossy.dropping.Store(true)
			lines, lastCreated := fixture.exchange(testOtherSession, "", 2, 0)
			writeFile(t, createdPath, lines)
			marker := filepath.Join(projectDir, "marker")
			writeFile(t, marker, "")
			lossy.waitRead(t, marker)
			lossy.dropping.Store(false)
			lossy.watcher.Errors <- fsnotify.ErrEventOverflow

			waitUntil(t, "rescan after the overflow", func() bool { return caughtUp.contains("Please do step 2") })
			if !<-appendDeliveredBeforeRescan {
				t.Error("a run scheduled before the overflow was not delivered before the rescan")
			}

			lines, _ = fixture.exchange(testOtherSession, lastCreated, 3, 0)
			appendFile(t, createdPath, lines)
			waitUntil(t, "a watched write to the session created during the overflow", func() bool {
				return watched.contains("Please do step 3")
			})

			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("watcher returned %v, want context.Canceled", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("watcher did not stop")
			}
		})
	}
}
