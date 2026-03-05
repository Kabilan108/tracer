package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tracer-ai/tracer-cli/pkg/config"
)

func TestValidateFlags_CloudSyncMutualExclusion(t *testing.T) {
	// Save original global flag values
	origOnlyCloudSync := onlyCloudSync
	origNoCloudSync := noCloudSync
	origConsole := console
	origSilent := silent
	origDebug := debug
	origLogFile := logFile

	// Restore original values after test
	defer func() {
		onlyCloudSync = origOnlyCloudSync
		noCloudSync = origNoCloudSync
		console = origConsole
		silent = origSilent
		debug = origDebug
		logFile = origLogFile
	}()

	tests := []struct {
		name          string
		onlyCloudSync bool
		noCloudSync   bool
		expectError   bool
	}{
		{
			name:          "both flags set - mutually exclusive error",
			onlyCloudSync: true,
			noCloudSync:   true,
			expectError:   true,
		},
		{
			name:          "only-cloud-sync alone - valid",
			onlyCloudSync: true,
			noCloudSync:   false,
			expectError:   false,
		},
		{
			name:          "no-cloud-sync alone - valid",
			onlyCloudSync: false,
			noCloudSync:   true,
			expectError:   false,
		},
		{
			name:          "neither flag set - valid",
			onlyCloudSync: false,
			noCloudSync:   false,
			expectError:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set flags to known valid state for other validations
			console = false
			silent = false
			debug = false
			logFile = false

			// Set the flags under test
			onlyCloudSync = tt.onlyCloudSync
			noCloudSync = tt.noCloudSync

			err := validateFlags()

			if tt.expectError && err == nil {
				t.Errorf("validateFlags() expected error for onlyCloudSync=%v, noCloudSync=%v, got nil",
					tt.onlyCloudSync, tt.noCloudSync)
			}
			if !tt.expectError && err != nil {
				t.Errorf("validateFlags() unexpected error for onlyCloudSync=%v, noCloudSync=%v: %v",
					tt.onlyCloudSync, tt.noCloudSync, err)
			}
		})
	}
}

func TestAcquireDaemonLock_SingleInstance(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "daemon.lock")

	firstLock, err := acquireDaemonLock(lockPath)
	if err != nil {
		t.Fatalf("acquireDaemonLock() first lock error = %v", err)
	}

	if _, err := acquireDaemonLock(lockPath); err == nil {
		t.Fatal("acquireDaemonLock() second lock should fail while first lock is held")
	}

	releaseDaemonLock(firstLock)

	secondLock, err := acquireDaemonLock(lockPath)
	if err != nil {
		t.Fatalf("acquireDaemonLock() after release error = %v", err)
	}
	releaseDaemonLock(secondLock)

	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("daemon lock file should be removed after release, stat error = %v", err)
	}
}

func TestIngestCommand_SkipsExcludedProject(t *testing.T) {
	origLoadedConfig := loadedConfig
	origOutputDir := outputDir
	origDebugDir := debugDir
	origLocalTimeZone := localTimeZone
	origSilent := silent
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	defer func() {
		loadedConfig = origLoadedConfig
		outputDir = origOutputDir
		debugDir = origDebugDir
		localTimeZone = origLocalTimeZone
		silent = origSilent
		_ = os.Chdir(origCwd)
	}()

	excludedDir := filepath.Join(t.TempDir(), "excluded")
	if err := os.MkdirAll(excludedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.Chdir(excludedDir); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}

	archiveRoot := filepath.Join(t.TempDir(), "archive")
	loadedConfig = &config.Config{
		Ingest: config.IngestConfig{
			ExcludePathGlobs: []string{excludedDir},
		},
	}
	outputDir = archiveRoot
	debugDir = ""
	localTimeZone = false
	silent = true

	cmd := createIngestCommand()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("ingest command should skip excluded project without error: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(archiveRoot, "*", "*", "*.md"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no markdown files for excluded project, found %d", len(matches))
	}
}

func TestDaemonRunCommand_SkipsExcludedProject(t *testing.T) {
	origLoadedConfig := loadedConfig
	origOutputDir := outputDir
	origDebugDir := debugDir
	origLocalTimeZone := localTimeZone
	origSilent := silent
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	defer func() {
		loadedConfig = origLoadedConfig
		outputDir = origOutputDir
		debugDir = origDebugDir
		localTimeZone = origLocalTimeZone
		silent = origSilent
		_ = os.Chdir(origCwd)
	}()

	excludedDir := filepath.Join(t.TempDir(), "excluded-daemon")
	if err := os.MkdirAll(excludedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.Chdir(excludedDir); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}

	archiveRoot := filepath.Join(t.TempDir(), "daemon-archive")
	loadedConfig = &config.Config{
		Ingest: config.IngestConfig{
			ExcludePathGlobs: []string{excludedDir},
		},
	}
	outputDir = archiveRoot
	debugDir = ""
	localTimeZone = false
	silent = true

	cmd := createDaemonCommand()
	cmd.SetArgs([]string{"run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("daemon run command should skip excluded project without error: %v", err)
	}

	lockPath := filepath.Join(archiveRoot, "daemon.lock")
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("daemon lock file should not be created for excluded project, stat error = %v", err)
	}
}
