package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	cmdpkg "github.com/tracer-ai/tracer-cli/pkg/cmd"
	"github.com/tracer-ai/tracer-cli/pkg/config"
	"github.com/tracer-ai/tracer-cli/pkg/engine"
	sessionpkg "github.com/tracer-ai/tracer-cli/pkg/session"
)

func TestSkillReferencesExistInCommandTree(t *testing.T) {
	const testVersion = "0.2.0-test"
	var output strings.Builder
	skillCommand := cmdpkg.CreateSkillCommand(testVersion)
	skillCommand.SetOut(&output)
	if err := skillCommand.Execute(); err != nil {
		t.Fatalf("skill command Execute() error = %v", err)
	}

	root := createCommandTree(testVersion)
	globalFlagNames := make(map[string]bool)
	commandsByPath := make(map[string]*cobra.Command)
	var walk func(*cobra.Command)
	walk = func(command *cobra.Command) {
		commandsByPath[command.CommandPath()] = command
		command.LocalNonPersistentFlags().VisitAll(func(flag *pflag.Flag) {
			globalFlagNames["--"+flag.Name] = true
		})
		command.PersistentFlags().VisitAll(func(flag *pflag.Flag) {
			globalFlagNames["--"+flag.Name] = true
		})
		for _, child := range command.Commands() {
			walk(child)
		}
	}
	walk(root)

	flagPattern := regexp.MustCompile(`--[A-Za-z0-9][A-Za-z0-9-]*`)
	commandPattern := regexp.MustCompile(`\btracer[ \t]+([a-z][a-z-]*)(?:[ \t]+([a-z][a-z-]*))?`)
	for lineNumber, line := range strings.Split(output.String(), "\n") {
		flags := flagPattern.FindAllString(line, -1)
		mentions := commandPattern.FindAllStringSubmatch(line, -1)
		if len(mentions) == 0 {
			for _, flag := range flags {
				if !globalFlagNames[flag] {
					t.Errorf("skill line %d references unknown flag %s", lineNumber+1, flag)
				}
			}
			continue
		}

		for _, mention := range mentions {
			command := commandsByPath["tracer "+mention[1]]
			if command == nil {
				t.Errorf("skill line %d references unknown command %q", lineNumber+1, mention[0])
				continue
			}
			if command.HasSubCommands() && mention[2] != "" {
				command = commandsByPath[command.CommandPath()+" "+mention[2]]
				if command == nil {
					t.Errorf("skill line %d references unknown subcommand %q", lineNumber+1, mention[0])
					continue
				}
			}

			commandFlagNames := make(map[string]bool)
			visitFlag := func(flag *pflag.Flag) {
				commandFlagNames["--"+flag.Name] = true
			}
			command.LocalNonPersistentFlags().VisitAll(visitFlag)
			command.PersistentFlags().VisitAll(visitFlag)
			command.InheritedFlags().VisitAll(visitFlag)
			for _, flag := range flags {
				if !commandFlagNames[flag] {
					t.Errorf("skill line %d references flag %s outside %s", lineNumber+1, flag, command.CommandPath())
				}
			}
		}
	}
}

type fileInfoStub struct {
	mode os.FileMode
}

func (f fileInfoStub) Name() string       { return "stub" }
func (f fileInfoStub) Size() int64        { return 0 }
func (f fileInfoStub) Mode() os.FileMode  { return f.mode }
func (f fileInfoStub) ModTime() time.Time { return time.Time{} }
func (f fileInfoStub) IsDir() bool        { return false }
func (f fileInfoStub) Sys() any           { return nil }

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

func writeArchivedGetSession(t *testing.T, archiveRoot string, providerID string, project string, sessionID string, body string) string {
	t.Helper()

	path := filepath.Join(archiveRoot, providerID, project, sessionID+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("failed to create archive directory: %v", err)
	}
	content := "---\nsession_id: " + sessionID + "\nprovider: " + providerID + "\n---\n\n" + body
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write archived session: %v", err)
	}
	return path
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

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Time
		fail  bool
	}{
		{name: "duration", value: "24h", want: now.Add(-24 * time.Hour)},
		{name: "timestamp", value: "2026-07-01T00:00:00Z", want: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{name: "invalid", value: "last-week", fail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSince(tt.value, now)
			if (err != nil) != tt.fail {
				t.Fatalf("parseSince() error = %v", err)
			}
			if !tt.fail && !got.Equal(tt.want) {
				t.Fatalf("parseSince() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMetadataMatches(t *testing.T) {
	metadata := sessionpkg.Metadata{
		Provider: "codex",
		CWD:      "/home/kabilan/dotfiles",
		Ended:    "2026-07-13T10:00:00Z",
		Outcome:  "done",
		Tags:     []string{"gold", "Ready"},
	}
	untagged := sessionpkg.Metadata{
		Provider: "codex",
		CWD:      "/home/kabilan/dotfiles",
		Ended:    "2026-07-13T10:00:00Z",
	}
	tests := []struct {
		name     string
		metadata sessionpkg.Metadata
		flags    listFlags
		tags     []string
		want     bool
	}{
		{name: "all filters", metadata: metadata, flags: listFlags{provider: "codex", project: "dotfiles", outcome: "done"}, tags: []string{"gold"}, want: true},
		{name: "provider mismatch", metadata: metadata, flags: listFlags{provider: "claude"}, want: false},
		{name: "project path match", metadata: metadata, flags: listFlags{project: "kabilan/dot"}, want: true},
		{name: "single positive", metadata: metadata, tags: []string{"gold"}, want: true},
		{name: "single negative", metadata: metadata, tags: []string{"!gold"}, want: false},
		{name: "mixed positive and negative", metadata: metadata, tags: []string{"gold", "!review"}, want: true},
		{name: "repeated positives", metadata: metadata, tags: []string{"gold", "ready"}, want: true},
		{name: "absent tag with negation", metadata: metadata, tags: []string{"!review"}, want: true},
		{name: "present tag with negation", metadata: metadata, tags: []string{"!ready"}, want: false},
		{name: "case insensitive", metadata: metadata, tags: []string{"GOLD", "!REVIEW"}, want: true},
		{name: "missing tag", metadata: metadata, tags: []string{"review"}, want: false},
		{name: "whitespace inside negation", metadata: metadata, tags: []string{"! gold"}, want: false},
		{name: "nil tags rejects positive", metadata: untagged, tags: []string{"gold"}, want: false},
		{name: "nil tags satisfies negation", metadata: untagged, tags: []string{"!wiki:compiled"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filters, err := parseTagFilters(tt.tags)
			if err != nil {
				t.Fatalf("parseTagFilters() error = %v", err)
			}
			tt.flags.tagFilters = filters
			if got := metadataMatches(tt.metadata, tt.flags, time.Time{}); got != tt.want {
				t.Fatalf("metadataMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseTagFilters(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want []tagFilter
		fail bool
	}{
		{name: "no tags", want: []tagFilter{}},
		{name: "valid positive", tags: []string{"gold"}, want: []tagFilter{{tag: "gold"}}},
		{name: "valid negative", tags: []string{"!wiki:compiled"}, want: []tagFilter{{tag: "wiki:compiled", negated: true}}},
		{name: "whitespace inside negation", tags: []string{"! gold"}, want: []tagFilter{{tag: "gold", negated: true}}},
		{name: "lowercased", tags: []string{"GOLD"}, want: []tagFilter{{tag: "gold"}}},
		{name: "empty", tags: []string{""}, fail: true},
		{name: "whitespace", tags: []string{"  "}, fail: true},
		{name: "bare negation", tags: []string{"!"}, fail: true},
		{name: "spaced bare negation", tags: []string{" ! "}, fail: true},
		{name: "negation of only whitespace", tags: []string{"!  "}, fail: true},
		{name: "contradiction", tags: []string{"gold", "!gold"}, fail: true},
		{name: "contradiction case insensitive", tags: []string{"GOLD", "!gold"}, fail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filters, err := parseTagFilters(tt.tags)
			if (err != nil) != tt.fail {
				t.Fatalf("parseTagFilters() error = %v, want failure %v", err, tt.fail)
			}
			if tt.fail {
				return
			}
			if len(filters) != len(tt.want) {
				t.Fatalf("parseTagFilters() = %v, want %v", filters, tt.want)
			}
			for i, filter := range filters {
				if filter != tt.want[i] {
					t.Fatalf("parseTagFilters()[%d] = %v, want %v", i, filter, tt.want[i])
				}
			}
		})
	}
}

func TestMutationReadRoots(t *testing.T) {
	tempDir := t.TempDir()
	primaryRoot := filepath.Join(tempDir, "primary")
	annotatableRoot := filepath.Join(tempDir, "annotatable")
	readOnlyRoot := filepath.Join(tempDir, "read-only")
	primaryPath := writeArchivedGetSession(t, primaryRoot, "codex", "project", "primary-only", "primary\n")
	annotatablePath := writeArchivedGetSession(t, annotatableRoot, "codex", "project", "annotatable-only", "annotatable\n")
	writeArchivedGetSession(t, readOnlyRoot, "codex", "project", "read-only-only", "read only\n")
	duplicatePrimaryPath := writeArchivedGetSession(t, primaryRoot, "codex", "project", "duplicate", "primary duplicate\n")
	duplicateAnnotatablePath := writeArchivedGetSession(t, annotatableRoot, "codex", "project", "duplicate", "annotatable duplicate\n")
	withGetCommandGlobals(t, primaryRoot, filepath.Join(tempDir, "debug"), &config.Config{Archive: config.ArchiveConfig{
		AdditionalRoots:  []string{annotatableRoot, readOnlyRoot},
		AnnotatableRoots: []string{annotatableRoot},
	}})

	tests := []struct {
		name        string
		argument    string
		wantPath    string
		wantError   string
		wantDetails []string
	}{
		{name: "ID in primary only", argument: "primary-only", wantPath: primaryPath},
		{name: "ID in annotatable root only", argument: "annotatable-only", wantPath: annotatablePath},
		{name: "ID in both roots", argument: "duplicate", wantError: "ambiguous", wantDetails: []string{duplicatePrimaryPath, duplicateAnnotatablePath}},
		{name: "ID in non-annotatable additional root", argument: "read-only-only", wantError: "found in non-annotatable root"},
		{name: "explicit path anywhere", argument: filepath.Join(readOnlyRoot, "codex", "project", "read-only-only.md"), wantPath: filepath.Join(readOnlyRoot, "codex", "project", "read-only-only.md")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata, err := resolveMutationTranscript(tt.argument)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("resolveMutationTranscript() error = %v, want %q", err, tt.wantError)
				}
				for _, detail := range tt.wantDetails {
					if !strings.Contains(err.Error(), detail) {
						t.Errorf("resolveMutationTranscript() error = %v, want candidate path %q", err, detail)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if metadata.Path != tt.wantPath {
				t.Fatalf("resolveMutationTranscript() path = %q, want %q", metadata.Path, tt.wantPath)
			}
		})
	}
}

func TestMutationCommands_AnnotatableRoots(t *testing.T) {
	t.Run("tag by ID through symlinked annotatable root", func(t *testing.T) {
		tempDir := t.TempDir()
		primaryRoot := filepath.Join(tempDir, "primary")
		archiveRoot := filepath.Join(tempDir, "received")
		symlinkRoot := filepath.Join(tempDir, "received-link")
		path := writeArchivedGetSession(t, archiveRoot, "codex", "project", "taggable", "body\n")
		if err := os.Symlink(archiveRoot, symlinkRoot); err != nil {
			t.Fatal(err)
		}
		withGetCommandGlobals(t, primaryRoot, filepath.Join(tempDir, "debug"), &config.Config{Archive: config.ArchiveConfig{
			AdditionalRoots:  []string{symlinkRoot},
			AnnotatableRoots: []string{symlinkRoot},
		}})

		cmd := createTagCommand(false)
		cmd.SetArgs([]string{"taggable", "gold"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		metadata, _, err := sessionpkg.ParseFrontmatter(content)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(metadata.Tags, []string{"gold"}) {
			t.Fatalf("tags = %v, want [gold]", metadata.Tags)
		}
	})

	t.Run("outcome by ID into annotatable root", func(t *testing.T) {
		tempDir := t.TempDir()
		primaryRoot := filepath.Join(tempDir, "primary")
		archiveRoot := filepath.Join(tempDir, "received")
		path := writeArchivedGetSession(t, archiveRoot, "codex", "project", "finishable", "body\n")
		withGetCommandGlobals(t, primaryRoot, filepath.Join(tempDir, "debug"), &config.Config{Archive: config.ArchiveConfig{
			AdditionalRoots:  []string{archiveRoot},
			AnnotatableRoots: []string{archiveRoot},
		}})

		cmd := createOutcomeCommand()
		cmd.SetArgs([]string{"finishable", "done"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		metadata, _, err := sessionpkg.ParseFrontmatter(content)
		if err != nil {
			t.Fatal(err)
		}
		if metadata.Outcome != "done" {
			t.Fatalf("outcome = %q, want done", metadata.Outcome)
		}
	})

	t.Run("non-annotatable root has helpful error", func(t *testing.T) {
		tempDir := t.TempDir()
		primaryRoot := filepath.Join(tempDir, "primary")
		archiveRoot := filepath.Join(tempDir, "read-only")
		path := writeArchivedGetSession(t, archiveRoot, "codex", "project", "read-only-session", "body\n")
		withGetCommandGlobals(t, primaryRoot, filepath.Join(tempDir, "debug"), &config.Config{Archive: config.ArchiveConfig{
			AdditionalRoots: []string{archiveRoot},
		}})

		cmd := createTagCommand(false)
		cmd.SetArgs([]string{"read-only-session", "gold"})
		err := cmd.Execute()
		want := "found in non-annotatable root " + path + "; pass the explicit path or add the root to archive.annotatable_roots"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("tag error = %v, want %q", err, want)
		}
	})

	t.Run("subset violation fails at mutation time", func(t *testing.T) {
		tempDir := t.TempDir()
		withGetCommandGlobals(t, filepath.Join(tempDir, "primary"), filepath.Join(tempDir, "debug"), &config.Config{Archive: config.ArchiveConfig{
			AdditionalRoots:  []string{filepath.Join(tempDir, "additional")},
			AnnotatableRoots: []string{filepath.Join(tempDir, "other")},
		}})

		cmd := createOutcomeCommand()
		cmd.SetArgs([]string{"session", "done"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "must also be listed in archive.additional_roots") {
			t.Fatalf("outcome error = %v, want subset validation error", err)
		}
	})
}

func TestValidateTagName(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		want      string
		wantError string
	}{
		{name: "gold", value: "gold", want: "gold"},
		{name: "namespaced", value: "wiki:compiled", want: "wiki:compiled"},
		{name: "trimmed", value: "  Wiki:Compiled  ", want: "Wiki:Compiled"},
		{name: "empty", value: "   ", wantError: "must not be empty"},
		{name: "leading negation", value: "!wiki:compiled", wantError: "must not start with !"},
		{name: "embedded space", value: "wiki compiled", wantError: "must not contain whitespace"},
		{name: "comma", value: "wiki,compiled", wantError: "must not contain a comma"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateTagName(tt.value)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("validateTagName(%q) error = %v, want %q", tt.value, err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateTagName(%q) error = %v", tt.value, err)
			}
			if got != tt.want {
				t.Errorf("validateTagName(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestTagCommandRejectsInvalidTagNames(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantError string
	}{
		{name: "empty", value: "", wantError: "must not be empty"},
		{name: "leading negation", value: "!wiki:compiled", wantError: "must not start with !"},
		{name: "embedded space", value: "wiki compiled", wantError: "must not contain whitespace"},
		{name: "comma", value: "wiki,compiled", wantError: "must not contain a comma"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := createTagCommand(false)
			command.SetArgs([]string{"session-id", tt.value})
			err := command.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("tag command error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

func TestTagCommands_NamespacedTag(t *testing.T) {
	tempDir := t.TempDir()
	archiveRoot := filepath.Join(tempDir, "primary")
	path := writeArchivedGetSession(t, archiveRoot, "codex", "project", "namespaced-tag", "body\n")
	withGetCommandGlobals(t, archiveRoot, filepath.Join(tempDir, "debug"), nil)

	tagCommand := createTagCommand(false)
	tagCommand.SetArgs([]string{"namespaced-tag", "Wiki:Compiled"})
	if err := tagCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _, err := sessionpkg.ParseFrontmatter(content)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(metadata.Tags, []string{"wiki:compiled"}) {
		t.Fatalf("tags = %v, want [wiki:compiled]", metadata.Tags)
	}

	untagCommand := createTagCommand(true)
	untagCommand.SetArgs([]string{"namespaced-tag", "WIKI:COMPILED"})
	if err := untagCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _, err = sessionpkg.ParseFrontmatter(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.Tags) != 0 {
		t.Fatalf("tags = %v, want none", metadata.Tags)
	}
}

func TestListCommand_SymlinkedAdditionalRoot(t *testing.T) {
	tempDir := t.TempDir()
	primaryRoot := filepath.Join(tempDir, "primary")
	archiveRoot := filepath.Join(tempDir, "received")
	symlinkRoot := filepath.Join(tempDir, "received-link")
	writeArchivedGetSession(t, archiveRoot, "codex", "project", "linked-session", "body\n")
	if err := os.Symlink(archiveRoot, symlinkRoot); err != nil {
		t.Fatal(err)
	}
	withGetCommandGlobals(t, primaryRoot, filepath.Join(tempDir, "debug"), &config.Config{Archive: config.ArchiveConfig{
		AdditionalRoots: []string{symlinkRoot},
	}})

	cmd := createListCommand()
	cmd.SetArgs([]string{"--json", "--no-pager"})
	stdout, err := captureStdout(t, cmd.Execute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"session_id": "linked-session"`) {
		t.Fatalf("list output did not include symlinked additional root: %s", stdout)
	}
}

func TestListCommand_ArchiveJSON(t *testing.T) {
	previousOutputDir := outputDir
	previousConfig := loadedConfig
	previousStdout := os.Stdout
	t.Cleanup(func() {
		outputDir = previousOutputDir
		loadedConfig = previousConfig
		os.Stdout = previousStdout
	})
	outputDir = filepath.Join("tests", "fixtures", "archive")
	loadedConfig = nil

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	cmd := createListCommand()
	cmd.SetArgs([]string{"--json"})
	executeErr := cmd.Execute()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = previousStdout
	output, readErr := io.ReadAll(reader)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if executeErr != nil {
		t.Fatalf("list command error = %v", executeErr)
	}
	var sessions []sessionpkg.Metadata
	if err := json.Unmarshal(output, &sessions); err != nil {
		t.Fatalf("decode list JSON: %v\n%s", err, output)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(sessions))
	}
}

// TestListCommand_TagFlags exercises the cobra flag layer end to end: the
// StringArrayVar choice (commas stay literal), repeated flags, negation on an
// untagged fixture session, and validation errors surfacing through Execute.
func TestListCommand_TagFlags(t *testing.T) {
	previousOutputDir := outputDir
	previousConfig := loadedConfig
	previousStdout := os.Stdout
	t.Cleanup(func() {
		outputDir = previousOutputDir
		loadedConfig = previousConfig
		os.Stdout = previousStdout
	})
	outputDir = filepath.Join("tests", "fixtures", "archive")
	loadedConfig = nil

	runList := func(t *testing.T, args ...string) ([]sessionpkg.Metadata, error) {
		t.Helper()
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = writer
		cmd := createListCommand()
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetArgs(append([]string{"--json"}, args...))
		executeErr := cmd.Execute()
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		os.Stdout = previousStdout
		output, readErr := io.ReadAll(reader)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if executeErr != nil {
			return nil, executeErr
		}
		var sessions []sessionpkg.Metadata
		if err := json.Unmarshal(output, &sessions); err != nil {
			t.Fatalf("decode list JSON: %v\n%s", err, output)
		}
		return sessions, nil
	}

	t.Run("negation includes untagged sessions", func(t *testing.T) {
		sessions, err := runList(t, "--tag", "!wiki:compiled")
		if err != nil {
			t.Fatalf("list error = %v", err)
		}
		if len(sessions) != 2 {
			t.Fatalf("sessions = %d, want 2 (untagged sessions must satisfy negation)", len(sessions))
		}
	})
	t.Run("repeated flags AND together", func(t *testing.T) {
		sessions, err := runList(t, "--tag", "gold", "--tag", "!wiki:compiled")
		if err != nil {
			t.Fatalf("list error = %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("sessions = %d, want 1", len(sessions))
		}
	})
	t.Run("commas stay literal", func(t *testing.T) {
		sessions, err := runList(t, "--tag", "gold,ready")
		if err != nil {
			t.Fatalf("list error = %v", err)
		}
		if len(sessions) != 0 {
			t.Fatalf("sessions = %d, want 0 (comma value must be one literal tag, not split)", len(sessions))
		}
	})
	t.Run("invalid value errors through Execute", func(t *testing.T) {
		if _, err := runList(t, "--tag", "!"); err == nil {
			t.Fatal("expected error for bare ! tag")
		}
	})
	t.Run("contradiction errors through Execute", func(t *testing.T) {
		if _, err := runList(t, "--tag", "gold", "--tag", "!gold"); err == nil {
			t.Fatal("expected error for contradictory tag filters")
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

func TestGetCommandMarkdownOutput(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "get-markdown-session"
	archiveRoot := filepath.Join(tempDir, "archive")
	writeArchivedGetSession(t, archiveRoot, "codex", "get-project", sessionID, "hello from archived get\n")
	withGetCommandGlobals(t, archiveRoot, filepath.Join(tempDir, "debug"), nil)

	cmd := createGetCommand()
	cmd.SetArgs([]string{sessionID})

	stdout, err := captureStdout(t, cmd.Execute)
	if err != nil {
		t.Fatalf("get command error = %v", err)
	}
	if !strings.Contains(stdout, "hello from archived get") {
		t.Fatalf("get command stdout missing archived content: %s", stdout)
	}
}

func TestGetCommandPathOutput(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "get-path-session"
	archiveRoot := filepath.Join(tempDir, "archive")
	wantPath := writeArchivedGetSession(t, archiveRoot, "codex", "get-project", sessionID, "path output\n")
	withGetCommandGlobals(t, archiveRoot, filepath.Join(tempDir, "debug"), nil)

	cmd := createGetCommand()
	cmd.SetArgs([]string{sessionID, "-P"})

	stdout, err := captureStdout(t, cmd.Execute)
	if err != nil {
		t.Fatalf("get command error = %v", err)
	}

	if strings.TrimSpace(stdout) != wantPath {
		t.Fatalf("get command path stdout = %q, want %q", strings.TrimSpace(stdout), wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected archived markdown at %s: %v", wantPath, err)
	}
}

func TestGetCommandFiltersArchivedToolOutput(t *testing.T) {
	tempDir := t.TempDir()
	archiveRoot := filepath.Join(tempDir, "archive")
	sessionID := "get-filter-session"
	body := "<tool-use data-tool-type=\"shell\" data-tool-name=\"exec\"><details>\n<summary>Run</summary>\nsecret output\n</details></tool-use>\n"
	path := writeArchivedGetSession(t, archiveRoot, "codex", "get-project", sessionID, body)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	withGetCommandGlobals(t, archiveRoot, filepath.Join(tempDir, "debug"), nil)

	cmd := createGetCommand()
	cmd.SetArgs([]string{sessionID, "--tool-output=none"})
	stdout, err := captureStdout(t, cmd.Execute)
	if err != nil {
		t.Fatalf("get command error = %v", err)
	}
	if strings.Contains(stdout, "secret output") || !strings.Contains(stdout, "<summary>Run</summary>") {
		t.Fatalf("get command returned unexpected filtered output: %s", stdout)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("get command modified the archived transcript")
	}
}

func TestGetCommandPrefersEarliestArchiveRootForDuplicateProvider(t *testing.T) {
	tempDir := t.TempDir()
	primaryRoot := filepath.Join(tempDir, "primary")
	additionalRoot := filepath.Join(tempDir, "additional")
	sessionID := "duplicate-session"
	wantPath := writeArchivedGetSession(t, primaryRoot, "codex", "primary-project", sessionID, "primary copy\n")
	writeArchivedGetSession(t, additionalRoot, "codex", "additional-project", sessionID, "additional copy\n")
	withGetCommandGlobals(t, primaryRoot, filepath.Join(tempDir, "debug"), &config.Config{
		Archive: config.ArchiveConfig{AdditionalRoots: []string{additionalRoot}},
	})

	cmd := createGetCommand()
	cmd.SetArgs([]string{sessionID, "--path"})
	stdout, err := captureStdout(t, cmd.Execute)
	if err != nil {
		t.Fatalf("get command error = %v", err)
	}
	if strings.TrimSpace(stdout) != wantPath {
		t.Fatalf("get command path = %q, want earliest-root path %q", strings.TrimSpace(stdout), wantPath)
	}
}

func TestGetCommandSelectsUnknownProviderCaseInsensitively(t *testing.T) {
	tempDir := t.TempDir()
	archiveRoot := filepath.Join(tempDir, "archive")
	sessionID := "future-provider-session"
	writeArchivedGetSession(t, archiveRoot, "Future-Agent", "project", sessionID, "future provider copy\n")
	withGetCommandGlobals(t, archiveRoot, filepath.Join(tempDir, "debug"), nil)

	cmd := createGetCommand()
	cmd.SetArgs([]string{sessionID, "--provider=future-agent"})
	stdout, err := captureStdout(t, cmd.Execute)
	if err != nil {
		t.Fatalf("get command error = %v", err)
	}
	if !strings.Contains(stdout, "future provider copy") {
		t.Fatalf("get command did not select unknown provider archive: %s", stdout)
	}
}

func TestParseGetTurnsFlag(t *testing.T) {
	tests := []struct {
		value string
		want  bool
		fail  bool
	}{
		{value: "", want: false},
		{value: "user,agent", want: true},
		{value: "agent,user", want: true},
		{value: "user", fail: true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := parseGetTurnsFlag(tt.value)
			if (err != nil) != tt.fail {
				t.Fatalf("parseGetTurnsFlag(%q) error = %v", tt.value, err)
			}
			if !tt.fail && got != tt.want {
				t.Errorf("parseGetTurnsFlag(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
