package session

import (
	"bufio"
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A contended lock must stay unacquired for lockBlockedWindow; a released
// one must be acquired within lockAcquireTimeout. The timeout is generous
// because -race and a loaded machine slow goroutine scheduling.
const (
	lockBlockedWindow  = 100 * time.Millisecond
	lockAcquireTimeout = 5 * time.Second
)

// acquireTranscriptLockAsync takes the transcript lock on a goroutine and
// delivers its unlock func once the lock is held.
func acquireTranscriptLockAsync(t *testing.T, path string) <-chan func() {
	t.Helper()
	acquired := make(chan func(), 1)
	go func() {
		unlock, err := LockTranscript(path)
		if err != nil {
			t.Errorf("LockTranscript(%s) error = %v", path, err)
			unlock = func() {}
		}
		acquired <- unlock
	}()
	return acquired
}

func TestLockTranscript_Exclusion(t *testing.T) {
	tests := []struct {
		name        string
		second      string
		wantBlocked bool
	}{
		{name: "same transcript", second: "codex/project/one.md", wantBlocked: true},
		{name: "sibling transcript in the same directory", second: "codex/project/two.md", wantBlocked: true},
		{name: "transcript in another directory", second: "codex/other/three.md", wantBlocked: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"codex/project", "codex/other"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			unlockFirst, err := LockTranscript(filepath.Join(root, "codex/project/one.md"))
			if err != nil {
				t.Fatalf("LockTranscript() error = %v", err)
			}
			releaseFirst := sync.OnceFunc(unlockFirst)
			t.Cleanup(releaseFirst)

			acquired := acquireTranscriptLockAsync(t, filepath.Join(root, tt.second))
			if tt.wantBlocked {
				select {
				case unlock := <-acquired:
					unlock()
					t.Fatal("second lock was acquired while the first was held")
				case <-time.After(lockBlockedWindow):
				}
				releaseFirst()
			}
			select {
			case unlock := <-acquired:
				unlock()
			case <-time.After(lockAcquireTimeout):
				t.Fatal("second lock was not acquired")
			}
			releaseFirst()

			err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr == nil && !entry.IsDir() {
					t.Errorf("locking left a file in the archive: %s", path)
				}
				return walkErr
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestLockTranscript_HelperProcess is not a real test. It holds the lock on
// TRACER_TEST_LOCK_PATH in a child process until stdin closes, so
// TestLockTranscript_ExcludesOtherProcess can contend with another process.
func TestLockTranscript_HelperProcess(t *testing.T) {
	path := os.Getenv("TRACER_TEST_LOCK_PATH")
	if path == "" {
		return
	}
	unlock, err := LockTranscript(path)
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(2)
	}
	_, _ = os.Stdout.WriteString("locked\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
	unlock()
	os.Exit(0)
}

func TestLockTranscript_ExcludesOtherProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one.md")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockTranscript_HelperProcess$")
	cmd.Env = append(os.Environ(), "TRACER_TEST_LOCK_PATH="+path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Why a cleanup: if an assertion fails, closing stdin still lets the
	// child unlock and exit instead of outliving the test.
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	scanner := bufio.NewScanner(stdout)
	locked := false
	for !locked && scanner.Scan() {
		locked = scanner.Text() == "locked"
	}
	if !locked {
		t.Fatalf("helper process did not take the lock: %s", stderr.String())
	}

	acquired := acquireTranscriptLockAsync(t, path)
	select {
	case unlock := <-acquired:
		unlock()
		t.Fatal("lock was acquired while another process held it")
	case <-time.After(lockBlockedWindow):
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case unlock := <-acquired:
		unlock()
	case <-time.After(lockAcquireTimeout):
		t.Fatal("lock was not acquired after the other process released it")
	}
}

func writeArchiveTestTranscript(t *testing.T, root, name string, metadata Metadata) string {
	t.Helper()
	path := filepath.Join(root, name+".md")
	frontmatter, err := RenderFrontmatter(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(frontmatter+"# Body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScanArchives_MultipleRootsAndSort(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	writeArchiveTestTranscript(t, root, "older", Metadata{SessionID: "older", Models: []string{}, Ended: "2026-07-12T10:00:00Z"})
	writeArchiveTestTranscript(t, extra, "newer", Metadata{SessionID: "newer", Models: []string{}, Ended: "2026-07-13T10:00:00Z"})
	if err := os.WriteFile(filepath.Join(root, "legacy.md"), []byte("# legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions, err := ScanArchives([]string{root, root, extra})
	if err != nil {
		t.Fatalf("ScanArchives() error = %v", err)
	}
	if len(sessions) != 2 || sessions[0].SessionID != "newer" || sessions[1].SessionID != "older" {
		t.Fatalf("sessions = %+v", sessions)
	}
}

func TestScanArchives_ResolvesAndDeduplicatesSymlinkRoots(t *testing.T) {
	root := t.TempDir()
	symlink := filepath.Join(t.TempDir(), "archive-link")
	if err := os.Symlink(root, symlink); err != nil {
		t.Fatal(err)
	}
	wantPath := writeArchiveTestTranscript(t, root, "session", Metadata{SessionID: "session", Models: []string{}})

	sessions, err := ScanArchives([]string{symlink, root})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Path != wantPath {
		t.Fatalf("ScanArchives() = %+v, want one resolved transcript at %s", sessions, wantPath)
	}
	metadata, err := ResolveTranscriptStrict([]string{symlink}, "session")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Path != wantPath {
		t.Fatalf("ResolveTranscriptStrict() path = %q, want %q", metadata.Path, wantPath)
	}
}

func TestResolveTranscriptStrict_RejectsUnresolvableRoot(t *testing.T) {
	loop := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTranscriptStrict([]string{loop}, "session"); err == nil {
		t.Fatal("ResolveTranscriptStrict() error = nil, want root resolution error")
	}
	if _, err := ResolveTranscript([]string{loop}, "session"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("ResolveTranscript() error = %v, want tolerant not-found result", err)
	}
}

func TestResolveTranscript_Ambiguous(t *testing.T) {
	root := t.TempDir()
	writeArchiveTestTranscript(t, root, "one", Metadata{SessionID: "same", Models: []string{}})
	writeArchiveTestTranscript(t, root, "two", Metadata{SessionID: "same", Models: []string{}})
	_, err := ResolveTranscript([]string{root}, "same")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ResolveTranscript() error = %v", err)
	}
}

func TestWriteMetadata_PreservesBody(t *testing.T) {
	root := t.TempDir()
	path := writeArchiveTestTranscript(t, root, "session", Metadata{SessionID: "session", Models: []string{}})
	metadata, err := ResolveTranscript([]string{root}, "session")
	if err != nil {
		t.Fatal(err)
	}
	metadata.Outcome = "done"
	metadata.Tags = []string{"gold"}
	if err := WriteMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, body, err := ParseFrontmatter(content)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Outcome != "done" || len(parsed.Tags) != 1 || string(body) != "# Body\n" {
		t.Fatalf("metadata = %+v, body = %q", parsed, body)
	}
}

func TestWriteMetadata_PreservesConcurrentDerivedMetadataChange(t *testing.T) {
	root := t.TempDir()
	path := writeArchiveTestTranscript(t, root, "session", Metadata{
		SessionID: "session",
		Title:     "original",
		Models:    []string{},
	})
	stale, err := ResolveTranscript([]string{root}, "session")
	if err != nil {
		t.Fatal(err)
	}

	current := stale
	current.Title = "refreshed while annotation was pending"
	frontmatter, err := RenderFrontmatter(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(frontmatter+"# refreshed body\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stale.Outcome = "done"
	if err := WriteMetadata(stale); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	metadata, body, err := ParseFrontmatter(content)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Title != current.Title || string(body) != "# refreshed body\n" {
		t.Fatalf("derived metadata/body regressed: metadata=%+v body=%q", metadata, body)
	}
	if metadata.Outcome != "done" {
		t.Fatalf("outcome = %q, want done", metadata.Outcome)
	}
}

func TestWriteMetadata_WaitsForTranscriptLock(t *testing.T) {
	root := t.TempDir()
	path := writeArchiveTestTranscript(t, root, "session", Metadata{SessionID: "session", Models: []string{}})
	metadata, err := ResolveTranscript([]string{root}, "session")
	if err != nil {
		t.Fatal(err)
	}
	metadata.Outcome = "done"

	unlock, err := LockTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(unlock)
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() { done <- WriteMetadata(metadata) }()
	select {
	case err := <-done:
		t.Fatalf("WriteMetadata() returned %v while the transcript lock was held", err)
	case <-time.After(lockBlockedWindow):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WriteMetadata() error = %v", err)
		}
	case <-time.After(lockAcquireTimeout):
		t.Fatal("WriteMetadata() did not finish after the lock was released")
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	written, _, err := ParseFrontmatter(content)
	if err != nil {
		t.Fatal(err)
	}
	if written.Outcome != "done" {
		t.Errorf("outcome = %q, want done", written.Outcome)
	}
}
