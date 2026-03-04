package engine

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStateStore_UpsertAndGet(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	store, err := OpenStateStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStateStore() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	now := time.Now().UTC().Round(time.Second)
	input := SessionState{
		ProviderID:  "claude",
		SessionID:   "session-1",
		ContentHash: "abc123",
		OutputPath:  "/tmp/session-1.md",
		UpdatedAt:   now,
	}
	if err := store.Upsert(input); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	got, ok, err := store.Get("claude", "session-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !ok {
		t.Fatal("Get() returned ok=false")
	}
	if got.ProviderID != input.ProviderID {
		t.Fatalf("ProviderID = %q, want %q", got.ProviderID, input.ProviderID)
	}
	if got.SessionID != input.SessionID {
		t.Fatalf("SessionID = %q, want %q", got.SessionID, input.SessionID)
	}
	if got.ContentHash != input.ContentHash {
		t.Fatalf("ContentHash = %q, want %q", got.ContentHash, input.ContentHash)
	}
	if got.OutputPath != input.OutputPath {
		t.Fatalf("OutputPath = %q, want %q", got.OutputPath, input.OutputPath)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt should not be zero")
	}
}
