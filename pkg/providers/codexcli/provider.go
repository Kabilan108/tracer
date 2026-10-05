package codexcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tracer-ai/tracer-cli/pkg/log"
	"github.com/tracer-ai/tracer-cli/pkg/spi"
)

const (
	KB                    = 1024
	MB                    = 1024 * 1024
	maxReasonableLineSize = 250 * MB // 250MB sanity limit to prevent OOM from malformed or malicious files
)

// codexSessionMetaPayload captures the payload embedded in the Codex CLI session metadata record.
type codexSessionMetaPayload struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
}

// codexSessionMeta represents the metadata stored as the first line of a Codex CLI session JSONL file.
type codexSessionMeta struct {
	Type      string                  `json:"type"`
	Timestamp string                  `json:"timestamp"`
	Payload   codexSessionMetaPayload `json:"payload"`
}

// codexSessionInfo holds information about a discovered Codex CLI session.
type codexSessionInfo struct {
	SessionID   string
	SessionPath string
	Meta        *codexSessionMeta
}

// Provider implements the SPI Provider interface for the OpenAI Codex CLI.
type Provider struct{}

// NewProvider creates a new Codex CLI provider instance.
func NewProvider() *Provider {
	return &Provider{}
}

// Name returns the human-readable name of this provider.
func (p *Provider) Name() string {
	return "Codex CLI"
}

// buildCheckErrorMessage creates a user-facing error message tailored to the failure type.
func buildCheckErrorMessage(errorType string, codexCmd string, isCustom bool, stderrOutput string) string {
	var errorMsg strings.Builder

	switch errorType {
	case "not_found":
		fmt.Fprintf(&errorMsg, "Could not find Codex CLI at: %s\n\n", codexCmd)
		errorMsg.WriteString("How to fix:\n")
		if isCustom {
			errorMsg.WriteString("- Double-check the custom command/path.\n")
			errorMsg.WriteString("- Ensure the file exists and is executable.\n")
			errorMsg.WriteString("- If the binary lives elsewhere, use an absolute path.")
		} else {
			errorMsg.WriteString("- Check common installation locations:\n")
			errorMsg.WriteString("  Homebrew: /opt/homebrew/bin/codex (brew install codex)\n")
			errorMsg.WriteString("  npm: $(npm bin -g)/codex (npm install -g codex)\n")
			errorMsg.WriteString("- If installed already, ensure `codex` is in PATH.\n")
			errorMsg.WriteString("- Use `-c` to specify the full path.\n")
			errorMsg.WriteString("- Example: tracer config check codex -c \"/opt/local/bin/codex\"")
		}
	case "permission_denied":
		fmt.Fprintf(&errorMsg, "Permission denied when trying to run: %s\n\n", codexCmd)
		errorMsg.WriteString("How to fix:\n")
		fmt.Fprintf(&errorMsg, "- Ensure the binary is executable: chmod +x %s\n", codexCmd)
		errorMsg.WriteString("- Run the command manually to confirm it works outside Tracer.")
	case "no_output":
		errorMsg.WriteString("No version information from codex\n\n")
		errorMsg.WriteString("The command ran but produced no output.\n")
		errorMsg.WriteString("- Try running '" + codexCmd + " --version' directly.\n")
		errorMsg.WriteString("- If using a wrapper script, pass the real codex binary with -c.")
	default:
		fmt.Fprintf(&errorMsg, "Error running '%s --version'\n", codexCmd)
		if stderrOutput != "" {
			fmt.Fprintf(&errorMsg, "Details: %s\n", stderrOutput)
		}
		errorMsg.WriteString("\nTroubleshooting:\n")
		errorMsg.WriteString("- Make sure Codex CLI is correctly installed.\n")
		errorMsg.WriteString("- Run 'codex --version' directly in your terminal.\n")
		errorMsg.WriteString("- Install docs: https://developers.openai.com/codex/quickstart")
	}

	return errorMsg.String()
}

// Check verifies OpenAI Codex CLI installation and returns version info.
func (p *Provider) Check(customCommand string) spi.CheckResult {
	codexCmd, _ := parseCodexCommand(customCommand)
	isCustomCommand := customCommand != ""

	resolvedPath := codexCmd
	if !filepath.IsAbs(codexCmd) {
		if path, err := exec.LookPath(codexCmd); err == nil {
			resolvedPath = path
		}
	}

	versionOutput, versionFlag, stderrOutput, err := runCodexVersionCommand(codexCmd)
	if err != nil {
		errorType := classifyCheckError(err)
		errorMessage := buildCheckErrorMessage(errorType, codexCmd, isCustomCommand, stderrOutput)

		return spi.CheckResult{
			Success:      false,
			Version:      "",
			Location:     resolvedPath,
			ErrorMessage: errorMessage,
		}
	}

	if versionOutput == "" {
		errorType := "no_output"
		errorMessage := buildCheckErrorMessage(errorType, codexCmd, isCustomCommand, stderrOutput)

		return spi.CheckResult{
			Success:      false,
			Version:      "",
			Location:     resolvedPath,
			ErrorMessage: errorMessage,
		}
	}

	slog.Debug("Codex CLI check successful", "version", versionOutput, "location", resolvedPath, "flag", versionFlag)

	return spi.CheckResult{
		Success:      true,
		Version:      versionOutput,
		Location:     resolvedPath,
		ErrorMessage: "",
	}
}

// DetectAgent checks if OpenAI Codex CLI has been used in the given project path.
func (p *Provider) DetectAgent(projectPath string, helpOutput bool) bool {
	// Try to find sessions using the shared helper (stop on first match)
	sessions, err := findCodexSessions(projectPath, "", true)

	// Handle different error cases with helpful output
	if err != nil {
		// Check if it's a home directory error
		if strings.Contains(err.Error(), "home directory") {
			slog.Debug("DetectAgent: Failed to resolve user home directory", "error", err)
			if helpOutput {
				fmt.Println()
				log.UserWarn("Could not scan Codex CLI sessions for this directory.\n")
				log.UserMessage("Reason: failed to determine your home directory.\n")
				fmt.Println()
			}
			return false
		}

		// Check if it's a sessions directory error
		if strings.Contains(err.Error(), "sessions directory not accessible") || strings.Contains(err.Error(), "sessions root") {
			homeDir, _ := osUserHomeDir()
			sessionsRoot := codexSessionsRoot(homeDir)

			slog.Debug("DetectAgent: Codex sessions directory not accessible", "error", err)
			if helpOutput {
				fmt.Println()
				log.UserWarn("No Codex CLI sessions were found for this directory.\n")
				log.UserMessage("Codex CLI stores activity under ~/.codex/sessions/YYYY/MM/DD/. We couldn't find that directory.\n\n")
				log.UserMessage("To fix this:\n")
				log.UserMessage("  1. Start Codex CLI manually in a project directory\n")
				log.UserMessage("  2. Run `tracer sync codex` to backfill sessions\n")
				log.UserMessage("  3. Run `tracer watch codex` for continuous updates\n\n")
				log.UserMessage("Expected sessions directory: %s\n", sessionsRoot)
				fmt.Println()
			}
			return false
		}

		// Unknown error
		slog.Debug("DetectAgent: Error finding sessions", "error", err)
		return false
	}

	// Check if we found any sessions
	if len(sessions) > 0 {
		session := sessions[0]
		slog.Debug("DetectAgent: Codex CLI activity detected",
			"sessionID", session.SessionID,
			"sessionPath", session.SessionPath)
		return true
	}

	// No sessions found - provide helpful output
	if helpOutput {
		homeDir, _ := osUserHomeDir()
		sessionsRoot := codexSessionsRoot(homeDir)

		fmt.Println()
		log.UserWarn("No Codex CLI sessions were found for this directory.\n")
		log.UserMessage("Codex CLI hasn't saved a session with this working directory yet.\n")
		log.UserMessage("Codex stores sessions in ~/.codex/sessions/YYYY/MM/DD/ as JSONL files.\n\n")
		log.UserMessage("To fix this:\n")
		log.UserMessage("  1. Open Codex CLI manually in this project\n")
		log.UserMessage("  2. Run `tracer sync codex` to backfill sessions\n")
		log.UserMessage("  3. Run `tracer watch codex` for continuous updates\n\n")
		log.UserMessage("Checked sessions directory: %s\n", sessionsRoot)
		fmt.Println()
	}

	return false
}

// GetAgentChatSessions retrieves chat sessions for the given project path.
func (p *Provider) GetAgentChatSessions(projectPath string, debugRaw bool, progress spi.ProgressCallback) ([]spi.AgentChatSession, error) {
	var result []spi.AgentChatSession
	total, done := 0, 0
	err := p.StreamAgentChatSessions(context.Background(), projectPath, debugRaw, spi.SessionVisitor{
		Sources: func(n int) { total = n },
		Session: func(_ spi.SessionSource, session *spi.AgentChatSession) {
			result = append(result, *session)
		},
		Done: func(spi.SessionSource, error) {
			done++
			if progress != nil {
				progress(done, total)
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find codex sessions: %w", err)
	}
	return result, nil
}

// StreamAgentChatSessions parses one session file at a time, newest first, and reports its session.
// Each Codex session lives in exactly one rollout file, so the file is the source.
func (p *Provider) StreamAgentChatSessions(ctx context.Context, projectPath string, debugRaw bool, visitor spi.SessionVisitor) error {
	sessionsRoot, err := codexSessionsDir()
	if err != nil {
		// No sessions directory means no sessions, not a failure
		visitor.ReportSources(0)
		return nil
	}
	sessionPaths, err := codexSessionFiles(sessionsRoot)
	if err != nil {
		return err
	}
	visitor.ReportSources(len(sessionPaths))

	normalizedProjectPath := normalizeCodexPath(projectPath)
	for _, sessionPath := range sessionPaths {
		if err := ctx.Err(); err != nil {
			return err
		}

		stat, err := spi.StatFile(sessionPath)
		src := spi.SessionSource{Key: sessionPath, Files: []spi.FileStat{stat}}
		if err != nil {
			visitor.ReportDone(src, err)
			continue
		}
		if !visitor.ParseNeeded(src) {
			continue
		}

		session, err := parseCodexSessionFile(sessionPath, projectPath, normalizedProjectPath, debugRaw)
		if session != nil {
			visitor.ReportSession(src, session)
		}
		visitor.ReportDone(src, err)
	}
	return nil
}

// parseCodexSessionFile returns the session stored in sessionPath when it belongs to projectPath.
// Files that are not Codex sessions, or belong to other projects, yield nil without an error.
// Failing to read the file is an error.
// Why the distinction: the engine records an unchanged source that yielded no
// session as done, so treating an unreadable file as "no session" would skip
// it on every later startup even after it becomes readable.
func parseCodexSessionFile(sessionPath string, projectPath string, normalizedProjectPath string, debugRaw bool) (*spi.AgentChatSession, error) {
	meta, err := loadCodexSessionMeta(sessionPath)
	if errors.Is(err, errNotCodexSession) {
		slog.Debug("parseCodexSessionFile: Not a Codex session", "path", sessionPath, "error", err)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load codex session meta: %w", err)
	}
	if !codexSessionMatches(meta, projectPath, normalizedProjectPath) {
		return nil, nil
	}

	sessionInfo := &codexSessionInfo{
		SessionID:   strings.TrimSpace(meta.Payload.ID),
		SessionPath: sessionPath,
		Meta:        meta,
	}
	session, err := processSessionToAgentChat(sessionInfo, projectPath, debugRaw)
	if err != nil {
		slog.Debug("parseCodexSessionFile: Failed to process session",
			"sessionID", sessionInfo.SessionID,
			"error", err)
		return nil, err
	}
	if session == nil {
		slog.Debug("parseCodexSessionFile: Skipping empty session", "sessionID", sessionInfo.SessionID)
	}
	return session, nil
}

// codexSessionMatches reports whether a session's working directory belongs to projectPath.
// An empty projectPath means global mode: every session with a working directory matches.
func codexSessionMatches(meta *codexSessionMeta, projectPath string, normalizedProjectPath string) bool {
	normalizedCWD := normalizeCodexPath(meta.Payload.CWD)
	if normalizedCWD == "" {
		slog.Debug("codexSessionMatches: Session meta missing cwd", "sessionID", strings.TrimSpace(meta.Payload.ID))
		return false
	}

	if strings.TrimSpace(projectPath) == "" {
		return true
	}
	if normalizedProjectPath != "" {
		return normalizedCWD == normalizedProjectPath || strings.EqualFold(normalizedCWD, normalizedProjectPath)
	}
	return normalizedCWD == projectPath || strings.EqualFold(normalizedCWD, projectPath)
}

// GetAgentChatSession retrieves one Codex CLI chat session by ID.
func (p *Provider) GetAgentChatSession(projectPath string, sessionID string, debugRaw bool) (*spi.AgentChatSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}

	sessions, err := findCodexSessions(projectPath, sessionID, false)
	if err != nil {
		if strings.Contains(err.Error(), "sessions directory not accessible") ||
			strings.Contains(err.Error(), "sessions root") ||
			strings.Contains(err.Error(), "home directory") {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to find codex session: %w", err)
	}
	if len(sessions) == 0 {
		return nil, nil
	}

	session, err := processSessionToAgentChat(&sessions[0], projectPath, debugRaw)
	if err != nil {
		return nil, fmt.Errorf("failed to process codex session %q: %w", sessionID, err)
	}
	return session, nil
}

// WatchAgent watches for Codex CLI agent activity and calls the callback with AgentChatSession
// Does NOT execute the agent - only watches for existing activity
// Runs until error or context cancellation (blocks indefinitely)
func (p *Provider) WatchAgent(ctx context.Context, projectPath string, debugRaw bool, sessionCallback func(*spi.AgentChatSession)) error {
	return p.WatchAgentWithCatchUp(ctx, projectPath, debugRaw, nil, sessionCallback)
}

// WatchAgentWithCatchUp is WatchAgent with a catch-up pass; see spi.CatchUpWatcher.
func (p *Provider) WatchAgentWithCatchUp(ctx context.Context, projectPath string, debugRaw bool, catchUp func(), sessionCallback func(*spi.AgentChatSession)) error {
	slog.Info("WatchAgent: Starting Codex CLI activity monitoring",
		"projectPath", projectPath,
		"debugRaw", debugRaw)
	slog.Info("WatchAgent: Initializing Codex session monitoring")

	wrappedCallback := func(agentChatSession *spi.AgentChatSession) {
		slog.Debug("WatchAgent: Received AgentChatSession",
			"sessionID", agentChatSession.SessionID,
			"hasSessionData", agentChatSession.SessionData != nil)
		sessionCallback(agentChatSession)
	}

	if err := WatchForCodexSessions(ctx, projectPath, debugRaw, catchUp, wrappedCallback); err != nil {
		slog.Error("WatchAgent: Codex session watcher stopped", "error", err)
		return fmt.Errorf("watch codex sessions: %w", err)
	}

	return nil
}

// findCodexSessions traverses the Codex CLI sessions directory structure and finds
// sessions that match the given project path.
//
// Short-circuit behavior:
//   - If targetSessionID is provided: stops when that specific session is found (ignores stopOnFirst)
//   - If targetSessionID is empty and stopOnFirst is true: stops after finding first matching session
//   - If targetSessionID is empty and stopOnFirst is false: returns all matching sessions
func findCodexSessions(projectPath string, targetSessionID string, stopOnFirst bool) ([]codexSessionInfo, error) {
	normalizedProjectPath := normalizeCodexPath(projectPath)
	if normalizedProjectPath == "" {
		slog.Debug("findCodexSessions: Unable to normalize project path", "projectPath", projectPath)
	}

	sessionsRoot, err := codexSessionsDir()
	if err != nil {
		return nil, err
	}
	sessionPaths, err := codexSessionFiles(sessionsRoot)
	if err != nil {
		return nil, err
	}

	var sessions []codexSessionInfo
	for _, sessionPath := range sessionPaths {
		meta, err := loadCodexSessionMeta(sessionPath)
		if err != nil {
			slog.Debug("findCodexSessions: Failed to load Codex session meta", "path", sessionPath, "error", err)
			continue
		}
		if !codexSessionMatches(meta, projectPath, normalizedProjectPath) {
			continue
		}

		sessionID := strings.TrimSpace(meta.Payload.ID)
		// If we're searching for a specific session, skip sessions that don't match
		if targetSessionID != "" && sessionID != targetSessionID {
			continue
		}

		slog.Debug("findCodexSessions: Codex CLI session matched project",
			"sessionID", sessionID,
			"sessionPath", sessionPath)

		sessions = append(sessions, codexSessionInfo{
			SessionID:   sessionID,
			SessionPath: sessionPath,
			Meta:        meta,
		})

		// Short-circuit logic:
		// 1. If we have a target sessionID and this matches, we're done
		// 2. If stopOnFirst is true (and no target sessionID), return first match
		// 3. Otherwise, continue collecting all matching sessions
		if targetSessionID != "" || stopOnFirst {
			return sessions, nil
		}
	}

	return sessions, nil
}

// codexSessionsDir returns the Codex sessions root, failing with the
// "sessions directory not accessible" / "sessions root" errors callers recognize.
func codexSessionsDir() (string, error) {
	homeDir, err := osUserHomeDir()
	if err != nil {
		slog.Debug("codexSessionsDir: Failed to resolve user home directory", "error", err)
		return "", fmt.Errorf("failed to determine home directory: %w", err)
	}

	sessionsRoot := codexSessionsRoot(homeDir)
	rootInfo, err := os.Stat(sessionsRoot)
	if err != nil {
		slog.Debug("codexSessionsDir: Codex sessions directory not accessible", "path", sessionsRoot, "error", err)
		return "", fmt.Errorf("sessions directory not accessible: %w", err)
	}
	if !rootInfo.IsDir() {
		slog.Debug("codexSessionsDir: Codex sessions root is not a directory", "path", sessionsRoot)
		return "", fmt.Errorf("sessions root is not a directory: %s", sessionsRoot)
	}
	return sessionsRoot, nil
}

// codexSessionFiles lists session files under the YYYY/MM/DD tree of sessionsRoot,
// newest first: years, months, days and files each in descending name order.
// Unreadable subdirectories are skipped.
func codexSessionFiles(sessionsRoot string) ([]string, error) {
	yearEntries, err := readDirSortedDesc(sessionsRoot)
	if err != nil {
		slog.Debug("codexSessionFiles: Failed to read Codex sessions root", "path", sessionsRoot, "error", err)
		return nil, fmt.Errorf("failed to read sessions root: %w", err)
	}

	var sessionPaths []string
	for _, yearEntry := range yearEntries {
		if !yearEntry.IsDir() {
			continue
		}
		yearPath := filepath.Join(sessionsRoot, yearEntry.Name())
		monthEntries, err := readDirSortedDesc(yearPath)
		if err != nil {
			slog.Debug("codexSessionFiles: Failed to read Codex year directory", "path", yearPath, "error", err)
			continue
		}

		for _, monthEntry := range monthEntries {
			if !monthEntry.IsDir() {
				continue
			}
			monthPath := filepath.Join(yearPath, monthEntry.Name())
			dayEntries, err := readDirSortedDesc(monthPath)
			if err != nil {
				slog.Debug("codexSessionFiles: Failed to read Codex month directory", "path", monthPath, "error", err)
				continue
			}

			for _, dayEntry := range dayEntries {
				if !dayEntry.IsDir() {
					continue
				}
				dayPath := filepath.Join(monthPath, dayEntry.Name())
				sessionEntries, err := readDirSortedDesc(dayPath)
				if err != nil {
					slog.Debug("codexSessionFiles: Failed to read Codex day directory", "path", dayPath, "error", err)
					continue
				}

				for _, sessionEntry := range sessionEntries {
					if sessionEntry.IsDir() || filepath.Ext(sessionEntry.Name()) != ".jsonl" {
						continue
					}
					sessionPaths = append(sessionPaths, filepath.Join(dayPath, sessionEntry.Name()))
				}
			}
		}
	}
	return sessionPaths, nil
}

// readSessionRecords reads and parses all JSONL lines from a Codex CLI session file.
func readSessionRecords(sessionPath string) ([]map[string]interface{}, error) {
	file, err := os.Open(sessionPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open session file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	recordReader := codexRecordReader{sessionPath: sessionPath}

	// Use bufio.Reader instead of Scanner to handle arbitrarily large lines
	// Scanner has a token size limit (even with custom buffer), but Reader does not
	reader := bufio.NewReader(file)

	for {
		// Read line using ReadBytes which has no size limit
		line, err := reader.ReadBytes('\n')

		// EOF is expected at end of file, other errors are genuine failures
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("error reading line %d: %w", recordReader.lineNumber+1, err)
		}
		atEOF := err == io.EOF
		if atEOF && len(line) == 0 {
			break
		}

		if err := recordReader.consumeLine(bytes.TrimSuffix(line, []byte("\n"))); err != nil {
			return nil, err
		}
		if atEOF {
			break
		}
	}

	return recordReader.records, nil
}

// codexRecordReader decodes Codex session lines one at a time.
// Why a separate type: the watcher resumes a file where it last stopped and
// must decode appended lines with exactly the rules a full read applies.
type codexRecordReader struct {
	sessionPath string
	lineNumber  int
	records     []map[string]interface{}
}

// consumeLine decodes one line, without its trailing newline.
// Every line counts toward the line number, including empty ones, to match text editor line numbers.
func (r *codexRecordReader) consumeLine(line []byte) error {
	r.lineNumber++
	if len(line) == 0 {
		return nil
	}

	// Sanity check to prevent OOM from pathological files
	if len(line) > maxReasonableLineSize {
		slog.Warn("line exceeds reasonable size limit",
			"lineNumber", r.lineNumber,
			"sizeMB", len(line)/MB,
			"limitMB", maxReasonableLineSize/MB,
			"file", filepath.Base(r.sessionPath))
		return fmt.Errorf("line %d exceeds reasonable size limit (%d MB): refusing to process potentially malformed file",
			r.lineNumber, maxReasonableLineSize/MB)
	}

	// Log when processing unusually large lines (helps debug performance issues)
	if len(line) > 10*MB {
		slog.Debug("processing large JSONL line",
			"lineNumber", r.lineNumber,
			"sizeMB", len(line)/MB,
			"file", filepath.Base(r.sessionPath))
	}

	// Trim whitespace and skip empty lines
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}

	var record map[string]interface{}
	if err := json.Unmarshal(line, &record); err != nil {
		// Log warning and skip corrupted lines rather than failing entire parse
		slog.Warn("Skipping corrupted JSONL line",
			"file", filepath.Base(r.sessionPath),
			"line", r.lineNumber,
			"error", err)
		return nil
	}

	r.records = append(r.records, record)
	return nil
}

// errNotCodexSession marks a file whose content is not a Codex session, as
// opposed to a file that could not be read.
var errNotCodexSession = errors.New("not a codex session")

// errLineTooLong marks a line over the per-record size limit.
var errLineTooLong = errors.New("line exceeds reasonable size limit")

// loadCodexSessionMeta reads the first JSON line from a session file and parses the session metadata.
// Content that is not session metadata yields an error wrapping errNotCodexSession;
// open and read failures, and a first line over maxReasonableLineSize, are
// errors of the source, like a record a full read refuses.
func loadCodexSessionMeta(sessionPath string) (*codexSessionMeta, error) {
	return readCodexSessionMeta(sessionPath, maxReasonableLineSize)
}

func readCodexSessionMeta(sessionPath string, lineLimit int) (*codexSessionMeta, error) {
	file, err := os.Open(sessionPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()

	line, err := readLineLimited(bufio.NewReader(file), lineLimit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return parseCodexSessionMeta(line)
}

// readLineLimited reads up to and including the next newline, failing with
// errLineTooLong once the line, without its newline, exceeds limit bytes.
// Why not ReadBytes: it buffers the whole line before the size can be checked,
// so one huge first line in a file that is not a session at all would be held
// in memory just to be rejected. Why not Scanner: its token limit is far below
// the size of valid meta lines.
func readLineLimited(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if len(bytes.TrimSuffix(line, []byte("\n"))) > limit {
			return nil, fmt.Errorf("%w of %d bytes", errLineTooLong, limit)
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

// parseCodexSessionMeta parses the first line of a session file as session metadata.
func parseCodexSessionMeta(line []byte) (*codexSessionMeta, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, fmt.Errorf("%w: session meta is empty", errNotCodexSession)
	}

	var meta codexSessionMeta
	if err := json.Unmarshal(line, &meta); err != nil {
		return nil, fmt.Errorf("%w: failed to parse session meta: %v", errNotCodexSession, err)
	}

	if meta.Type != "session_meta" {
		return nil, fmt.Errorf("%w: unexpected record type: %s", errNotCodexSession, meta.Type)
	}

	return &meta, nil
}

// processSessionToAgentChat converts a codexSessionInfo into an AgentChatSession by reading
// the session data, generating SessionData, and optionally writing debug files. Returns nil if
// the session is empty or cannot be read.
func processSessionToAgentChat(sessionInfo *codexSessionInfo, workspaceRoot string, debugRaw bool) (*spi.AgentChatSession, error) {
	// Read the session data
	records, err := readSessionRecords(sessionInfo.SessionPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read session data: %w", err)
	}

	return agentChatFromRecords(sessionInfo, records, workspaceRoot, debugRaw)
}

// agentChatFromRecords builds an AgentChatSession from decoded session records.
// Returns nil if there are no records.
func agentChatFromRecords(sessionInfo *codexSessionInfo, records []map[string]interface{}, workspaceRoot string, debugRaw bool) (*spi.AgentChatSession, error) {
	// Skip empty sessions
	if len(records) == 0 {
		return nil, nil
	}

	// Get timestamp from metadata
	timestamp := sessionInfo.Meta.Timestamp

	// Generate slug from first user message
	firstUserMessage := findFirstUserMessage(records)
	slug := spi.GenerateFilenameFromUserMessage(firstUserMessage)
	if slug == "" {
		slog.Debug("processSessionToAgentChat: No user message for slug yet",
			"sessionID", sessionInfo.SessionID)
	} else {
		slog.Debug("processSessionToAgentChat: Generated slug from user message",
			"sessionID", sessionInfo.SessionID,
			"slug", slug)
	}

	// Generate SessionData from records
	sessionData, err := GenerateAgentSession(records, workspaceRoot)
	if err != nil {
		slog.Error("Failed to generate SessionData", "sessionId", sessionInfo.SessionID, "error", err)
		return nil, fmt.Errorf("failed to generate SessionData: %w", err)
	}

	// Write provider-specific debug files if requested
	if debugRaw {
		if err := writeDebugRawFiles(sessionInfo.SessionID, records); err != nil {
			slog.Debug("processSessionToAgentChat: Failed to write debug files",
				"sessionID", sessionInfo.SessionID,
				"error", err)
			// Don't fail the operation if debug output fails
		}
	}

	return &spi.AgentChatSession{
		SessionID:   sessionInfo.SessionID,
		CreatedAt:   timestamp,
		Slug:        slug,
		SessionData: sessionData,
	}, nil
}

// writeDebugRawFiles writes debug JSON files for a Codex CLI session.
// Each record is written as a numbered JSON file in .tracer/debug/<session-id>/
func writeDebugRawFiles(sessionID string, records []map[string]interface{}) error {
	// Get the debug directory path
	debugDir := spi.GetDebugDir(sessionID)

	// Create the debug directory
	if err := os.MkdirAll(debugDir, 0755); err != nil {
		return fmt.Errorf("failed to create debug directory: %w", err)
	}

	// Write each record as a pretty-printed JSON file
	for index, record := range records {
		// Create filename with 1-based index for readability
		filename := fmt.Sprintf("%d.json", index+1)
		debugPath := filepath.Join(debugDir, filename)

		// Pretty print the record
		prettyJSON, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			slog.Debug("writeDebugRawFiles: Failed to marshal record to JSON",
				"index", index,
				"error", err)
			continue
		}

		// Write the file
		if err := os.WriteFile(debugPath, prettyJSON, 0644); err != nil {
			slog.Debug("writeDebugRawFiles: Failed to write debug file",
				"path", debugPath,
				"error", err)
			continue
		}

		slog.Debug("writeDebugRawFiles: Wrote debug file",
			"path", debugPath,
			"index", index)
	}

	return nil
}

// findFirstUserMessage extracts the first user message from Codex CLI session records.
// Returns empty string if no user message is found.
func findFirstUserMessage(records []map[string]interface{}) string {
	for _, record := range records {
		// Get record type
		recordType, ok := record["type"].(string)
		if !ok || recordType != "event_msg" {
			continue
		}

		// Get payload
		payload, ok := record["payload"].(map[string]interface{})
		if !ok {
			continue
		}

		// Check if this is a user message
		payloadType, ok := payload["type"].(string)
		if !ok || payloadType != "user_message" {
			continue
		}

		// Extract the message content
		message, ok := payload["message"].(string)
		if !ok || message == "" {
			continue
		}

		// Found the first user message
		slog.Debug("findFirstUserMessage: Found first user message",
			"message", message[:min(len(message), 100)])
		return message
	}

	slog.Debug("findFirstUserMessage: No user message found in session")
	return ""
}

// ListAgentChatSessions retrieves lightweight session metadata without full parsing
// This is much faster than GetAgentChatSessions as it only reads minimal data from each session
func (p *Provider) ListAgentChatSessions(projectPath string) ([]spi.SessionMetadata, error) {
	// Find all sessions for this project (don't stop on first)
	sessions, err := findCodexSessions(projectPath, "", false)
	if err != nil {
		// If sessions directory doesn't exist, return empty list (not an error)
		if strings.Contains(err.Error(), "sessions directory not accessible") ||
			strings.Contains(err.Error(), "sessions root") ||
			strings.Contains(err.Error(), "home directory") {
			return []spi.SessionMetadata{}, nil
		}
		return nil, fmt.Errorf("failed to find codex sessions: %w", err)
	}

	// Extract metadata from each session
	result := make([]spi.SessionMetadata, 0, len(sessions))
	for _, sessionInfo := range sessions {
		metadata, err := extractCodexSessionMetadata(&sessionInfo)
		if err != nil {
			slog.Warn("Failed to extract session metadata",
				"sessionID", sessionInfo.SessionID,
				"path", sessionInfo.SessionPath,
				"error", err)
			continue
		}

		// Skip empty sessions (no metadata means empty session)
		if metadata == nil {
			slog.Debug("Skipping empty session", "sessionID", sessionInfo.SessionID)
			continue
		}

		result = append(result, *metadata)
	}

	return result, nil
}

// extractCodexSessionMetadata reads minimal data from a Codex CLI session to extract metadata
// Returns nil if the session is empty or has no user messages
func extractCodexSessionMetadata(sessionInfo *codexSessionInfo) (*spi.SessionMetadata, error) {
	file, err := os.Open(sessionInfo.SessionPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open session file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	reader := bufio.NewReader(file)
	var firstUserMessage string
	lineNum := 0

	// Read records until we find a user message or reach EOF.
	// Why: ReadString can return data AND io.EOF on the last line (no trailing newline),
	// so we always process the line first, then check for EOF once at the bottom.
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, fmt.Errorf("failed to read line: %w", readErr)
		}

		lineNum++
		line = strings.TrimSpace(line)

		if line != "" {
			// Parse JSON record
			var record map[string]interface{}
			if jsonErr := json.Unmarshal([]byte(line), &record); jsonErr != nil {
				slog.Warn("Skipping malformed JSONL line",
					"file", filepath.Base(sessionInfo.SessionPath),
					"line", lineNum,
					"error", jsonErr)
			} else if recordType, ok := record["type"].(string); ok && recordType == "event_msg" {
				if payload, ok := record["payload"].(map[string]interface{}); ok {
					if payloadType, ok := payload["type"].(string); ok && payloadType == "user_message" {
						if message, ok := payload["message"].(string); ok && message != "" {
							firstUserMessage = message
						}
					}
				}
			}
		}

		// Single exit: found what we need, or reached end of file
		if firstUserMessage != "" || readErr == io.EOF {
			break
		}
	}

	// If no user message found, session is empty
	if firstUserMessage == "" {
		return nil, nil
	}

	// Generate slug from first user message
	slug := spi.GenerateFilenameFromUserMessage(firstUserMessage)

	// Generate human-readable name from first user message
	name := spi.GenerateReadableName(firstUserMessage)

	return &spi.SessionMetadata{
		SessionID:     sessionInfo.SessionID,
		CreatedAt:     sessionInfo.Meta.Timestamp,
		Slug:          slug,
		Name:          name,
		WorkspaceRoot: strings.TrimSpace(sessionInfo.Meta.Payload.CWD),
	}, nil
}
