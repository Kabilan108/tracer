package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfigFile(t *testing.T, dir string, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestGetUserConfigPath(t *testing.T) {
	origXDG := os.Getenv("XDG_CONFIG_HOME")
	origHOME := os.Getenv("HOME")
	defer func() {
		_ = os.Setenv("XDG_CONFIG_HOME", origXDG)
		_ = os.Setenv("HOME", origHOME)
	}()

	t.Run("uses XDG_CONFIG_HOME when set", func(t *testing.T) {
		xdg := t.TempDir()
		if err := os.Setenv("XDG_CONFIG_HOME", xdg); err != nil {
			t.Fatalf("set XDG_CONFIG_HOME: %v", err)
		}

		got := GetUserConfigPath()
		want := filepath.Join(xdg, "tracer", ConfigFileName)
		if got != want {
			t.Fatalf("GetUserConfigPath() = %q, want %q", got, want)
		}
	})

	t.Run("falls back to HOME/.config", func(t *testing.T) {
		home := t.TempDir()
		if err := os.Setenv("XDG_CONFIG_HOME", ""); err != nil {
			t.Fatalf("clear XDG_CONFIG_HOME: %v", err)
		}
		if err := os.Setenv("HOME", home); err != nil {
			t.Fatalf("set HOME: %v", err)
		}

		got := GetUserConfigPath()
		want := filepath.Join(home, ".config", "tracer", ConfigFileName)
		if got != want {
			t.Fatalf("GetUserConfigPath() = %q, want %q", got, want)
		}
	})
}

func TestLoadPathAndOverrides(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigFile(t, dir, `
[archive]
root_dir = "/tmp/archive"

[logging]
debug_dir = "/tmp/debug"
console = false
log = false
debug = false
silent = false

[local_sync]
local_time_zone = false

[telemetry]
endpoint = "localhost:4317"
service_name = "from-config"
prompts = true

[ingest]
enabled_providers = [" Claude ", "CODEX", ""]
exclude_projects = ["skip-me"]
exclude_path_globs = ["/tmp/*"]
`)

	cfg, err := LoadPath(path, &CLIOverrides{
		OutputDir:            "/override/archive",
		DebugDir:             "/override/debug",
		LocalTimeZone:        true,
		Console:              true,
		Log:                  true,
		Debug:                true,
		Silent:               true,
		TelemetryEndpoint:    "collector:4317",
		TelemetryServiceName: "override-service",
	})
	if err != nil {
		t.Fatalf("LoadPath() error = %v", err)
	}

	if got := cfg.GetArchiveRoot(); got != "/override/archive" {
		t.Fatalf("GetArchiveRoot() = %q, want %q", got, "/override/archive")
	}
	if got := cfg.GetDebugDir(); got != "/override/debug" {
		t.Fatalf("GetDebugDir() = %q, want %q", got, "/override/debug")
	}
	if !cfg.IsLocalTimeZoneEnabled() {
		t.Fatal("IsLocalTimeZoneEnabled() = false, want true")
	}
	if !cfg.IsConsoleEnabled() || !cfg.IsLogEnabled() || !cfg.IsDebugEnabled() || !cfg.IsSilentEnabled() {
		t.Fatal("expected logging flags from overrides to be enabled")
	}
	if got := cfg.GetTelemetryEndpoint(); got != "collector:4317" {
		t.Fatalf("GetTelemetryEndpoint() = %q, want %q", got, "collector:4317")
	}
	if got := cfg.GetTelemetryServiceName(); got != "override-service" {
		t.Fatalf("GetTelemetryServiceName() = %q, want %q", got, "override-service")
	}

	providers := cfg.GetEnabledProviders()
	if len(providers) != 2 || providers[0] != "claude" || providers[1] != "codex" {
		t.Fatalf("GetEnabledProviders() = %#v, want [claude codex]", providers)
	}
	if !cfg.IsProjectExcluded("/home/user/skip-me") {
		t.Fatal("IsProjectExcluded() should match excluded project name")
	}
	if !cfg.IsProjectExcluded("/tmp/project") {
		t.Fatal("IsProjectExcluded() should match excluded path glob")
	}
}

func TestLoadPathMissingFile(t *testing.T) {
	cfg, err := LoadPath(filepath.Join(t.TempDir(), "missing.toml"), nil)
	if err != nil {
		t.Fatalf("LoadPath() missing file error = %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadPath() returned nil config")
	}
}

func TestValidateConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigFile(t, dir, `
[archive]
root_dir = "/tmp/archive"

[unknown_section]
foo = "bar"
`)

	result := ValidateConfigFile(path)
	if !result.Exists {
		t.Fatal("ValidateConfigFile().Exists = false, want true")
	}
	if !result.ValidTOML {
		t.Fatalf("ValidateConfigFile().ValidTOML = false, parse error: %s", result.ParseError)
	}
	if len(result.UnknownKeys) == 0 {
		t.Fatal("ValidateConfigFile().UnknownKeys should include unknown_section")
	}
}

func TestValidateConfigFile_IgnoresLegacyRemovedKeys(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigFile(t, dir, `
[version_check]
enabled = false

[telemetry]
prompts = false
`)

	result := ValidateConfigFile(path)
	if !result.Exists {
		t.Fatal("ValidateConfigFile().Exists = false, want true")
	}
	if !result.ValidTOML {
		t.Fatalf("ValidateConfigFile().ValidTOML = false, parse error: %s", result.ParseError)
	}
	if len(result.UnknownKeys) != 0 {
		t.Fatalf("ValidateConfigFile().UnknownKeys = %#v, want none", result.UnknownKeys)
	}
}

func TestDefaultTemplate(t *testing.T) {
	template := DefaultTemplate()
	if template == "" {
		t.Fatal("DefaultTemplate() returned empty template")
	}
}
