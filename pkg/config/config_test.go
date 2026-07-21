package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

[ingest]
enabled_providers = [" Claude ", "CODEX", ""]
exclude_projects = ["skip-me"]
exclude_path_globs = ["/tmp/*"]
`)

	cfg, err := LoadPath(path, &CLIOverrides{
		OutputDir:     "/override/archive",
		DebugDir:      "/override/debug",
		LocalTimeZone: true,
		Console:       true,
		Log:           true,
		Debug:         true,
		Silent:        true,
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

func TestLoadPath_AdditionalArchiveRoots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[archive]\nadditional_roots = [\"/archive/one\", \"/archive/two\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadPath(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/archive/one", "/archive/two"}
	if got := cfg.GetAdditionalArchiveRoots(); !reflect.DeepEqual(got, want) {
		t.Fatalf("GetAdditionalArchiveRoots() = %v, want %v", got, want)
	}
}

func TestLoadPath_AnnotatableRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeConfigFile(t, t.TempDir(), fmt.Sprintf(`
[archive]
additional_roots = [%q]
annotatable_roots = ["~/ingest"]
`, filepath.Join(home, "ingest")))
	cfg, err := LoadPath(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(home, "ingest")}
	if got := cfg.GetAnnotatableRoots(); !reflect.DeepEqual(got, want) {
		t.Fatalf("GetAnnotatableRoots() = %v, want %v", got, want)
	}
	result := ValidateConfigFile(path)
	if !result.ValidTOML || len(result.UnknownKeys) != 0 {
		t.Fatalf("ValidateConfigFile() = %+v, want valid known annotatable_roots key", result)
	}
}

func TestLoadPath_AnnotatableRootMustBeAdditional(t *testing.T) {
	path := writeConfigFile(t, t.TempDir(), `
[archive]
additional_roots = ["/archive/read-only"]
annotatable_roots = ["/archive/writable"]
unknown_key = true
`)
	if _, err := LoadPath(path, nil); err != nil {
		t.Fatalf("LoadPath() error = %v, subset validation must be deferred", err)
	}
	result := ValidateConfigFile(path)
	if !result.ValidTOML || result.ParseError != "" || !strings.Contains(result.ValidationError, "must also be listed") {
		t.Fatalf("ValidateConfigFile() = %+v, want annotatable root validation error", result)
	}
	if !reflect.DeepEqual(result.UnknownKeys, []string{"archive.unknown_key"}) {
		t.Fatalf("ValidateConfigFile().UnknownKeys = %v, want scan to continue", result.UnknownKeys)
	}
}

func TestLoadPath_RejectsEmptyArchiveRootEntries(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "additional root", content: "[archive]\nadditional_roots = [\"\"]\n", want: "archive.additional_roots[0]"},
		{name: "annotatable root", content: "[archive]\nannotatable_roots = [\"  \"]\n", want: "archive.annotatable_roots[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigFile(t, t.TempDir(), tt.content)
			_, err := LoadPath(path, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadPath() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadPath_PushRemotes(t *testing.T) {
	path := writeConfigFile(t, t.TempDir(), `
[[push.remotes]]
name = "sietch"
host = "sietch"
dest = "/vault/userdata/tracer-ingest/jacurutu"
`)
	cfg, err := LoadPath(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []PushRemote{{Name: "sietch", Host: "sietch", Dest: "/vault/userdata/tracer-ingest/jacurutu"}}
	if got := cfg.GetPushRemotes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("GetPushRemotes() = %#v, want %#v", got, want)
	}
	result := ValidateConfigFile(path)
	if !result.ValidTOML || len(result.UnknownKeys) != 0 {
		t.Fatalf("ValidateConfigFile() = %+v, want valid known push keys", result)
	}
}

func TestValidatePushRemote(t *testing.T) {
	tests := []struct {
		name       string
		remotes    []PushRemote
		selectName string
		want       string
	}{
		{name: "name required", remotes: []PushRemote{{Host: "host", Dest: "/dest"}}, want: "must match"},
		{name: "name cannot start with option", remotes: []PushRemote{{Name: "-remote", Host: "host", Dest: "/dest"}}, selectName: "-remote", want: "must match"},
		{name: "name rejects whitespace", remotes: []PushRemote{{Name: "bad name", Host: "host", Dest: "/dest"}}, selectName: "bad name", want: "must match"},
		{name: "host required", remotes: []PushRemote{{Name: "remote", Dest: "/dest"}}, selectName: "remote", want: "host"},
		{name: "host rejects ssh option", remotes: []PushRemote{{Name: "remote", Host: "-oProxyCommand=evil", Dest: "/dest"}}, selectName: "remote", want: "host"},
		{name: "host rejects shell metacharacter", remotes: []PushRemote{{Name: "remote", Host: "host;evil", Dest: "/dest"}}, selectName: "remote", want: "host"},
		{name: "dest required", remotes: []PushRemote{{Name: "remote", Host: "host"}}, selectName: "remote", want: "absolute path"},
		{name: "dest must be absolute", remotes: []PushRemote{{Name: "remote", Host: "host", Dest: "relative"}}, selectName: "remote", want: "absolute path"},
		{name: "dest rejects whitespace", remotes: []PushRemote{{Name: "remote", Host: "host", Dest: "/bad path"}}, selectName: "remote", want: "absolute path"},
		{name: "dest rejects shell metacharacter", remotes: []PushRemote{{Name: "remote", Host: "host", Dest: "/dest;evil"}}, selectName: "remote", want: "absolute path"},
		{
			name: "names unique",
			remotes: []PushRemote{
				{Name: "remote", Host: "one", Dest: "/one"},
				{Name: "remote", Host: "two", Dest: "/two"},
			},
			selectName: "remote",
			want:       "duplicated",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (&Config{Push: PushConfig{Remotes: tt.remotes}}).ValidatePushRemote(tt.selectName)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidatePushRemote() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadPath_DoesNotValidatePushRemotes(t *testing.T) {
	path := writeConfigFile(t, t.TempDir(), `
[[push.remotes]]
name = "remote"
host = "-oProxyCommand=evil"
dest = "/bad path"
`)
	if _, err := LoadPath(path, nil); err != nil {
		t.Fatalf("LoadPath() error = %v, push validation must be deferred", err)
	}
	result := ValidateConfigFile(path)
	if !result.ValidTOML || len(result.UnknownKeys) != 0 {
		t.Fatalf("ValidateConfigFile() = %+v, want decoded known keys", result)
	}
}

func TestValidatePushRemote_IgnoresUnusedHostAndDest(t *testing.T) {
	cfg := &Config{Push: PushConfig{Remotes: []PushRemote{
		{Name: "unused", Host: "-oProxyCommand=evil", Dest: "/bad path"},
		{Name: "selected", Host: "safe-host", Dest: "/safe/path"},
	}}}
	remote, err := cfg.ValidatePushRemote("selected")
	if err != nil {
		t.Fatalf("ValidatePushRemote() error = %v", err)
	}
	if remote.Name != "selected" {
		t.Fatalf("ValidatePushRemote() = %+v, want selected remote", remote)
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
