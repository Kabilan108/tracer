package codexcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracer-ai/tracer-cli/pkg/spi"
)

const testCodexSessionID = "019a0000-0000-7000-8000-000000000001"

func codexJSONLine(t *testing.T, record map[string]interface{}) string {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal fixture record: %v", err)
	}
	return string(data) + "\n"
}

func codexMetaLine(t *testing.T, sessionID string) string {
	return codexJSONLine(t, map[string]interface{}{
		"type":      "session_meta",
		"timestamp": "2026-10-01T12:00:00.000Z",
		"payload": map[string]interface{}{
			"id":        sessionID,
			"timestamp": "2026-10-01T12:00:00.000Z",
			"cwd":       "/tmp/tracer-incremental-test",
		},
	})
}

// codexExchangeLines returns one user turn with a tool call and reply. padding
// grows the tool output so fixtures can be made large.
func codexExchangeLines(t *testing.T, index int, padding int) string {
	ts := func(offset int) string {
		return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).
			Add(time.Duration(index*10+offset) * time.Second).
			Format("2006-01-02T15:04:05.000Z")
	}
	callID := fmt.Sprintf("call_%d", index)
	var b strings.Builder
	b.WriteString(codexJSONLine(t, map[string]interface{}{
		"type": "event_msg", "timestamp": ts(0),
		"payload": map[string]interface{}{"type": "user_message", "message": fmt.Sprintf("Please run step %d", index)},
	}))
	b.WriteString(codexJSONLine(t, map[string]interface{}{
		"type": "response_item", "timestamp": ts(1),
		"payload": map[string]interface{}{
			"type": "function_call", "name": "shell_command", "call_id": callID,
			"arguments": fmt.Sprintf(`{"command":"echo step %d"}`, index),
		},
	}))
	b.WriteString(codexJSONLine(t, map[string]interface{}{
		"type": "response_item", "timestamp": ts(2),
		"payload": map[string]interface{}{
			"type": "function_call_output", "call_id": callID,
			"output": fmt.Sprintf(`{"output":"step %d %s"}`, index, strings.Repeat("x", padding)),
		},
	}))
	b.WriteString(codexJSONLine(t, map[string]interface{}{
		"type": "event_msg", "timestamp": ts(3),
		"payload": map[string]interface{}{"type": "agent_message", "message": fmt.Sprintf("Step %d done", index)},
	}))
	return b.String()
}

func writeCodexFixture(t *testing.T, path string, exchanges int, padding int) {
	t.Helper()
	var b strings.Builder
	b.WriteString(codexMetaLine(t, testCodexSessionID))
	for i := 0; i < exchanges; i++ {
		b.WriteString(codexExchangeLines(t, i, padding))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func appendToFile(t *testing.T, path string, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := file.WriteString(content); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// fullCodexSession parses sessionPath from scratch, the way sync does.
func fullCodexSession(t *testing.T, sessionPath string) *spi.AgentChatSession {
	t.Helper()
	meta, err := loadCodexSessionMeta(sessionPath)
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}
	session, err := processSessionToAgentChat(&codexSessionInfo{
		SessionID:   meta.Payload.ID,
		SessionPath: sessionPath,
		Meta:        meta,
	}, "", false)
	if err != nil {
		t.Fatalf("full parse: %v", err)
	}
	return session
}

// incrementalCodexSession runs the watcher's per-file parse and returns the emitted session.
func incrementalCodexSession(t *testing.T, tails *codexTailCache, sessionPath string) *spi.AgentChatSession {
	t.Helper()
	emitted := make(chan *spi.AgentChatSession, 1)
	if err := processCodexSessionFileWith(tails, sessionPath, "", "", false, func(s *spi.AgentChatSession) { emitted <- s }); err != nil {
		t.Fatalf("processCodexSessionFileWith() error = %v", err)
	}
	select {
	case session := <-emitted:
		return session
	case <-time.After(5 * time.Second):
		t.Fatal("no session emitted")
		return nil
	}
}

func assertSameCodexSession(t *testing.T, got, want *spi.AgentChatSession) {
	t.Helper()
	if got.SessionID != want.SessionID || got.CreatedAt != want.CreatedAt || got.Slug != want.Slug {
		t.Errorf("identity = (%q, %q, %q), want (%q, %q, %q)",
			got.SessionID, got.CreatedAt, got.Slug, want.SessionID, want.CreatedAt, want.Slug)
	}
	gotData, _ := json.Marshal(got.SessionData)
	wantData, _ := json.Marshal(want.SessionData)
	if string(gotData) != string(wantData) {
		t.Errorf("SessionData differs from a full parse\n got: %.400s\nwant: %.400s", gotData, wantData)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func TestCodexIncremental_AppendDecodesOnlyAppendedBytes(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "rollout.jsonl")
	writeCodexFixture(t, sessionPath, 400, 4096)
	initialSize := fileSize(t, sessionPath)

	tails := newCodexTailCache(time.Hour)
	assertSameCodexSession(t, incrementalCodexSession(t, tails, sessionPath), fullCodexSession(t, sessionPath))
	if got := tails.bytesRead.Load(); got != initialSize {
		t.Fatalf("first read consumed %d bytes, want the whole file (%d)", got, initialSize)
	}

	for round := 0; round < 3; round++ {
		appended := codexExchangeLines(t, 400+round, 16)
		appendToFile(t, sessionPath, appended)

		before := tails.bytesRead.Load()
		got := incrementalCodexSession(t, tails, sessionPath)
		consumed := tails.bytesRead.Load() - before

		if consumed != int64(len(appended)) {
			t.Errorf("round %d consumed %d bytes for a %d-byte append to a %d-byte file",
				round, consumed, len(appended), fileSize(t, sessionPath))
		}
		assertSameCodexSession(t, got, fullCodexSession(t, sessionPath))
	}
}

func TestCodexIncremental_PartialTrailingLine(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "rollout.jsonl")
	writeCodexFixture(t, sessionPath, 3, 0)

	tails := newCodexTailCache(time.Hour)
	first := incrementalCodexSession(t, tails, sessionPath)

	next := codexExchangeLines(t, 3, 0)
	cut := strings.Index(next, "\n") / 2
	appendToFile(t, sessionPath, next[:cut])

	before := tails.bytesRead.Load()
	partial := incrementalCodexSession(t, tails, sessionPath)
	if consumed := tails.bytesRead.Load() - before; consumed != 0 {
		t.Errorf("consumed %d bytes of an incomplete line, want 0", consumed)
	}
	assertSameCodexSession(t, partial, first)

	appendToFile(t, sessionPath, next[cut:])
	completed := incrementalCodexSession(t, tails, sessionPath)
	if consumed := tails.bytesRead.Load() - before; consumed != int64(len(next)) {
		t.Errorf("consumed %d bytes after completing the line, want %d", consumed, len(next))
	}
	assertSameCodexSession(t, completed, fullCodexSession(t, sessionPath))
}

// TestCodexIncremental_FinalRecordWithoutNewline: a complete final record
// whose newline is not written yet is part of a full read, so it must be in
// the incremental output, without entering the cache it is read into again
// once the newline arrives.
func TestCodexIncremental_FinalRecordWithoutNewline(t *testing.T) {
	tests := []struct {
		name    string
		content func(t *testing.T) string
	}{
		{
			name: "last record of a session",
			content: func(t *testing.T) string {
				return codexMetaLine(t, testCodexSessionID) + codexExchangeLines(t, 0, 0)
			},
		},
		{
			name:    "only the meta line",
			content: func(t *testing.T) string { return codexMetaLine(t, testCodexSessionID) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionPath := filepath.Join(t.TempDir(), "rollout.jsonl")
			content := strings.TrimSuffix(tt.content(t), "\n")
			if err := os.WriteFile(sessionPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			full := func() string {
				records, err := readSessionRecords(sessionPath)
				if err != nil {
					t.Fatalf("full read: %v", err)
				}
				data, _ := json.Marshal(records)
				return string(data)
			}
			accept := func(*codexSessionMeta) bool { return true }

			tails := newCodexTailCache(time.Hour)
			for _, step := range []string{"first read", "re-read without changes"} {
				meta, records, err := tails.read(sessionPath, accept)
				if err != nil || meta == nil {
					t.Fatalf("%s: meta = %v, err = %v", step, meta, err)
				}
				if got, _ := json.Marshal(records); string(got) != full() {
					t.Errorf("%s: records differ from a full read\n got: %s\nwant: %s", step, got, full())
				}
			}

			more := codexExchangeLines(t, 1, 0)
			appendToFile(t, sessionPath, "\n"+more)
			_, records, err := tails.read(sessionPath, accept)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := json.Marshal(records); string(got) != full() {
				t.Errorf("after the newline: records differ from a full read\n got: %s\nwant: %s", got, full())
			}
			if consumed := tails.bytesRead.Load(); consumed != fileSize(t, sessionPath) {
				t.Errorf("consumed %d bytes in total, want each byte once (%d)", consumed, fileSize(t, sessionPath))
			}
		})
	}
}

func TestCodexIncremental_RestartsWhenFileIsRewritten(t *testing.T) {
	tests := []struct {
		name    string
		rewrite func(t *testing.T, sessionPath string)
	}{
		{
			name: "truncated to fewer exchanges",
			rewrite: func(t *testing.T, sessionPath string) {
				writeCodexFixture(t, sessionPath, 2, 0)
			},
		},
		{
			name: "rewritten in place to the same size",
			rewrite: func(t *testing.T, sessionPath string) {
				content, err := os.ReadFile(sessionPath)
				if err != nil {
					t.Fatal(err)
				}
				// Changing the last record keeps the result independent of the
				// filesystem's mtime granularity.
				rewritten := strings.ReplaceAll(string(content), " done", " fine")
				if len(rewritten) != len(content) {
					t.Fatalf("fixture: rewrite changed the size")
				}
				if err := os.WriteFile(sessionPath, []byte(rewritten), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "replaced by a larger file with a new inode",
			rewrite: func(t *testing.T, sessionPath string) {
				replacement := sessionPath + ".tmp"
				writeCodexFixture(t, replacement, 30, 64)
				if err := os.Rename(replacement, sessionPath); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionPath := filepath.Join(t.TempDir(), "rollout.jsonl")
			writeCodexFixture(t, sessionPath, 10, 32)

			tails := newCodexTailCache(time.Hour)
			incrementalCodexSession(t, tails, sessionPath)

			tt.rewrite(t, sessionPath)
			before := tails.bytesRead.Load()
			got := incrementalCodexSession(t, tails, sessionPath)

			if consumed := tails.bytesRead.Load() - before; consumed != fileSize(t, sessionPath) {
				t.Errorf("consumed %d bytes, want a full re-read of %d", consumed, fileSize(t, sessionPath))
			}
			assertSameCodexSession(t, got, fullCodexSession(t, sessionPath))
		})
	}
}

func TestCodexIncremental_UncachedCases(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "rollout.jsonl")
	writeCodexFixture(t, sessionPath, 2, 0)
	tails := newCodexTailCache(time.Hour)

	emitted := false
	otherProject := "/tmp/some-other-project"
	err := processCodexSessionFileWith(tails, sessionPath, otherProject, normalizeCodexPath(otherProject), false,
		func(*spi.AgentChatSession) { emitted = true })
	if err != nil {
		t.Fatalf("processCodexSessionFileWith() error = %v", err)
	}
	if emitted {
		t.Error("session from another project was emitted")
	}
	if tails.files.Len() != 0 {
		t.Errorf("session from another project was cached (%d entries)", tails.files.Len())
	}
	if consumed := tails.bytesRead.Load(); consumed != 0 {
		t.Errorf("consumed %d bytes of another project's file, want 0", consumed)
	}

	incrementalCodexSession(t, tails, sessionPath)
	tails.forget(sessionPath)
	if tails.files.Len() != 0 {
		t.Errorf("forget left %d entries", tails.files.Len())
	}
}
