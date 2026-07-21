package cmd

import (
	"io"
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

func TestPrintConfigResult_ReportsValidationErrorSeparately(t *testing.T) {
	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = originalStdout })

	printConfigResult("User config", config.ConfigValidationResult{
		Path:            "/tmp/config.toml",
		Exists:          true,
		ValidTOML:       true,
		ValidationError: "archive roots are inconsistent",
	})
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	text := string(output)
	if !strings.Contains(text, "invalid configuration") || strings.Contains(text, "invalid TOML") {
		t.Fatalf("printConfigResult() output = %q", text)
	}
}
