// Package config provides configuration management for the Tracer CLI.
// Configuration is loaded with the following priority (highest to lowest):
//  1. CLI flags
//  2. User-level config: ~/.config/tracer/config.toml
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

var ignoredLegacyKeys = map[string]struct{}{
	"version_check":         {},
	"version_check.enabled": {},
}

const (
	// ConfigFileName is the name of the configuration file.
	ConfigFileName = "config.toml"
)

const defaultConfigTemplate = `# Tracer CLI Configuration
#
# This file configures Tracer globally for your user account.
# Path: ~/.config/tracer/config.toml

[archive]
# Global archive root for generated markdown.
# Sessions are written as: provider/project/session.md
# Default: ~/.local/share/tracer/archive
# root_dir = "~/.local/share/tracer/archive"

[logging]
# Optional debug output directory.
# Default: ~/.local/state/tracer/debug
# debug_dir = "~/.local/state/tracer/debug"

# Write log output to file (default: false)
# log = true

# Enable debug-level logs (requires console or log) (default: false)
# debug = true

# Print logs to stdout (default: false)
# console = true

# Suppress non-error output (default: false)
# silent = true

[local_sync]
# Use local timezone for file names and timestamps (default: false)
# local_time_zone = true

[ingest]
# Limit sync/watch processing to these providers.
# Empty means all registered providers.
# enabled_providers = ["claude", "codex"]

# Skip project names entirely.
# exclude_projects = ["scratch-playground"]

# Skip project paths matching filepath globs.
# exclude_path_globs = ["/tmp/*", "/home/user/archive/*"]
`

// Config represents the complete CLI configuration.
type Config struct {
	LocalSync LocalSyncConfig `toml:"local_sync"`
	Logging   LoggingConfig   `toml:"logging"`
	Archive   ArchiveConfig   `toml:"archive"`
	Ingest    IngestConfig    `toml:"ingest"`
}

// LocalSyncConfig holds local file sync settings.
type LocalSyncConfig struct {
	LocalTimeZone *bool `toml:"local_time_zone"`
}

// LoggingConfig holds logging settings.
type LoggingConfig struct {
	DebugDir string `toml:"debug_dir"`
	Console  *bool  `toml:"console"`
	Log      *bool  `toml:"log"`
	Debug    *bool  `toml:"debug"`
	Silent   *bool  `toml:"silent"`
}

// ArchiveConfig configures the global markdown archive output root.
type ArchiveConfig struct {
	RootDir string `toml:"root_dir"`
}

// IngestConfig controls provider selection and exclusion behavior for sync/watch modes.
type IngestConfig struct {
	EnabledProviders []string `toml:"enabled_providers"`
	ExcludeProjects  []string `toml:"exclude_projects"`
	ExcludePathGlobs []string `toml:"exclude_path_globs"`
}

// CLIOverrides holds CLI flag values that override config file settings.
type CLIOverrides struct {
	OutputDir     string
	LocalTimeZone bool

	DebugDir string
	Console  bool
	Log      bool
	Debug    bool
	Silent   bool
}

// ConfigValidationResult holds the result of validating a config file.
type ConfigValidationResult struct {
	Path        string
	Exists      bool
	ValidTOML   bool
	ParseError  string
	UnknownKeys []string
}

// Load reads the user config and applies CLI overrides.
func Load(cliOverrides *CLIOverrides) (*Config, error) {
	return LoadPath("", cliOverrides)
}

// LoadPath reads a config from an explicit path or the default user config path.
func LoadPath(configPath string, cliOverrides *CLIOverrides) (*Config, error) {
	cfg := &Config{}

	path := configPath
	if path == "" {
		path = getUserConfigPath()
	}

	if path != "" {
		if err := loadTOMLFile(path, cfg); err != nil {
			if !os.IsNotExist(err) {
				return cfg, fmt.Errorf("failed to load config %s: %w", path, err)
			}
		} else {
			slog.Debug("Loaded config", "path", path)
		}
	}

	if cliOverrides != nil {
		applyCLIOverrides(cfg, cliOverrides)
	}

	return cfg, nil
}

// DefaultTemplate returns a commented reference config.
func DefaultTemplate() string {
	return defaultConfigTemplate
}

// GetUserConfigPath returns ~/.config/tracer/config.toml.
func GetUserConfigPath() string {
	return getUserConfigPath()
}

func getUserConfigPath() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			slog.Debug("Could not determine home directory", "error", err)
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "tracer", ConfigFileName)
}

func loadTOMLFile(path string, cfg *Config) error {
	_, err := toml.DecodeFile(path, cfg)
	return err
}

// ValidateConfigFile checks a config file for TOML validity and unknown keys.
func ValidateConfigFile(path string) ConfigValidationResult {
	result := ConfigValidationResult{Path: path}

	if path == "" {
		return result
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return result
	}
	result.Exists = true

	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		result.ParseError = err.Error()
		return result
	}
	result.ValidTOML = true

	undecoded := md.Undecoded()
	unknownSections := make(map[string]bool)
	for _, key := range undecoded {
		if isIgnoredLegacyKey(key) {
			continue
		}
		if len(key) == 1 {
			unknownSections[key[0]] = true
		}
	}
	for _, key := range undecoded {
		if isIgnoredLegacyKey(key) {
			continue
		}
		if len(key) > 1 && unknownSections[key[0]] {
			continue
		}
		result.UnknownKeys = append(result.UnknownKeys, key.String())
	}

	return result
}

func isIgnoredLegacyKey(key toml.Key) bool {
	_, ok := ignoredLegacyKeys[key.String()]
	return ok
}

func applyCLIOverrides(cfg *Config, o *CLIOverrides) {
	if o.LocalTimeZone {
		enabled := true
		cfg.LocalSync.LocalTimeZone = &enabled
	}
	if o.OutputDir != "" {
		cfg.Archive.RootDir = o.OutputDir
	}
	if o.DebugDir != "" {
		cfg.Logging.DebugDir = o.DebugDir
	}
	if o.Console {
		enabled := true
		cfg.Logging.Console = &enabled
	}
	if o.Log {
		enabled := true
		cfg.Logging.Log = &enabled
	}
	if o.Debug {
		enabled := true
		cfg.Logging.Debug = &enabled
	}
	if o.Silent {
		enabled := true
		cfg.Logging.Silent = &enabled
	}

}

func (c *Config) GetOutputDir() string {
	return c.Archive.RootDir
}

func (c *Config) GetArchiveRoot() string {
	return c.Archive.RootDir
}

func (c *Config) IsConsoleEnabled() bool {
	if c.Logging.Console != nil {
		return *c.Logging.Console
	}
	return false
}

func (c *Config) IsLogEnabled() bool {
	if c.Logging.Log != nil {
		return *c.Logging.Log
	}
	return false
}

func (c *Config) IsDebugEnabled() bool {
	if c.Logging.Debug != nil {
		return *c.Logging.Debug
	}
	return false
}

func (c *Config) IsSilentEnabled() bool {
	if c.Logging.Silent != nil {
		return *c.Logging.Silent
	}
	return false
}

func (c *Config) GetDebugDir() string {
	return c.Logging.DebugDir
}

func (c *Config) IsLocalTimeZoneEnabled() bool {
	if c.LocalSync.LocalTimeZone != nil {
		return *c.LocalSync.LocalTimeZone
	}
	return false
}

func (c *Config) GetProviderCmd(providerID string) string {
	_ = providerID
	return ""
}

func (c *Config) GetEnabledProviders() []string {
	if len(c.Ingest.EnabledProviders) == 0 {
		return nil
	}
	result := make([]string, 0, len(c.Ingest.EnabledProviders))
	for _, providerID := range c.Ingest.EnabledProviders {
		trimmed := strings.TrimSpace(strings.ToLower(providerID))
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (c *Config) IsProjectExcluded(projectPath string) bool {
	base := strings.ToLower(filepath.Base(projectPath))

	for _, project := range c.Ingest.ExcludeProjects {
		if strings.ToLower(strings.TrimSpace(project)) == base {
			return true
		}
	}

	cleaned := filepath.Clean(projectPath)
	for _, glob := range c.Ingest.ExcludePathGlobs {
		pattern := strings.TrimSpace(glob)
		if pattern == "" {
			continue
		}
		matched, err := filepath.Match(pattern, cleaned)
		if err == nil && matched {
			return true
		}
	}

	return false
}
