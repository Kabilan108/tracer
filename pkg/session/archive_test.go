package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
