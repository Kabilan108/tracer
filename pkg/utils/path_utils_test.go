package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}

	got := ExpandTilde("~/tmp")
	want := filepath.Join(home, "tmp")
	if got != want {
		t.Fatalf("ExpandTilde() = %q, want %q", got, want)
	}
}

func TestNewOutputPathConfig(t *testing.T) {
	archiveDir := filepath.Join(t.TempDir(), "archive")
	debugDir := filepath.Join(t.TempDir(), "debug")

	cfg, err := NewOutputPathConfig(archiveDir, debugDir)
	if err != nil {
		t.Fatalf("NewOutputPathConfig() error = %v", err)
	}

	if cfg.BaseDir == "" || cfg.DebugBaseDir == "" {
		t.Fatal("expected BaseDir and DebugBaseDir to be set")
	}
	if !filepath.IsAbs(cfg.BaseDir) || !filepath.IsAbs(cfg.DebugBaseDir) {
		t.Fatal("expected BaseDir and DebugBaseDir to be absolute paths")
	}
}

func TestOutputPathConfigMethods_Defaults(t *testing.T) {
	cfg := &OutputPathConfig{}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}

	expectedHistory := filepath.Join(home, ".local", "share", "tracer", "archive")
	if got := cfg.GetHistoryDir(); got != expectedHistory {
		t.Fatalf("GetHistoryDir() = %q, want %q", got, expectedHistory)
	}

	expectedState := filepath.Join(home, ".local", "state", "tracer")
	if got := cfg.GetTracerDir(); got != expectedState {
		t.Fatalf("GetTracerDir() = %q, want %q", got, expectedState)
	}

	expectedDebug := filepath.Join(expectedState, DEBUG_DIR)
	if got := cfg.GetDebugDir(); got != expectedDebug {
		t.Fatalf("GetDebugDir() = %q, want %q", got, expectedDebug)
	}

	expectedLog := filepath.Join(expectedDebug, DEBUG_LOG_FILE)
	if got := cfg.GetLogPath(); got != expectedLog {
		t.Fatalf("GetLogPath() = %q, want %q", got, expectedLog)
	}

	expectedStats := filepath.Join(expectedState, STATISTICS_FILE)
	if got := cfg.GetStatisticsPath(); got != expectedStats {
		t.Fatalf("GetStatisticsPath() = %q, want %q", got, expectedStats)
	}

	expectedDB := filepath.Join(expectedState, RUNTIME_STATE_DB_FILE)
	if got := cfg.GetRuntimeStateDBPath(); got != expectedDB {
		t.Fatalf("GetRuntimeStateDBPath() = %q, want %q", got, expectedDB)
	}
}

func TestEnsureDirectories(t *testing.T) {
	archiveDir := filepath.Join(t.TempDir(), "archive")
	stateDir := filepath.Join(t.TempDir(), "state")

	cfg := &OutputPathConfig{BaseDir: archiveDir, DebugBaseDir: filepath.Join(stateDir, "debug")}

	if err := EnsureHistoryDirectoryExists(cfg); err != nil {
		t.Fatalf("EnsureHistoryDirectoryExists() error = %v", err)
	}
	if _, err := os.Stat(archiveDir); err != nil {
		t.Fatalf("history dir not created: %v", err)
	}

	cfg2 := &OutputPathConfig{BaseDir: archiveDir}
	origHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", origHome) }()
	if err := os.Setenv("HOME", t.TempDir()); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	if err := EnsureStateDirectoryExists(cfg2); err != nil {
		t.Fatalf("EnsureStateDirectoryExists() error = %v", err)
	}
	if _, err := os.Stat(cfg2.GetTracerDir()); err != nil {
		t.Fatalf("state dir not created: %v", err)
	}
}
