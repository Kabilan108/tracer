package transfer

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
	manifestData := []byte(fmt.Sprintf(`{"protocol":%d,"sender_host":%q,"count":%d}`, manifest.Protocol, manifest.SenderHost, manifest.Count))
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
	stream := tarStream(t, Manifest{Protocol: 2, SenderHost: "sender"}, nil)
	_, err := Receive(bytes.NewReader(stream), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "upgrade tracer") {
		t.Fatalf("Receive() error = %v, want upgrade tracer", err)
	}
}

func writeSenderTranscript(t *testing.T, root, relPath, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, transcriptBytes(t, strings.TrimSuffix(filepath.Base(relPath), ".md"), "", nil, body), 0o644); err != nil {
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
