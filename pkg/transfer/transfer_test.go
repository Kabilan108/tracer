package transfer

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tracer-ai/tracer-cli/pkg/session"
)

func transcriptBytes(t *testing.T, id, outcome string, tags []string, body string) []byte {
	t.Helper()
	frontmatter, err := session.RenderFrontmatter(session.Metadata{
		SessionID: id,
		Provider:  "codex",
		Models:    []string{},
		Outcome:   outcome,
		Tags:      tags,
	})
	if err != nil {
		t.Fatalf("RenderFrontmatter() error = %v", err)
	}
	return []byte(frontmatter + body)
}

func readAnnotations(t *testing.T, path string) (session.Annotations, string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	_, body, err := session.ParseFrontmatter(content)
	if err != nil {
		t.Fatalf("ParseFrontmatter(%s) error = %v", path, err)
	}
	return session.ExtractAnnotations(content), string(body)
}

func TestWriteReceivedFile_MergeMatrix(t *testing.T) {
	tests := []struct {
		name             string
		existing         []byte
		incoming         []byte
		wantMerged       bool
		wantOutcome      string
		wantTags         []string
		wantIncomingBody string
	}{
		{
			name:             "no local copy",
			incoming:         transcriptBytes(t, "one", "done", []string{"Sender"}, "# incoming\n"),
			wantOutcome:      "done",
			wantTags:         []string{"sender"},
			wantIncomingBody: "# incoming\n",
		},
		{
			name:             "local has tags only",
			existing:         transcriptBytes(t, "one", "", []string{"Local"}, "# old\n"),
			incoming:         transcriptBytes(t, "one", "done", []string{"sender"}, "# incoming\n"),
			wantMerged:       true,
			wantOutcome:      "done",
			wantTags:         []string{"local", "sender"},
			wantIncomingBody: "# incoming\n",
		},
		{
			name:             "local has outcome only",
			existing:         transcriptBytes(t, "one", "abandoned", nil, "# old\n"),
			incoming:         transcriptBytes(t, "one", "done", []string{"sender"}, "# incoming\n"),
			wantMerged:       true,
			wantOutcome:      "abandoned",
			wantTags:         []string{"sender"},
			wantIncomingBody: "# incoming\n",
		},
		{
			name:             "both sides annotated",
			existing:         transcriptBytes(t, "one", "done", []string{"local", "shared"}, "# old\n"),
			incoming:         transcriptBytes(t, "one", "abandoned", []string{"sender", "shared"}, "# incoming\n"),
			wantMerged:       true,
			wantOutcome:      "done",
			wantTags:         []string{"local", "sender", "shared"},
			wantIncomingBody: "# incoming\n",
		},
		{
			name:             "both empty",
			existing:         transcriptBytes(t, "one", "", nil, "# old\n"),
			incoming:         transcriptBytes(t, "one", "", nil, "# incoming\n"),
			wantMerged:       true,
			wantOutcome:      "",
			wantTags:         nil,
			wantIncomingBody: "# incoming\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "codex", "project", "one.md")
			if tt.existing != nil {
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, tt.existing, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			merged, err := writeReceivedFile(root, target, tt.incoming)
			if err != nil {
				t.Fatalf("writeReceivedFile() error = %v", err)
			}
			if merged != tt.wantMerged {
				t.Errorf("writeReceivedFile() merged = %v, want %v", merged, tt.wantMerged)
			}
			annotations, body := readAnnotations(t, target)
			if annotations.Outcome != tt.wantOutcome {
				t.Errorf("outcome = %q, want %q", annotations.Outcome, tt.wantOutcome)
			}
			if !reflect.DeepEqual(annotations.Tags, tt.wantTags) {
				t.Errorf("tags = %#v, want %#v", annotations.Tags, tt.wantTags)
			}
			if body != tt.wantIncomingBody {
				t.Errorf("body = %q, want %q", body, tt.wantIncomingBody)
			}
		})
	}
}

func TestWriteReceivedFile_WaitsForTranscriptLock(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "codex", "project", "one.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	// Why a sibling: the lock covers the transcript's directory, so a
	// tag on another session in the same project must also hold off receive.
	unlock, err := session.LockTranscript(filepath.Join(filepath.Dir(target), "two.md"))
	if err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(unlock)
	t.Cleanup(release)

	incoming := transcriptBytes(t, "one", "", nil, "# incoming\n")
	done := make(chan error, 1)
	go func() {
		_, err := writeReceivedFile(root, target, incoming)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("writeReceivedFile() returned %v while the transcript lock was held", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("transcript was written while the lock was held: stat error = %v", err)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("writeReceivedFile() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writeReceivedFile() did not finish after the lock was released")
	}
	if _, body := readAnnotations(t, target); body != "# incoming\n" {
		t.Errorf("body = %q, want incoming body", body)
	}
}

func TestScanPending_CursorDiff(t *testing.T) {
	tests := []struct {
		name       string
		cursorHash func(PendingFile) string
		wantFiles  int
		wantSkip   int
	}{
		{name: "new", cursorHash: func(PendingFile) string { return "" }, wantFiles: 1},
		{name: "changed", cursorHash: func(PendingFile) string { return "different" }, wantFiles: 1},
		{name: "unchanged", cursorHash: func(file PendingFile) string { return file.ContentHash }, wantSkip: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeSenderTranscript(t, root, "codex/project/one.md", "content")
			initial, err := ScanPending(root, nil)
			if err != nil {
				t.Fatal(err)
			}
			cursor := map[string]string{initial.Files[0].RelPath: tt.cursorHash(initial.Files[0])}
			got, err := ScanPending(root, cursor)
			if err != nil {
				t.Fatalf("ScanPending() error = %v", err)
			}
			if got.Scanned != 1 || len(got.Files) != tt.wantFiles || got.Skipped != tt.wantSkip {
				t.Fatalf("ScanPending() = %+v, want files=%d skipped=%d", got, tt.wantFiles, tt.wantSkip)
			}
		})
	}
}

func TestScanPending_SkipsInvalidFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeSenderTranscript(t, root, "codex/project/valid.md", "content")
	legacy := filepath.Join(root, "claude", "old", "legacy.md")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("# pre-frontmatter transcript\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ScanPending(root, nil)
	if err != nil {
		t.Fatalf("ScanPending() error = %v", err)
	}
	if got.Invalid != 1 {
		t.Fatalf("Invalid = %d, want 1", got.Invalid)
	}
	if got.Scanned != 1 || len(got.Files) != 1 || got.Files[0].RelPath != "codex/project/valid.md" {
		t.Fatalf("scan should contain only the valid transcript, got %+v", got)
	}
	for _, path := range got.AllPaths {
		if path == "claude/old/legacy.md" {
			t.Fatal("invalid transcript must not appear in AllPaths (cursor pruning scope)")
		}
	}
}

func TestScanPending_SkipsSymlinkLeaves(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "leak.md")); err != nil {
		t.Fatal(err)
	}
	result, err := ScanPending(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 0 || len(result.Files) != 0 || len(result.AllPaths) != 0 {
		t.Fatalf("ScanPending() = %+v, want symlink skipped", result)
	}
}

func TestPush_FailedSendLeavesCursorUntouched(t *testing.T) {
	root := t.TempDir()
	writeSenderTranscript(t, root, "codex/project/one.md", "# body\n")
	statePath := filepath.Join(t.TempDir(), "runtime-state.db")

	summary, err := Push(PushOptions{
		Remote:      "receiver",
		ArchiveRoot: root,
		StateDBPath: statePath,
		SenderHost:  "sender",
		Send: func(func(io.Writer) (TarResult, error)) (TarResult, error) {
			return TarResult{}, fmt.Errorf("ssh failed")
		},
	})
	if err == nil || summary.Failed != 1 {
		t.Fatalf("Push() summary=%+v error=%v, want failed push", summary, err)
	}
	cursor, err := OpenCursorStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer cursor.Close()
	hashes, err := cursor.LoadHashes("receiver")
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 0 {
		t.Fatalf("cursor hashes = %v, want untouched cursor", hashes)
	}
}

func TestPush_DryRunDoesNotCreateCursorDatabase(t *testing.T) {
	root := t.TempDir()
	writeSenderTranscript(t, root, "codex/project/one.md", "# body\n")
	statePath := filepath.Join(t.TempDir(), "runtime-state.db")
	var output strings.Builder
	summary, err := Push(PushOptions{
		Remote:      "receiver",
		ArchiveRoot: root,
		StateDBPath: statePath,
		DryRun:      true,
		Output:      &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Scanned != 1 || output.String() != "codex/project/one.md\n" {
		t.Fatalf("Push() summary=%+v output=%q", summary, output.String())
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("dry-run cursor database exists, stat error = %v", err)
	}
}

func TestPush_SkipsFileChangedAfterScan(t *testing.T) {
	root := t.TempDir()
	relPath := "codex/project/one.md"
	writeSenderTranscript(t, root, relPath, "# first\n")
	statePath := filepath.Join(t.TempDir(), "runtime-state.db")
	summary, err := Push(PushOptions{
		Remote:      "receiver",
		ArchiveRoot: root,
		StateDBPath: statePath,
		SenderHost:  "sender",
		Send: func(writeTar func(io.Writer) (TarResult, error)) (TarResult, error) {
			writeSenderTranscript(t, root, relPath, "# changed after scan\n")
			return writeTar(io.Discard)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Transferred != 0 || summary.Skipped != 1 {
		t.Fatalf("Push() summary = %+v, want changed file skipped", summary)
	}
	cursor, err := OpenCursorStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer cursor.Close()
	hashes, err := cursor.LoadHashes("receiver")
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 0 {
		t.Fatalf("cursor hashes = %v, want changed file uncheckpointed", hashes)
	}
}

func TestPush_PrunesDeletedCursorPaths(t *testing.T) {
	root := t.TempDir()
	firstPath := "codex/project/one.md"
	secondPath := "codex/project/two.md"
	writeSenderTranscript(t, root, firstPath, "# one\n")
	writeSenderTranscript(t, root, secondPath, "# two\n")
	statePath := filepath.Join(t.TempDir(), "runtime-state.db")
	push := func() {
		_, err := Push(PushOptions{
			Remote:      "receiver",
			ArchiveRoot: root,
			StateDBPath: statePath,
			SenderHost:  "sender",
			Send: func(writeTar func(io.Writer) (TarResult, error)) (TarResult, error) {
				return writeTar(io.Discard)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	push()
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(secondPath))); err != nil {
		t.Fatal(err)
	}
	push()

	cursor, err := OpenCursorStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer cursor.Close()
	hashes, err := cursor.LoadHashes("receiver")
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 1 || hashes[firstPath] == "" {
		t.Fatalf("cursor hashes = %v, want only %s", hashes, firstPath)
	}
}

func TestAcquirePushLock_RejectsConcurrentPush(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "runtime-state.db")
	unlock, err := acquirePushLock(statePath, "receiver")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquirePushLock(statePath, "receiver"); err == nil || !strings.Contains(err.Error(), "push to receiver already in progress") {
		t.Fatalf("second acquirePushLock() error = %v", err)
	}
	unlock()
	unlockAgain, err := acquirePushLock(statePath, "receiver")
	if err != nil {
		t.Fatalf("acquirePushLock() after release error = %v", err)
	}
	unlockAgain()
}

type testTarEntry struct {
	name     string
	typeflag byte
	content  []byte
}

func tarStreamEntries(t *testing.T, manifest Manifest, entries []testTarEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTarEntry(writer, manifestPath, manifestData); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		header := &tar.Header{
			Name:     entry.name,
			Mode:     0o644,
			Typeflag: entry.typeflag,
		}
		if entry.typeflag == tar.TypeReg {
			header.Size = int64(len(entry.content))
		}
		if entry.typeflag == tar.TypeLink || entry.typeflag == tar.TypeSymlink {
			header.Linkname = "target"
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.typeflag == tar.TypeReg {
			if _, err := writer.Write(entry.content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func tarStream(t *testing.T, manifest Manifest, entries map[string][]byte) []byte {
	t.Helper()
	ordered := make([]testTarEntry, 0, len(entries))
	for name, content := range entries {
		ordered = append(ordered, testTarEntry{name: name, typeflag: tar.TypeReg, content: content})
	}
	return tarStreamEntries(t, manifest, ordered)
}

func TestReceive_RejectsPathTraversal(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "absolute", path: "/tmp/escape.md"},
		{name: "parent traversal", path: "../escape.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := tarStream(t, Manifest{Protocol: 1, SenderHost: "sender", Count: 1}, map[string][]byte{tt.path: []byte("bad")})
			_, err := Receive(bytes.NewReader(stream), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), "unsafe archive path") {
				t.Fatalf("Receive() error = %v, want unsafe archive path", err)
			}
		})
	}
}

func TestReceive_RejectsNonRegularTarTypes(t *testing.T) {
	tests := []struct {
		name     string
		typeflag byte
	}{
		{name: "hard link", typeflag: tar.TypeLink},
		{name: "symbolic link", typeflag: tar.TypeSymlink},
		{name: "directory", typeflag: tar.TypeDir},
		{name: "character device", typeflag: tar.TypeChar},
		{name: "block device", typeflag: tar.TypeBlock},
		{name: "fifo", typeflag: tar.TypeFifo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := tarStreamEntries(t, Manifest{Protocol: 1, SenderHost: "sender", Count: 1}, []testTarEntry{{
				name:     "bad.md",
				typeflag: tt.typeflag,
			}})
			summary, err := Receive(bytes.NewReader(stream), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("Receive() summary=%+v error=%v, want rejected type", summary, err)
			}
			if summary.Failed != 1 || summary.Received != 0 {
				t.Fatalf("Receive() summary=%+v, want one failed entry", summary)
			}
		})
	}
}

func TestReceive_RejectsSymlinkedParentEscape(t *testing.T) {
	dest := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dest, "linked")); err != nil {
		t.Fatal(err)
	}
	incoming := transcriptBytes(t, "escape", "", nil, "# body\n")
	stream := tarStream(t, Manifest{Protocol: 1, SenderHost: "sender", Count: 1}, map[string][]byte{
		"linked/escape.md": incoming,
	})
	summary, err := Receive(bytes.NewReader(stream), dest)
	if err == nil || !strings.Contains(err.Error(), "escapes destination") {
		t.Fatalf("Receive() summary=%+v error=%v, want symlink escape rejection", summary, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.md")); !os.IsNotExist(err) {
		t.Fatalf("outside file was created, stat error = %v", err)
	}
}

func TestReceive_ReplacesInvalidExistingFrontmatter(t *testing.T) {
	dest := t.TempDir()
	target := filepath.Join(dest, "codex", "project", "one.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("# invalid existing transcript\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	incoming := transcriptBytes(t, "one", "done", []string{"sender"}, "# replacement\n")
	stream := tarStream(t, Manifest{Protocol: 1, SenderHost: "sender", Count: 1}, map[string][]byte{
		"codex/project/one.md": incoming,
	})
	summary, err := Receive(bytes.NewReader(stream), dest)
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	if summary.Merged != 1 || summary.Failed != 0 {
		t.Fatalf("Receive() summary = %+v, want recovered merge", summary)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, incoming) {
		t.Fatalf("target content = %q, want incoming bytes", content)
	}
}

func TestReceive_InvalidIncomingContinuesBatch(t *testing.T) {
	dest := t.TempDir()
	good := transcriptBytes(t, "good", "", nil, "# good\n")
	stream := tarStreamEntries(t, Manifest{Protocol: 1, SenderHost: "sender", Count: 2}, []testTarEntry{
		{name: "bad.md", typeflag: tar.TypeReg, content: []byte("# no frontmatter\n")},
		{name: "good.md", typeflag: tar.TypeReg, content: good},
	})
	summary, err := Receive(bytes.NewReader(stream), dest)
	if err == nil || !strings.Contains(err.Error(), "parse incoming frontmatter") {
		t.Fatalf("Receive() summary=%+v error=%v, want batch error", summary, err)
	}
	if summary.Failed != 1 || summary.Received != 1 || summary.Created != 1 {
		t.Fatalf("Receive() summary = %+v, want one failure and one creation", summary)
	}
	if _, err := os.Stat(filepath.Join(dest, "bad.md")); !os.IsNotExist(err) {
		t.Fatalf("invalid incoming file exists, stat error = %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(dest, "good.md")); err != nil || !bytes.Equal(content, good) {
		t.Fatalf("good file content=%q error=%v", content, err)
	}
}

func TestReceive_WriteFailureContinuesBatch(t *testing.T) {
	dest := t.TempDir()
	if err := os.Mkdir(filepath.Join(dest, "blocked.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	blocked := transcriptBytes(t, "blocked", "", nil, "# blocked\n")
	good := transcriptBytes(t, "good", "", nil, "# good\n")
	stream := tarStreamEntries(t, Manifest{Protocol: 1, SenderHost: "sender", Count: 2}, []testTarEntry{
		{name: "blocked.md", typeflag: tar.TypeReg, content: blocked},
		{name: "good.md", typeflag: tar.TypeReg, content: good},
	})
	summary, err := Receive(bytes.NewReader(stream), dest)
	if err == nil || !strings.Contains(err.Error(), "receive blocked.md") {
		t.Fatalf("Receive() summary=%+v error=%v, want write failure", summary, err)
	}
	if summary.Failed != 1 || summary.Received != 1 {
		t.Fatalf("Receive() summary = %+v, want continued batch", summary)
	}
	if _, err := os.Stat(filepath.Join(dest, "good.md")); err != nil {
		t.Fatalf("good file was not written: %v", err)
	}
}

func TestReceive_ProtocolMismatch(t *testing.T) {
	stream := tarStream(t, Manifest{Protocol: protocolRetract + 1, SenderHost: "sender"}, nil)
	_, err := Receive(bytes.NewReader(stream), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "upgrade tracer") {
		t.Fatalf("Receive() error = %v, want upgrade tracer", err)
	}
}

func writeSenderTranscript(t *testing.T, root, relPath, body string) {
	t.Helper()
	writeTaggedSenderTranscript(t, root, relPath, body, nil)
}

func writeTaggedSenderTranscript(t *testing.T, root, relPath, body string, tags []string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, transcriptBytes(t, strings.TrimSuffix(filepath.Base(relPath), ".md"), "", tags, body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pipePushToReceive(t *testing.T, writeTar func(io.Writer) (TarResult, error), receiverRoot string) (TarResult, ReceiveSummary) {
	t.Helper()
	reader, writer := io.Pipe()
	type writeResult struct {
		result TarResult
		err    error
	}
	writeDone := make(chan writeResult, 1)
	go func() {
		result, err := writeTar(writer)
		_ = writer.CloseWithError(err)
		writeDone <- writeResult{result: result, err: err}
	}()
	summary, receiveErr := Receive(reader, receiverRoot)
	result := <-writeDone
	if result.err != nil {
		t.Fatalf("WriteTar() error = %v", result.err)
	}
	if receiveErr != nil {
		t.Fatalf("Receive() error = %v", receiveErr)
	}
	return result.result, summary
}

func TestPushReceiveEndToEnd_ReceiverAnnotationsSurvive(t *testing.T) {
	senderRoot := t.TempDir()
	receiverRoot := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "runtime-state.db")
	remote := "receiver"
	firstPath := "codex/project/one.md"
	secondPath := "codex/project/two.md"
	writeSenderTranscript(t, senderRoot, firstPath, "# first version\n")
	writeSenderTranscript(t, senderRoot, secondPath, "# unchanged\n")

	push := func() (PushSummary, ReceiveSummary) {
		var receiveSummary ReceiveSummary
		pushSummary, err := Push(PushOptions{
			Remote:      remote,
			ArchiveRoot: senderRoot,
			StateDBPath: statePath,
			SenderHost:  "sender",
			Send: func(writeTar func(io.Writer) (TarResult, error)) (TarResult, error) {
				result, summary := pipePushToReceive(t, writeTar, receiverRoot)
				receiveSummary = summary
				return result, nil
			},
		})
		if err != nil {
			t.Fatalf("Push() error = %v", err)
		}
		return pushSummary, receiveSummary
	}

	firstPush, firstReceive := push()
	if firstPush.Transferred != 2 || firstReceive.Created != 2 {
		t.Fatalf("first push=%+v receive=%+v", firstPush, firstReceive)
	}

	receiverPath := filepath.Join(receiverRoot, filepath.FromSlash(firstPath))
	content, err := os.ReadFile(receiverPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _, err := session.ParseFrontmatter(content)
	if err != nil {
		t.Fatal(err)
	}
	metadata.Path = receiverPath
	metadata.Tags = []string{"receiver-only"}
	if err := session.WriteMetadata(metadata); err != nil {
		t.Fatal(err)
	}

	writeSenderTranscript(t, senderRoot, firstPath, "# second version\n")
	secondPush, secondReceive := push()
	if secondPush.Transferred != 1 || secondPush.Skipped != 1 {
		t.Fatalf("second push = %+v, want one transferred and one skipped", secondPush)
	}
	if secondReceive.Merged != 1 || secondReceive.Received != 1 {
		t.Fatalf("second receive = %+v, want one merged file", secondReceive)
	}
	annotations, body := readAnnotations(t, receiverPath)
	if !reflect.DeepEqual(annotations.Tags, []string{"receiver-only"}) {
		t.Fatalf("receiver tags = %v, want receiver-only", annotations.Tags)
	}
	if body != "# second version\n" {
		t.Fatalf("receiver body = %q, want second sender version", body)
	}
}

func TestScanPending_NoPushTag(t *testing.T) {
	relPath := "codex/project/one.md"
	tests := []struct {
		name         string
		tags         []string
		handEdited   bool
		pushedBefore bool
		wantFiles    int
		wantExcluded []string
		wantRetract  []string
	}{
		{name: "untagged", wantFiles: 1},
		{name: "other tags only", tags: []string{"wiki:compiled"}, wantFiles: 1},
		{name: "tagged and never pushed", tags: []string{NoPushTag}, wantExcluded: []string{relPath}},
		{name: "hand-edited tag case", tags: []string{NoPushTag}, handEdited: true, wantExcluded: []string{relPath}},
		{name: "tagged after an earlier push", tags: []string{NoPushTag}, pushedBefore: true, wantRetract: []string{relPath}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeTaggedSenderTranscript(t, root, relPath, "# body\n", tt.tags)
			if tt.handEdited {
				path := filepath.Join(root, filepath.FromSlash(relPath))
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				content = bytes.Replace(content, []byte("- "+NoPushTag), []byte("- No-Push"), 1)
				if err := os.WriteFile(path, content, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cursor := map[string]string{}
			if tt.pushedBefore {
				cursor[relPath] = "hash-of-earlier-version"
			}

			got, err := ScanPending(root, cursor)
			if err != nil {
				t.Fatalf("ScanPending() error = %v", err)
			}
			if len(got.Files) != tt.wantFiles {
				t.Errorf("ScanPending() files = %+v, want %d", got.Files, tt.wantFiles)
			}
			if !reflect.DeepEqual(got.Excluded, tt.wantExcluded) {
				t.Errorf("ScanPending() excluded = %v, want %v", got.Excluded, tt.wantExcluded)
			}
			if !reflect.DeepEqual(got.Retract, tt.wantRetract) {
				t.Errorf("ScanPending() retract = %v, want %v", got.Retract, tt.wantRetract)
			}
			wantPresent := len(tt.wantExcluded)+len(tt.wantRetract) == 0
			if present := slices.Contains(got.AllPaths, relPath); present != wantPresent {
				t.Errorf("ScanPending() AllPaths contains %s = %v, want %v", relPath, present, wantPresent)
			}
		})
	}
}

func TestWriteTar_ProtocolFollowsRetractions(t *testing.T) {
	tests := []struct {
		name         string
		retract      []string
		wantProtocol int
	}{
		{name: "no retractions stays readable by older receivers", wantProtocol: protocolBase},
		{name: "retractions require a receiver that applies them", retract: []string{"codex/project/gone.md"}, wantProtocol: protocolRetract},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buffer bytes.Buffer
			if _, err := WriteTar(&buffer, "sender", t.TempDir(), nil, tt.retract); err != nil {
				t.Fatalf("WriteTar() error = %v", err)
			}
			reader := tar.NewReader(&buffer)
			if _, err := reader.Next(); err != nil {
				t.Fatal(err)
			}
			var manifest Manifest
			if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.Protocol != tt.wantProtocol {
				t.Errorf("manifest protocol = %d, want %d", manifest.Protocol, tt.wantProtocol)
			}
			if !reflect.DeepEqual(manifest.Retract, tt.retract) {
				t.Errorf("manifest retract = %v, want %v", manifest.Retract, tt.retract)
			}
		})
	}
}

func TestRetractFile(t *testing.T) {
	tests := []struct {
		name        string
		relPath     string
		wantRemoved bool
		wantErr     string
		// wantRemain is relative to the directory holding dest, so it can
		// name files both inside and outside the destination root.
		wantRemain string
	}{
		{name: "existing transcript", relPath: "codex/project/one.md", wantRemoved: true},
		{name: "already absent transcript", relPath: "codex/project/missing.md"},
		{name: "absent project directory", relPath: "codex/missing/one.md"},
		{name: "non-transcript file", relPath: "codex/project/notes.txt", wantErr: "non-transcript", wantRemain: "dest/codex/project/notes.txt"},
		{name: "parent traversal", relPath: "../victim.md", wantErr: "path traversal", wantRemain: "victim.md"},
		{name: "symlinked parent escape", relPath: "codex/linked/victim.md", wantErr: "escapes destination", wantRemain: "victim.md"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(base, "dest")
			writeSenderTranscript(t, dest, "codex/project/one.md", "# body\n")
			if err := os.WriteFile(filepath.Join(dest, "codex", "project", "notes.txt"), []byte("notes"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(base, "victim.md"), []byte("victim"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(base, filepath.Join(dest, "codex", "linked")); err != nil {
				t.Fatal(err)
			}

			removed, err := retractFile(dest, tt.relPath)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("retractFile() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("retractFile() error = %v, want %q", err, tt.wantErr)
			}
			if removed != tt.wantRemoved {
				t.Errorf("retractFile() removed = %v, want %v", removed, tt.wantRemoved)
			}
			if tt.wantRemoved {
				if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(tt.relPath))); !os.IsNotExist(err) {
					t.Errorf("retracted transcript still exists: %v", err)
				}
			}
			if tt.wantRemain != "" {
				if _, err := os.Stat(filepath.Join(base, filepath.FromSlash(tt.wantRemain))); err != nil {
					t.Errorf("%s should survive a rejected retraction: %v", tt.wantRemain, err)
				}
			}
		})
	}
}

// TestPushReceiveEndToEnd_NoPush covers the issue #23 acceptance flow: a
// no-push session that keeps growing never reaches the remote, one tagged
// after an earlier push is retracted, and removing the tag re-sends it.
func TestPushReceiveEndToEnd_NoPush(t *testing.T) {
	senderRoot := t.TempDir()
	receiverRoot := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "runtime-state.db")
	keepPath := "codex/project/keep.md"
	privatePath := "codex/project/private.md"
	leakedPath := "codex/project/leaked.md"
	writeSenderTranscript(t, senderRoot, keepPath, "# keep\n")
	writeTaggedSenderTranscript(t, senderRoot, privatePath, "# secret\n", []string{NoPushTag})
	writeSenderTranscript(t, senderRoot, leakedPath, "# shared before tagging\n")

	sends := 0
	failNextSend := false
	var lastReceive ReceiveSummary
	push := func(dryRun bool) (PushSummary, string, error) {
		t.Helper()
		var output bytes.Buffer
		summary, err := Push(PushOptions{
			Remote:      "receiver",
			ArchiveRoot: senderRoot,
			StateDBPath: statePath,
			SenderHost:  "sender",
			DryRun:      dryRun,
			Output:      &output,
			Send: func(writeTar func(io.Writer) (TarResult, error)) (TarResult, error) {
				sends++
				if failNextSend {
					failNextSend = false
					return TarResult{}, fmt.Errorf("ssh failed")
				}
				result, receiveSummary := pipePushToReceive(t, writeTar, receiverRoot)
				lastReceive = receiveSummary
				return result, nil
			},
		})
		return summary, output.String(), err
	}
	receiverHas := func(relPath string) bool {
		t.Helper()
		_, err := os.Stat(filepath.Join(receiverRoot, filepath.FromSlash(relPath)))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return err == nil
	}

	first, _, err := push(false)
	if err != nil {
		t.Fatalf("first push error = %v", err)
	}
	if first.Transferred != 2 || first.Excluded != 1 || receiverHas(privatePath) {
		t.Fatalf("first push = %+v, receiver has private = %v; want two sent and private excluded", first, receiverHas(privatePath))
	}

	writeTaggedSenderTranscript(t, senderRoot, privatePath, "# secret\n# more secret\n", []string{NoPushTag})
	second, _, err := push(false)
	if err != nil {
		t.Fatalf("second push error = %v", err)
	}
	if second.Transferred != 0 || second.Excluded != 1 || receiverHas(privatePath) {
		t.Fatalf("second push = %+v; want the grown private session still excluded", second)
	}

	writeTaggedSenderTranscript(t, senderRoot, leakedPath, "# shared before tagging\n# now private\n", []string{NoPushTag})
	_, dryRunOutput, err := push(true)
	if err != nil {
		t.Fatalf("dry-run push error = %v", err)
	}
	wantDryRun := "excluded: " + privatePath + "\nretract: " + leakedPath + "\n"
	if dryRunOutput != wantDryRun {
		t.Fatalf("dry-run output = %q, want %q", dryRunOutput, wantDryRun)
	}

	failNextSend = true
	if _, _, err := push(false); err == nil {
		t.Fatal("push with failing transport succeeded")
	}
	if !receiverHas(leakedPath) {
		t.Fatal("receiver lost leaked transcript although the retracting push failed")
	}
	third, _, err := push(false)
	if err != nil {
		t.Fatalf("retrying push error = %v", err)
	}
	if third.Retracted != 1 || lastReceive.Retracted != 1 || receiverHas(leakedPath) {
		t.Fatalf("retrying push = %+v receive = %+v; want leaked transcript retracted after the failed attempt", third, lastReceive)
	}
	if !receiverHas(keepPath) {
		t.Fatal("retraction removed an untagged transcript")
	}

	sendsBefore := sends
	fourth, _, err := push(false)
	if err != nil {
		t.Fatalf("fourth push error = %v", err)
	}
	if fourth.Retracted != 0 || fourth.Excluded != 2 || sends != sendsBefore {
		t.Fatalf("fourth push = %+v sends = %d; want no repeated retraction and no transport call", fourth, sends-sendsBefore)
	}

	writeTaggedSenderTranscript(t, senderRoot, privatePath, "# secret\n# more secret\n", nil)
	fifth, _, err := push(false)
	if err != nil {
		t.Fatalf("fifth push error = %v", err)
	}
	if fifth.Transferred != 1 || !receiverHas(privatePath) {
		t.Fatalf("fifth push = %+v; want the untagged session sent", fifth)
	}
	if _, body := readAnnotations(t, filepath.Join(receiverRoot, filepath.FromSlash(privatePath))); body != "# secret\n# more secret\n" {
		t.Fatalf("receiver private body = %q, want the full sender version", body)
	}
}

// tagWhenStreamed runs onFile the first time the tar stream writes the header
// for relPath. WriteTar has opened the file by then, so a rename-based write
// in onFile models `tracer tag` landing while the old inode is streamed.
type tagWhenStreamed struct {
	writer  io.Writer
	relPath string
	onFile  func()
	fired   bool
}

func (w *tagWhenStreamed) Write(p []byte) (int, error) {
	if !w.fired && bytes.Contains(p, []byte(w.relPath)) {
		w.fired = true
		w.onFile()
	}
	return w.writer.Write(p)
}

func TestPush_RetractsTranscriptTaggedWhileStreaming(t *testing.T) {
	senderRoot := t.TempDir()
	receiverRoot := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "runtime-state.db")
	relPath := "codex/project/one.md"
	senderPath := filepath.Join(senderRoot, filepath.FromSlash(relPath))
	writeSenderTranscript(t, senderRoot, relPath, "# body\n")
	tagSender := func() {
		content, err := os.ReadFile(senderPath)
		if err != nil {
			t.Fatal(err)
		}
		metadata, _, err := session.ParseFrontmatter(content)
		if err != nil {
			t.Fatal(err)
		}
		metadata.Path = senderPath
		metadata.Tags = []string{NoPushTag}
		if err := session.WriteMetadata(metadata); err != nil {
			t.Fatal(err)
		}
	}
	push := func(wrap func(io.Writer) io.Writer) PushSummary {
		t.Helper()
		summary, err := Push(PushOptions{
			Remote:      "receiver",
			ArchiveRoot: senderRoot,
			StateDBPath: statePath,
			SenderHost:  "sender",
			Send: func(writeTar func(io.Writer) (TarResult, error)) (TarResult, error) {
				result, _ := pipePushToReceive(t, func(writer io.Writer) (TarResult, error) {
					return writeTar(wrap(writer))
				}, receiverRoot)
				return result, nil
			},
		})
		if err != nil {
			t.Fatalf("Push() error = %v", err)
		}
		return summary
	}

	push(func(writer io.Writer) io.Writer {
		return &tagWhenStreamed{writer: writer, relPath: relPath, onFile: tagSender}
	})
	receiverPath := filepath.Join(receiverRoot, filepath.FromSlash(relPath))
	if _, err := os.Stat(receiverPath); err != nil {
		t.Fatalf("receiver should hold the untagged bytes streamed before the tag landed: %v", err)
	}

	second := push(func(writer io.Writer) io.Writer { return writer })
	if second.Retracted != 1 {
		t.Fatalf("second push = %+v, want the copy delivered mid-tag retracted", second)
	}
	if _, err := os.Stat(receiverPath); !os.IsNotExist(err) {
		t.Fatalf("receiver still holds transcript tagged during the earlier push: %v", err)
	}
}

func TestReceive_AppliesRetractions(t *testing.T) {
	tests := []struct {
		name          string
		retract       []string
		wantErr       string
		wantRetracted int
		wantFailed    int
		wantRemain    []string
		wantGone      []string
	}{
		{
			name:          "retracts listed transcript",
			retract:       []string{"codex/project/old.md"},
			wantRetracted: 1,
			wantRemain:    []string{"codex/project/notes.txt", "codex/project/new.md"},
			wantGone:      []string{"codex/project/old.md"},
		},
		{
			name:       "failed retraction fails the stream but keeps processing",
			retract:    []string{"codex/project/notes.txt"},
			wantErr:    "non-transcript",
			wantFailed: 1,
			wantRemain: []string{"codex/project/notes.txt", "codex/project/old.md", "codex/project/new.md"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeSenderTranscript(t, root, "codex/project/old.md", "# old\n")
			if err := os.WriteFile(filepath.Join(root, "codex", "project", "notes.txt"), []byte("notes"), 0o644); err != nil {
				t.Fatal(err)
			}
			stream := tarStream(t, Manifest{Protocol: protocolRetract, SenderHost: "sender", Count: 1, Retract: tt.retract}, map[string][]byte{
				"codex/project/new.md": transcriptBytes(t, "new", "", nil, "# new\n"),
			})

			summary, err := Receive(bytes.NewReader(stream), root)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Receive() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Receive() error = %v, want %q", err, tt.wantErr)
			}
			if summary.Retracted != tt.wantRetracted || summary.Failed != tt.wantFailed || summary.Created != 1 {
				t.Errorf("Receive() summary = %+v, want retracted=%d failed=%d created=1", summary, tt.wantRetracted, tt.wantFailed)
			}
			for _, relPath := range tt.wantRemain {
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relPath))); err != nil {
					t.Errorf("%s should remain: %v", relPath, err)
				}
			}
			for _, relPath := range tt.wantGone {
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relPath))); !os.IsNotExist(err) {
					t.Errorf("%s should be retracted: %v", relPath, err)
				}
			}
		})
	}
}
