package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracer-ai/tracer-cli/pkg/config"
)

func TestWriteDefaultConfigCreatesFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "nested", "tracer", "config.toml")

	if err := writeDefaultConfig(configPath, false); err != nil {
		t.Fatalf("writeDefaultConfig() error = %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read config file: %v", err)
	}
	if string(data) != config.DefaultTemplate() {
		t.Fatal("config file content does not match default template")
	}
}

func TestWriteDefaultConfigRejectsExistingFileWithoutForce(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("existing"), 0o644); err != nil {
		t.Fatalf("failed to seed config file: %v", err)
	}

	err := writeDefaultConfig(configPath, false)
	if err == nil {
		t.Fatal("expected error for existing file without --force")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWriteDefaultConfigOverwritesWithForce(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("existing"), 0o644); err != nil {
		t.Fatalf("failed to seed config file: %v", err)
	}

	if err := writeDefaultConfig(configPath, true); err != nil {
		t.Fatalf("writeDefaultConfig(..., true) error = %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read overwritten config file: %v", err)
	}
	if string(data) != config.DefaultTemplate() {
		t.Fatal("forced overwrite did not write default template")
	}
}
