package claudecode

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

// Provider implements the SPI Provider interface for Claude Code
type Provider struct{}

// NewProvider creates a new Claude Code provider instance
func NewProvider() *Provider {
	return &Provider{}
}

// filterWarmupMessages removes warmup messages (sidechain messages before first real message)
func filterWarmupMessages(records []JSONLRecord) []JSONLRecord {
	// Find first non-sidechain message
	firstRealMessageIndex := -1
	for i, record := range records {
		if isSidechain, ok := record.Data["isSidechain"].(bool); !ok || !isSidechain {
			firstRealMessageIndex = i
			break
		}
	}

	// No real messages found
	if firstRealMessageIndex == -1 {
		return []JSONLRecord{}
	}

	// Return records starting from first real message
	return records[firstRealMessageIndex:]
}

// processSession filters warmup messages and converts a Session to an AgentChatSession
// Returns nil if the session is warmup-only (no real messages)
func processSession(session Session, workspaceRoot string, debugRaw bool) *spi.AgentChatSession {
	// Write debug files first (even for warmup-only sessions)
	if debugRaw {
		// Write debug files and get the record-to-file mapping
		_ = writeDebugRawFiles(session) // Unused but needed for side effect
	}

	// Filter warmup messages
	filteredRecords := filterWarmupMessages(session.Records)

	// Skip if no real messages remain after filtering warmup
	if len(filteredRecords) == 0 {
		slog.Debug("Skipping warmup-only session", "sessionId", session.SessionUuid)
		return nil
	}

	// Create session with filtered records
	filteredSession := Session{
		SessionUuid: session.SessionUuid,
		Records:     filteredRecords,
	}

	// Get timestamp from root record
	rootRecord := filteredSession.Records[0]
	timestamp := rootRecord.Data["timestamp"].(string)

	// Get the slug
	slug := FileSlugFromRootRecord(filteredSession)

	// Generate SessionData from filtered session
	sessionData, err := GenerateAgentSession(filteredSession, workspaceRoot)
	if err != nil {
		slog.Error("Failed to generate SessionData", "sessionId", session.SessionUuid, "error", err)
		return nil
	}

	// Convert session records to JSONL format for raw data
	// Note: Uses unfiltered session.Records (not filteredSession.Records) to preserve
	// all records including warmup messages in the raw data for complete audit trail
	var rawDataBuilder strings.Builder
	for _, record := range session.Records {
		jsonBytes, _ := json.Marshal(record.Data)
		rawDataBuilder.Write(jsonBytes)
		rawDataBuilder.WriteString("\n")
	}

	return &spi.AgentChatSession{
		SessionID:   session.SessionUuid,
		CreatedAt:   timestamp, // ISO 8601 timestamp
		Slug:        slug,
		SessionData: sessionData,
		RawData:     rawDataBuilder.String(),
	}
}

// Name returns the human-readable name of this provider
func (p *Provider) Name() string {
	return "Claude Code"
}

// buildCheckErrorMessage creates a user-facing error message tailored to the failure type.
func buildCheckErrorMessage(errorType string, claudeCmd string, isCustom bool, stderrOutput string) string {
	var errorMsg strings.Builder

	switch errorType {
	case "not_found":
		fmt.Fprintf(&errorMsg, "Could not find Claude Code at: %s\n\n", claudeCmd)
		errorMsg.WriteString("How to fix:\n")
		if isCustom {
			errorMsg.WriteString("- The specified path does not exist.\n")
			fmt.Fprintf(&errorMsg, "- Verify Claude Code is installed at %s.\n", claudeCmd)
			errorMsg.WriteString("- Confirm the path is typed correctly.")
		} else {
			errorMsg.WriteString("- Install Claude Code:\n")
			errorMsg.WriteString("  https://docs.claude.com/en/docs/claude-code/quickstart\n")
			errorMsg.WriteString("- If installed already, check whether `claude` is in PATH.\n")
			errorMsg.WriteString("- Use `-c` to point to an absolute binary path.\n")
			errorMsg.WriteString("- Example: tracer config check claude -c \"~/.claude/local/claude\"")
		}
	case "permission_denied":
		fmt.Fprintf(&errorMsg, "Permission denied when trying to run: %s\n\n", claudeCmd)
		errorMsg.WriteString("How to fix:\n")
		fmt.Fprintf(&errorMsg, "- Check file permissions: chmod +x %s\n", claudeCmd)
		errorMsg.WriteString("- Try running with elevated permissions if needed.")
	case "unexpected_output":
		fmt.Fprintf(&errorMsg, "Unexpected output from 'claude -v': %s\n\n", strings.TrimSpace(stderrOutput))
		errorMsg.WriteString("This may not be Claude Code. Expected output containing '(Claude Code)'.\n")
		errorMsg.WriteString("- Make sure this is Claude Code and not another `claude` executable.\n")
		errorMsg.WriteString("- Install docs: https://docs.anthropic.com/en/docs/claude-code/quickstart")
	default:
		errorMsg.WriteString("Error running 'claude -v'\n")
		if stderrOutput != "" {
			fmt.Fprintf(&errorMsg, "Details: %s\n", stderrOutput)
		}
		errorMsg.WriteString("\nTroubleshooting:\n")
		errorMsg.WriteString("- Make sure Claude Code is properly installed.\n")
		errorMsg.WriteString("- Run `claude -v` directly in your terminal.\n")
		errorMsg.WriteString("- Install docs: https://docs.claude.com/en/docs/claude-code/quickstart")
	}

	return errorMsg.String()
}

// Check verifies Claude Code installation and returns version info
func (p *Provider) Check(customCommand string) spi.CheckResult {
	// Parse the command (no resume session for version check)
	claudeCmd, _ := parseClaudeCommand(customCommand, "")

	// Determine if custom command was used
	isCustomCommand := customCommand != ""

	// Resolve the actual path of the command
	resolvedPath := claudeCmd
	if !filepath.IsAbs(claudeCmd) {
		// Try to find the command in PATH
		if path, err := exec.LookPath(claudeCmd); err == nil {
			resolvedPath = path
		}
	}

	// Run claude -v to check version (ignore custom args for version check)
	cmd := exec.Command(claudeCmd, "-v")
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	if err := cmd.Run(); err != nil {
		// Track installation check failure
		errorType := "unknown"

		var execErr *exec.Error
		var pathErr *os.PathError

		// Check error types in order of specificity
		switch {
		case errors.As(err, &execErr) && execErr.Err == exec.ErrNotFound:
			errorType = "not_found"
		case errors.As(err, &pathErr):
			if errors.Is(pathErr.Err, os.ErrNotExist) {
				errorType = "not_found"
			} else if errors.Is(pathErr.Err, os.ErrPermission) {
				errorType = "permission_denied"
			}
		case errors.Is(err, os.ErrPermission):
			errorType = "permission_denied"
		}

		stderrOutput := strings.TrimSpace(errOut.String())
		errorMessage := buildCheckErrorMessage(errorType, claudeCmd, isCustomCommand, stderrOutput)

		return spi.CheckResult{
			Success:      false,
			Version:      "",
			Location:     resolvedPath,
			ErrorMessage: errorMessage,
		}
	}

	// Check if output contains "Claude Code"
	output := out.String()
	if !strings.Contains(output, "(Claude Code)") {
		errorMessage := buildCheckErrorMessage("unexpected_output", claudeCmd, isCustomCommand, output)

		return spi.CheckResult{
			Success:      false,
			Version:      "",
			Location:     resolvedPath,
			ErrorMessage: errorMessage,
		}
	}

	return spi.CheckResult{
		Success:      true,
		Version:      strings.TrimSpace(output),
		Location:     resolvedPath,
		ErrorMessage: "",
	}
}

// DetectAgent checks if Claude Code has been used in the given project directory
func (p *Provider) DetectAgent(projectPath string, helpOutput bool) bool {
	// Get the Claude Code project directory for the given path
	claudeProjectPath, err := GetClaudeCodeProjectDir(projectPath)
	if err != nil {
		slog.Debug("DetectAgent: Failed to get Claude Code project directory", "error", err)
		return false
	}

	// Check if the Claude Code project directory exists
	if info, err := os.Stat(claudeProjectPath); err == nil && info.IsDir() {
		slog.Debug("DetectAgent: Claude Code project found", "path", claudeProjectPath)
		return true
	}

	slog.Debug("DetectAgent: No Claude Code project found", "expected_path", claudeProjectPath)

	// If helpOutput is requested, provide helpful guidance
	if helpOutput {
		fmt.Println() // Add visual separation
		log.UserWarn("No Claude Code project found for this directory.\n")
		log.UserMessage("Claude Code hasn't created a project folder for your current directory yet.\n")
		log.UserMessage("This happens when Claude Code hasn't been run in this directory.\n\n")
		log.UserMessage("To fix this:\n")
		log.UserMessage("  1. Run Claude Code directly with `claude` in this directory\n")
		log.UserMessage("  2. Run `tracer sync claude` to backfill sessions\n")
		log.UserMessage("  3. Run `tracer watch claude` for continuous updates\n\n")
		log.UserMessage("Expected project folder: %s\n", claudeProjectPath)
		fmt.Println() // Add trailing newline
	}

	return false
}

// GetAgentChatSessions retrieves all chat sessions for the given project path
func (p *Provider) GetAgentChatSessions(projectPath string, debugRaw bool, progress spi.ProgressCallback) ([]spi.AgentChatSession, error) {
	if strings.TrimSpace(projectPath) == "" {
		projectDirs, err := ListClaudeCodeProjectDirs()
		if err != nil {
			return nil, err
		}
		all := make([]spi.AgentChatSession, 0)
		for _, projectDir := range projectDirs {
			sessions, err := parseProjectSessions(projectDir, "", debugRaw, nil)
			if err != nil {
				slog.Warn("GetAgentChatSessions: failed to parse Claude project", "projectDir", projectDir, "error", err)
				continue
			}
			all = append(all, sessions...)
		}
		return all, nil
	}

	claudeProjectDir, err := GetClaudeCodeProjectDir(projectPath)
	if err != nil {
		return nil, err
	}
	return parseProjectSessions(claudeProjectDir, projectPath, debugRaw, progress)
}

// GetAgentChatSession retrieves one Claude Code chat session by ID.
func (p *Provider) GetAgentChatSession(projectPath string, sessionID string, debugRaw bool) (*spi.AgentChatSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}

	projectDirs := []string{}
	if strings.TrimSpace(projectPath) == "" {
		dirs, err := ListClaudeCodeProjectDirs()
		if err != nil {
			return nil, err
		}
		projectDirs = dirs
	} else {
		claudeProjectDir, err := GetClaudeCodeProjectDir(projectPath)
		if err != nil {
			return nil, err
		}
		projectDirs = []string{claudeProjectDir}
	}

	for _, projectDir := range projectDirs {
		if _, err := os.Stat(projectDir); os.IsNotExist(err) {
			continue
		}

		parser := NewJSONLParser()
		if err := parser.ParseSingleSession(projectDir, sessionID); err != nil {
			if strings.Contains(err.Error(), "no session found") {
				continue
			}
			return nil, err
		}
		if len(parser.Sessions) == 0 {
			continue
		}
		session := processSession(parser.Sessions[0], projectPath, debugRaw)
		if session != nil {
			return session, nil
		}
	}

	return nil, nil
}

func parseProjectSessions(claudeProjectDir string, workspaceRoot string, debugRaw bool, progress spi.ProgressCallback) ([]spi.AgentChatSession, error) {
	if _, err := os.Stat(claudeProjectDir); os.IsNotExist(err) {
		return []spi.AgentChatSession{}, nil
	}

	parser := NewJSONLParser()
	if err := parser.ParseProjectSessionsWithProgress(claudeProjectDir, progress); err != nil {
		return nil, err
	}

	result := make([]spi.AgentChatSession, 0, len(parser.Sessions))
	for _, session := range parser.Sessions {
		if len(session.Records) == 0 {
			continue
		}
		chatSession := processSession(session, workspaceRoot, debugRaw)
		if chatSession != nil {
			result = append(result, *chatSession)
		}
	}
	return result, nil
}

// WatchAgent watches for Claude Code agent activity and calls the callback with AgentChatSession
// Does NOT execute the agent - only watches for existing activity
// Runs until error or context cancellation
func (p *Provider) WatchAgent(ctx context.Context, projectPath string, debugRaw bool, sessionCallback func(*spi.AgentChatSession)) error {
	slog.Info("WatchAgent: Starting Claude Code activity monitoring",
		"projectPath", projectPath,
		"debugRaw", debugRaw)

	if strings.TrimSpace(projectPath) == "" {
		slog.Info("WatchAgent: Using global event-driven mode for Claude projects")
		return watchClaudeProjects(ctx, debugRaw, sessionCallback)
	}

	claudeProjectDir, err := GetClaudeCodeProjectDir(projectPath)
	if err != nil {
		slog.Error("WatchAgent: Failed to get Claude Code project directory", "error", err)
		return fmt.Errorf("failed to get project directory: %w", err)
	}

	slog.Info("WatchAgent: Project directory found", "directory", claudeProjectDir)
	return watchClaudeProject(ctx, claudeProjectDir, debugRaw, sessionCallback)
}

// isSyntheticMessage checks if a message is synthetic/internal and should be skipped
// when looking for the first real user message.
// Currently filters warmup messages and Claude Code session title generation prompts.
func isSyntheticMessage(content string) bool {
	if strings.Contains(strings.ToLower(content), "warmup") {
		return true
	}
	// Claude Code wraps the real user prompt in <TEXTBLOCK> tags for title generation
	if strings.Contains(content, "<TEXTBLOCK>") {
		return true
	}
	return false
}

// findFirstUserMessage finds the first user message in a session for slug generation
// Returns empty string if no suitable user message is found
func findFirstUserMessage(session Session) string {
	for _, record := range session.Records {
		// Check if this is a user message
		if recordType, ok := record.Data["type"].(string); !ok || recordType != "user" {
			continue
		}

		// Skip meta user messages
		if isMeta, ok := record.Data["isMeta"].(bool); ok && isMeta {
			continue
		}

		// Extract content from message
		if message, ok := record.Data["message"].(map[string]interface{}); ok {
			if content, ok := message["content"].(string); ok && content != "" {
				if isSyntheticMessage(content) {
					continue
				}
				return content
			}
		}
	}
	return ""
}

// FileSlugFromRootRecord generates the slug part of the filename from the session
// Returns the human-readable slug derived from the first user message, or empty string if none
func FileSlugFromRootRecord(session Session) string {
	// Find the first user message and generate slug from it
	firstUserMessage := findFirstUserMessage(session)
	slog.Debug("FileSlugFromRootRecord: First user message",
		"sessionId", session.SessionUuid,
		"message", firstUserMessage)

	slug := spi.GenerateFilenameFromUserMessage(firstUserMessage)
	slog.Debug("FileSlugFromRootRecord: Generated slug",
		"sessionId", session.SessionUuid,
		"slug", slug)

	return slug
}

// ListAgentChatSessions retrieves lightweight session metadata without full parsing
// This is much faster than GetAgentChatSessions as it only reads minimal data from each session
func (p *Provider) ListAgentChatSessions(projectPath string) ([]spi.SessionMetadata, error) {
	projectDirs := []string{}
	if strings.TrimSpace(projectPath) == "" {
		dirs, err := ListClaudeCodeProjectDirs()
		if err != nil {
			return nil, err
		}
		projectDirs = dirs
	} else {
		claudeProjectDir, err := GetClaudeCodeProjectDir(projectPath)
		if err != nil {
			return nil, err
		}
		projectDirs = []string{claudeProjectDir}
	}

	var sessionFiles []string
	for _, projectDir := range projectDirs {
		if _, err := os.Stat(projectDir); os.IsNotExist(err) {
			continue
		}

		err := filepath.Walk(projectDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			sessionFiles = append(sessionFiles, path)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("failed to scan project directory: %w", err)
		}
	}

	// Extract metadata from each session file
	sessions := make([]spi.SessionMetadata, 0, len(sessionFiles))
	for _, filePath := range sessionFiles {
		metadata, err := extractSessionMetadata(filePath)
		if err != nil {
			slog.Warn("Failed to extract session metadata",
				"file", filePath,
				"error", err)
			continue
		}

		// Skip warmup-only sessions (no metadata means warmup-only)
		if metadata == nil {
			slog.Debug("Skipping warmup-only session", "file", filePath)
			continue
		}

		sessions = append(sessions, *metadata)
	}

	return sessions, nil
}

// extractSessionMetadata reads minimal data from a session file to extract metadata
// Returns nil if the session is warmup-only (no real messages)
func extractSessionMetadata(filePath string) (*spi.SessionMetadata, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open session file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	reader := bufio.NewReader(file)
	var sessionID string
	var timestamp string
	var firstUserMessage string
	var workspaceRoot string
	foundRealMessage := false

	// Read records until we find everything we need.
	// Why: ReadString can return data AND io.EOF on the last line (no trailing newline),
	// so we always process the line first, then check for EOF once at the bottom.
	lineNum := 0
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
					"file", filepath.Base(filePath),
					"line", lineNum,
					"error", jsonErr)
			} else {
				// Extract session ID (from any record)
				if sessionID == "" {
					if sid, ok := record["sessionId"].(string); ok {
						sessionID = sid
					}
				}
				if workspaceRoot == "" {
					if cwd, ok := record["cwd"].(string); ok && strings.TrimSpace(cwd) != "" {
						workspaceRoot = strings.TrimSpace(cwd)
					}
				}

				// Only process non-sidechain, non-system records for message extraction
				isSidechain, _ := record["isSidechain"].(bool)
				recordType, hasType := record["type"].(string)
				isSystemRecord := hasType && (recordType == "file-history-snapshot" || recordType == "file-change")

				if !isSidechain && !isSystemRecord {
					// This is the first real message record - extract timestamp
					if !foundRealMessage {
						foundRealMessage = true
						if ts, ok := record["timestamp"].(string); ok {
							timestamp = ts
						}
					}

					// Extract first user message for slug (if this is a user message)
					if firstUserMessage == "" && hasType && recordType == "user" {
						isMeta, _ := record["isMeta"].(bool)
						if !isMeta {
							if message, ok := record["message"].(map[string]interface{}); ok {
								if content, ok := message["content"].(string); ok && content != "" {
									if !isSyntheticMessage(content) {
										firstUserMessage = content
									}
								}
							}
						}
					}
				}
			}
		}

		// Single exit: found everything we need, or reached end of file
		if (sessionID != "" && timestamp != "" && firstUserMessage != "") || readErr == io.EOF {
			break
		}
	}

	// If no real message was found, this is a warmup-only session
	if !foundRealMessage {
		return nil, nil
	}

	// Generate slug from first user message
	slug := spi.GenerateFilenameFromUserMessage(firstUserMessage)

	// Generate human-readable name from first user message
	name := spi.GenerateReadableName(firstUserMessage)

	return &spi.SessionMetadata{
		SessionID:     sessionID,
		CreatedAt:     timestamp,
		Slug:          slug,
		Name:          name,
		WorkspaceRoot: workspaceRoot,
	}, nil
}
