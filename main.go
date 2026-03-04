package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/specstoryai/getspecstory/specstory-cli/pkg/cloud"
	cmdpkg "github.com/specstoryai/getspecstory/specstory-cli/pkg/cmd" // Aliased to avoid shadowing cobra's `cmd` parameter

	"github.com/specstoryai/getspecstory/specstory-cli/pkg/config"
	"github.com/specstoryai/getspecstory/specstory-cli/pkg/engine"
	"github.com/specstoryai/getspecstory/specstory-cli/pkg/log"
	"github.com/specstoryai/getspecstory/specstory-cli/pkg/provenance"
	sessionpkg "github.com/specstoryai/getspecstory/specstory-cli/pkg/session"
	"github.com/specstoryai/getspecstory/specstory-cli/pkg/spi"
	"github.com/specstoryai/getspecstory/specstory-cli/pkg/spi/factory"
	"github.com/specstoryai/getspecstory/specstory-cli/pkg/telemetry"
	"github.com/specstoryai/getspecstory/specstory-cli/pkg/utils"
)

// The current version of the CLI
var version = "dev" // Replaced with actual version in the production build process

// Flags / Modes / Options

// General Options
var noVersionCheck bool // flag to skip checking for newer versions
var outputDir string    // custom output directory for markdown files
var debugDir string     // custom output directory for debug files
var localTimeZone bool  // flag to use local timezone instead of UTC
// Sync Options
var noCloudSync bool   // flag to disable cloud sync
var onlyCloudSync bool // flag to skip local markdown writes and only sync to cloud
var onlyStats bool     // flag to only update statistics, skip local markdown and cloud sync
var printToStdout bool // flag to output markdown to stdout instead of saving (only with -s flag)
var cloudURL string    // custom cloud API URL (hidden flag)
// Authentication Options
var cloudToken string // cloud refresh token for this session only (used by VSC VSIX, bypasses normal login)
// Logging and Debugging Options
var console bool // flag to enable logging to the console
var logFile bool // flag to enable logging to the log file
var debug bool   // flag to enable debug level logging
var silent bool  // flag to enable silent output (no user messages)
// Provenance Options
var provenanceEnabled bool // flag to enable AI provenance tracking

// Loaded configuration (populated in main before commands are created)
var loadedConfig *config.Config

// Telemetry State
var telemetryEndpoint string    // OTLP gRPC collector endpoint
var telemetryServiceName string // override the default service name
var noTelemetryPrompts bool     // flag to disable sending prompt text in telemetry

// Run Mode State
var lastRunSessionID string // tracks the session ID from the most recent run command for deep linking

// pluralSession returns "session" or "sessions" based on count for proper grammar
func pluralSession(count int) string {
	if count == 1 {
		return "session"
	}
	return "sessions"
}

// SyncStats tracks the results of a sync operation
type SyncStats struct {
	TotalSessions   int
	SessionsSkipped int // Already up to date
	SessionsUpdated int // Existed but needed update
	SessionsCreated int // Newly created markdown files
}

// validateFlags checks for mutually exclusive flag combinations
func validateFlags() error {
	if console && silent {
		return utils.ValidationError{Message: "cannot use `console` and `silent` together. These are mutually exclusive"}
	}
	if debug && !console && !logFile {
		return utils.ValidationError{Message: "`debug` requires either `console` or `log` to be specified"}
	}
	if onlyCloudSync && noCloudSync {
		return utils.ValidationError{Message: "cannot use `only-cloud-sync` and `no-cloud-sync` together. These are mutually exclusive"}
	}
	if onlyStats && onlyCloudSync {
		return utils.ValidationError{Message: "cannot use --only-stats and --only-cloud-sync together. These flags are mutually exclusive"}
	}
	if onlyStats && noCloudSync {
		return utils.ValidationError{Message: "--only-stats already skips cloud sync, no need for --no-cloud-sync"}
	}
	if onlyStats && printToStdout {
		return utils.ValidationError{Message: "cannot use --only-stats and --print together. These flags are mutually exclusive"}
	}
	if printToStdout && onlyCloudSync {
		return utils.ValidationError{Message: "cannot use --print and `only-cloud-sync` together. These are mutually exclusive"}
	}
	if printToStdout && console {
		return utils.ValidationError{Message: "cannot use --print and `console` together. Console debug output would interleave with markdown on stdout"}
	}
	return nil
}

// createRootCommand dynamically creates the root command with provider information
func createRootCommand() *cobra.Command {
	registry := factory.GetRegistry()
	ids := registry.ListIDs()
	providerList := registry.GetProviderList()

	// Build dynamic examples based on registered providers
	examples := `
# Check terminal coding agents installation
tracer check

# Run the default agent with auto-saving
tracer run`

	// Add provider-specific examples if we have providers
	if len(ids) > 0 {
		examples += "\n\n# Run a specific agent with auto-saving"
		for _, id := range ids {
			if provider, _ := registry.Get(id); provider != nil {
				examples += fmt.Sprintf("\ntracer run %s", id)
			}
		}

		// Use first provider for custom command example
		examples += fmt.Sprintf("\n\n# Run with custom command\ntracer run %s -c \"/custom/path/to/agent\"", ids[0])
	}

	examples += `

# Generate markdown files for all agent sessions associated with the current directory
tracer sync

# Generate markdown files for specific agent sessions
tracer sync -s <session-id>
tracer sync -s <session-id-1> -s <session-id-2>

# Watch for any agent activity in the current directory and generate markdown files
tracer watch`

	longDesc := `SpecStory is a wrapper for terminal coding agents that auto-saves markdown files of all your chat interactions.`
	if providerList != "No providers registered" {
		longDesc += "\n\nSupported agents: " + providerList + "."
	}

	return &cobra.Command{
		Use:               "tracer [command]",
		Short:             "SpecStory auto-saves terminal coding agent chat interactions",
		Long:              longDesc,
		Example:           examples,
		SilenceUsage:      true,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Validate flags before any other setup
			if err := validateFlags(); err != nil {
				fmt.Println() // Add visual separation before error message for better CLI readability
				return err
			}

			// Configure logging based on flags
			if console || logFile {
				// Create output config to get proper log path if needed
				var logPath string
				if logFile {
					config, err := utils.SetupOutputConfig(outputDir, debugDir)
					if err != nil {
						return err
					}
					logPath = config.GetLogPath()
				}

				// Set up logger
				if err := log.SetupLogger(console, logFile, debug, logPath); err != nil {
					return fmt.Errorf("failed to set up logger: %v", err)
				}

				// Log startup information
				slog.Info("=== SpecStory Starting ===")
				slog.Info("Version", "version", version)
				slog.Info("Command line", "args", strings.Join(os.Args, " "))
				if cwd, err := os.Getwd(); err == nil {
					slog.Info("Current working directory", "cwd", cwd)
				}
				slog.Info("========================")
			} else {
				// No logging - set up discard logger
				if err := log.SetupLogger(false, false, false, ""); err != nil {
					return err
				}
			}

			// Set silent mode for user messages
			log.SetSilent(silent)

			// Initialize cloud sync manager
			cloud.InitSyncManager(!noCloudSync)
			cloud.SetSilent(silent)
			cloud.SetClientVersion(version)
			// Set custom cloud URL if provided (otherwise cloud package uses its default)
			if cloudURL != "" {
				cloud.SetAPIBaseURL(cloudURL)
			}

			// If --cloud-token flag was provided, verify the refresh token works
			// This bypasses normal authentication and uses the token for this session only
			if cloudToken != "" {
				slog.Info("Using session-only refresh token from --cloud-token flag")
				if err := cloud.SetSessionRefreshToken(cloudToken); err != nil {
					fmt.Fprintln(os.Stderr) // Visual separation
					fmt.Fprintln(os.Stderr, "❌ Failed to authenticate with the provided token:")
					fmt.Fprintf(os.Stderr, "   %v\n", err)
					fmt.Fprintln(os.Stderr)
					fmt.Fprintln(os.Stderr, "💡 The token may be invalid, expired, or revoked.")
					fmt.Fprintln(os.Stderr, "   Please check your token and try again, or use 'specstory login' for interactive authentication.")
					fmt.Fprintln(os.Stderr)
					return fmt.Errorf("authentication failed with provided token")
				}
				if !silent {
					fmt.Println()
					fmt.Println("🔑 Authenticated using provided refresh token (session-only)")
					fmt.Println()
				}
			}

			// Validate that --only-cloud-sync requires authentication
			if onlyCloudSync && !cloud.IsAuthenticated() {
				return utils.ValidationError{Message: "--only-cloud-sync requires authentication. Please run 'specstory login' first"}
			}

			return nil
		},
		Run: func(c *cobra.Command, args []string) {
			// If no command is specified, show logo then help
			cmdpkg.DisplayLogoAndHelp(c)
		},
	}
}

var rootCmd *cobra.Command

// createRunCommand dynamically creates the run command with provider information
func createRunCommand() *cobra.Command {
	registry := factory.GetRegistry()
	ids := registry.ListIDs()
	providerList := registry.GetProviderList()

	// Build dynamic examples
	examples := `
# Run default agent with auto-saving
specstory run`

	if len(ids) > 0 {
		examples += "\n\n# Run specific agent"
		for _, id := range ids {
			examples += fmt.Sprintf("\nspecstory run %s", id)
		}

		// Use first provider for custom command example
		examples += fmt.Sprintf("\n\n# Run with custom command\nspecstory run %s -c \"/custom/path/to/agent\"", ids[0])
	}

	examples += `

# Resume a specific session
specstory run --resume 5c5c2876-febd-4c87-b80c-d0655f1cd3fd

# Run with custom output directory
specstory run --output-dir ~/my-sessions`

	// Determine default agent name
	defaultAgent := "the default agent"
	if len(ids) > 0 {
		if provider, err := registry.Get(ids[0]); err == nil {
			defaultAgent = provider.Name()
		}
	}

	longDesc := fmt.Sprintf(`Launch terminal coding agents in interactive mode with auto-save markdown file generation.

By default, launches %s. Specify a specific agent ID to use a different agent.`, defaultAgent)
	if providerList != "No providers registered" {
		longDesc += "\n\nAvailable provider IDs: " + providerList + "."
	}

	return &cobra.Command{
		Use:     "run [provider-id]",
		Aliases: []string{"r"},
		Short:   "Launch terminal coding agents in interactive mode with auto-save",
		Long:    longDesc,
		Example: examples,
		Args:    cobra.MaximumNArgs(1), // Accept 0 or 1 argument (provider ID)
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// Get custom command if provided via flag
			customCmd, _ := cmd.Flags().GetString("command")

			// Validate that -c flag requires a provider
			if customCmd != "" && len(args) == 0 {
				registry := factory.GetRegistry()
				ids := registry.ListIDs()
				example := "specstory run <provider> -c \"/custom/path/to/agent\""
				if len(ids) > 0 {
					example = fmt.Sprintf("specstory run %s -c \"/custom/path/to/agent\"", ids[0])
				}
				return utils.ValidationError{
					Message: "The -c/--command flag requires a provider to be specified.\n" +
						"Example: " + example,
				}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			config.EnsureDefaultProjectConfig()
			slog.Info("Running in interactive mode")

			// Get custom command if provided via flag
			customCmd, _ := cmd.Flags().GetString("command")

			// Get the provider
			registry := factory.GetRegistry()
			var providerID string
			if len(args) == 0 {
				// Default to first registered provider
				ids := registry.ListIDs()
				if len(ids) > 0 {
					providerID = ids[0]
				} else {
					return fmt.Errorf("no providers registered")
				}
			} else {
				providerID = args[0]
			}

			provider, err := registry.Get(providerID)
			if err != nil {
				// Provider not found - show helpful error
				fmt.Printf("❌ Provider '%s' is not a valid provider implementation\n\n", providerID)

				ids := registry.ListIDs()
				if len(ids) > 0 {
					fmt.Println("The registered providers are:")
					for _, id := range ids {
						if p, _ := registry.Get(id); p != nil {
							fmt.Printf("  • %s - %s\n", id, p.Name())
						}
					}
					fmt.Println("\nExample: specstory run " + ids[0])
				}
				return err
			}

			// Fall back to config file provider command if -c flag wasn't provided
			if customCmd == "" && loadedConfig != nil {
				customCmd = loadedConfig.GetProviderCmd(providerID)
			}

			slog.Info("Launching agent", "provider", provider.Name())

			// Setup output configuration
			config, err := utils.SetupOutputConfig(outputDir, debugDir)
			if err != nil {
				return err
			}
			// Ensure history directory exists for interactive mode
			if err := utils.EnsureHistoryDirectoryExists(config); err != nil {
				return err
			}

			// Initialize project identity
			cwd, err := os.Getwd()
			if err != nil {
				slog.Error("Failed to get current working directory", "error", err)
				return err
			}
			identityManager := utils.NewProjectIdentityManager(cwd)
			if _, err := identityManager.EnsureProjectIdentity(); err != nil {
				// Log error but don't fail the command
				slog.Error("Failed to ensure project identity", "error", err)
			}

			// Create context for graceful cancellation (Ctrl+C handling)
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			// Start provenance infrastructure before the agent so all file changes are captured
			provenanceEngine, provenanceCleanup, err := provenance.StartEngine(provenanceEnabled)
			if err != nil {
				return err
			}
			defer provenanceCleanup()

			fsCleanup, err := provenance.StartFSWatcher(ctx, provenanceEngine, cwd)
			if err != nil {
				return err
			}
			defer fsCleanup()

			// Check authentication for cloud sync
			cmdpkg.CheckAndWarnAuthentication(noCloudSync)

			// Get resume session ID if provided
			resumeSessionID, _ := cmd.Flags().GetString("resume")
			if resumeSessionID != "" {
				resumeSessionID = strings.TrimSpace(resumeSessionID)
				// Note: Different providers may have different session ID formats
				// Let the provider validate its own format
				slog.Info("Resuming session", "sessionId", resumeSessionID)
			}

			// Get debug-raw flag value (must be before callback to capture in closure)
			debugRaw, _ := cmd.Flags().GetBool("debug-raw")
			useUTC := !localTimeZone

			// This callback pattern enables real-time processing of agent sessions
			// without blocking the agent's execution. As the agent writes updates to its
			// data files, the provider's watcher detects changes and invokes this callback,
			// allowing immediate markdown generation and cloud sync. Errors are logged but
			// don't stop execution because transient failures (e.g., network issues) shouldn't
			// interrupt the user's coding session.
			sessionCallback := func(session *spi.AgentChatSession) {
				if session == nil {
					return
				}

				// Track the session ID for deep linking on exit
				lastRunSessionID = session.SessionID

				// Process the session (write markdown and sync to cloud)
				// Don't show output during interactive run mode
				// This is autosave mode (true)
				_, err := sessionpkg.ProcessSingleSession(context.Background(), session, config, sessionpkg.ProcessingOptions{
					OnlyCloudSync:      onlyCloudSync,
					IsAutosave:         true,
					DebugRaw:           debugRaw,
					UseUTC:             useUTC,
					NoTelemetryPrompts: noTelemetryPrompts,
				})
				if err != nil {
					// Log error but continue - don't fail the whole run
					// In interactive mode, we prioritize keeping the agent running.
					// Failed markdown writes or cloud syncs can be retried later via
					// the sync command, so we just log and continue.
					slog.Error("Failed to process session update",
						"sessionId", session.SessionID,
						"error", err)
				}

				// Push agent events to provenance engine for correlation
				provenance.ProcessEvents(ctx, provenanceEngine, session)
			}

			// Execute the agent and watch for updates
			slog.Info("Starting agent execution and monitoring", "provider", provider.Name())
			err = provider.ExecAgentAndWatch(cwd, customCmd, resumeSessionID, debugRaw, sessionCallback)

			if err != nil {
				slog.Error("Agent execution failed", "provider", provider.Name(), "error", err)
			}

			return err
		},
	}
}

var runCmd *cobra.Command

func resolveProviders(registry *factory.Registry, projectPath string, args []string) (map[string]spi.Provider, error) {
	enabled := map[string]bool{}
	if loadedConfig != nil {
		for _, providerID := range loadedConfig.GetEnabledProviders() {
			enabled[providerID] = true
		}
	}

	isEnabled := func(providerID string) bool {
		if len(enabled) == 0 {
			return true
		}
		return enabled[strings.ToLower(providerID)]
	}

	providers := make(map[string]spi.Provider)
	if len(args) > 0 {
		providerID := args[0]
		if !isEnabled(providerID) {
			return nil, fmt.Errorf("provider %q is disabled by config", providerID)
		}
		provider, err := registry.Get(providerID)
		if err != nil {
			return nil, err
		}
		providers[providerID] = provider
		return providers, nil
	}

	for _, providerID := range registry.ListIDs() {
		if !isEnabled(providerID) {
			continue
		}
		provider, err := registry.Get(providerID)
		if err != nil {
			continue
		}
		if provider.DetectAgent(projectPath, false) {
			providers[providerID] = provider
		}
	}

	if len(providers) == 0 {
		return nil, fmt.Errorf("no providers with activity found in this project")
	}
	return providers, nil
}

func sanitizeArchiveSegment(value string) string {
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		" ", "-",
		":", "-",
		"*", "-",
		"?", "-",
		"\"", "-",
		"<", "-",
		">", "-",
		"|", "-",
	)
	sanitized := strings.Trim(replacer.Replace(strings.TrimSpace(value)), "-.")
	if sanitized == "" {
		return "unknown"
	}
	return sanitized
}

func archivePathBuilder(outputConfig *utils.OutputPathConfig, projectPath string) engine.PathBuilder {
	projectSegment := sanitizeArchiveSegment(filepath.Base(projectPath))
	historyDir := outputConfig.GetHistoryDir()

	return func(providerID string, session *spi.AgentChatSession) string {
		providerSegment := sanitizeArchiveSegment(providerID)
		sessionSegment := sanitizeArchiveSegment(session.SessionID)
		return filepath.Join(historyDir, providerSegment, projectSegment, sessionSegment+".md")
	}
}

func acquireDaemonLock(lockPath string) (*os.File, error) {
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open daemon lock file: %w", err)
	}

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("daemon already running (lock: %s)", lockPath)
	}

	if err := lockFile.Truncate(0); err == nil {
		_, _ = lockFile.WriteString(fmt.Sprintf("%d\n", os.Getpid()))
	}

	return lockFile, nil
}

func releaseDaemonLock(lockFile *os.File) {
	if lockFile == nil {
		return
	}
	lockPath := lockFile.Name()
	_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	_ = lockFile.Close()
	_ = os.Remove(lockPath)
}

func engineOptionsFromOutputConfig(config *utils.OutputPathConfig, projectPath string, useUTC bool, debounce time.Duration) engine.Options {
	return engine.Options{
		HistoryDir:     config.GetHistoryDir(),
		StatisticsPath: config.GetStatisticsPath(),
		StateDBPath:    filepath.Join(config.GetSpecStoryDir(), "runtime-state.db"),
		UseUTC:         useUTC,
		Debounce:       debounce,
		PathBuilder:    archivePathBuilder(config, projectPath),
	}
}

func createIngestCommand() *cobra.Command {
	registry := factory.GetRegistry()
	providerList := registry.GetProviderList()

	longDesc := "Backfill session markdown for all providers using the shared ingest engine."
	if providerList != "No providers registered" {
		longDesc += "\n\nAvailable provider IDs: " + providerList + "."
	}

	ingestCmd := &cobra.Command{
		Use:   "ingest [provider-id]",
		Short: "Ingest historical sessions into markdown archive",
		Long:  longDesc,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			debugRaw, _ := cmd.Flags().GetBool("debug-raw")
			useUTC := !localTimeZone

			outputConfig, err := utils.SetupOutputConfig(outputDir, debugDir)
			if err != nil {
				return err
			}
			if err := utils.EnsureHistoryDirectoryExists(outputConfig); err != nil {
				return err
			}

			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			if loadedConfig != nil && loadedConfig.IsProjectExcluded(cwd) {
				if !silent {
					fmt.Println()
					fmt.Println("Project is excluded by ingest config; skipping.")
					fmt.Println()
				}
				return nil
			}

			providers, err := resolveProviders(registry, cwd, args)
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			summary, err := engine.RunIngest(ctx, engineOptionsFromOutputConfig(outputConfig, cwd, useUTC, 0), cwd, providers, debugRaw)
			if !silent {
				fmt.Println()
				fmt.Printf("Ingest complete: %d created, %d updated, %d skipped, %d errors\n",
					summary.Created,
					summary.Updated,
					summary.Skipped,
					summary.Errors)
				fmt.Println()
			}
			return err
		},
	}

	ingestCmd.Flags().StringVar(&outputDir, "archive-root", outputDir, "global archive root for markdown output (default: ./.specstory/history)")
	ingestCmd.Flags().StringVar(&outputDir, "output-dir", outputDir, "custom output directory for markdown files (deprecated; use --archive-root)")
	_ = ingestCmd.Flags().MarkHidden("output-dir")
	ingestCmd.Flags().StringVar(&debugDir, "debug-dir", debugDir, "custom output directory for debug data (default: ./.specstory/debug)")
	ingestCmd.Flags().BoolVar(&localTimeZone, "local-time-zone", localTimeZone, "use local timezone for file name and content timestamps (when not present: UTC)")
	ingestCmd.Flags().Bool("debug-raw", false, "debug mode to output pretty-printed raw data files")
	_ = ingestCmd.Flags().MarkHidden("debug-raw")

	return ingestCmd
}

func createDaemonCommand() *cobra.Command {
	registry := factory.GetRegistry()
	providerList := registry.GetProviderList()

	daemonCmd := &cobra.Command{
		Use:   "daemon",
		Short: "Daemon commands",
	}

	var debounce time.Duration
	runCmd := &cobra.Command{
		Use:   "run [provider-id]",
		Short: "Run continuous ingest daemon (historical + live updates)",
		Long: "Runs a foreground daemon that first ingests historical sessions and then watches for session updates.\n\n" +
			"Press Ctrl+C to stop.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			debugRaw, _ := cmd.Flags().GetBool("debug-raw")
			useUTC := !localTimeZone

			outputConfig, err := utils.SetupOutputConfig(outputDir, debugDir)
			if err != nil {
				return err
			}
			if err := utils.EnsureHistoryDirectoryExists(outputConfig); err != nil {
				return err
			}

			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			if loadedConfig != nil && loadedConfig.IsProjectExcluded(cwd) {
				if !silent {
					fmt.Println()
					fmt.Println("Project is excluded by ingest config; daemon not started.")
					fmt.Println()
				}
				return nil
			}

			providers, err := resolveProviders(registry, cwd, args)
			if err != nil {
				return err
			}

			if providerList != "No providers registered" && !silent {
				fmt.Println()
				fmt.Println("Starting daemon ingest and watcher...")
				fmt.Println("Press Ctrl+C to stop.")
				fmt.Println()
			}

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			lockPath := filepath.Join(outputConfig.GetSpecStoryDir(), "daemon.lock")
			lockFile, err := acquireDaemonLock(lockPath)
			if err != nil {
				return err
			}
			defer releaseDaemonLock(lockFile)

			summary, err := engine.RunDaemon(ctx, engineOptionsFromOutputConfig(outputConfig, cwd, useUTC, debounce), cwd, providers, debugRaw)
			if !silent {
				fmt.Println()
				fmt.Printf("Daemon stopped: %d created, %d updated, %d skipped, %d errors\n",
					summary.Created,
					summary.Updated,
					summary.Skipped,
					summary.Errors)
				fmt.Println()
			}
			return err
		},
	}

	runCmd.Flags().StringVar(&outputDir, "archive-root", outputDir, "global archive root for markdown output (default: ./.specstory/history)")
	runCmd.Flags().StringVar(&outputDir, "output-dir", outputDir, "custom output directory for markdown files (deprecated; use --archive-root)")
	_ = runCmd.Flags().MarkHidden("output-dir")
	runCmd.Flags().StringVar(&debugDir, "debug-dir", debugDir, "custom output directory for debug data (default: ./.specstory/debug)")
	runCmd.Flags().BoolVar(&localTimeZone, "local-time-zone", localTimeZone, "use local timezone for file name and content timestamps (when not present: UTC)")
	runCmd.Flags().DurationVar(&debounce, "debounce", 750*time.Millisecond, "debounce duration for write updates")
	runCmd.Flags().Bool("debug-raw", false, "debug mode to output pretty-printed raw data files")
	_ = runCmd.Flags().MarkHidden("debug-raw")

	daemonCmd.AddCommand(runCmd)
	return daemonCmd
}

// createSyncCommand dynamically creates the sync command with provider information
func createSyncCommand() *cobra.Command {
	registry := factory.GetRegistry()
	ids := registry.ListIDs()
	providerList := registry.GetProviderList()

	// Build dynamic examples
	examples := `
# Sync all agents with activity
specstory sync`

	if len(ids) > 0 {
		examples += "\n\n# Sync specific agent"
		for _, id := range ids {
			examples += fmt.Sprintf("\nspecstory sync %s", id)
		}
	}

	examples += `

# Sync a specific session by UUID
specstory sync -s <session-id>

# Sync multiple sessions
specstory sync -s <session-id-1> -s <session-id-2> -s <session-id-3>

# Output session markdown to stdout without saving
specstory sync -s <session-id> --print

# Output multiple sessions to stdout
specstory sync -s <session-id-1> -s <session-id-2> --print

# Sync all sessions for the current directory, with console output
specstory sync --console

# Sync all sessions for the current directory, with a log file
specstory sync --log`

	longDesc := `Create or update markdown files for the agent sessions in the current working directory.

By default, syncs all registered providers that have activity.
Provide a specific agent ID to sync a specific provider.`
	if providerList != "No providers registered" {
		longDesc += "\n\nAvailable provider IDs: " + providerList + "."
	}

	return &cobra.Command{
		Use:     "sync [provider-id]",
		Aliases: []string{"s"},
		Short:   "Sync markdown files for terminal coding agent sessions",
		Long:    longDesc,
		Example: examples,
		Args:    cobra.MaximumNArgs(1), // Accept 0 or 1 argument (provider ID)
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// Validate that --print requires -s flag
			sessionIDs, _ := cmd.Flags().GetStringSlice("session")
			if printToStdout && len(sessionIDs) == 0 {
				return utils.ValidationError{Message: "--print requires the -s/--session flag to specify which sessions to output"}
			}
			if onlyStats && len(sessionIDs) > 0 {
				return utils.ValidationError{Message: "cannot use --only-stats with -s/--session. Use --only-stats without -s to collect statistics for all sessions"}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			config.EnsureDefaultProjectConfig()

			// Get session IDs if provided via flag
			sessionIDs, _ := cmd.Flags().GetStringSlice("session")

			// Handle specific session sync if -s flag is provided
			if len(sessionIDs) > 0 {
				return syncSpecificSessions(cmd, args, sessionIDs)
			}

			slog.Info("Running sync command")
			registry := factory.GetRegistry()

			// Check if user specified a provider
			if len(args) > 0 {
				// Sync specific provider
				return syncSingleProvider(registry, args[0], cmd)
			} else {
				// Sync all providers with activity
				return syncAllProviders(registry, cmd)
			}
		},
	}
}

// syncSpecificSessions syncs one or more sessions by their IDs
// When printToStdout is set, outputs markdown to stdout instead of saving to files.
// args[0] is the optional provider ID
func syncSpecificSessions(cmd *cobra.Command, args []string, sessionIDs []string) error {
	if len(sessionIDs) == 1 {
		slog.Info("Running single session sync", "sessionId", sessionIDs[0])
	} else {
		slog.Info("Running multiple session sync", "sessionCount", len(sessionIDs))
	}

	// Get debug-raw flag value
	debugRaw, _ := cmd.Flags().GetBool("debug-raw")
	useUTC := !localTimeZone

	cwd, err := os.Getwd()
	if err != nil {
		slog.Error("Failed to get current working directory", "error", err)
		return err
	}

	// Setup file output and cloud sync (not needed for --print mode)
	var config utils.OutputConfig
	if !printToStdout {
		config, err = utils.SetupOutputConfig(outputDir, debugDir)
		if err != nil {
			return err
		}

		identityManager := utils.NewProjectIdentityManager(cwd)
		if _, err := identityManager.EnsureProjectIdentity(); err != nil {
			slog.Error("Failed to ensure project identity", "error", err)
		}

		cmdpkg.CheckAndWarnAuthentication(noCloudSync || onlyStats)

		if err := utils.EnsureHistoryDirectoryExists(config); err != nil {
			return err
		}
	}

	registry := factory.GetRegistry()

	// Track statistics for summary output
	var successCount, notFoundCount, errorCount int
	var lastError error

	// Resolve provider once if specified (fail fast if provider not found)
	var specifiedProvider spi.Provider
	if len(args) > 0 {
		providerID := args[0]
		provider, err := registry.Get(providerID)
		if err != nil {
			if !printToStdout {
				fmt.Printf("❌ Provider '%s' not found\n", providerID)
			}
			return fmt.Errorf("provider '%s' not found: %w", providerID, err)
		}
		specifiedProvider = provider
	}

	// Process each session ID
	var printedSessions int // tracks sessions printed to stdout for separator logic
	for _, sessionID := range sessionIDs {
		sessionID = strings.TrimSpace(sessionID)
		if sessionID == "" {
			continue // Skip empty session IDs
		}

		// Find session across providers
		var session *spi.AgentChatSession

		// Case A: Provider was specified - use it directly
		if specifiedProvider != nil {
			session, err = specifiedProvider.GetAgentChatSession(cwd, sessionID, debugRaw)
			if err != nil {
				if !printToStdout {
					fmt.Printf("❌ Error getting session '%s' from %s: %v\n", sessionID, specifiedProvider.Name(), err)
				}
				slog.Error("Error getting session from provider", "sessionId", sessionID, "provider", specifiedProvider.Name(), "error", err)
				errorCount++
				lastError = err
				continue
			}
			if session == nil {
				if !printToStdout {
					fmt.Printf("❌ Session '%s' not found in %s\n", sessionID, specifiedProvider.Name())
				}
				slog.Warn("Session not found in provider", "sessionId", sessionID, "provider", specifiedProvider.Name())
				notFoundCount++
				continue
			}
		} else {
			// Case B: No provider specified - try all providers
			providerIDs := registry.ListIDs()
			for _, id := range providerIDs {
				provider, err := registry.Get(id)
				if err != nil {
					continue
				}

				session, err = provider.GetAgentChatSession(cwd, sessionID, debugRaw)
				if err != nil {
					slog.Debug("Error checking provider for session", "provider", id, "sessionId", sessionID, "error", err)
					continue
				}
				if session != nil {
					if !silent && !printToStdout {
						fmt.Printf("✅ Found session '%s' for %s\n", sessionID, provider.Name())
					}
					break // Found it, don't check other providers
				}
			}

			if session == nil {
				if !printToStdout {
					fmt.Printf("❌ Session '%s' not found in any provider\n", sessionID)
				}
				slog.Warn("Session not found in any provider", "sessionId", sessionID)
				notFoundCount++
				continue
			}
		}

		// Process the found session
		if printToStdout {
			sessionpkg.ValidateSessionData(session, debugRaw)
			sessionpkg.WriteDebugSessionData(session, debugRaw)

			markdownContent, err := sessionpkg.GenerateMarkdownFromAgentSession(session.SessionData, false, useUTC)
			if err != nil {
				slog.Error("Failed to generate markdown", "sessionId", session.SessionID, "error", err)
				errorCount++
				lastError = err
				continue
			}

			// Separate multiple sessions with a horizontal rule
			if printedSessions > 0 {
				fmt.Print("\n---\n\n")
			}
			fmt.Print(markdownContent)
			printedSessions++
			successCount++
		} else {
			// Normal sync: write to file and optionally cloud sync
			if _, err := sessionpkg.ProcessSingleSession(context.Background(), session, config, sessionpkg.ProcessingOptions{
				OnlyCloudSync:      onlyCloudSync,
				ShowOutput:         true,
				DebugRaw:           debugRaw,
				UseUTC:             useUTC,
				NoTelemetryPrompts: noTelemetryPrompts,
			}); err != nil {
				errorCount++
				lastError = err
			} else {
				successCount++
			}
		}
	}

	// Print summary if multiple sessions were processed (not for --print mode)
	if !printToStdout && len(sessionIDs) > 1 && !silent {
		fmt.Println()
		fmt.Println("📊 Session sync summary:")
		fmt.Printf("  ✅ %d %s successfully synced\n", successCount, pluralSession(successCount))
		if notFoundCount > 0 {
			fmt.Printf("  ❌ %d %s not found\n", notFoundCount, pluralSession(notFoundCount))
		}
		if errorCount > 0 {
			fmt.Printf("  ❌ %d %s failed with errors\n", errorCount, pluralSession(errorCount))
		}
		fmt.Println()
	}

	// Return error if any sessions failed
	if errorCount > 0 || (notFoundCount > 0 && successCount == 0) {
		if lastError != nil {
			return lastError
		}
		return fmt.Errorf("%d %s not found", notFoundCount, pluralSession(notFoundCount))
	}

	return nil
}

// preloadBulkSessionSizesIfNeeded optimizes bulk syncs by fetching all session sizes upfront.
// This avoids making individual HEAD requests for each session during sync operations.
//
// The function only performs the preload if:
//   - Cloud sync is enabled (noCloudSync flag is false)
//   - User is authenticated with SpecStory Cloud
//   - A valid projectID can be determined from the identity manager
//
// Parameters:
//   - identityManager: Provides project identity (git_id or workspace_id) for the current project
//
// The preloaded sizes are cached in the SyncManager and shared across all providers since they
// use the same projectID. If the bulk fetch fails or if projectID cannot be determined, individual
// sessions will gracefully fall back to HEAD requests during sync.
//
// This function is safe to call multiple times but should typically be called once before processing
// multiple sessions in batch sync operations.
func preloadBulkSessionSizesIfNeeded(identityManager *utils.ProjectIdentityManager) {
	// Skip if cloud sync is disabled or user is not authenticated
	if noCloudSync || !cloud.IsAuthenticated() {
		return
	}

	// Get projectID from identity manager - required for cloud sync
	projectID, err := identityManager.GetProjectID()
	if err != nil {
		slog.Warn("Cannot preload session sizes: failed to get project ID", "error", err)
		return
	}

	// Preload bulk sizes once - shared across all providers since they use same projectID
	if syncMgr := cloud.GetSyncManager(); syncMgr != nil {
		syncMgr.PreloadSessionSizes(projectID)
	} else {
		slog.Warn("Cannot preload session sizes: sync manager is nil despite authentication")
	}
}

// syncProvider performs the actual sync for a single provider.
// The caller-provided statsCollector accumulates statistics across providers so
// they can be flushed to disk in a single I/O operation after all providers are done.
// Returns (sessionCount, error).
func syncProvider(provider spi.Provider, providerID string, config utils.OutputConfig, debugRaw bool, useUTC bool, statsCollector *sessionpkg.StatisticsCollector) (int, error) {
	cwd, err := os.Getwd()
	if err != nil {
		slog.Error("Failed to get current working directory", "error", err)
		return 0, err
	}

	// Create progress callback for parsing phase
	// The callback updates the "Parsing..." line in place with [n/m] progress
	var parseProgress spi.ProgressCallback
	if !silent {
		providerName := provider.Name()
		parseProgress = func(current, total int) {
			fmt.Printf("\rParsing %s sessions [%d/%d]", providerName, current, total)
			_ = os.Stdout.Sync()
		}
	}

	// Get all sessions from the provider
	sessions, err := provider.GetAgentChatSessions(cwd, debugRaw, parseProgress)
	if err != nil {
		return 0, fmt.Errorf("failed to get sessions: %w", err)
	}

	sessionCount := len(sessions)

	if sessionCount == 0 && !silent {
		// This comes after "Parsing..." message, on the same line
		fmt.Printf(", no non-empty sessions found for %s\n", provider.Name())
		fmt.Println()
		return 0, nil
	}

	if !silent && !onlyStats {
		fmt.Printf("\nSyncing markdown files for %s", provider.Name())
	}

	// Initialize statistics
	stats := &SyncStats{
		TotalSessions: sessionCount,
	}

	historyPath := config.GetHistoryDir()
	agentName := provider.Name()
	ctx := context.Background()

	// Process each session
	for i := range sessions {
		session := &sessions[i]

		// Process session in a closure so defers scope to each iteration,
		// ensuring spans are ended and metrics recorded even on early returns.
		func() {
			processingStart := time.Now()

			// Create a context with a deterministic trace ID from the session ID
			sessionCtx := telemetry.ContextWithSessionTrace(ctx, session.SessionID)

			// Start an OTel span for this session processing
			sessionCtx, span := telemetry.Tracer("specstory").Start(sessionCtx, "process_session")
			defer span.End()

			// Compute session statistics
			sessionStats := telemetry.ComputeSessionStats(agentName, session)
			defer func() {
				telemetry.RecordSessionMetrics(sessionCtx, sessionStats, time.Since(processingStart))
			}()

			// Set session span attributes
			telemetry.SetSessionSpanAttributes(span, sessionStats)

			// Create child spans for each exchange
			if session.SessionData != nil && len(session.SessionData.Exchanges) > 0 {
				telemetry.ProcessExchangeSpans(sessionCtx, sessionStats, session.SessionData.Exchanges, noTelemetryPrompts)
			}

			sessionpkg.ValidateSessionData(session, debugRaw)
			sessionpkg.WriteDebugSessionData(session, debugRaw)

			// Generate markdown from SessionData
			markdownContent, err := sessionpkg.GenerateMarkdownFromAgentSession(session.SessionData, false, useUTC)
			if err != nil {
				slog.Error("Failed to generate markdown from SessionData",
					"sessionId", session.SessionID,
					"error", err)
				return
			}

			// Compute statistics from the SessionData
			sessionStatistics := sessionpkg.ComputeSessionStatistics(session.SessionData, markdownContent, providerID)
			statsCollector.AddSessionStats(session.SessionID, sessionStatistics)

			// In only-stats mode, skip file writes and cloud sync
			if onlyStats {
				stats.SessionsSkipped++
				slog.Info("Skipping local file write (only-stats mode)", "sessionId", session.SessionID)
				return
			}

			// Generate filename from timestamp and slug
			fileFullPath := sessionpkg.BuildSessionFilePath(session, historyPath, useUTC)

			// Check if file already exists with same content
			identicalContent := false
			fileExists := false
			if existingContent, err := os.ReadFile(fileFullPath); err == nil {
				fileExists = true
				if string(existingContent) == markdownContent {
					identicalContent = true
					slog.Info("Markdown file already exists with same content, skipping write",
						"sessionId", session.SessionID,
						"path", fileFullPath)
				}
			}

			// Write file if needed (skip if only-cloud-sync is enabled)
			if !onlyCloudSync {
				if !identicalContent {
					// Ensure history directory exists (handles deletion during long-running sync)
					if err := utils.EnsureHistoryDirectoryExists(config); err != nil {
						slog.Error("Failed to ensure history directory", "error", err)
						return
					}
					err := os.WriteFile(fileFullPath, []byte(markdownContent), 0644)
					if err != nil {
						slog.Error("Error writing markdown file",
							"sessionId", session.SessionID,
							"error", err)
						return
					}
					slog.Info("Successfully wrote file",
						"sessionId", session.SessionID,
						"path", fileFullPath)
				}

				// Update statistics for normal mode
				if identicalContent {
					stats.SessionsSkipped++
				} else if fileExists {
					stats.SessionsUpdated++
				} else {
					stats.SessionsCreated++
				}
			} else {
				// In cloud-only mode, count as skipped since no local file operation occurred
				stats.SessionsSkipped++
				slog.Info("Skipping local file write (only-cloud-sync mode)",
					"sessionId", session.SessionID)
			}

			// Trigger cloud sync with provider-specific data
			// Manual sync command: perform immediate sync with HEAD check (not autosave mode)
			// In only-cloud-sync mode: always sync
			cloud.SyncSessionToCloud(session.SessionID, fileFullPath, markdownContent, []byte(session.RawData), provider.Name(), false)
		}()

		// Print progress with [n/m] format
		if !silent && !onlyStats {
			fmt.Printf("\rSyncing markdown files for %s [%d/%d]", provider.Name(), i+1, sessionCount)
			_ = os.Stdout.Sync()
		}
	}

	// Print newline after progress
	if !silent && sessionCount > 0 && !onlyCloudSync && !onlyStats {
		fmt.Println()

		// Calculate actual total of processed sessions
		actualTotal := stats.SessionsSkipped + stats.SessionsUpdated + stats.SessionsCreated

		// Print summary message with proper pluralization
		fmt.Printf("\n%s✅ %s sync complete!%s 📊 %s%d%s %s processed\n",
			log.ColorBoldGreen, provider.Name(), log.ColorReset,
			log.ColorBoldCyan, actualTotal, log.ColorReset, pluralSession(actualTotal))
		fmt.Println()

		fmt.Printf("  ⏭️ %s%d %s up to date (skipped)%s\n",
			log.ColorCyan, stats.SessionsSkipped, pluralSession(stats.SessionsSkipped), log.ColorReset)
		fmt.Printf("  ♻️ %s%d %s updated%s\n",
			log.ColorYellow, stats.SessionsUpdated, pluralSession(stats.SessionsUpdated), log.ColorReset)
		fmt.Printf("  ✨ %s%d new %s created%s\n",
			log.ColorGreen, stats.SessionsCreated, pluralSession(stats.SessionsCreated), log.ColorReset)
		fmt.Println()
	}

	return sessionCount, nil
}

// syncAllProviders syncs all providers that have activity in the current directory
func syncAllProviders(registry *factory.Registry, cmd *cobra.Command) error {
	// Get debug-raw flag value
	debugRaw, _ := cmd.Flags().GetBool("debug-raw")
	useUTC := !localTimeZone

	cwd, err := os.Getwd()
	if err != nil {
		slog.Error("Failed to get current working directory", "error", err)
		return err
	}

	providerIDs := registry.ListIDs()
	providersWithActivity := []string{}

	// Check each provider for activity
	for _, id := range providerIDs {
		provider, err := registry.Get(id)
		if err != nil {
			slog.Warn("Failed to get provider", "id", id, "error", err)
			continue
		}

		if provider.DetectAgent(cwd, false) {
			providersWithActivity = append(providersWithActivity, id)
		}
	}

	// If no providers have activity, show helpful message
	if len(providersWithActivity) == 0 {
		if !silent {
			fmt.Println() // Add visual separation
			log.UserWarn("No coding agent activity found for this project directory.\n\n")

			log.UserMessage("We checked for activity in '%s' from the following agents:\n", cwd)
			for _, id := range providerIDs {
				if provider, err := registry.Get(id); err == nil {
					log.UserMessage("- %s\n", provider.Name())
				}
			}
			log.UserMessage("\nBut didn't find any activity.\n\n")

			log.UserMessage("To fix this:\n")
			log.UserMessage("  1. Run 'specstory run' to start the default agent in this directory\n")
			log.UserMessage("  2. Run 'specstory run <agent>' to start a specific agent in this directory\n")
			log.UserMessage("  3. Or run the agent directly first, then try syncing again\n")
			fmt.Println() // Add trailing newline
		}
		return nil
	}

	// Setup output configuration (once for all providers)
	config, err := utils.SetupOutputConfig(outputDir, debugDir)
	if err != nil {
		return err
	}

	// Initialize project identity (once for all providers)
	identityManager := utils.NewProjectIdentityManager(cwd)
	if _, err := identityManager.EnsureProjectIdentity(); err != nil {
		slog.Error("Failed to ensure project identity", "error", err)
	}

	// Check authentication for cloud sync (once)
	cmdpkg.CheckAndWarnAuthentication(noCloudSync || onlyStats)

	// Ensure history directory exists (once)
	if err := utils.EnsureHistoryDirectoryExists(config); err != nil {
		return err
	}

	// Preload session sizes for batch sync optimization (ONCE for all providers)
	preloadBulkSessionSizesIfNeeded(identityManager)

	// Create a single statistics collector shared across all providers so the
	// statistics.json file is written only once after all providers are processed.
	statsCollector := sessionpkg.NewStatisticsCollector(config.GetStatisticsPath())

	// Sync each provider with activity
	totalSessionCount := 0
	var lastError error

	for idx, id := range providersWithActivity {
		provider, err := registry.Get(id)
		if err != nil {
			continue
		}

		if !silent {
			fmt.Printf("\nParsing %s sessions", provider.Name())
		}

		sessionCount, err := syncProvider(provider, id, config, debugRaw, useUTC, statsCollector)
		totalSessionCount += sessionCount

		if err != nil {
			lastError = err
			slog.Error("Error syncing provider", "provider", id, "error", err)
		}

		// Print divider between provider sync summaries (not after the last one,
		// since the cloud sync output prints its own divider)
		isLast := idx == len(providersWithActivity)-1
		if !silent && !onlyCloudSync && !onlyStats && sessionCount > 0 && !isLast {
			fmt.Println("────────────")
		}
	}

	// Flush all accumulated statistics to disk in a single write
	if err := statsCollector.Flush(); err != nil {
		slog.Warn("Failed to flush statistics", "error", err)
	}

	// Show statistics path once after all providers are done (only in --only-stats mode)
	if totalSessionCount > 0 && !silent && onlyStats {
		fmt.Printf("\n📊 Statistics collected: %s\n", config.GetStatisticsPath())
	}

	return lastError
}

// syncSingleProvider syncs a specific provider
func syncSingleProvider(registry *factory.Registry, providerID string, cmd *cobra.Command) error {
	// Get debug-raw flag value
	debugRaw, _ := cmd.Flags().GetBool("debug-raw")
	useUTC := !localTimeZone

	provider, err := registry.Get(providerID)
	if err != nil {
		// Provider not found - show helpful error
		fmt.Printf("❌ Provider '%s' is not a valid provider implementation\n\n", providerID)

		ids := registry.ListIDs()
		if len(ids) > 0 {
			fmt.Println("The registered providers are:")
			for _, id := range ids {
				if p, _ := registry.Get(id); p != nil {
					fmt.Printf("  • %s - %s\n", id, p.Name())
				}
			}
			fmt.Println("\nExample: specstory sync " + ids[0])
		}
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		slog.Error("Failed to get current working directory", "error", err)
		return err
	}

	// Check if provider has activity, with helpful output if not
	if !provider.DetectAgent(cwd, true) {
		// Provider already output helpful message
		return nil
	}

	// Setup output configuration
	config, err := utils.SetupOutputConfig(outputDir, debugDir)
	if err != nil {
		return err
	}

	// Initialize project identity
	identityManager := utils.NewProjectIdentityManager(cwd)
	if _, err := identityManager.EnsureProjectIdentity(); err != nil {
		slog.Error("Failed to ensure project identity", "error", err)
	}

	// Check authentication for cloud sync
	cmdpkg.CheckAndWarnAuthentication(noCloudSync || onlyStats)

	// Ensure history directory exists
	if err := utils.EnsureHistoryDirectoryExists(config); err != nil {
		return err
	}

	// Preload session sizes for batch sync optimization
	preloadBulkSessionSizesIfNeeded(identityManager)

	// Create statistics collector for this provider
	statsCollector := sessionpkg.NewStatisticsCollector(config.GetStatisticsPath())

	if !silent {
		fmt.Printf("\nParsing %s sessions", provider.Name())
	}

	// Perform the sync
	sessionCount, syncErr := syncProvider(provider, providerID, config, debugRaw, useUTC, statsCollector)

	// Flush statistics to disk
	if err := statsCollector.Flush(); err != nil {
		slog.Warn("Failed to flush statistics", "error", err)
	}

	// Show statistics path (only in --only-stats mode)
	if sessionCount > 0 && !silent && onlyStats {
		fmt.Printf("\n📊 Statistics collected: %s\n", config.GetStatisticsPath())
	}

	return syncErr
}

var syncCmd *cobra.Command

// Main entry point for the CLI
func main() {
	// Parse critical flags early by manually checking os.Args
	// This is necessary because cobra's ParseFlags doesn't work correctly before subcommands are added
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--console":
			console = true
		case "--log":
			logFile = true
		case "--debug":
			debug = true
		case "--silent":
			silent = true
		case "--no-version-check":
			noVersionCheck = true
		case "--no-telemetry-prompts":
			noTelemetryPrompts = true
		case "--output-dir":
			// Handle --output-dir <value> format (space-separated)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				outputDir = utils.ExpandTilde(args[i+1])
				i++ // Skip the value in next iteration
			}
		case "--telemetry-endpoint":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				telemetryEndpoint = args[i+1]
				i++ // Skip the value in next iteration
			}
		case "--telemetry-service-name":
			// Handle --telemetry-service-name <value> format (space-separated)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				telemetryServiceName = args[i+1]
				i++ // Skip the value in next iteration
			}
		case "--debug-dir":
			// Handle --debug-dir <value> format (space-separated)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				debugDir = utils.ExpandTilde(args[i+1])
				i++ // Skip the value in next iteration
			}
		case "--local-time-zone":
			localTimeZone = true
		}
		// Handle --output-dir=value format
		if strings.HasPrefix(arg, "--output-dir=") {
			outputDir = utils.ExpandTilde(strings.TrimPrefix(arg, "--output-dir="))
		}
		// Handle --debug-dir=value format
		if strings.HasPrefix(arg, "--debug-dir=") {
			debugDir = utils.ExpandTilde(strings.TrimPrefix(arg, "--debug-dir="))
		}
		// Handle --telemetry-endpoint=value format
		if strings.HasPrefix(arg, "--telemetry-endpoint=") {
			telemetryEndpoint = strings.TrimPrefix(arg, "--telemetry-endpoint=")
		}
		// Handle --telemetry-service-name=value format
		if strings.HasPrefix(arg, "--telemetry-service-name=") {
			telemetryServiceName = strings.TrimPrefix(arg, "--telemetry-service-name=")
		}
	}

	// Load configuration early (before logging setup) so TOML settings can affect logging
	// Priority: CLI flags > local project config > user-level config
	// Note: OTEL_* env vars take highest priority for telemetry
	cfg, cfgErr := config.Load(&config.CLIOverrides{
		OutputDir:            outputDir,
		LocalTimeZone:        localTimeZone,
		NoVersionCheck:       noVersionCheck,
		NoCloudSync:          noCloudSync,
		OnlyCloudSync:        onlyCloudSync,
		DebugDir:             debugDir,
		Console:              console,
		Log:                  logFile,
		Debug:                debug,
		Silent:               silent,
		TelemetryEndpoint:    telemetryEndpoint,
		TelemetryServiceName: telemetryServiceName,
		NoTelemetryPrompts:   noTelemetryPrompts,
	})
	if cfgErr != nil {
		// Use fallback empty config if load fails - will log error after logging is set up
		cfg = &config.Config{}
	}
	// Store config for use by command functions (e.g., provider commands in run)
	loadedConfig = cfg

	// Apply config values to flag variables so the rest of the code can use them unchanged.
	// This merges TOML config with CLI flags (CLI flags take precedence via config.Load).
	if cfg.GetArchiveRoot() != "" {
		outputDir = utils.ExpandTilde(cfg.GetArchiveRoot())
	}
	if cfg.GetDebugDir() != "" {
		debugDir = utils.ExpandTilde(cfg.GetDebugDir())
	}
	localTimeZone = cfg.IsLocalTimeZoneEnabled()
	noVersionCheck = !cfg.IsVersionCheckEnabled()
	noCloudSync = !cfg.IsCloudSyncEnabled()
	onlyCloudSync = !cfg.IsLocalSyncEnabled()
	console = cfg.IsConsoleEnabled()
	logFile = cfg.IsLogEnabled()
	debug = cfg.IsDebugEnabled()
	silent = cfg.IsSilentEnabled()

	noTelemetryPrompts = noTelemetryPrompts || cfg.IsTelemetryPromptsDisabled()

	// Set SPI debug dir override before any commands run
	if debugDir != "" {
		spi.SetDebugBaseDir(debugDir)
	}

	// Set up logging early before creating commands (which access the registry)
	if console || logFile {
		var logPath string
		if logFile {
			config, _ := utils.SetupOutputConfig(outputDir, debugDir)
			logPath = config.GetLogPath()
		}
		_ = log.SetupLogger(console, logFile, debug, logPath)
	} else {
		// Set up discard logger to prevent default slog output
		_ = log.SetupLogger(false, false, false, "")
	}

	// NOW create the commands - after logging is configured
	rootCmd = createRootCommand()
	runCmd = createRunCommand()
	watchCmd := cmdpkg.CreateWatchCommand(&cloudURL, localTimeZone, debugDir)
	syncCmd = createSyncCommand()
	ingestCmd := createIngestCommand()
	daemonCmd := createDaemonCommand()
	listCmd := cmdpkg.CreateListCommand()
	checkCmd := cmdpkg.CreateCheckCommand()
	versionCmd := cmdpkg.CreateVersionCommand(version)
	loginCmd := cmdpkg.CreateLoginCommand(&cloudURL)
	logoutCmd := cmdpkg.CreateLogoutCommand(&cloudURL)

	// Set version for the automatic version flag
	rootCmd.Version = version

	// Override the default version template to match our version command output
	rootCmd.SetVersionTemplate("{{.Version}} (SpecStory)")

	// Set our custom help command (for "specstory help")
	helpCmd := cmdpkg.CreateHelpCommand(rootCmd)
	rootCmd.SetHelpCommand(helpCmd)

	// Add the subcommands
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(watchCmd)
	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(ingestCmd)
	rootCmd.AddCommand(daemonCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(checkCmd)
	rootCmd.AddCommand(loginCmd)
	rootCmd.AddCommand(logoutCmd)

	// Global flags available on all commands
	// Use current variable values as defaults so config file values are preserved
	rootCmd.PersistentFlags().BoolVar(&console, "console", console, "enable error/warn/info output to stdout")
	rootCmd.PersistentFlags().BoolVar(&logFile, "log", logFile, "write error/warn/info output to ./.specstory/debug/debug.log")
	rootCmd.PersistentFlags().BoolVar(&debug, "debug", debug, "enable debug-level output (requires --console or --log)")
	rootCmd.PersistentFlags().BoolVar(&silent, "silent", silent, "suppress all non-error output")
	rootCmd.PersistentFlags().BoolVar(&noVersionCheck, "no-version-check", noVersionCheck, "skip checking for newer versions")
	rootCmd.PersistentFlags().StringVar(&cloudToken, "cloud-token", "", "use a SpecStory Cloud refresh token for this session (bypasses login)")
	_ = rootCmd.PersistentFlags().MarkHidden("cloud-token") // Hidden flag

	// Command-specific flags
	syncCmd.Flags().StringSliceP("session", "s", []string{}, "optional session IDs to sync (can be specified multiple times, provider-specific format)")
	syncCmd.Flags().BoolVar(&printToStdout, "print", printToStdout, "output session markdown to stdout instead of saving (requires -s flag)")
	syncCmd.Flags().StringVar(&outputDir, "output-dir", outputDir, "custom output directory for markdown files (default: ./.specstory/history)")
	syncCmd.Flags().StringVar(&debugDir, "debug-dir", debugDir, "custom output directory for debug data (default: ./.specstory/debug)")
	syncCmd.Flags().BoolVar(&noCloudSync, "no-cloud-sync", noCloudSync, "disable cloud sync functionality")
	syncCmd.Flags().BoolVar(&onlyCloudSync, "only-cloud-sync", onlyCloudSync, "skip local markdown file saves, only upload to cloud (requires authentication)")
	syncCmd.Flags().BoolVar(&onlyStats, "only-stats", onlyStats, "only update statistics, skip local markdown files and cloud sync")
	syncCmd.Flags().StringVar(&cloudURL, "cloud-url", "", "override the default cloud API base URL")
	_ = syncCmd.Flags().MarkHidden("cloud-url") // Hidden flag
	syncCmd.Flags().Bool("debug-raw", false, "debug mode to output pretty-printed raw data files")
	_ = syncCmd.Flags().MarkHidden("debug-raw") // Hidden flag
	syncCmd.Flags().BoolVar(&localTimeZone, "local-time-zone", localTimeZone, "use local timezone for file name and content timestamps (when not present: UTC)")
	syncCmd.Flags().StringVar(&telemetryEndpoint, "telemetry-endpoint", "", "Open Telemetry Protocol (OTLP) gRPC collector endpoint (default is off, e.g., localhost:4317)")
	syncCmd.Flags().StringVar(&telemetryServiceName, "telemetry-service-name", "", "override the default service name for telemetry, if telemetry is enabled")
	syncCmd.Flags().BoolVar(&noTelemetryPrompts, "no-telemetry-prompts", noTelemetryPrompts, "exclude prompt text from telemetry spans, if telemetry is enabled")

	runCmd.Flags().BoolVar(&provenanceEnabled, "provenance", false, "enable AI provenance tracking (correlate file changes to agent activity)")
	_ = runCmd.Flags().MarkHidden("provenance") // Hidden flag
	runCmd.Flags().StringP("command", "c", "", "custom agent execution command for the provider")
	runCmd.Flags().String("resume", "", "resume a specific session by ID")
	runCmd.Flags().StringVar(&outputDir, "output-dir", outputDir, "custom output directory for markdown files (default: ./.specstory/history)")
	runCmd.Flags().StringVar(&debugDir, "debug-dir", debugDir, "custom output directory for debug data (default: ./.specstory/debug)")
	runCmd.Flags().BoolVar(&noCloudSync, "no-cloud-sync", noCloudSync, "disable cloud sync functionality")
	runCmd.Flags().BoolVar(&onlyCloudSync, "only-cloud-sync", onlyCloudSync, "skip local markdown file saves, only upload to cloud (requires authentication)")
	runCmd.Flags().StringVar(&cloudURL, "cloud-url", "", "override the default cloud API base URL")
	_ = runCmd.Flags().MarkHidden("cloud-url") // Hidden flag
	runCmd.Flags().Bool("debug-raw", false, "debug mode to output pretty-printed raw data files")
	_ = runCmd.Flags().MarkHidden("debug-raw") // Hidden flag
	runCmd.Flags().BoolVar(&localTimeZone, "local-time-zone", localTimeZone, "use local timezone for file name and content timestamps (when not present: UTC)")
	runCmd.Flags().StringVar(&telemetryEndpoint, "telemetry-endpoint", "", "Open Telemetry Protocol (OTLP) gRPC collector endpoint (default is off, e.g., localhost:4317)")
	runCmd.Flags().StringVar(&telemetryServiceName, "telemetry-service-name", "", "override the default service name for telemetry, if telemetry is enabled")
	runCmd.Flags().BoolVar(&noTelemetryPrompts, "no-telemetry-prompts", noTelemetryPrompts, "exclude prompt text from telemetry spans, if telemetry is enabled")

	// Log config load error after logging is set up
	if cfgErr != nil {
		slog.Warn("Failed to load config file, using defaults", "error", cfgErr)
	}

	// Initialize telemetry (after logging is configured)
	if err := telemetry.Init(context.Background(), telemetry.Options{
		ServiceName: cfg.GetTelemetryServiceName(),
		Endpoint:    cfg.GetTelemetryEndpoint(),
		Enabled:     cfg.IsTelemetryEnabled(),
	}); err != nil {
		slog.Warn("Failed to initialize telemetry", "error", err)
	}
	// Shutdown flushes pending spans/metrics before closing providers.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := telemetry.Shutdown(shutdownCtx); err != nil {
			slog.Warn("Failed to shutdown telemetry", "error", err)
		}
	}()

	// Check for updates (blocking)
	utils.CheckForUpdates(version, noVersionCheck, silent)

	// Ensure proper cleanup and logging on exit
	defer func() {
		if r := recover(); r != nil {
			slog.Error("=== SpecStory PANIC ===", "panic", r)
			// Still try to wait for cloud sync even on panic
			_ = cloud.Shutdown(cloud.CloudSyncTimeout)
			log.CloseLogger()
			panic(r) // Re-panic after logging
		}
		// Wait for cloud sync operations to complete before exiting
		cloudStats := cloud.Shutdown(cloud.CloudSyncTimeout)

		if cloudStats != nil {
			// Calculate total attempted (all sessions that started sync)
			totalCloudSessions := cloudStats.SessionsAttempted

			// Display cloud sync stats if not in silent mode
			if !silent && totalCloudSessions > 0 {
				fmt.Println("────────────") // Visual divider from provider sync output

				// Determine if sync was complete or incomplete based on errors/timeouts
				if cloudStats.SessionsErrored > 0 || cloudStats.SessionsTimedOut > 0 {
					fmt.Printf("❌ Cloud sync incomplete! 📊 %s%d%s %s processed\n",
						log.ColorBoldCyan, totalCloudSessions, log.ColorReset, pluralSession(int(totalCloudSessions)))
				} else {
					fmt.Printf("☁️  Cloud sync complete! 📊 %s%d%s %s processed\n",
						log.ColorBoldCyan, totalCloudSessions, log.ColorReset, pluralSession(int(totalCloudSessions)))
				}
				fmt.Println()

				fmt.Printf("  ⏭️ %s%d %s up to date (skipped)%s\n",
					log.ColorCyan, cloudStats.SessionsSkipped, pluralSession(int(cloudStats.SessionsSkipped)), log.ColorReset)
				fmt.Printf("  ♻️ %s%d %s updated%s\n",
					log.ColorYellow, cloudStats.SessionsUpdated, pluralSession(int(cloudStats.SessionsUpdated)), log.ColorReset)
				fmt.Printf("  ✨ %s%d new %s created%s\n",
					log.ColorGreen, cloudStats.SessionsCreated, pluralSession(int(cloudStats.SessionsCreated)), log.ColorReset)

				// Only show errors if there were any
				if cloudStats.SessionsErrored > 0 {
					fmt.Printf("  ❌ %s%d %s errored%s\n",
						log.ColorRed, cloudStats.SessionsErrored, pluralSession(int(cloudStats.SessionsErrored)), log.ColorReset)
				}

				// Only show timed out sessions if there were any
				if cloudStats.SessionsTimedOut > 0 {
					fmt.Printf("  ⏱️  %s%d %s timed out%s\n",
						log.ColorRed, cloudStats.SessionsTimedOut, pluralSession(int(cloudStats.SessionsTimedOut)), log.ColorReset)
				}
				fmt.Println()

				// Display link to SpecStory Cloud (deep link to session if from run command)
				cwd, cwdErr := os.Getwd()
				if cwdErr == nil {
					identityManager := utils.NewProjectIdentityManager(cwd)
					if projectID, err := identityManager.GetProjectID(); err == nil {
						fmt.Printf("💡 Search and chat with your AI conversation history at:\n")
						if lastRunSessionID != "" {
							// Deep link to the specific session from run command
							fmt.Printf("   %shttps://cloud.specstory.com/projects/%s/sessions/%s%s\n\n",
								log.ColorBoldCyan, projectID, lastRunSessionID, log.ColorReset)
						} else {
							// Link to project overview for sync command
							fmt.Printf("   %shttps://cloud.specstory.com/projects/%s%s\n\n",
								log.ColorBoldCyan, projectID, log.ColorReset)
						}
					}
				}
			}
		}

		if console || logFile {
			slog.Info("=== SpecStory Exiting ===", "code", 0, "status", "normal termination")
		}
		log.CloseLogger()
	}()

	if err := fang.Execute(context.Background(), rootCmd, fang.WithVersion(version)); err != nil {
		// Check if we're running the check command by looking at the executed command
		executedCmd, _, _ := rootCmd.Find(os.Args[1:])
		if executedCmd == checkCmd {
			if console || logFile {
				slog.Error("=== SpecStory Exiting ===", "code", 2, "status", "agent execution failure")
				slog.Error("Error", "error", err)
			}
			// For check command, the error details are handled by checkSingleProvider/checkAllProviders
			// So we just exit silently here
			_ = cloud.Shutdown(cloud.CloudSyncTimeout)
			os.Exit(2)
		} else {
			if console || logFile {
				slog.Error("=== SpecStory Exiting ===", "code", 1, "status", "error")
				slog.Error("Error", "error", err)
			}
			fmt.Fprintln(os.Stderr) // Visual separation makes error output more noticeable

			// Only show usage for actual command/flag errors from Cobra
			// These are errors like "unknown command", "unknown flag", "invalid argument", etc.
			// For all other errors (authentication, network, file system, etc.), we should NOT show usage
			errMsg := err.Error()
			isCommandError := strings.Contains(errMsg, "unknown command") ||
				strings.Contains(errMsg, "unknown flag") ||
				strings.Contains(errMsg, "invalid argument") ||
				strings.Contains(errMsg, "required flag") ||
				strings.Contains(errMsg, "accepts") || // e.g., "accepts 1 arg(s), received 2"
				strings.Contains(errMsg, "no such flag") ||
				strings.Contains(errMsg, "flag needs an argument")

			if isCommandError {
				_ = rootCmd.Usage() // Ignore error; we're exiting anyway
				fmt.Println()       // Add visual separation after usage for better CLI readability
			}
			_ = cloud.Shutdown(cloud.CloudSyncTimeout)
			os.Exit(1)
		}
	}
}
