package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	cmdpkg "github.com/tracer-ai/tracer-cli/pkg/cmd"
	"github.com/tracer-ai/tracer-cli/pkg/config"
	"github.com/tracer-ai/tracer-cli/pkg/engine"
	"github.com/tracer-ai/tracer-cli/pkg/log"
	"github.com/tracer-ai/tracer-cli/pkg/spi"
	"github.com/tracer-ai/tracer-cli/pkg/spi/factory"
	"github.com/tracer-ai/tracer-cli/pkg/telemetry"
	"github.com/tracer-ai/tracer-cli/pkg/ui"
	"github.com/tracer-ai/tracer-cli/pkg/utils"
)

var version = "dev"

var (
	outputDir            string
	debugDir             string
	localTimeZone        bool
	console              bool
	logFile              bool
	debug                bool
	silent               bool
	telemetryEndpoint    string
	telemetryServiceName string

	loadedConfig       *config.Config
	telemetryInitError error
)

type listFlags struct {
	json     bool
	sessions bool
}

const helpTemplate = `{{with (or .Long .Short)}}{{. | trimTrailingWhitespaces}}

{{end}}{{if or .Runnable .HasSubCommands}}{{section "Usage"}}
{{formatUseLine .UseLine}}
{{end}}{{if .HasAvailableSubCommands}}

{{section "Commands"}}
{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}{{formatCommandUse .Use}}  {{.Short}}
{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

{{section "Flags"}}
{{formatFlagUsages .LocalFlags}}
{{end}}{{if .HasAvailableInheritedFlags}}

{{section "Flags"}}
{{formatFlagUsages .InheritedFlags}}
{{end}}{{if .HasAvailableSubCommands}}

{{section "More"}}
{{.CommandPath}} [command] --help
{{end}}`

const usageTemplate = `{{if .Runnable}}{{section "Usage"}}
{{formatUseLine .UseLine}}{{end}}{{if .HasAvailableSubCommands}}

{{section "Commands"}}
{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}{{formatCommandUse .Use}}  {{.Short}}
{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

{{section "Flags"}}
{{formatFlagUsages .LocalFlags}}
{{end}}{{if .HasAvailableInheritedFlags}}

{{section "Flags"}}
{{formatFlagUsages .InheritedFlags}}
{{end}}`

type providerSyncProgress struct {
	ScanStarted bool
	ScanDone    bool
	ScanError   string

	Total     int
	Processed int
	Created   int
	Updated   int
	Skipped   int
	Errors    int
}

type syncProgressTracker struct {
	mu sync.Mutex

	providers map[string]*providerSyncProgress
	order     []string

	start       time.Time
	interactive bool
	out         *os.File
	lastLines   int

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func newSyncProgressTracker(providerIDs []string, interactive bool, out *os.File) *syncProgressTracker {
	state := make(map[string]*providerSyncProgress, len(providerIDs))
	for _, providerID := range providerIDs {
		state[providerID] = &providerSyncProgress{}
	}

	return &syncProgressTracker{
		providers:   state,
		order:       providerIDs,
		start:       time.Now(),
		interactive: interactive,
		out:         out,
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
}

func (t *syncProgressTracker) onProviderScanStart(providerID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	state, ok := t.providers[providerID]
	if !ok {
		return
	}
	state.ScanStarted = true
}

func (t *syncProgressTracker) onProviderScanComplete(providerID string, totalSessions int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	state, ok := t.providers[providerID]
	if !ok {
		return
	}

	state.ScanDone = true
	state.Total = totalSessions
	if err != nil {
		state.ScanError = err.Error()
	}
}

func (t *syncProgressTracker) onSessionProcessed(providerID string, outcome engine.ProcessOutcome, processed int, total int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	state, ok := t.providers[providerID]
	if !ok {
		return
	}

	state.Processed = processed
	if total > 0 {
		state.Total = total
	}

	switch outcome {
	case engine.OutcomeCreated:
		state.Created++
	case engine.OutcomeUpdated:
		state.Updated++
	case engine.OutcomeSkipped:
		state.Skipped++
	case engine.OutcomeError:
		state.Errors++
	}
}

func (t *syncProgressTracker) startRendering(period time.Duration) func() {
	t.render(false)

	ticker := time.NewTicker(period)
	go func() {
		defer close(t.doneCh)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				t.render(false)
			case <-t.stopCh:
				t.render(true)
				return
			}
		}
	}()

	return t.stop
}

func (t *syncProgressTracker) stop() {
	t.stopOnce.Do(func() {
		close(t.stopCh)
	})
	<-t.doneCh
}

func (t *syncProgressTracker) render(clearOnly bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if clearOnly {
		if t.interactive {
			t.clearLocked()
		}
		return
	}

	lines := t.renderLinesLocked()
	if t.interactive {
		t.redrawLocked(lines)
		return
	}

	for _, line := range lines {
		fmt.Fprintln(t.out, line)
	}
	fmt.Fprintln(t.out)
}

func (t *syncProgressTracker) renderLinesLocked() []string {
	lines := make([]string, 0, len(t.order)+2)
	elapsed := time.Since(t.start).Round(time.Second)
	lines = append(lines, fmt.Sprintf("%s [%s]", ui.Section("Sync progress"), elapsed))

	overallTotal := 0
	overallProcessed := 0
	overallCreated := 0
	overallUpdated := 0
	overallSkipped := 0
	overallErrors := 0

	for _, providerID := range t.order {
		state := t.providers[providerID]
		if state == nil {
			continue
		}
		providerLabel := ui.Command(providerID)

		if state.ScanDone {
			overallTotal += state.Total
			overallProcessed += state.Processed
			overallCreated += state.Created
			overallUpdated += state.Updated
			overallSkipped += state.Skipped
			overallErrors += state.Errors
		}

		switch {
		case state.ScanError != "":
			lines = append(lines, fmt.Sprintf("  %s: %s (%s)", providerLabel, ui.Error("scan failed"), state.ScanError))
		case !state.ScanDone && state.ScanStarted:
			lines = append(lines, fmt.Sprintf("  %s: scanning sessions...", providerLabel))
		case !state.ScanDone:
			lines = append(lines, fmt.Sprintf("  %s: pending...", providerLabel))
		default:
			lines = append(lines, fmt.Sprintf("  %s: %d/%d processed (c:%d u:%d s:%d e:%d)",
				providerLabel,
				state.Processed,
				state.Total,
				state.Created,
				state.Updated,
				state.Skipped,
				state.Errors))
		}
	}

	lines = append(lines, fmt.Sprintf("  %s: %d/%d processed (c:%d u:%d s:%d e:%d)",
		ui.Bold("overall"),
		overallProcessed,
		overallTotal,
		overallCreated,
		overallUpdated,
		overallSkipped,
		overallErrors))

	return lines
}

func (t *syncProgressTracker) redrawLocked(lines []string) {
	if t.lastLines > 0 {
		fmt.Fprintf(t.out, "\x1b[%dA", t.lastLines)
	}
	for _, line := range lines {
		fmt.Fprintf(t.out, "\x1b[2K\r%s\n", line)
	}
	t.lastLines = len(lines)
}

func (t *syncProgressTracker) clearLocked() {
	if t.lastLines == 0 {
		return
	}

	fmt.Fprintf(t.out, "\x1b[%dA", t.lastLines)
	for i := 0; i < t.lastLines; i++ {
		fmt.Fprint(t.out, "\x1b[2K\r\n")
	}
	fmt.Fprintf(t.out, "\x1b[%dA", t.lastLines)
	t.lastLines = 0
}

func validateFlags() error {
	if console && silent {
		return utils.ValidationError{Message: "cannot use `console` and `silent` together. These are mutually exclusive"}
	}
	if debug && !console && !logFile {
		return utils.ValidationError{Message: "`debug` requires either `console` or `log` to be specified"}
	}
	return nil
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

func projectSegment(session *spi.AgentChatSession) string {
	if session != nil && session.SessionData != nil {
		root := strings.TrimSpace(session.SessionData.WorkspaceRoot)
		if root != "" {
			return sanitizeArchiveSegment(filepath.Base(root))
		}
	}
	return "unknown-project"
}

func archivePathBuilder(outputConfig *utils.OutputPathConfig) engine.PathBuilder {
	historyDir := outputConfig.GetHistoryDir()
	return func(providerID string, session *spi.AgentChatSession) string {
		providerSegment := sanitizeArchiveSegment(providerID)
		project := projectSegment(session)
		sessionSegment := "unknown-session"
		if session != nil && strings.TrimSpace(session.SessionID) != "" {
			sessionSegment = sanitizeArchiveSegment(session.SessionID)
		}
		return filepath.Join(historyDir, providerSegment, project, sessionSegment+".md")
	}
}

func engineOptionsFromOutputConfig(config *utils.OutputPathConfig, useUTC bool, debounce time.Duration) engine.Options {
	shouldProcessSession := func(providerID string, session *spi.AgentChatSession) bool {
		_ = providerID
		if loadedConfig == nil || session == nil || session.SessionData == nil {
			return true
		}
		projectPath := strings.TrimSpace(session.SessionData.WorkspaceRoot)
		if projectPath == "" {
			return true
		}
		return !loadedConfig.IsProjectExcluded(projectPath)
	}

	return engine.Options{
		HistoryDir:           config.GetHistoryDir(),
		StatisticsPath:       config.GetStatisticsPath(),
		StateDBPath:          config.GetRuntimeStateDBPath(),
		UseUTC:               useUTC,
		Debounce:             debounce,
		PathBuilder:          archivePathBuilder(config),
		ShouldProcessSession: shouldProcessSession,
	}
}

func sortedProviderIDs(providers map[string]spi.Provider) []string {
	ids := make([]string, 0, len(providers))
	for providerID := range providers {
		ids = append(ids, providerID)
	}
	sort.Strings(ids)
	return ids
}

func shouldUseInteractiveProgress() bool {
	stdoutInfo, stdoutErr := os.Stdout.Stat()
	stderrInfo, stderrErr := os.Stderr.Stat()
	if stdoutErr != nil || stderrErr != nil {
		return false
	}

	if stdoutInfo.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if stderrInfo.Mode()&os.ModeCharDevice == 0 {
		return false
	}

	term := strings.TrimSpace(os.Getenv("TERM"))
	return term != "" && term != "dumb"
}

func styleFirstToken(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return line
	}

	parts := strings.SplitN(trimmed, " ", 2)
	if len(parts) == 1 {
		return ui.Command(parts[0])
	}
	return ui.Command(parts[0]) + " " + parts[1]
}

func formatUseLine(useLine string) string {
	return styleFirstToken(useLine)
}

func formatCommandUse(use string) string {
	return styleFirstToken(use)
}

func formatFlagUsages(flagSet *pflag.FlagSet) string {
	if flagSet == nil {
		return ""
	}

	usages := strings.TrimRight(flagSet.FlagUsagesWrapped(120), "\n")
	if usages == "" {
		return ""
	}

	lines := strings.Split(usages, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trimmed, "-") {
			idx := strings.Index(trimmed, "  ")
			if idx > 0 {
				left := strings.TrimSpace(trimmed[:idx])
				right := strings.TrimLeft(trimmed[idx:], " ")
				lines[i] = fmt.Sprintf("%s  %s", ui.Command(left), right)
				continue
			}
			lines[i] = ui.Command(trimmed)
			continue
		}
		lines[i] = trimmed
	}
	return strings.Join(lines, "\n")
}

func configureHelpFormatting(rootCmd *cobra.Command) {
	cobra.AddTemplateFunc("section", ui.Section)
	cobra.AddTemplateFunc("formatUseLine", formatUseLine)
	cobra.AddTemplateFunc("formatCommandUse", formatCommandUse)
	cobra.AddTemplateFunc("formatFlagUsages", formatFlagUsages)

	rootCmd.SetHelpTemplate(helpTemplate)
	rootCmd.SetUsageTemplate(usageTemplate)
}

func resolveProviders(registry *factory.Registry, args []string) (map[string]spi.Provider, error) {
	providers := make(map[string]spi.Provider)

	if len(args) > 0 {
		providerID := strings.ToLower(strings.TrimSpace(args[0]))
		provider, err := registry.Get(providerID)
		if err != nil {
			return nil, err
		}
		providers[providerID] = provider
		return providers, nil
	}

	enabled := []string{}
	if loadedConfig != nil {
		enabled = loadedConfig.GetEnabledProviders()
	}

	var ids []string
	if len(enabled) > 0 {
		ids = enabled
	} else {
		ids = registry.ListIDs()
	}

	for _, id := range ids {
		provider, err := registry.Get(id)
		if err != nil {
			return nil, fmt.Errorf("provider %q is configured but not registered", id)
		}
		providers[id] = provider
	}

	if len(providers) == 0 {
		return nil, fmt.Errorf("no providers available")
	}
	return providers, nil
}

func acquireWatchLock(lockPath string) (*os.File, error) {
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open watch lock file: %w", err)
	}

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("watch already running (lock: %s)", lockPath)
	}

	if err := lockFile.Truncate(0); err == nil {
		_, _ = lockFile.WriteString(fmt.Sprintf("%d\n", os.Getpid()))
	}

	return lockFile, nil
}

func releaseWatchLock(lockFile *os.File) {
	if lockFile == nil {
		return
	}
	lockPath := lockFile.Name()
	_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	_ = lockFile.Close()
	_ = os.Remove(lockPath)
}

func createSyncCommand() *cobra.Command {
	registry := factory.GetRegistry()
	providerList := registry.GetProviderList()

	longDesc := "Backfill session markdown for all providers using the shared sync engine."
	if providerList != "No providers registered" {
		longDesc += "\n\nAvailable provider IDs: " + providerList + "."
	}

	cmd := &cobra.Command{
		Use:   "sync [provider-id]",
		Short: "Sync historical sessions into markdown archive",
		Long:  longDesc,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			debugRaw, _ := cmd.Flags().GetBool("debug-raw")
			useUTC := !localTimeZone

			outputConfig, err := utils.SetupOutputConfig(outputDir, debugDir)
			if err != nil {
				return err
			}
			if err := utils.EnsureStateDirectoryExists(outputConfig); err != nil {
				return err
			}
			if err := utils.EnsureHistoryDirectoryExists(outputConfig); err != nil {
				return err
			}

			providers, err := resolveProviders(registry, args)
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			opts := engineOptionsFromOutputConfig(outputConfig, useUTC, 0)
			var stopProgress func()
			if !silent {
				tracker := newSyncProgressTracker(sortedProviderIDs(providers), shouldUseInteractiveProgress(), os.Stderr)
				opts.OnProviderScanStart = tracker.onProviderScanStart
				opts.OnProviderScanComplete = tracker.onProviderScanComplete
				opts.OnSessionProcessed = tracker.onSessionProcessed
				stopProgress = tracker.startRendering(1 * time.Second)
			}

			summary, err := engine.RunIngest(ctx, opts, "", providers, debugRaw)
			if stopProgress != nil {
				stopProgress()
			}

			if !silent {
				fmt.Println()
				fmt.Printf("%s: %d created, %d updated, %d skipped, %d errors\n",
					ui.Success("Sync complete"),
					summary.Created,
					summary.Updated,
					summary.Skipped,
					summary.Errors)
				fmt.Println()
			}
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}

	cmd.Flags().Bool("debug-raw", false, "debug mode to output pretty-printed raw data files")
	_ = cmd.Flags().MarkHidden("debug-raw")
	return cmd
}

func createWatchCommand() *cobra.Command {
	registry := factory.GetRegistry()
	providerList := registry.GetProviderList()

	var debounce time.Duration
	cmd := &cobra.Command{
		Use:   "watch [provider-id]",
		Short: "Run continuous session watcher (historical + live updates)",
		Long: "Runs a foreground watcher that first backfills historical sessions, then watches for incremental updates across projects.\n\n" +
			"Press Ctrl+C to stop.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			debugRaw, _ := cmd.Flags().GetBool("debug-raw")
			useUTC := !localTimeZone

			outputConfig, err := utils.SetupOutputConfig(outputDir, debugDir)
			if err != nil {
				return err
			}
			if err := utils.EnsureStateDirectoryExists(outputConfig); err != nil {
				return err
			}
			if err := utils.EnsureHistoryDirectoryExists(outputConfig); err != nil {
				return err
			}

			providers, err := resolveProviders(registry, args)
			if err != nil {
				return err
			}

			if providerList != "No providers registered" && !silent {
				fmt.Println()
				fmt.Printf("%s\n", ui.Section("Starting watcher"))
				fmt.Println("historical + live updates")
				fmt.Println("Press Ctrl+C to stop.")
				fmt.Println()
			}

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			lockPath := filepath.Join(outputConfig.GetTracerDir(), "watch.lock")
			lockFile, err := acquireWatchLock(lockPath)
			if err != nil {
				return err
			}
			defer releaseWatchLock(lockFile)

			summary, err := engine.RunDaemon(ctx, engineOptionsFromOutputConfig(outputConfig, useUTC, debounce), "", providers, debugRaw)
			if !silent {
				fmt.Println()
				fmt.Printf("%s: %d created, %d updated, %d skipped, %d errors\n",
					ui.Warning("Watcher stopped"),
					summary.Created,
					summary.Updated,
					summary.Skipped,
					summary.Errors)
				fmt.Println()
			}
			return err
		},
	}

	cmd.Flags().DurationVar(&debounce, "debounce", 750*time.Millisecond, "debounce duration for write updates")
	cmd.Flags().Bool("debug-raw", false, "debug mode to output pretty-printed raw data files")
	_ = cmd.Flags().MarkHidden("debug-raw")
	return cmd
}

type sessionRow struct {
	ProviderID  string `json:"provider"`
	Project     string `json:"project"`
	ProjectPath string `json:"project_path"`
	SessionID   string `json:"session_id"`
	CreatedAt   string `json:"created_at"`
	Slug        string `json:"slug"`
}

type projectSummary struct {
	ProviderID   string `json:"provider"`
	Project      string `json:"project"`
	ProjectPath  string `json:"project_path"`
	SessionCount int    `json:"session_count"`
}

type projectBucket struct {
	providerID  string
	project     string
	projectPath string
	sessions    map[string]sessionRow
}

func createListCommand() *cobra.Command {
	registry := factory.GetRegistry()
	flags := listFlags{}

	cmd := &cobra.Command{
		Use:   "list [provider-id] [project]",
		Short: "List projects with session counts",
		Long: "Lists projects first (with session counts), aggregated across providers.\n\n" +
			"Optional args:\n" +
			"  provider-id: filter by provider\n" +
			"  project: filter by project name or path substring",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			providerArgs := []string{}
			if len(args) > 0 {
				providerArgs = append(providerArgs, args[0])
			}
			projectFilter := ""
			if len(args) > 1 {
				projectFilter = strings.ToLower(strings.TrimSpace(args[1]))
			}

			providers, err := resolveProviders(registry, providerArgs)
			if err != nil {
				return err
			}

			buckets := map[string]*projectBucket{}
			for providerID, provider := range providers {
				sessions, err := provider.ListAgentChatSessions("")
				if err != nil {
					slog.Warn("Failed to list sessions for provider", "provider", providerID, "error", err)
					continue
				}

				for i := range sessions {
					s := sessions[i]
					path := strings.TrimSpace(s.WorkspaceRoot)
					project := sanitizeArchiveSegment(filepath.Base(path))
					if project == "unknown" {
						project = "unknown-project"
					}

					if projectFilter != "" {
						if !strings.Contains(strings.ToLower(project), projectFilter) && !strings.Contains(strings.ToLower(path), projectFilter) {
							continue
						}
					}

					bucketKey := providerID + "|" + project + "|" + path
					bucket, ok := buckets[bucketKey]
					if !ok {
						bucket = &projectBucket{
							providerID:  providerID,
							project:     project,
							projectPath: path,
							sessions:    map[string]sessionRow{},
						}
						buckets[bucketKey] = bucket
					}

					bucket.sessions[s.SessionID] = sessionRow{
						ProviderID:  providerID,
						Project:     project,
						ProjectPath: path,
						SessionID:   s.SessionID,
						CreatedAt:   s.CreatedAt,
						Slug:        s.Slug,
					}
				}
			}

			projects := make([]projectSummary, 0, len(buckets))
			sessions := make([]sessionRow, 0)
			for _, bucket := range buckets {
				projects = append(projects, projectSummary{
					ProviderID:   bucket.providerID,
					Project:      bucket.project,
					ProjectPath:  bucket.projectPath,
					SessionCount: len(bucket.sessions),
				})
				for _, row := range bucket.sessions {
					sessions = append(sessions, row)
				}
			}

			sort.Slice(projects, func(i, j int) bool {
				if projects[i].SessionCount != projects[j].SessionCount {
					return projects[i].SessionCount > projects[j].SessionCount
				}
				if projects[i].ProviderID != projects[j].ProviderID {
					return projects[i].ProviderID < projects[j].ProviderID
				}
				return projects[i].Project < projects[j].Project
			})
			sort.Slice(sessions, func(i, j int) bool {
				if sessions[i].CreatedAt != sessions[j].CreatedAt {
					return sessions[i].CreatedAt > sessions[j].CreatedAt
				}
				return sessions[i].SessionID < sessions[j].SessionID
			})

			if flags.json {
				payload := map[string]interface{}{"projects": projects}
				if flags.sessions {
					payload["sessions"] = sessions
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(payload)
			}

			if len(projects) == 0 {
				if !silent {
					fmt.Println(ui.Warning("No projects found."))
				}
				return nil
			}

			fmt.Println(ui.Section(fmt.Sprintf("%-10s  %-24s  %-8s  %s", "PROVIDER", "PROJECT", "SESSIONS", "PROJECT PATH")))
			fmt.Println(strings.Repeat("-", 96))
			for _, p := range projects {
				fmt.Printf("%-10s  %-24s  %-8d  %s\n", p.ProviderID, p.Project, p.SessionCount, p.ProjectPath)
			}

			if flags.sessions {
				fmt.Println()
				fmt.Println(ui.Section(fmt.Sprintf("%-10s  %-24s  %-36s  %-20s  %s", "PROVIDER", "PROJECT", "SESSION ID", "CREATED", "SLUG")))
				fmt.Println(strings.Repeat("-", 128))
				for _, s := range sessions {
					created := s.CreatedAt
					if len(created) > 19 {
						created = created[:19]
					}
					fmt.Printf("%-10s  %-24s  %-36s  %-20s  %s\n", s.ProviderID, s.Project, s.SessionID, created, s.Slug)
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&flags.json, "json", false, "output as JSON")
	cmd.Flags().BoolVar(&flags.sessions, "sessions", false, "include per-session output")
	return cmd
}

func createRootCommand() *cobra.Command {
	registry := factory.GetRegistry()
	providerList := registry.GetProviderList()

	longDesc := "Tracer archives terminal coding sessions to markdown for Claude and Codex.\n\n"
	if providerList != "No providers registered" {
		longDesc += "Supported providers: " + providerList + "."
	}

	return &cobra.Command{
		Use:               "tracer [command]",
		Short:             "Archive terminal coding agent sessions",
		Long:              longDesc,
		SilenceUsage:      true,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if err := validateFlags(); err != nil {
				return err
			}

			outputConfig, err := utils.SetupOutputConfig(outputDir, debugDir)
			if err != nil {
				return err
			}
			spi.SetDebugBaseDir(outputConfig.GetDebugDir())

			var logPath string
			if logFile {
				if err := utils.EnsureStateDirectoryExists(outputConfig); err != nil {
					return err
				}
				logPath = outputConfig.GetLogPath()
			}
			if err := log.SetupLogger(console, logFile, debug, logPath); err != nil {
				return fmt.Errorf("failed to set up logger: %v", err)
			}
			log.SetSilent(silent)

			if telemetryInitError != nil {
				return telemetryInitError
			}
			if endpoint := strings.TrimSpace(telemetryEndpoint); endpoint != "" {
				telemetryInitError = telemetry.Init(context.Background(), telemetry.Options{
					Enabled:     true,
					Endpoint:    endpoint,
					ServiceName: telemetryServiceName,
				})
				if telemetryInitError != nil {
					return telemetryInitError
				}
			}
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}
}

func applyConfigDefaults(cfg *config.Config) {
	if cfg == nil {
		return
	}
	if cfg.GetArchiveRoot() != "" {
		outputDir = utils.ExpandTilde(cfg.GetArchiveRoot())
	}
	if cfg.GetDebugDir() != "" {
		debugDir = utils.ExpandTilde(cfg.GetDebugDir())
	}
	localTimeZone = cfg.IsLocalTimeZoneEnabled()
	console = cfg.IsConsoleEnabled()
	logFile = cfg.IsLogEnabled()
	debug = cfg.IsDebugEnabled()
	silent = cfg.IsSilentEnabled()
	telemetryEndpoint = cfg.GetTelemetryEndpoint()
	telemetryServiceName = cfg.GetTelemetryServiceName()
}

func main() {
	cfg, err := config.Load(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", ui.Error("Failed to load config"), err)
		os.Exit(1)
	}
	loadedConfig = cfg
	applyConfigDefaults(cfg)

	rootCmd := createRootCommand()
	configureHelpFormatting(rootCmd)
	syncCmd := createSyncCommand()
	watchCmd := createWatchCommand()
	listCmd := createListCommand()
	configCmd := cmdpkg.CreateConfigCommand()
	versionCmd := cmdpkg.CreateVersionCommand(version)

	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(watchCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(versionCmd)

	rootCmd.PersistentFlags().StringVar(&outputDir, "archive-root", outputDir, "archive root for markdown output (default: ~/.local/share/tracer/archive)")
	rootCmd.PersistentFlags().StringVar(&debugDir, "debug-dir", debugDir, "debug output directory (default: ~/.local/state/tracer/debug)")
	rootCmd.PersistentFlags().BoolVar(&localTimeZone, "local-time-zone", localTimeZone, "use local timezone for file names and timestamps")
	rootCmd.PersistentFlags().BoolVar(&console, "console", console, "enable error/warn/info output to stdout")
	rootCmd.PersistentFlags().BoolVar(&logFile, "log", logFile, "write error/warn/info output to ~/.local/state/tracer/debug.log")
	rootCmd.PersistentFlags().BoolVar(&debug, "debug", debug, "enable debug-level output (requires --console or --log)")
	rootCmd.PersistentFlags().BoolVar(&silent, "silent", silent, "suppress all non-error output")
	rootCmd.PersistentFlags().StringVar(&telemetryEndpoint, "telemetry-endpoint", telemetryEndpoint, "OTLP gRPC collector endpoint (default off)")
	rootCmd.PersistentFlags().StringVar(&telemetryServiceName, "telemetry-service-name", telemetryServiceName, "override telemetry service name")

	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		if !silent {
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "%s: %v\n", ui.Error("Error"), err)
			fmt.Fprintln(os.Stderr)
		}
		_ = telemetry.Shutdown(context.Background())
		os.Exit(1)
	}

	_ = telemetry.Shutdown(context.Background())
}
