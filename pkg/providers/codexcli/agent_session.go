package codexcli

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/tracer-ai/tracer-cli/pkg/spi"
	"github.com/tracer-ai/tracer-cli/pkg/spi/schema"
)

// Type aliases for convenience - use the shared schema types
type (
	SessionData  = schema.SessionData
	ProviderInfo = schema.ProviderInfo
	Exchange     = schema.Exchange
	Message      = schema.Message
	ContentPart  = schema.ContentPart
	ToolInfo     = schema.ToolInfo
	Usage        = schema.Usage
)

// Codex-specific record structures for type safety

// CodexSessionMeta represents the first line of a Codex session JSONL file
type CodexSessionMeta struct {
	Type      string `json:"type"` // "session_meta"
	Timestamp string `json:"timestamp"`
	Payload   struct {
		ID        string `json:"id"`        // Session UUID
		Timestamp string `json:"timestamp"` // Session creation timestamp
		CWD       string `json:"cwd"`       // Working directory
	} `json:"payload"`
}

// CodexEventMsg represents user messages, agent messages, and agent reasoning
type CodexEventMsg struct {
	Type      string `json:"type"` // "event_msg"
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type    string `json:"type"`    // "user_message", "agent_message", "agent_reasoning"
		Message string `json:"message"` // For user_message and agent_message
		Text    string `json:"text"`    // For agent_reasoning
	} `json:"payload"`
}

// CodexResponseItem represents function calls and custom tool calls
type CodexResponseItem struct {
	Type      string `json:"type"` // "response_item"
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type      string `json:"type"` // "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output"
		Name      string `json:"name"` // Tool name
		CallID    string `json:"call_id"`
		Arguments string `json:"arguments"` // JSON string for function_call
		Input     string `json:"input"`     // Plain text for custom_tool_call
		Output    string `json:"output"`    // JSON string for outputs
	} `json:"payload"`
}

// CodexTurnContext represents model information
type CodexTurnContext struct {
	Type    string `json:"type"` // "turn_context"
	Payload struct {
		Model string `json:"model"`
	} `json:"payload"`
}

// PendingToolCall holds tool call information waiting for output
type PendingToolCall struct {
	message   *Message // The message containing this tool call
	toolInfo  *ToolInfo
	timestamp string
}

// GenerateAgentSession creates a SessionData from Codex CLI JSONL records
// The records should be in chronological order from the JSONL file
func GenerateAgentSession(records []map[string]interface{}, workspaceRoot string) (*SessionData, error) {
	slog.Info("GenerateAgentSession: Starting", "recordCount", len(records))

	if len(records) == 0 {
		return nil, fmt.Errorf("no records provided")
	}

	// Extract session metadata from first record (session_meta)
	sessionID, createdAt, cwd, err := extractSessionMetadata(records)
	if err != nil {
		return nil, fmt.Errorf("failed to extract session metadata: %w", err)
	}

	// Use provided workspaceRoot or fall back to CWD from session metadata
	if workspaceRoot == "" {
		workspaceRoot = cwd
	}

	slog.Info("GenerateAgentSession: Session metadata",
		"sessionID", sessionID,
		"createdAt", createdAt,
		"workspaceRoot", workspaceRoot)

	// Build exchanges from records
	exchanges, err := buildExchangesFromRecords(records, workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to build exchanges: %w", err)
	}

	// Assign exchangeId to each exchange (format: sessionId:index)
	for i := range exchanges {
		exchanges[i].ExchangeID = fmt.Sprintf("%s:%d", sessionID, i)
	}

	// Populate Summary and FormattedMarkdown for all tools
	for i := range exchanges {
		for j := range exchanges[i].Messages {
			msg := &exchanges[i].Messages[j]
			if msg.Tool != nil {
				summary, formattedMd := formatToolWithSummary(msg.Tool, workspaceRoot)
				if summary != "" {
					msg.Tool.Summary = &summary
				}
				if formattedMd != "" {
					msg.Tool.FormattedMarkdown = &formattedMd
				}
			}
		}
	}

	slog.Info("GenerateAgentSession: Built exchanges", "count", len(exchanges))

	metaPayload, _ := records[0]["payload"].(map[string]interface{})
	parentSessionID, subagentName := subagentInfo(metaPayload)

	sessionData := &SessionData{
		SchemaVersion: "1.0",
		Provider: ProviderInfo{
			ID:      "codex-cli",
			Name:    "Codex CLI",
			Version: "unknown", // Codex doesn't currently include version in JSONL
		},
		SessionID:       sessionID,
		CreatedAt:       createdAt,
		WorkspaceRoot:   workspaceRoot,
		ParentSessionID: parentSessionID,
		SubagentName:    subagentName,
		Exchanges:       exchanges,
	}

	return sessionData, nil
}

// extractSessionMetadata parses the session_meta record (first line)
func extractSessionMetadata(records []map[string]interface{}) (sessionID, createdAt, cwd string, err error) {
	if len(records) == 0 {
		return "", "", "", fmt.Errorf("no records to extract metadata from")
	}

	firstRecord := records[0]
	recordType, ok := firstRecord["type"].(string)
	if !ok || recordType != "session_meta" {
		return "", "", "", fmt.Errorf("first record is not session_meta: %v", recordType)
	}

	payload, ok := firstRecord["payload"].(map[string]interface{})
	if !ok {
		return "", "", "", fmt.Errorf("session_meta payload is missing or invalid")
	}

	sessionID, _ = payload["id"].(string)
	createdAt, _ = payload["timestamp"].(string)
	cwd, _ = payload["cwd"].(string)

	if sessionID == "" {
		return "", "", "", fmt.Errorf("session_meta is missing session ID")
	}

	if createdAt == "" {
		// Fall back to record timestamp
		createdAt, _ = firstRecord["timestamp"].(string)
	}

	return sessionID, createdAt, cwd, nil
}

// buildExchangesFromRecords groups JSONL records into exchanges
// An exchange starts with a user message and contains all subsequent agent responses and tool uses
func buildExchangesFromRecords(records []map[string]interface{}, workspaceRoot string) ([]Exchange, error) {
	var exchanges []Exchange
	var currentExchange *Exchange
	var currentModel string
	pendingTools := make(map[string]*PendingToolCall) // callID -> pending tool call

	// Why: newer Codex versions write each turn as an item_completed item
	// instead of the user_message, agent_message and agent_reasoning events.
	// Should a file carry both forms of the same turn, rendering both would
	// show it twice, so a legacy event is dropped when an item carries the
	// same text. Unmatched legacy events still render: a session started by
	// an older Codex and resumed by a newer one holds only the legacy form
	// for its early turns.
	itemTexts := itemTextCounts(records)

	appendText := func(index int, timestamp string, text codexText) {
		switch text.kind {
		case codexTextUser, codexTextInterAgent:
			// Each prompt, from a person or another agent, starts an exchange
			if currentExchange != nil && len(currentExchange.Messages) > 0 {
				exchanges = append(exchanges, *currentExchange)
			}
			// EndTime marks the latest activity, so a prompt that is not
			// answered yet still counts toward when the session ended.
			currentExchange = &Exchange{
				StartTime: timestamp,
				EndTime:   timestamp,
				Messages:  []Message{},
			}
			message := Message{
				ID:        fmt.Sprintf("u_%d", index),
				Timestamp: timestamp,
				Role:      "user",
				Content:   []ContentPart{{Type: "text", Text: text.text}},
			}
			if text.kind == codexTextInterAgent {
				// Why: another agent wrote it, so it must not count as a
				// user turn or become the session title.
				message.ID = fmt.Sprintf("ia_%d", index)
				message.Metadata = map[string]interface{}{"isMeta": true}
			}
			currentExchange.Messages = append(currentExchange.Messages, message)

		case codexTextAgent, codexTextReasoning:
			if currentExchange == nil {
				// Create exchange if missing (shouldn't happen)
				currentExchange = &Exchange{
					StartTime: timestamp,
					Messages:  []Message{},
				}
			}
			message := Message{
				ID:        fmt.Sprintf("a_%d", index),
				Timestamp: timestamp,
				Role:      "agent",
				Model:     currentModel,
				Content:   []ContentPart{{Type: "text", Text: text.text}},
			}
			if text.kind == codexTextReasoning {
				message.ID = fmt.Sprintf("r_%d", index)
				message.Content = []ContentPart{{Type: "thinking", Text: text.text}}
			}
			currentExchange.Messages = append(currentExchange.Messages, message)
			currentExchange.EndTime = timestamp
		}
	}

	for i, record := range records {
		recordType, _ := record["type"].(string)
		timestamp, _ := record["timestamp"].(string)

		if text := codexRecordText(record); text.kind != codexTextNone {
			key := text.key()
			if text.legacy && itemTexts[key] > 0 {
				itemTexts[key]--
			} else {
				appendText(i, timestamp, text)
			}
			continue
		}

		switch recordType {
		case "session_meta":
			// Skip session metadata - already processed
			continue

		case "turn_context":
			// Update current model for subsequent agent messages
			payload, ok := record["payload"].(map[string]interface{})
			if ok {
				if model, ok := payload["model"].(string); ok {
					currentModel = model
					slog.Debug("Updated current model", "model", currentModel)
				}
			}
			continue

		case "event_msg":
			payload, ok := record["payload"].(map[string]interface{})
			if !ok {
				continue
			}

			payloadType, _ := payload["type"].(string)

			switch payloadType {
			case "token_count":
				usage := extractUsageFromTokenCount(payload)
				if usage == nil {
					continue
				}

				if target := latestAgentMessage(exchanges, currentExchange); target != nil {
					target.Usage = usage
					slog.Debug("Attached token usage to agent message",
						"messageID", target.ID,
						"inputTokens", usage.InputTokens,
						"outputTokens", usage.OutputTokens,
						"cachedInputTokens", usage.CachedInputTokens,
						"reasoningOutputTokens", usage.ReasoningOutputTokens)
				}
			}

		case "response_item":
			// Handle function calls and custom tool calls
			payload, ok := record["payload"].(map[string]interface{})
			if !ok {
				continue
			}

			payloadType, _ := payload["type"].(string)

			switch payloadType {
			case "function_call":
				// Function call (e.g., shell command)
				if currentExchange == nil {
					currentExchange = &Exchange{
						StartTime: timestamp,
						Messages:  []Message{},
					}
				}

				toolName, _ := payload["name"].(string)
				argumentsJSON, _ := payload["arguments"].(string)
				callID, _ := payload["call_id"].(string)

				if toolName == "" {
					continue
				}

				// Parse arguments to extract input
				var input map[string]interface{}
				if argumentsJSON != "" {
					if err := json.Unmarshal([]byte(argumentsJSON), &input); err != nil {
						slog.Warn("Failed to parse function call arguments", "error", err, "arguments", argumentsJSON)
						input = map[string]interface{}{"raw": argumentsJSON}
					}
				}

				// Create tool message
				toolMsg := Message{
					ID:        fmt.Sprintf("t_%d", i),
					Timestamp: timestamp,
					Role:      "agent",
					Model:     currentModel,
					Tool: &ToolInfo{
						Name:   toolName,
						Type:   classifyToolType(toolName),
						UseID:  callID,
						Input:  input,
						Output: nil, // Will be filled when we find the output
					},
					PathHints: extractPathHints(toolName, input, "", workspaceRoot),
				}

				currentExchange.Messages = append(currentExchange.Messages, toolMsg)
				currentExchange.EndTime = timestamp

				// Track this tool call to match with output later
				if callID != "" {
					pendingTools[callID] = &PendingToolCall{
						message:   &currentExchange.Messages[len(currentExchange.Messages)-1],
						toolInfo:  currentExchange.Messages[len(currentExchange.Messages)-1].Tool,
						timestamp: timestamp,
					}
				}

			case "function_call_output":
				// Function call output - merge into the pending tool call
				callID, _ := payload["call_id"].(string)
				outputJSON, _ := payload["output"].(string)

				if callID == "" || outputJSON == "" {
					continue
				}

				// Find the pending tool call
				if pending, exists := pendingTools[callID]; exists {
					// Parse the output JSON
					var outputData map[string]interface{}
					if err := json.Unmarshal([]byte(outputJSON), &outputData); err == nil {
						pending.toolInfo.Output = outputData
					} else {
						pending.toolInfo.Output = map[string]interface{}{"raw": outputJSON}
					}
					delete(pendingTools, callID)
				}

			case "custom_tool_call":
				// Custom tool call (e.g., apply_patch)
				if currentExchange == nil {
					currentExchange = &Exchange{
						StartTime: timestamp,
						Messages:  []Message{},
					}
				}

				toolName, _ := payload["name"].(string)
				inputText, _ := payload["input"].(string)
				callID, _ := payload["call_id"].(string)

				if toolName == "" {
					continue
				}

				// For custom tools, input is plain text, not JSON
				input := map[string]interface{}{
					"input": inputText,
				}

				// Create tool message
				toolMsg := Message{
					ID:        fmt.Sprintf("ct_%d", i),
					Timestamp: timestamp,
					Role:      "agent",
					Model:     currentModel,
					Tool: &ToolInfo{
						Name:   toolName,
						Type:   classifyToolType(toolName),
						UseID:  callID,
						Input:  input,
						Output: nil, // Will be filled when we find the output
					},
					PathHints: extractPathHints(toolName, input, inputText, workspaceRoot),
				}

				currentExchange.Messages = append(currentExchange.Messages, toolMsg)
				currentExchange.EndTime = timestamp

				// Track this tool call to match with output later
				if callID != "" {
					pendingTools[callID] = &PendingToolCall{
						message:   &currentExchange.Messages[len(currentExchange.Messages)-1],
						toolInfo:  currentExchange.Messages[len(currentExchange.Messages)-1].Tool,
						timestamp: timestamp,
					}
				}

			case "custom_tool_call_output":
				// Custom tool call output - merge into the pending tool call
				callID, _ := payload["call_id"].(string)
				outputJSON, _ := payload["output"].(string)

				if callID == "" || outputJSON == "" {
					continue
				}

				// Find the pending tool call
				if pending, exists := pendingTools[callID]; exists {
					// Parse the output JSON
					var outputData map[string]interface{}
					if err := json.Unmarshal([]byte(outputJSON), &outputData); err == nil {
						pending.toolInfo.Output = outputData
					} else {
						pending.toolInfo.Output = map[string]interface{}{"raw": outputJSON}
					}
					delete(pendingTools, callID)
				}
			}
		}
	}

	// Add the last exchange if exists
	if currentExchange != nil && len(currentExchange.Messages) > 0 {
		exchanges = append(exchanges, *currentExchange)
	}

	return exchanges, nil
}

// latestAgentMessage returns the agent message a token_count event reports
// on: the latest agent message of the current turn, or nil if the turn has
// none yet.
// Why look past the current exchange: a message from another agent can
// arrive between a reply and its token_count. It opens a new exchange but
// does not start a new turn, so the search continues into earlier exchanges
// until it reaches a prompt from a person.
func latestAgentMessage(exchanges []Exchange, current *Exchange) *Message {
	candidates := make([]*Exchange, 0, len(exchanges)+1)
	if current != nil {
		candidates = append(candidates, current)
	}
	for i := len(exchanges) - 1; i >= 0; i-- {
		candidates = append(candidates, &exchanges[i])
	}

	var target *Message
	for _, exchange := range candidates {
		turnStarted := false
		for j := len(exchange.Messages) - 1; j >= 0 && target == nil && !turnStarted; j-- {
			message := &exchange.Messages[j]
			switch {
			case message.Role == "agent":
				target = message
			case !isInterAgentMessage(*message):
				turnStarted = true
			}
		}
		if target != nil || turnStarted {
			break
		}
	}
	return target
}

// isInterAgentMessage reports whether message came from another agent.
func isInterAgentMessage(message Message) bool {
	meta, _ := message.Metadata["isMeta"].(bool)
	return message.Role == "user" && meta
}

// codexTextKind classifies records that carry conversation text.
type codexTextKind int

const (
	codexTextNone codexTextKind = iota
	codexTextUser
	codexTextAgent
	codexTextReasoning
	// codexTextInterAgent is a message from another agent in a multi-agent
	// run, such as a parent assigning a task or a subagent reporting back.
	codexTextInterAgent
)

// codexText is the conversation text a record carries.
type codexText struct {
	kind codexTextKind
	text string
	// legacy marks the event_msg forms that older Codex versions wrote.
	legacy bool
	// parts holds a reasoning item's separate summaries, which text joins.
	parts []string
}

// codexRecordText returns the conversation text in record, in any of the
// forms Codex has written it. Records without text yield codexTextNone.
func codexRecordText(record map[string]interface{}) codexText {
	recordType, _ := record["type"].(string)
	payload, _ := record["payload"].(map[string]interface{})
	payloadType, _ := payload["type"].(string)

	var text codexText
	switch {
	case recordType == "event_msg" && payloadType == "item_completed":
		item, _ := payload["item"].(map[string]interface{})
		text = completedItemText(item)
	case recordType == "event_msg" && payloadType == "user_message":
		message, _ := payload["message"].(string)
		text = codexText{kind: codexTextUser, text: message, legacy: true}
	case recordType == "event_msg" && payloadType == "agent_message":
		message, _ := payload["message"].(string)
		text = codexText{kind: codexTextAgent, text: message, legacy: true}
	case recordType == "event_msg" && payloadType == "agent_reasoning":
		reasoning, _ := payload["text"].(string)
		text = codexText{kind: codexTextReasoning, text: reasoning, legacy: true}
	case recordType == "response_item" && payloadType == "agent_message":
		text = codexText{kind: codexTextInterAgent, text: interAgentText(payload)}
	}

	if strings.TrimSpace(text.text) == "" {
		text = codexText{}
	}
	return text
}

// completedItemText returns the text of a finished UserMessage, AgentMessage
// or Reasoning item.
func completedItemText(item map[string]interface{}) codexText {
	itemType, _ := item["type"].(string)
	var text codexText
	switch itemType {
	case "UserMessage":
		text = codexText{kind: codexTextUser, text: joinTextParts(item["content"])}
	case "AgentMessage":
		text = codexText{kind: codexTextAgent, text: joinTextParts(item["content"])}
	case "Reasoning":
		// summary_text is empty unless the model produced a reasoning summary;
		// the full reasoning is never written in readable form.
		summaries, _ := item["summary_text"].([]interface{})
		var parts []string
		for _, summary := range summaries {
			if summaryText, ok := summary.(string); ok && strings.TrimSpace(summaryText) != "" {
				parts = append(parts, summaryText)
			}
		}
		text = codexText{kind: codexTextReasoning, text: strings.Join(parts, "\n\n"), parts: parts}
	}
	return text
}

// joinTextParts concatenates the text parts of an item's content. Images and
// other non-text parts are skipped.
// Why concatenate without separators: Codex splits one message into parts
// that carry their own line breaks, so joining them restores the original.
func joinTextParts(content interface{}) string {
	parts, _ := content.([]interface{})
	var builder strings.Builder
	for _, part := range parts {
		partMap, _ := part.(map[string]interface{})
		// User message parts are typed "text" and agent message parts "Text"
		partType, _ := partMap["type"].(string)
		if !strings.EqualFold(partType, "text") {
			continue
		}
		partText, _ := partMap["text"].(string)
		builder.WriteString(partText)
	}
	return builder.String()
}

// interAgentText renders a message between agents. Codex keeps the routing
// header (message type, task and sender) as plain text but encrypts the
// payload of most messages, so only the header survives.
func interAgentText(payload map[string]interface{}) string {
	parts, _ := payload["content"].([]interface{})
	var builder strings.Builder
	encrypted := false
	for _, part := range parts {
		partMap, _ := part.(map[string]interface{})
		partType, _ := partMap["type"].(string)
		switch partType {
		case "input_text":
			partText, _ := partMap["text"].(string)
			builder.WriteString(partText)
		case "encrypted_content":
			encrypted = true
		}
	}

	text := strings.TrimRight(builder.String(), "\n")
	if encrypted {
		text += "\n[encrypted]"
	}
	return text
}

// codexTextKey identifies conversation text, so a legacy event can be
// matched to the item that carries the same turn.
type codexTextKey struct {
	kind codexTextKind
	text string
}

func (t codexText) key() codexTextKey {
	return codexTextKey{kind: t.kind, text: strings.TrimSpace(t.text)}
}

// itemTextCounts counts the conversation texts that records carry in item
// form. A reasoning item's summary parts also count one by one, because
// older versions wrote one agent_reasoning event per part.
func itemTextCounts(records []map[string]interface{}) map[codexTextKey]int {
	counts := make(map[codexTextKey]int)
	for _, record := range records {
		text := codexRecordText(record)
		if text.kind == codexTextNone || text.kind == codexTextInterAgent || text.legacy {
			continue
		}
		counts[text.key()]++
		if len(text.parts) > 1 {
			for _, part := range text.parts {
				counts[codexText{kind: text.kind, text: part}.key()]++
			}
		}
	}
	return counts
}

// subagentInfo returns the parent session and agent name that a subagent
// run's session_meta payload records. Sessions a person started have a
// string source ("cli", "vscode", "exec") and yield empty values. Subagent
// runs have a {"subagent": ...} source holding {"thread_spawn": {...}} for
// spawned agents, {"other": "guardian"} for approval reviewers, or a bare
// string for internal jobs such as memory consolidation.
func subagentInfo(metaPayload map[string]interface{}) (parentID, name string) {
	source, _ := metaPayload["source"].(map[string]interface{})
	switch subagent := source["subagent"].(type) {
	case string:
		name = subagent
	case map[string]interface{}:
		// Sorted so output stays stable if Codex ever records several kinds
		for _, kind := range slices.Sorted(maps.Keys(subagent)) {
			switch detail := subagent[kind].(type) {
			case string:
				name = detail
			case map[string]interface{}:
				parentID, _ = detail["parent_thread_id"].(string)
				name, _ = detail["agent_path"].(string)
				if name == "" {
					name, _ = detail["agent_nickname"].(string)
				}
			}
			if name == "" {
				name = kind
			}
		}
	}

	// Older versions record the parent only at the top level
	if name != "" && parentID == "" {
		parentID, _ = metaPayload["parent_thread_id"].(string)
	}
	return parentID, name
}

// formatToolWithSummary generates custom summary and formatted markdown for a Codex tool
// Returns (summary, formattedMarkdown) where summary is the custom content for <summary> tag
// and formattedMarkdown is additional content to display in the tool-use block
func formatToolWithSummary(tool *ToolInfo, workspaceRoot string) (string, string) {
	var summary string
	var formattedMd strings.Builder

	// Format tool input based on whether it's a function call or custom tool
	if tool.Input != nil {
		// Check if this is a custom tool (has string input in "input" field)
		if inputStr, ok := tool.Input["input"].(string); ok {
			// Custom tool with text input (e.g., apply_patch)
			formattedMd.WriteString(formatCustomToolCall(tool.Name, inputStr))
		} else if tool.Name == "shell_command" || tool.Name == "exec_command" {
			// Shell command: normalize "cmd" → "command" for exec_command
			toolInput := tool.Input
			if _, ok := toolInput["cmd"]; ok {
				if _, hasCommand := toolInput["command"]; !hasCommand {
					toolInput["command"] = toolInput["cmd"]
				}
			}
			inputJSON, _ := json.Marshal(toolInput)
			shellSummary, shellBody := formatShellWithSummary(string(inputJSON))
			if shellSummary != "" {
				// Single-line command goes in summary
				summary = fmt.Sprintf("Tool use: **%s** %s", tool.Name, shellSummary)
			}
			if shellBody != "" {
				formattedMd.WriteString(shellBody)
			}
		} else {
			// Other function calls with JSON arguments
			inputJSON, _ := json.Marshal(tool.Input)
			formattedMd.WriteString(formatToolCall(tool.Name, string(inputJSON)))
		}
	}

	// Format tool output if present
	if tool.Output != nil {
		if outputStr, ok := tool.Output["raw"].(string); ok {
			// Clean and truncate output if needed
			cleaned := strings.TrimSpace(outputStr)
			// Only show output if there's actual content
			if cleaned != "" {
				if formattedMd.Len() > 0 {
					formattedMd.WriteString("\n")
				}
				if len(cleaned) > 5000 {
					cleaned = cleaned[:5000] + "\n... (truncated)"
				}
				formattedMd.WriteString("```\n" + cleaned + "\n```")
			}
		}
	}

	return summary, strings.TrimSpace(formattedMd.String())
}

// classifyToolType maps Codex tool names to standard tool types
func classifyToolType(toolName string) string {
	switch toolName {
	case "shell", "shell_command", "exec_command": // `shell` is legacy
		return "shell"
	case "update_plan":
		return "task"
	case "view_image":
		return "read"
	case "apply_patch":
		return "write"
	case "list_mcp_resources", "list_mcp_resource_templates", "read_mcp_resource":
		return "generic"
	default:
		return "unknown"
	}
}

// extractPathHints extracts file paths from tool inputs.
// For apply_patch: parses patch format for file paths.
// For other tools: checks common path fields like "path", "file", etc.
func extractPathHints(toolName string, input map[string]interface{}, inputText string, workspaceRoot string) []string {
	var paths []string

	if toolName == "apply_patch" {
		// Parse patch format to extract file paths
		// Look for lines like "*** Modify File: path", "*** Create File: path", "*** Delete File: path"
		if inputText != "" {
			paths = extractPathsFromPatch(inputText, workspaceRoot)
		} else if inputStr, ok := input["input"].(string); ok {
			paths = extractPathsFromPatch(inputStr, workspaceRoot)
		}
	} else {
		// For other tools, check common path fields
		pathFields := []string{"path", "file", "filename", "file_path"}
		for _, field := range pathFields {
			if value, ok := input[field].(string); ok && value != "" {
				normalizedPath := spi.NormalizePath(value, workspaceRoot)
				if !contains(paths, normalizedPath) {
					paths = append(paths, normalizedPath)
				}
			}
		}

		// Extract paths from shell commands (redirect targets, file-creating commands)
		// exec_command uses "cmd", shell_command uses "command"
		command, _ := input["command"].(string)
		if command == "" {
			command, _ = input["cmd"].(string)
		}
		if command != "" {
			cwd, _ := input["workdir"].(string)
			if cwd == "" {
				cwd = workspaceRoot
			}
			shellPaths := spi.ExtractShellPathHints(command, cwd, workspaceRoot)
			for _, sp := range shellPaths {
				if !contains(paths, sp) {
					paths = append(paths, sp)
				}
			}
		}
	}

	return paths
}

// extractPathsFromPatch parses apply_patch input to find file paths
func extractPathsFromPatch(patchText string, workspaceRoot string) []string {
	var paths []string
	lines := strings.Split(patchText, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Look for patch markers - both "Create File" and "Add File" are valid
		markers := []string{
			"*** Modify File:",
			"*** Update File:",
			"*** Create File:",
			"*** Add File:",
			"*** Delete File:",
			"*** Rename File:",
			"*** Remove File:",
		}

		for _, marker := range markers {
			if strings.HasPrefix(line, marker) {
				path := strings.TrimSpace(strings.TrimPrefix(line, marker))
				if path != "" {
					normalizedPath := spi.NormalizePath(path, workspaceRoot)
					if !contains(paths, normalizedPath) {
						paths = append(paths, normalizedPath)
					}
				}
				break
			}
		}

		// For rename, also check for "*** New Name:" marker
		if strings.HasPrefix(line, "*** New Name:") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** New Name:"))
			if path != "" {
				normalizedPath := spi.NormalizePath(path, workspaceRoot)
				if !contains(paths, normalizedPath) {
					paths = append(paths, normalizedPath)
				}
			}
		}
	}

	return paths
}

// contains checks if a string slice contains a value
func contains(slice []string, value string) bool {
	for _, item := range slice {
		if item == value {
			return true
		}
	}
	return false
}

// extractUsageFromTokenCount extracts token usage from a token_count event's info.last_token_usage
// Codex CLI emits token_count events with the structure:
//
//	{
//	  "type": "event_msg",
//	  "payload": {
//	    "type": "token_count",
//	    "info": {
//	      "last_token_usage": { "input_tokens": N, "cached_input_tokens": N, "output_tokens": N, "reasoning_output_tokens": N, ... },
//	      "total_token_usage": { ... }
//	    }
//	  }
//	}
//
// We use last_token_usage for per-turn usage (total_token_usage is cumulative).
// Codex CLI uses its own token field names, stored in the Codex-specific fields of Usage.
func extractUsageFromTokenCount(payload map[string]interface{}) *Usage {
	info, ok := payload["info"].(map[string]interface{})
	if !ok {
		return nil
	}

	// Use last_token_usage for per-turn metrics (not cumulative)
	lastUsage, ok := info["last_token_usage"].(map[string]interface{})
	if !ok {
		return nil
	}

	return &Usage{
		// Common fields
		InputTokens:  schema.GetIntFromMap(lastUsage, "input_tokens"),
		OutputTokens: schema.GetIntFromMap(lastUsage, "output_tokens"),
		// Codex CLI specific fields
		CachedInputTokens:     schema.GetIntFromMap(lastUsage, "cached_input_tokens"),
		ReasoningOutputTokens: schema.GetIntFromMap(lastUsage, "reasoning_output_tokens"),
	}
}
