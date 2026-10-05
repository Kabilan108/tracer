package factory

import (
	"reflect"
	"testing"
)

func TestRegistry_OnlyClaudeAndCodexRegistered(t *testing.T) {
	registry := GetRegistry()

	ids := registry.ListIDs()
	want := []string{"claude", "codex"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ListIDs() = %v, want %v", ids, want)
	}

	if _, err := registry.Get("cursor"); err == nil {
		t.Fatal("Get(cursor) should fail because cursor provider is not registered")
	}
	if _, err := registry.Get("gemini"); err == nil {
		t.Fatal("Get(gemini) should fail because gemini provider is not registered")
	}
	if _, err := registry.Get("droid"); err == nil {
		t.Fatal("Get(droid) should fail because droid provider is not registered")
	}
}

func TestRegistry_GetUnknownListsAvailableIDs(t *testing.T) {
	_, err := GetRegistry().Get("codex-cli")
	if err == nil {
		t.Fatal("Get(codex-cli) should fail because registry IDs are claude and codex")
	}
	want := "provider 'codex-cli' not found; available providers: claude, codex"
	if err.Error() != want {
		t.Errorf("Get(codex-cli) error = %q, want %q", err, want)
	}
}
