package session

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxToolOutputBytes = 8192
	toolUseOpenPrefix  = "<tool-use data-tool-type=\""
	thinkOpenPrefix    = "<think><details>"
	thinkCloseTag      = "</details></think>"
)

type ToolOutputMode string

const (
	ToolOutputFull     ToolOutputMode = "full"
	ToolOutputNone     ToolOutputMode = "none"
	ToolOutputTruncate ToolOutputMode = "truncate"
)

type FilterOptions struct {
	ToolOutput    ToolOutputMode
	TruncateLines int
	TurnsOnly     bool
}

func ParseToolOutputFlag(value string) (ToolOutputMode, int, error) {
	switch value {
	case "full":
		return ToolOutputFull, 0, nil
	case "none":
		return ToolOutputNone, 0, nil
	}

	const prefix = "truncate:"
	if strings.HasPrefix(value, prefix) {
		count := strings.TrimPrefix(value, prefix)
		if isASCIIDigits(count) {
			lines, err := strconv.Atoi(count)
			if err == nil && lines > 0 {
				return ToolOutputTruncate, lines, nil
			}
		}
	}

	return "", 0, fmt.Errorf("invalid --tool-output %q: accepted forms are full, none, or truncate:N where N is a positive integer", value)
}

func isASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func FilterTranscript(markdown string, opts FilterOptions) string {
	if opts.ToolOutput == ToolOutputFull && !opts.TurnsOnly {
		return markdown
	}

	var filtered strings.Builder
	frontmatterEnd := findFrontmatterEnd(markdown)
	filtered.WriteString(markdown[:frontmatterEnd])
	remaining := markdown[frontmatterEnd:]
	for len(remaining) > 0 {
		blockType, start := nextTranscriptBlock(remaining, opts.TurnsOnly)
		if start < 0 {
			filtered.WriteString(remaining)
			break
		}

		filtered.WriteString(remaining[:start])
		match, ok := findBalancedBlockEnd(remaining, start, blockType)
		if !ok {
			// An incomplete block is ordinary archived text because filtering it would
			// risk discarding the remainder of the transcript.
			filtered.WriteString(remaining[start:])
			break
		}

		block := remaining[start:match.end]
		if !opts.TurnsOnly {
			filtered.WriteString(filterToolBlock(block, match.closeStart-start, opts))
		}
		remaining = remaining[match.end:]
		if opts.TurnsOnly {
			remaining = consumeOneNewline(remaining)
		}
	}

	return filtered.String()
}

func findFrontmatterEnd(markdown string) int {
	firstLineEnd := findLineEnd(markdown, 0, len(markdown))
	if strings.TrimSpace(markdown[:firstLineEnd]) != "---" {
		return 0
	}

	position := firstLineEnd
	for position < len(markdown) {
		lineEnd := findLineEnd(markdown, position, len(markdown))
		if strings.TrimSpace(markdown[position:lineEnd]) == "---" {
			return lineEnd
		}
		position = lineEnd
	}
	return 0
}

type transcriptBlock int

const (
	toolBlock transcriptBlock = iota
	thinkBlock
)

type blockMatch struct {
	closeStart int
	end        int
}

func nextTranscriptBlock(markdown string, includeThinking bool) (transcriptBlock, int) {
	for position := 0; position < len(markdown); {
		lineEnd := findLineEnd(markdown, position, len(markdown))
		line := markdown[position:lineEnd]
		if isBlockOpen(line, toolBlock) {
			return toolBlock, position
		}
		if includeThinking && isBlockOpen(line, thinkBlock) {
			return thinkBlock, position
		}
		position = lineEnd
	}
	return toolBlock, -1
}

func findBalancedBlockEnd(markdown string, start int, blockType transcriptBlock) (blockMatch, bool) {
	depth := 0
	for position := start; position < len(markdown); {
		lineEnd := findLineEnd(markdown, position, len(markdown))
		line := markdown[position:lineEnd]
		if isBlockOpen(line, blockType) {
			depth++
		}
		if isBlockClose(line, blockType) {
			depth--
			if depth == 0 {
				return blockMatch{closeStart: position, end: lineEnd}, true
			}
		}
		position = lineEnd
	}
	return blockMatch{}, false
}

func isBlockOpen(line string, blockType transcriptBlock) bool {
	if blockType == thinkBlock {
		return strings.HasPrefix(line, thinkOpenPrefix)
	}
	return strings.HasPrefix(line, toolUseOpenPrefix)
}

func isBlockClose(line string, blockType transcriptBlock) bool {
	if blockType == thinkBlock {
		return strings.TrimSpace(line) == thinkCloseTag
	}
	return strings.TrimSpace(line) == ToolUseCloseTag
}

func filterToolBlock(block string, closeStart int, opts FilterOptions) string {
	closeTag := block[closeStart:]
	openEnd := findLineEnd(block, 0, closeStart)
	summaryEnd := findSummaryEnd(block, openEnd, closeStart)
	if summaryEnd < 0 {
		summaryEnd = openEnd
	}
	stub := block[:summaryEnd]
	if opts.ToolOutput == ToolOutputNone {
		return joinFilteredBlock(stub, "", closeTag, false)
	}

	body := block[summaryEnd:closeStart]
	kept, truncated := truncateToolBody(body, opts.TruncateLines)
	return joinFilteredBlock(stub, kept, closeTag, truncated)
}

func findSummaryEnd(block string, start int, limit int) int {
	summaryStart := strings.Index(block[start:limit], "<summary")
	if summaryStart < 0 {
		return -1
	}
	summaryStart += start
	summaryClose := strings.Index(block[summaryStart:limit], "</summary>")
	if summaryClose < 0 {
		return -1
	}
	summaryClose += summaryStart + len("</summary>")
	return findLineEnd(block, summaryClose, limit)
}

func findLineEnd(value string, start int, limit int) int {
	limit = min(limit, len(value))
	if start < 0 {
		return 0
	}
	if start >= limit {
		return limit
	}
	newline := strings.IndexByte(value[start:limit], '\n')
	if newline < 0 {
		return limit
	}
	return start + newline + 1
}

func truncateToolBody(body string, lineLimit int) (string, bool) {
	lineEnd := 0
	for lines := 0; lines < lineLimit && lineEnd < len(body); lines++ {
		lineEnd = findLineEnd(body, lineEnd, len(body))
	}

	keptBytes := lineEnd
	if keptBytes > maxToolOutputBytes {
		keptBytes = maxToolOutputBytes
		if lastNewline := strings.LastIndexByte(body[:keptBytes], '\n'); lastNewline >= 0 {
			keptBytes = lastNewline + 1
		} else {
			for keptBytes > 0 && !utf8.ValidString(body[:keptBytes]) {
				keptBytes--
			}
		}
	}
	return body[:keptBytes], keptBytes < len(body)
}

func joinFilteredBlock(stub string, body string, closeTag string, truncated bool) string {
	var result strings.Builder
	result.WriteString(stub)
	result.WriteString(body)
	if truncated {
		ensureTrailingNewline(&result)
		if hasUnclosedMarkdownFence(body) {
			result.WriteString("```\n")
		}
		result.WriteString("... (truncated)\n")
	}
	result.WriteString(closeTag)
	return result.String()
}

func ensureTrailingNewline(result *strings.Builder) {
	value := result.String()
	if value != "" && !strings.HasSuffix(value, "\n") {
		result.WriteByte('\n')
	}
}

func hasUnclosedMarkdownFence(body string) bool {
	fences := 0
	for position := 0; position < len(body); {
		lineEnd := findLineEnd(body, position, len(body))
		if strings.HasPrefix(strings.TrimSpace(body[position:lineEnd]), "```") {
			fences++
		}
		position = lineEnd
	}
	return fences%2 == 1
}

func consumeOneNewline(value string) string {
	if strings.HasPrefix(value, "\r\n") {
		return value[2:]
	}
	if strings.HasPrefix(value, "\n") {
		return value[1:]
	}
	return value
}
