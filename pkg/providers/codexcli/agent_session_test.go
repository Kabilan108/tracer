package codexcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/xeipuuv/gojsonschema"
)

// getSchemaPath returns the absolute path to the agent session schema
func getSchemaPath() string {
	// Get the directory of this test file
	_, filename, _, _ := runtime.Caller(0)
	testDir := filepath.Dir(filename)

	// Navigate to the schema file: pkg/providers/codexcli -> pkg/spi/schema
	schemaPath := filepath.Join(testDir, "..", "..", "spi", "schema", "session-data-v1.json")
	return schemaPath
}

// loadSchemaJSON loads the schema JSON from disk
func loadSchemaJSON() ([]byte, error) {
	return os.ReadFile(getSchemaPath())
}

// validateJSONDocument validates the JSON document against the schema using xeipuuv/gojsonschema
func validateJSONDocument(t *testing.T, jsonData []byte) error {
	// Load the schema
	agentSessionSchemaJSON, err := loadSchemaJSON()
	if err != nil {
		return fmt.Errorf("failed to load schema: %w", err)
	}

	// Create schema loader from schema
	schemaLoader := gojsonschema.NewBytesLoader(agentSessionSchemaJSON)

	// Create document loader from generated JSON
	documentLoader := gojsonschema.NewBytesLoader(jsonData)

	// Validate
	result, err := gojsonschema.Validate(schemaLoader, documentLoader)
	if err != nil {
		return fmt.Errorf("validation error: %w", err)
	}

	if result.Valid() {
		t.Log("  The document is valid")
		return nil
	}

	// Document is not valid, report errors
	t.Log("  The document is NOT valid. Errors:")
	for i, desc := range result.Errors() {
		t.Logf("    %d. %s", i+1, desc)
	}

	return fmt.Errorf("document failed schema validation with %d error(s)", len(result.Errors()))
}

// TestExtractUsageFromTokenCount tests the extractUsageFromTokenCount function
func TestExtractUsageFromTokenCount(t *testing.T) {
	tests := []struct {
		name           string
		payload        map[string]interface{}
		expectNil      bool
		expectedInput  int
		expectedOutput int
		expectedCached int
	}{
		{
			name:      "nil payload",
			payload:   nil,
			expectNil: true,
		},
		{
			name:      "missing info",
			payload:   map[string]interface{}{"type": "token_count"},
			expectNil: true,
		},
		{
			name: "missing last_token_usage",
			payload: map[string]interface{}{
				"info": map[string]interface{}{
					"total_token_usage": map[string]interface{}{
						"input_tokens":  float64(100),
						"output_tokens": float64(50),
					},
				},
			},
			expectNil: true,
		},
		{
			name: "valid token_count event",
			payload: map[string]interface{}{
				"type": "token_count",
				"info": map[string]interface{}{
					"last_token_usage": map[string]interface{}{
						"input_tokens":            float64(100),
						"cached_input_tokens":     float64(20),
						"output_tokens":           float64(50),
						"reasoning_output_tokens": float64(10),
						"total_tokens":            float64(180),
					},
					"total_token_usage": map[string]interface{}{
						"input_tokens":  float64(500),
						"output_tokens": float64(200),
					},
				},
			},
			expectNil:      false,
			expectedInput:  100,
			expectedOutput: 50,
			expectedCached: 20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractUsageFromTokenCount(tt.payload)
			if tt.expectNil {
				if result != nil {
					t.Errorf("extractUsageFromTokenCount() = %+v, want nil", result)
				}
				return
			}
			if result == nil {
				t.Fatal("extractUsageFromTokenCount() = nil, want non-nil")
			}
			if result.InputTokens != tt.expectedInput {
				t.Errorf("InputTokens = %d, want %d", result.InputTokens, tt.expectedInput)
			}
			if result.OutputTokens != tt.expectedOutput {
				t.Errorf("OutputTokens = %d, want %d", result.OutputTokens, tt.expectedOutput)
			}
			if result.CachedInputTokens != tt.expectedCached {
				t.Errorf("CachedInputTokens = %d, want %d", result.CachedInputTokens, tt.expectedCached)
			}
		})
	}
}

// TestGenerateAgentSession_WithTokenUsage tests that token_count events are processed
func TestGenerateAgentSession_WithTokenUsage(t *testing.T) {
	// Create sample records including a token_count event
	records := []map[string]interface{}{
		{
			"type":      "session_meta",
			"timestamp": "2025-11-16T00:00:00Z",
			"payload": map[string]interface{}{
				"id":        "test-session-123",
				"timestamp": "2025-11-16T00:00:00Z",
				"cwd":       "/test/workspace",
			},
		},
		{
			"type":      "event_msg",
			"timestamp": "2025-11-16T00:00:01Z",
			"payload": map[string]interface{}{
				"type":    "user_message",
				"message": "Hello, what's the weather?",
			},
		},
		{
			"type":      "event_msg",
			"timestamp": "2025-11-16T00:00:02Z",
			"payload": map[string]interface{}{
				"type":    "agent_message",
				"message": "I don't have access to weather data.",
			},
		},
		{
			"type":      "event_msg",
			"timestamp": "2025-11-16T00:00:03Z",
			"payload": map[string]interface{}{
				"type": "token_count",
				"info": map[string]interface{}{
					"last_token_usage": map[string]interface{}{
						"input_tokens":            float64(150),
						"cached_input_tokens":     float64(30),
						"output_tokens":           float64(75),
						"reasoning_output_tokens": float64(0),
						"total_tokens":            float64(255),
					},
					"total_token_usage": map[string]interface{}{
						"input_tokens":  float64(150),
						"output_tokens": float64(75),
					},
					"model_context_window": float64(128000),
				},
			},
		},
	}

	// Generate agent session
	session, err := GenerateAgentSession(records, "/test/workspace")
	if err != nil {
		t.Fatalf("GenerateAgentSession failed: %v", err)
	}

	// Validate we have one exchange
	if len(session.Exchanges) != 1 {
		t.Fatalf("Expected 1 exchange, got %d", len(session.Exchanges))
	}

	// Find the agent message and check it has usage attached
	exchange := session.Exchanges[0]
	var agentMsg *Message
	for i := range exchange.Messages {
		if exchange.Messages[i].Role == "agent" {
			agentMsg = &exchange.Messages[i]
			break
		}
	}

	if agentMsg == nil {
		t.Fatal("No agent message found")
	}

	if agentMsg.Usage == nil {
		t.Fatal("Agent message should have usage attached")
	}

	if agentMsg.Usage.InputTokens != 150 {
		t.Errorf("InputTokens = %d, want 150", agentMsg.Usage.InputTokens)
	}
	if agentMsg.Usage.OutputTokens != 75 {
		t.Errorf("OutputTokens = %d, want 75", agentMsg.Usage.OutputTokens)
	}
	if agentMsg.Usage.CachedInputTokens != 30 {
		t.Errorf("CachedInputTokens = %d, want 30", agentMsg.Usage.CachedInputTokens)
	}
}

// TestAgentSessionTypes validates that our type definitions are correct
func TestAgentSessionTypes(t *testing.T) {
	// Create a minimal valid session data for Codex
	session := &SessionData{
		SchemaVersion: "1.0",
		Provider: ProviderInfo{
			ID:      "codex-cli",
			Name:    "Codex CLI",
			Version: "unknown",
		},
		SessionID:     "test-session",
		CreatedAt:     "2025-11-16T00:00:00Z",
		WorkspaceRoot: "/test",
		Exchanges: []Exchange{
			{
				StartTime: "2025-11-16T00:00:00Z",
				EndTime:   "2025-11-16T00:00:10Z",
				Messages: []Message{
					{
						ID:        "u1",
						Timestamp: "2025-11-16T00:00:00Z",
						Role:      "user",
						Content: []ContentPart{
							{
								Type: "text",
								Text: "Run ls command",
							},
						},
					},
					{
						ID:        "t1",
						Timestamp: "2025-11-16T00:00:05Z",
						Role:      "agent",
						Model:     "gpt-5-codex",
						Tool: &ToolInfo{
							Name:  "shell",
							Type:  "shell",
							UseID: "call_123",
							Input: map[string]interface{}{
								"command": []string{"ls", "-la"},
							},
							Output: map[string]interface{}{
								"output": "total 8\ndrwxr-xr-x  3 user  staff   96 Nov 16 10:00 .\ndrwxr-xr-x  5 user  staff  160 Nov 16 09:00 ..",
								"metadata": map[string]interface{}{
									"exit_code": 0,
								},
							},
						},
						PathHints: []string{},
					},
				},
			},
		},
	}

	// Serialize to JSON
	jsonData, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal minimal session: %v", err)
	}

	// Validate against schema
	if err := validateJSONDocument(t, jsonData); err != nil {
		t.Errorf("Minimal session validation failed: %v", err)
	} else {
		t.Log("✓ Minimal session validation passed")
	}
}

// recordsFromJSONL decodes JSONL test input the way the session reader does.
func recordsFromJSONL(t *testing.T, lines ...string) []map[string]interface{} {
	t.Helper()
	records := make([]map[string]interface{}, 0, len(lines))
	for _, line := range lines {
		var record map[string]interface{}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid test record %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

const testSessionMetaLine = `{"type":"session_meta","timestamp":"2026-10-01T10:00:00Z","payload":{"id":"s1","timestamp":"2026-10-01T10:00:00Z","cwd":"/work"}}`

func TestGenerateAgentSession_ConversationText(t *testing.T) {
	type wantMessage struct {
		role     string
		partType string
		text     string
		meta     bool
	}
	tests := []struct {
		name          string
		lines         []string
		wantMessages  []wantMessage
		wantExchanges int
	}{
		{
			name: "item_completed turns",
			lines: []string{
				// Injected context and copied parent history arrive as raw
				// response_item messages and must not render.
				`{"type":"response_item","timestamp":"2026-10-01T10:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>ctx</environment_context>"}]}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:02Z","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"u","content":[{"type":"text","text":"Fix the flaky test"}]}}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:03Z","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"a1","phase":"commentary","content":[{"type":"Text","text":"Looking at it."}]}}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:04Z","payload":{"type":"item_completed","item":{"type":"Reasoning","id":"r1","summary_text":["First idea","Second idea"],"raw_content":[]}}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:05Z","payload":{"type":"item_completed","item":{"type":"Reasoning","id":"r2","summary_text":[],"raw_content":[]}}}`,
				`{"type":"response_item","timestamp":"2026-10-01T10:00:06Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Fixed."}]}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:06Z","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"a2","phase":"final_answer","content":[{"type":"Text","text":"Fixed."}]}}}`,
			},
			wantMessages: []wantMessage{
				{role: "user", partType: "text", text: "Fix the flaky test"},
				{role: "agent", partType: "text", text: "Looking at it."},
				{role: "agent", partType: "thinking", text: "First idea\n\nSecond idea"},
				{role: "agent", partType: "text", text: "Fixed."},
			},
			wantExchanges: 1,
		},
		{
			name: "legacy events",
			lines: []string{
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:01Z","payload":{"type":"user_message","message":"Fix the flaky test"}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:02Z","payload":{"type":"agent_reasoning","text":"Thinking"}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:03Z","payload":{"type":"agent_message","message":"Fixed."}}`,
			},
			wantMessages: []wantMessage{
				{role: "user", partType: "text", text: "Fix the flaky test"},
				{role: "agent", partType: "thinking", text: "Thinking"},
				{role: "agent", partType: "text", text: "Fixed."},
			},
			wantExchanges: 1,
		},
		{
			name: "file with both forms renders each turn once",
			lines: []string{
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:01Z","payload":{"type":"user_message","message":"Fix the flaky test"}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:01Z","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"Fix the flaky test"}]}}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:02Z","payload":{"type":"agent_reasoning","text":"Thinking"}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:03Z","payload":{"type":"agent_message","message":"Fixed."}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:03Z","payload":{"type":"item_completed","item":{"type":"AgentMessage","content":[{"type":"Text","text":"Fixed."}]}}}`,
			},
			wantMessages: []wantMessage{
				{role: "user", partType: "text", text: "Fix the flaky test"},
				{role: "agent", partType: "text", text: "Fixed."},
			},
			wantExchanges: 1,
		},
		{
			name: "text parts join without separators and images are skipped",
			lines: []string{
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:01Z","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"line one\n"},{"type":"image","image_url":"data:image/png;base64,AAAA"},{"type":"text","text":"line two"}]}}}`,
			},
			wantMessages: []wantMessage{
				{role: "user", partType: "text", text: "line one\nline two"},
			},
			wantExchanges: 1,
		},
		{
			name: "messages between agents start exchanges without counting as user turns",
			lines: []string{
				`{"type":"response_item","timestamp":"2026-10-01T10:00:01Z","payload":{"type":"agent_message","author":"/root","recipient":"/root/review","content":[{"type":"input_text","text":"Message Type: NEW_TASK\nTask name: /root/review\nSender: /root\nPayload:\n"},{"type":"encrypted_content","encrypted_content":"gAAAA"}]}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:02Z","payload":{"type":"item_completed","item":{"type":"AgentMessage","content":[{"type":"Text","text":"Reviewing."}]}}}`,
				`{"type":"response_item","timestamp":"2026-10-01T10:00:03Z","payload":{"type":"agent_message","author":"/root/review/check","recipient":"/root/review","content":[{"type":"input_text","text":"Message Type: FINAL_ANSWER\nTask name: /root/review\nSender: /root/review/check\nPayload:\nNo findings."}]}}`,
			},
			wantMessages: []wantMessage{
				{role: "user", partType: "text", text: "Message Type: NEW_TASK\nTask name: /root/review\nSender: /root\nPayload:\n[encrypted]", meta: true},
				{role: "agent", partType: "text", text: "Reviewing."},
				{role: "user", partType: "text", text: "Message Type: FINAL_ANSWER\nTask name: /root/review\nSender: /root/review/check\nPayload:\nNo findings.", meta: true},
			},
			wantExchanges: 2,
		},
		{
			name: "blank text is skipped",
			lines: []string{
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:01Z","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"  "}]}}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:02Z","payload":{"type":"item_completed","item":{"type":"AgentMessage","content":[]}}}`,
				`{"type":"event_msg","timestamp":"2026-10-01T10:00:03Z","payload":{"type":"user_message","message":""}}`,
			},
			wantMessages:  nil,
			wantExchanges: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records := recordsFromJSONL(t, append([]string{testSessionMetaLine}, tt.lines...)...)
			session, err := GenerateAgentSession(records, "/work")
			if err != nil {
				t.Fatalf("GenerateAgentSession() error = %v", err)
			}

			if len(session.Exchanges) != tt.wantExchanges {
				t.Errorf("exchanges = %d, want %d", len(session.Exchanges), tt.wantExchanges)
			}
			var got []wantMessage
			for _, exchange := range session.Exchanges {
				for _, message := range exchange.Messages {
					if len(message.Content) != 1 {
						t.Fatalf("message %s has %d content parts, want 1", message.ID, len(message.Content))
					}
					meta, _ := message.Metadata["isMeta"].(bool)
					got = append(got, wantMessage{
						role:     message.Role,
						partType: message.Content[0].Type,
						text:     message.Content[0].Text,
						meta:     meta,
					})
				}
			}
			if len(got) != len(tt.wantMessages) {
				t.Fatalf("messages = %+v, want %+v", got, tt.wantMessages)
			}
			for i := range got {
				if got[i] != tt.wantMessages[i] {
					t.Errorf("message %d = %+v, want %+v", i, got[i], tt.wantMessages[i])
				}
			}
		})
	}
}

func TestSubagentInfo(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantParent string
		wantName   string
	}{
		{name: "session a person started", payload: `{"source":"vscode"}`},
		{name: "no source", payload: `{}`},
		{name: "top-level parent without a subagent source", payload: `{"parent_thread_id":"p0","source":"cli"}`},
		{
			name:       "spawned agent prefers its nested parent and agent path",
			payload:    `{"parent_thread_id":"top","source":{"subagent":{"thread_spawn":{"parent_thread_id":"p1","agent_path":"/root/build_review","agent_nickname":"Boyle"}}}}`,
			wantParent: "p1",
			wantName:   "/root/build_review",
		},
		{
			name:       "spawned agent from before agent paths",
			payload:    `{"source":{"subagent":{"thread_spawn":{"parent_thread_id":"p1","agent_nickname":"Boyle"}}}}`,
			wantParent: "p1",
			wantName:   "Boyle",
		},
		{
			name:       "spawned agent without names falls back to its kind and top-level parent",
			payload:    `{"parent_thread_id":"p3","source":{"subagent":{"thread_spawn":{}}}}`,
			wantParent: "p3",
			wantName:   "thread_spawn",
		},
		{
			name:       "approval reviewer",
			payload:    `{"parent_thread_id":"p2","source":{"subagent":{"other":"guardian"}}}`,
			wantParent: "p2",
			wantName:   "guardian",
		},
		{
			name:     "internal job",
			payload:  `{"source":{"subagent":"memory_consolidation"}}`,
			wantName: "memory_consolidation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var payload map[string]interface{}
			if err := json.Unmarshal([]byte(tt.payload), &payload); err != nil {
				t.Fatalf("invalid payload: %v", err)
			}
			parent, name := subagentInfo(payload)
			if parent != tt.wantParent || name != tt.wantName {
				t.Errorf("subagentInfo() = (%q, %q), want (%q, %q)", parent, name, tt.wantParent, tt.wantName)
			}
		})
	}
}

func TestGenerateAgentSession_SubagentMatchesSchema(t *testing.T) {
	records := recordsFromJSONL(t,
		`{"type":"session_meta","timestamp":"2026-10-01T10:00:00Z","payload":{"id":"s2","timestamp":"2026-10-01T10:00:00Z","cwd":"/work","source":{"subagent":{"thread_spawn":{"parent_thread_id":"p1","agent_path":"/root/build_review"}}}}}`,
		`{"type":"response_item","timestamp":"2026-10-01T10:00:01Z","payload":{"type":"agent_message","content":[{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},{"type":"encrypted_content","encrypted_content":"gAAAA"}]}}`,
		`{"type":"event_msg","timestamp":"2026-10-01T10:00:02Z","payload":{"type":"item_completed","item":{"type":"AgentMessage","content":[{"type":"Text","text":"Done."}]}}}`,
	)
	session, err := GenerateAgentSession(records, "/work")
	if err != nil {
		t.Fatalf("GenerateAgentSession() error = %v", err)
	}
	if session.ParentSessionID != "p1" || session.SubagentName != "/root/build_review" {
		t.Errorf("parent, subagent = %q, %q; want p1, /root/build_review", session.ParentSessionID, session.SubagentName)
	}

	jsonData, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := validateJSONDocument(t, jsonData); err != nil {
		t.Errorf("subagent session does not match the schema: %v", err)
	}
}
