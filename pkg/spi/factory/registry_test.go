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
