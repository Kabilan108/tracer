package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tracer-ai/tracer-cli/pkg/providers/claudecode"
	"github.com/tracer-ai/tracer-cli/pkg/providers/codexcli"
	"github.com/tracer-ai/tracer-cli/pkg/spi"
)

// fileStreamProvider streams one session per file in dir, counting the files it parses.
type fileStreamProvider struct {
	testProvider
	dir    string
	parsed []string
}

func newFileStreamProvider(t *testing.T, dir string, files map[string]string) *fileStreamProvider {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &fileStreamProvider{testProvider: *newTestProvider("Stream"), dir: dir}
}

func (p *fileStreamProvider) StreamAgentChatSessions(ctx context.Context, projectPath string, debugRaw bool, visitor spi.SessionVisitor) error {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return err
	}
	visitor.ReportSources(len(entries))
	for _, entry := range entries {
		path := filepath.Join(p.dir, entry.Name())
		stat, err := spi.StatFile(path)
		src := spi.SessionSource{Key: path, Files: []spi.FileStat{stat}}
		if err != nil {
			visitor.ReportDone(src, err)
			continue
		}
		if !visitor.ParseNeeded(src) {
			continue
		}
		content, err := os.ReadFile(path)
		if err == nil {
			p.parsed = append(p.parsed, entry.Name())
			session := newSession("stream", "Stream", entry.Name(), entry.Name(), string(content))
			visitor.ReportSession(src, &session)
		}
		visitor.ReportDone(src, err)
	}
	return nil
}

func ingestStreamProvider(t *testing.T, opts Options, provider *fileStreamProvider, debugRaw bool) ([]string, Summary) {
	t.Helper()
	provider.parsed = nil
	engine, err := New(opts)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	summary, err := engine.IngestProviders(context.Background(), "", map[string]spi.Provider{"stream": provider}, debugRaw)
	if err != nil {
		t.Fatalf("IngestProviders() error = %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	sort.Strings(provider.parsed)
	return provider.parsed, summary
}

func TestIngestProviders_SkipsUnchangedSources(t *testing.T) {
	tempDir := t.TempDir()
	sourceDir := filepath.Join(tempDir, "sources")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := newFileStreamProvider(t, sourceDir, map[string]string{"a": "alpha", "b": "bravo", "c": "charlie"})

	watchOpts := newRunModeOptions(tempDir, 10*time.Millisecond)
	watchOpts.SkipUnchangedSources = true
	all := []string{"a", "b", "c"}

	steps := []struct {
		name        string
		change      func(t *testing.T)
		opts        func() Options
		debugRaw    bool
		wantParsed  []string
		wantSkipped int
	}{
		{name: "first startup parses everything", wantParsed: all},
		{name: "restart without changes parses nothing", wantParsed: nil, wantSkipped: 3},
		{
			name:        "appended source is re-parsed",
			change:      func(t *testing.T) { appendTo(t, filepath.Join(sourceDir, "b"), " more") },
			wantParsed:  []string{"b"},
			wantSkipped: 2,
		},
		{
			name: "source whose archived output was deleted is re-parsed",
			change: func(t *testing.T) {
				if err := os.Remove(filepath.Join(tempDir, "history", "stream", "c.md")); err != nil {
					t.Fatal(err)
				}
			},
			wantParsed:  []string{"c"},
			wantSkipped: 2,
		},
		{
			name: "fingerprints from another parser version are ignored",
			change: func(t *testing.T) {
				setStoredSourceVersion(t, watchOpts.StateDBPath, SourceFingerprintVersion-1)
			},
			wantParsed:  all,
			wantSkipped: 3, // re-parsed, then skipped by the unchanged markdown hash
		},
		{name: "restart after the version re-parse skips again", wantParsed: nil, wantSkipped: 3},
		{
			name: "changed source scope re-parses everything",
			opts: func() Options {
				opts := watchOpts
				opts.SourceScope = "exclude_projects=[\"scratch\"]"
				return opts
			},
			wantParsed:  all,
			wantSkipped: 3,
		},
		{
			name:        "debug-raw parses everything",
			opts:        func() Options { opts := watchOpts; opts.SourceScope = "exclude_projects=[\"scratch\"]"; return opts },
			debugRaw:    true,
			wantParsed:  all,
			wantSkipped: 3,
		},
		{
			name: "sync mode never skips",
			opts: func() Options {
				opts := watchOpts
				opts.SourceScope = "exclude_projects=[\"scratch\"]"
				opts.SkipUnchangedSources = false
				return opts
			},
			wantParsed:  all,
			wantSkipped: 3,
		},
	}

	for _, step := range steps {
		if step.change != nil {
			step.change(t)
		}
		opts := watchOpts
		if step.opts != nil {
			opts = step.opts()
		}
		parsed, summary := ingestStreamProvider(t, opts, provider, step.debugRaw)
		if strings.Join(parsed, ",") != strings.Join(step.wantParsed, ",") {
			t.Errorf("%s: parsed %v, want %v", step.name, parsed, step.wantParsed)
		}
		if summary.Skipped != step.wantSkipped || summary.Errors != 0 {
			t.Errorf("%s: summary = %+v, want %d skipped and no errors", step.name, summary, step.wantSkipped)
		}
	}

	for _, name := range all {
		if _, err := os.Stat(filepath.Join(tempDir, "history", "stream", name+".md")); err != nil {
			t.Errorf("archive output for %s missing after all steps: %v", name, err)
		}
	}
}

func TestIngestProviders_SourceProgress(t *testing.T) {
	tempDir := t.TempDir()
	sourceDir := filepath.Join(tempDir, "sources")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := newFileStreamProvider(t, sourceDir, map[string]string{"a": "alpha", "b": "bravo"})

	opts := newRunModeOptions(tempDir, 10*time.Millisecond)
	opts.SkipUnchangedSources = true
	ingestStreamProvider(t, opts, provider, false)

	var progress []string
	opts.OnSourceProcessed = func(providerID string, processed int, total int) {
		progress = append(progress, fmt.Sprintf("%d/%d", processed, total))
	}
	ingestStreamProvider(t, opts, provider, false)
	if got := strings.Join(progress, " "); got != "1/2 2/2" {
		t.Errorf("source progress for skipped sources = %q, want %q", got, "1/2 2/2")
	}
}

func appendTo(t *testing.T, path string, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// setStoredSourceVersion rewrites every stored fingerprint version, which is
// what the database looks like to a binary whose SourceFingerprintVersion differs.
func setStoredSourceVersion(t *testing.T, dbPath string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE source_state SET version = ?`, version); err != nil {
		t.Fatal(err)
	}
}

// parseCountingProvider wraps a real provider and counts the sources the engine asked it to parse.
type parseCountingProvider struct {
	spi.Provider
	parsed []string
}

func (p *parseCountingProvider) StreamAgentChatSessions(ctx context.Context, projectPath string, debugRaw bool, visitor spi.SessionVisitor) error {
	shouldParse := visitor.ShouldParse
	visitor.ShouldParse = func(src spi.SessionSource) bool {
		parse := shouldParse(src)
		if parse {
			p.parsed = append(p.parsed, filepath.Base(src.Key))
		}
		return parse
	}
	return p.Provider.(spi.SessionStreamer).StreamAgentChatSessions(ctx, projectPath, debugRaw, visitor)
}

func TestRunDaemonStartup_RealProvidersSkipUnchangedSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	codexPath := filepath.Join(home, ".codex", "sessions", "2026", "10", "01", "rollout-2026-10-01T12-00-00-019a0000-0000-7000-8000-00000000000a.jsonl")
	writeFixture(t, codexPath,
		`{"type":"session_meta","timestamp":"2026-10-01T12:00:00Z","payload":{"id":"019a0000-0000-7000-8000-00000000000a","timestamp":"2026-10-01T12:00:00Z","cwd":"/tmp/startup-project"}}`,
		`{"type":"event_msg","timestamp":"2026-10-01T12:00:01Z","payload":{"type":"user_message","message":"Codex prompt"}}`,
		`{"type":"event_msg","timestamp":"2026-10-01T12:00:02Z","payload":{"type":"agent_message","message":"Codex reply"}}`,
	)
	claudeProject := filepath.Join(home, ".claude", "projects", "-tmp-startup-project")
	claudePath := filepath.Join(claudeProject, "33333333-3333-4333-8333-333333333333.jsonl")
	writeFixture(t, claudePath,
		`{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"33333333-3333-4333-8333-333333333333","timestamp":"2026-10-01T12:00:00.000Z","cwd":"/tmp/startup-project","message":{"role":"user","content":"Claude prompt"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"33333333-3333-4333-8333-333333333333","timestamp":"2026-10-01T12:00:01.000Z","message":{"role":"assistant","model":"claude-test","content":[{"type":"text","text":"Claude reply"}]}}`,
	)

	codex := &parseCountingProvider{Provider: codexcli.NewProvider()}
	claude := &parseCountingProvider{Provider: claudecode.NewProvider()}
	providers := map[string]spi.Provider{"codex": codex, "claude": claude}
	opts := newRunModeOptions(t.TempDir(), 10*time.Millisecond)
	opts.SkipUnchangedSources = true

	startup := func() Summary {
		codex.parsed, claude.parsed = nil, nil
		engine, err := New(opts)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		summary, err := engine.IngestProviders(context.Background(), "", providers, false)
		if err != nil {
			t.Fatalf("IngestProviders() error = %v", err)
		}
		if err := engine.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		return summary
	}

	first := startup()
	if first.Created != 2 || len(codex.parsed) != 1 || len(claude.parsed) != 1 {
		t.Fatalf("first startup: summary %+v, parsed codex=%v claude=%v", first, codex.parsed, claude.parsed)
	}

	second := startup()
	if len(codex.parsed) != 0 || len(claude.parsed) != 0 {
		t.Errorf("restart without changes parsed codex=%v claude=%v, want nothing", codex.parsed, claude.parsed)
	}
	if second.Skipped != 2 || second.Created != 0 || second.Updated != 0 {
		t.Errorf("restart without changes summary = %+v, want 2 skipped", second)
	}

	appendTo(t, codexPath, `{"type":"event_msg","timestamp":"2026-10-01T12:00:03Z","payload":{"type":"agent_message","message":"More"}}`+"\n")
	third := startup()
	if len(codex.parsed) != 1 || len(claude.parsed) != 0 {
		t.Errorf("after a Codex append parsed codex=%v claude=%v, want only the Codex file", codex.parsed, claude.parsed)
	}
	if third.Updated != 1 || third.Skipped != 1 {
		t.Errorf("after a Codex append summary = %+v, want 1 updated and 1 skipped", third)
	}

	// A new file in the Claude project changes the project source
	writeFixture(t, filepath.Join(claudeProject, "44444444-4444-4444-8444-444444444444.jsonl"),
		`{"type":"user","uuid":"u2","parentUuid":null,"sessionId":"44444444-4444-4444-8444-444444444444","timestamp":"2026-10-02T12:00:00.000Z","cwd":"/tmp/startup-project","message":{"role":"user","content":"Second prompt"}}`,
	)
	fourth := startup()
	if len(codex.parsed) != 0 || len(claude.parsed) != 1 {
		t.Errorf("after a new Claude file parsed codex=%v claude=%v, want only the Claude project", codex.parsed, claude.parsed)
	}
	if fourth.Created != 1 || fourth.Skipped != 2 {
		t.Errorf("after a new Claude file summary = %+v, want 1 created and 2 skipped", fourth)
	}
}

func writeFixture(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
