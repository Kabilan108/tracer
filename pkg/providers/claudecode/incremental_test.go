package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tracer-ai/tracer-cli/pkg/spi"
)

const (
	testClaudeSession = "11111111-1111-4111-8111-111111111111"
	testOtherSession  = "22222222-2222-4222-8222-222222222222"
)

// claudeFixture writes Claude JSONL records with strictly increasing
// timestamps, so DAG flattening is deterministic and full and incremental
// parses can be compared byte for byte.
type claudeFixture struct {
	t     *testing.T
	clock time.Time
	seq   int
}

func newClaudeFixture(t *testing.T) *claudeFixture {
	return &claudeFixture{t: t, clock: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
}

func (f *claudeFixture) line(record map[string]interface{}) string {
	f.t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		f.t.Fatalf("marshal fixture record: %v", err)
	}
	return string(data) + "\n"
}

func (f *claudeFixture) record(sessionID, kind, parentUUID string, message map[string]interface{}, extra map[string]interface{}) (string, string) {
	f.seq++
	f.clock = f.clock.Add(time.Second)
	uuid := fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq)
	record := map[string]interface{}{
		"type":      kind,
		"uuid":      uuid,
		"sessionId": sessionID,
		"timestamp": f.clock.Format("2006-01-02T15:04:05.000Z"),
		"cwd":       "/tmp/claude-incremental-project",
		"message":   message,
	}
	if parentUUID == "" {
		record["parentUuid"] = nil
	} else {
		record["parentUuid"] = parentUUID
	}
	for k, v := range extra {
		record[k] = v
	}
	return f.line(record), uuid
}

// exchange writes a user prompt, an assistant tool call, its result and a reply,
// chained after parentUUID. It returns the lines and the last uuid.
func (f *claudeFixture) exchange(sessionID, parentUUID string, index int, padding int) (string, string) {
	var b strings.Builder
	userLine, userID := f.record(sessionID, "user", parentUUID,
		map[string]interface{}{"role": "user", "content": fmt.Sprintf("Please do step %d", index)}, nil)
	b.WriteString(userLine)

	toolID := fmt.Sprintf("toolu_%d_%d", f.seq, index)
	callLine, callID := f.record(sessionID, "assistant", userID, map[string]interface{}{
		"role":  "assistant",
		"model": "claude-test",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": fmt.Sprintf("Running step %d", index)},
			map[string]interface{}{"type": "tool_use", "id": toolID, "name": "Bash",
				"input": map[string]interface{}{"command": fmt.Sprintf("echo %d > out.txt", index), "description": "step"}},
		},
	}, nil)
	b.WriteString(callLine)

	resultLine, resultID := f.record(sessionID, "user", callID, map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{"type": "tool_result", "tool_use_id": toolID,
				"content": fmt.Sprintf("step %d output %s", index, strings.Repeat("y", padding))},
		},
	}, nil)
	b.WriteString(resultLine)

	replyLine, replyID := f.record(sessionID, "assistant", resultID, map[string]interface{}{
		"role": "assistant", "model": "claude-test",
		"content": []interface{}{map[string]interface{}{"type": "text", "text": fmt.Sprintf("Step %d done", index)}},
	}, nil)
	b.WriteString(replyLine)
	return b.String(), replyID
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path string, content string) {
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

func sizeOf(t *testing.T, paths ...string) int64 {
	t.Helper()
	var total int64
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	return total
}

// fullClaudeSessions parses every file of sessionID from scratch, the way the
// watcher did before incremental reading.
func fullClaudeSessions(t *testing.T, projectDir, sessionID string) []*spi.AgentChatSession {
	t.Helper()
	parser := NewJSONLParser()
	if err := parser.ParseProjectSessionsForSession(projectDir, true, sessionID); err != nil {
		t.Fatalf("full parse: %v", err)
	}
	var result []*spi.AgentChatSession
	for _, session := range parser.Sessions {
		if session.SessionUuid != sessionID {
			continue
		}
		if agentSession := convertToAgentChatSession(session, "", false); agentSession != nil {
			result = append(result, agentSession)
		}
	}
	return result
}

func incrementalClaudeSessions(t *testing.T, tails *claudeTailCache, projectDir, changedFile string) []*spi.AgentChatSession {
	t.Helper()
	sessions, err := tails.agentSessionsForFile(projectDir, changedFile, false)
	if err != nil {
		t.Fatalf("agentSessionsForFile() error = %v", err)
	}
	return sessions
}

func encodeSessions(t *testing.T, sessions []*spi.AgentChatSession) []string {
	t.Helper()
	encoded := make([]string, 0, len(sessions))
	for _, session := range sessions {
		data, err := json.Marshal(session)
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, string(data))
	}
	sort.Strings(encoded)
	return encoded
}

func assertSameClaudeSessions(t *testing.T, step string, got, want []*spi.AgentChatSession) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s: full parse produced no session; fixture is broken", step)
	}
	gotEncoded, wantEncoded := encodeSessions(t, got), encodeSessions(t, want)
	if strings.Join(gotEncoded, "\n") != strings.Join(wantEncoded, "\n") {
		t.Errorf("%s: incremental output differs from a full parse\n got: %.600s\nwant: %.600s",
			step, strings.Join(gotEncoded, "\n"), strings.Join(wantEncoded, "\n"))
	}
}

func TestClaudeIncremental_MatchesFullParseAcrossFiles(t *testing.T) {
	projectDir := t.TempDir()
	fixture := newClaudeFixture(t)

	// The original session file, made large so per-append work is visible
	mainPath := filepath.Join(projectDir, testClaudeSession+".jsonl")
	var mainContent strings.Builder
	last := ""
	for i := 0; i < 300; i++ {
		lines, lastID := fixture.exchange(testClaudeSession, last, i, 2048)
		mainContent.WriteString(lines)
		last = lastID
	}
	writeFile(t, mainPath, mainContent.String())
	duplicated := strings.SplitAfter(mainContent.String(), "\n")[1]

	// A resumed file for the same session: summary first, a new root, and a
	// copy of an earlier record that must be de-duplicated
	resumedPath := filepath.Join(projectDir, "resumed.jsonl")
	resumedContent := fixture.line(map[string]interface{}{"type": "summary", "summary": "Resumed work", "leafUuid": last})
	resumedLines, resumedLast := fixture.exchange(testClaudeSession, "", 1000, 0)
	writeFile(t, resumedPath, resumedContent+duplicated+resumedLines)

	// An unrelated session in the same project directory
	otherPath := filepath.Join(projectDir, testOtherSession+".jsonl")
	otherLines, _ := fixture.exchange(testOtherSession, "", 0, 0)
	writeFile(t, otherPath, otherLines)

	tails := newClaudeTailCache(time.Hour)

	got := incrementalClaudeSessions(t, tails, projectDir, mainPath)
	assertSameClaudeSessions(t, "initial", got, fullClaudeSessions(t, projectDir, testClaudeSession))
	if consumed := tails.bytesRead.Load(); consumed != sizeOf(t, mainPath, resumedPath) {
		t.Errorf("initial read consumed %d bytes, want both session files (%d) and not the unrelated one",
			consumed, sizeOf(t, mainPath, resumedPath))
	}

	steps := []struct {
		name    string
		changed string
		write   func() string // performs the change, returns the bytes it added
	}{
		{
			name:    "append with summary and parentless sidechain to the large file",
			changed: mainPath,
			write: func() string {
				lines, lastID := fixture.exchange(testClaudeSession, last, 300, 16)
				last = lastID
				lines += fixture.line(map[string]interface{}{"type": "summary", "summary": "Step summary", "leafUuid": last})
				sidechain, _ := fixture.record(testClaudeSession, "user", "",
					map[string]interface{}{"role": "user", "content": "Subagent prompt"},
					map[string]interface{}{"isSidechain": true})
				lines += sidechain
				appendFile(t, mainPath, lines)
				return lines
			},
		},
		{
			name:    "new subagent file for the same session",
			changed: filepath.Join(projectDir, testClaudeSession, "subagents", "agent-1.jsonl"),
			write: func() string {
				first, firstID := fixture.record(testClaudeSession, "user", "",
					map[string]interface{}{"role": "user", "content": "Investigate the failure"},
					map[string]interface{}{"isSidechain": true, "agentId": "agent-1"})
				reply, _ := fixture.record(testClaudeSession, "assistant", firstID,
					map[string]interface{}{"role": "assistant", "model": "claude-test",
						"content": []interface{}{map[string]interface{}{"type": "text", "text": "Found it"}}},
					map[string]interface{}{"isSidechain": true, "agentId": "agent-1"})
				writeFile(t, filepath.Join(projectDir, testClaudeSession, "subagents", "agent-1.jsonl"), first+reply)
				return first + reply
			},
		},
		{
			name:    "append to the resumed file",
			changed: resumedPath,
			write: func() string {
				lines, lastID := fixture.exchange(testClaudeSession, resumedLast, 1001, 0)
				resumedLast = lastID
				appendFile(t, resumedPath, lines)
				return lines
			},
		},
	}

	for _, step := range steps {
		added := step.write()
		before := tails.bytesRead.Load()
		got := incrementalClaudeSessions(t, tails, projectDir, step.changed)

		if consumed := tails.bytesRead.Load() - before; consumed != int64(len(added)) {
			t.Errorf("%s: consumed %d bytes for %d added bytes", step.name, consumed, len(added))
		}
		assertSameClaudeSessions(t, step.name, got, fullClaudeSessions(t, projectDir, testClaudeSession))
	}

	gotOther := incrementalClaudeSessions(t, tails, projectDir, otherPath)
	assertSameClaudeSessions(t, "unrelated session", gotOther, fullClaudeSessions(t, projectDir, testOtherSession))
}

func TestClaudeIncremental_PartialLineAndRewrites(t *testing.T) {
	projectDir := t.TempDir()
	fixture := newClaudeFixture(t)
	sessionPath := filepath.Join(projectDir, testClaudeSession+".jsonl")

	initial, last := fixture.exchange(testClaudeSession, "", 0, 0)
	writeFile(t, sessionPath, initial)
	tails := newClaudeTailCache(time.Hour)
	first := incrementalClaudeSessions(t, tails, projectDir, sessionPath)

	next, _ := fixture.exchange(testClaudeSession, last, 1, 0)
	cut := strings.Index(next, "\n") / 2
	appendFile(t, sessionPath, next[:cut])
	before := tails.bytesRead.Load()
	partial := incrementalClaudeSessions(t, tails, projectDir, sessionPath)
	if consumed := tails.bytesRead.Load() - before; consumed != 0 {
		t.Errorf("consumed %d bytes of an incomplete line, want 0", consumed)
	}
	assertSameClaudeSessions(t, "partial line", partial, first)

	appendFile(t, sessionPath, next[cut:])
	completed := incrementalClaudeSessions(t, tails, projectDir, sessionPath)
	assertSameClaudeSessions(t, "completed line", completed, fullClaudeSessions(t, projectDir, testClaudeSession))

	// Truncation: rewrite the file shorter in place
	short, _ := newClaudeFixture(t).exchange(testClaudeSession, "", 5, 0)
	writeFile(t, sessionPath, short)
	before = tails.bytesRead.Load()
	truncated := incrementalClaudeSessions(t, tails, projectDir, sessionPath)
	if consumed := tails.bytesRead.Load() - before; consumed != sizeOf(t, sessionPath) {
		t.Errorf("truncation consumed %d bytes, want a full re-read of %d", consumed, sizeOf(t, sessionPath))
	}
	assertSameClaudeSessions(t, "truncated", truncated, fullClaudeSessions(t, projectDir, testClaudeSession))

	// Replacement: a larger file renamed over the original
	replacement := filepath.Join(t.TempDir(), "replacement.jsonl")
	var replaced strings.Builder
	last = ""
	for i := 0; i < 5; i++ {
		lines, lastID := fixture.exchange(testClaudeSession, last, 10+i, 0)
		replaced.WriteString(lines)
		last = lastID
	}
	writeFile(t, replacement, replaced.String())
	if err := os.Rename(replacement, sessionPath); err != nil {
		t.Fatal(err)
	}
	before = tails.bytesRead.Load()
	got := incrementalClaudeSessions(t, tails, projectDir, sessionPath)
	if consumed := tails.bytesRead.Load() - before; consumed != sizeOf(t, sessionPath) {
		t.Errorf("replacement consumed %d bytes, want a full re-read of %d", consumed, sizeOf(t, sessionPath))
	}
	assertSameClaudeSessions(t, "replaced", got, fullClaudeSessions(t, projectDir, testClaudeSession))

	tails.forget(sessionPath)
	if tails.files.Len() != 0 || len(tails.sessionIDs) != 0 {
		t.Errorf("forget left %d file states and %d session ids", tails.files.Len(), len(tails.sessionIDs))
	}
}

// TestClaudeIncremental_RebuildDoesNotRaceRendering guards the tool input
// clone: rebuilding from cached records must not write into maps that a
// previously emitted SessionData still shares. Run with -race.
func TestClaudeIncremental_RebuildDoesNotRaceRendering(t *testing.T) {
	projectDir := t.TempDir()
	fixture := newClaudeFixture(t)
	sessionPath := filepath.Join(projectDir, testClaudeSession+".jsonl")
	initial, last := fixture.exchange(testClaudeSession, "", 0, 0)
	writeFile(t, sessionPath, initial)

	tails := newClaudeTailCache(time.Hour)
	emitted := make(chan *spi.AgentChatSession, 16)
	var renderers sync.WaitGroup
	renderers.Add(1)
	go func() {
		defer renderers.Done()
		for session := range emitted {
			for i := 0; i < 5; i++ {
				_, _ = json.Marshal(session.SessionData)
			}
		}
	}()

	for i := 1; i <= 10; i++ {
		lines, lastID := fixture.exchange(testClaudeSession, last, i, 0)
		last = lastID
		appendFile(t, sessionPath, lines)
		for _, session := range incrementalClaudeSessions(t, tails, projectDir, sessionPath) {
			emitted <- session
		}
	}
	close(emitted)
	renderers.Wait()
}

// TestClaudeIncremental_RewriteToAnotherSession: a file rewritten in place to
// hold another session must be emitted as that session, even though its inode
// is unchanged and it did not shrink.
func TestClaudeIncremental_RewriteToAnotherSession(t *testing.T) {
	projectDir := t.TempDir()
	fixture := newClaudeFixture(t)
	sessionPath := filepath.Join(projectDir, "session.jsonl")

	before, last := fixture.exchange(testClaudeSession, "", 0, 0)
	writeFile(t, sessionPath, before)
	tails := newClaudeTailCache(time.Hour)
	incrementalClaudeSessions(t, tails, projectDir, sessionPath)
	// A second, continued read leaves both the tail and the session ID cached.
	appended, _ := fixture.exchange(testClaudeSession, last, 1, 0)
	appendFile(t, sessionPath, appended)
	before += appended
	assertSameClaudeSessions(t, "before rewrite",
		incrementalClaudeSessions(t, tails, projectDir, sessionPath), fullClaudeSessions(t, projectDir, testClaudeSession))

	after, _ := fixture.exchange(testOtherSession, "", 2, 2048)
	if len(after) < len(before) {
		t.Fatalf("fixture: rewrite must not shrink the file (%d < %d)", len(after), len(before))
	}
	writeFile(t, sessionPath, after)
	assertSameClaudeSessions(t, "after rewrite",
		incrementalClaudeSessions(t, tails, projectDir, sessionPath), fullClaudeSessions(t, projectDir, testOtherSession))
}

// TestClaudeIncremental_FinalRecordWithoutNewline: a complete final record
// whose newline is not written yet is part of a full parse, so it must be in
// the incremental output, without entering the cache it is read into again
// once the newline arrives.
func TestClaudeIncremental_FinalRecordWithoutNewline(t *testing.T) {
	fixture := newClaudeFixture(t)
	userLine, userID := fixture.record(testClaudeSession, "user", "",
		map[string]interface{}{"role": "user", "content": "Start"}, nil)
	assistantLine, assistantID := fixture.record(testClaudeSession, "assistant", userID,
		map[string]interface{}{"role": "assistant", "content": []interface{}{map[string]interface{}{"type": "text", "text": "ok"}}}, nil)
	summaryLine := fixture.line(map[string]interface{}{"type": "summary", "summary": "Attached summary", "leafUuid": assistantID})
	sidechainLine, _ := fixture.record(testClaudeSession, "user", "",
		map[string]interface{}{"role": "user", "content": "Subagent"}, map[string]interface{}{"isSidechain": true})
	laterLine, _ := fixture.record(testClaudeSession, "user", assistantID,
		map[string]interface{}{"role": "user", "content": "Later"}, nil)

	tests := []struct {
		name    string
		head    string // newline-terminated lines already in the file
		pending string // final line, written without its newline
	}{
		{name: "regular record", head: userLine + assistantLine, pending: laterLine},
		{name: "summary attaching to the previous record", head: userLine + assistantLine, pending: summaryLine},
		{name: "parentless sidechain record", head: userLine + assistantLine, pending: sidechainLine},
		{name: "only record in the file", pending: userLine},
		{name: "half-written record", head: userLine + assistantLine, pending: laterLine[:len(laterLine)/2] + "\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionPath := filepath.Join(t.TempDir(), testClaudeSession+".jsonl")
			pending := strings.TrimSuffix(tt.pending, "\n")
			writeFile(t, sessionPath, tt.head+pending)
			full := func() []JSONLRecord {
				records, err := NewJSONLParser().parseSessionFile(sessionPath)
				if err != nil {
					t.Fatalf("full parse: %v", err)
				}
				return records
			}
			assertRecords := func(step string, got, want []JSONLRecord) {
				t.Helper()
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				if string(gotJSON) != string(wantJSON) {
					t.Errorf("%s: records differ\n got: %s\nwant: %s", step, gotJSON, wantJSON)
				}
			}

			tails := newClaudeTailCache(time.Hour)
			for _, step := range []string{"first read", "re-read without changes"} {
				got, _, err := tails.readLocked(sessionPath)
				if err != nil {
					t.Fatalf("%s: %v", step, err)
				}
				assertRecords(step, got, full())
			}

			// The cache holds only the newline-terminated lines.
			headPath := filepath.Join(t.TempDir(), "head.jsonl")
			writeFile(t, headPath, tt.head)
			headRecords, err := NewJSONLParser().parseSessionFile(headPath)
			if err != nil {
				t.Fatal(err)
			}
			for i := range headRecords {
				headRecords[i].File = sessionPath
			}
			tail, _ := tails.files.Get(sessionPath)
			assertRecords("cached records", tail.parser.records, headRecords)

			appendFile(t, sessionPath, "\n"+laterLine)
			got, restarted, err := tails.readLocked(sessionPath)
			if err != nil {
				t.Fatal(err)
			}
			if restarted {
				t.Error("completing the pending line restarted the read")
			}
			assertRecords("after the newline", got, full())
		})
	}
}

// TestClaudeIncremental_ResumesFileStateAtEveryBoundary checks that summary
// and sidechain handling carry across reads: splitting a file at any line
// boundary must yield exactly the records of a single full parse.
func TestClaudeIncremental_ResumesFileStateAtEveryBoundary(t *testing.T) {
	fixture := newClaudeFixture(t)
	var lines []string
	lines = append(lines, fixture.line(map[string]interface{}{"type": "summary", "summary": "Pending summary"}))
	userLine, userID := fixture.record(testClaudeSession, "user", "",
		map[string]interface{}{"role": "user", "content": "Start"}, nil)
	lines = append(lines, userLine)
	lines = append(lines, "\n")
	assistantLine, assistantID := fixture.record(testClaudeSession, "assistant", userID,
		map[string]interface{}{"role": "assistant", "content": []interface{}{map[string]interface{}{"type": "text", "text": "ok"}}}, nil)
	lines = append(lines, assistantLine)
	lines = append(lines, fixture.line(map[string]interface{}{"type": "summary", "summary": "Attached summary", "leafUuid": assistantID}))
	sidechainLine, _ := fixture.record(testClaudeSession, "user", "",
		map[string]interface{}{"role": "user", "content": "Subagent"}, map[string]interface{}{"isSidechain": true})
	lines = append(lines, sidechainLine)
	lines = append(lines, "{corrupted\n")
	tailLine, _ := fixture.record(testClaudeSession, "user", "",
		map[string]interface{}{"role": "user", "content": "Later"}, map[string]interface{}{"isSidechain": true})
	lines = append(lines, tailLine)

	for split := 0; split <= len(lines); split++ {
		t.Run(fmt.Sprintf("split_%d", split), func(t *testing.T) {
			sessionPath := filepath.Join(t.TempDir(), testClaudeSession+".jsonl")
			writeFile(t, sessionPath, strings.Join(lines[:split], ""))

			tails := newClaudeTailCache(time.Hour)
			if _, _, err := tails.readLocked(sessionPath); err != nil {
				t.Fatalf("first read: %v", err)
			}
			appendFile(t, sessionPath, strings.Join(lines[split:], ""))
			got, _, err := tails.readLocked(sessionPath)
			if err != nil {
				t.Fatalf("second read: %v", err)
			}

			want, err := NewJSONLParser().parseSessionFile(sessionPath)
			if err != nil {
				t.Fatalf("full parse: %v", err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("records differ from a full parse\n got: %s\nwant: %s", gotJSON, wantJSON)
			}
		})
	}
}
