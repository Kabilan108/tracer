package session

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/tracer-ai/tracer-cli/pkg/spi/schema"
	"gopkg.in/yaml.v3"
)

const frontmatterDelimiter = "---\n"

type Metadata struct {
	SessionID  string   `yaml:"session_id" json:"session_id"`
	Title      string   `yaml:"title" json:"title"`
	Host       string   `yaml:"host" json:"host"`
	CWD        string   `yaml:"cwd" json:"cwd"`
	Provider   string   `yaml:"provider" json:"provider"`
	Models     []string `yaml:"models" json:"models"`
	Started    string   `yaml:"started" json:"started"`
	Ended      string   `yaml:"ended" json:"ended"`
	UserTurns  int      `yaml:"user_turns" json:"user_turns"`
	AgentTurns int      `yaml:"agent_turns" json:"agent_turns"`
	ToolCalls  int      `yaml:"tool_calls" json:"tool_calls"`
	Outcome    string   `yaml:"outcome,omitempty" json:"outcome,omitempty"`
	Tags       []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	Path       string   `yaml:"-" json:"path,omitempty"`
}

type Annotations struct {
	Outcome string
	Tags    []string
}

func DeriveMetadata(data *schema.SessionData, host string) Metadata {
	title := deriveTitle(data)
	if title == "" {
		title = data.CreatedAt
	}
	metadata := Metadata{
		SessionID: data.SessionID,
		Title:     title,
		Host:      host,
		CWD:       strings.TrimSpace(data.WorkspaceRoot),
		Provider:  data.Provider.ID,
		Started:   data.CreatedAt,
		Ended:     data.CreatedAt,
		Models:    []string{},
	}
	if len(data.Exchanges) > 0 && data.Exchanges[0].StartTime != "" {
		metadata.Started = data.Exchanges[0].StartTime
	}
	if len(data.Exchanges) > 0 && data.Exchanges[len(data.Exchanges)-1].EndTime != "" {
		metadata.Ended = data.Exchanges[len(data.Exchanges)-1].EndTime
	} else if data.UpdatedAt != "" {
		metadata.Ended = data.UpdatedAt
	}

	models := make(map[string]struct{})
	for _, exchange := range data.Exchanges {
		for _, message := range exchange.Messages {
			if message.Model != "" {
				models[message.Model] = struct{}{}
			}
			if message.Tool != nil {
				metadata.ToolCalls++
			}
			if isSidechain(message) || isInternalMessage(message) {
				continue
			}
			switch message.Role {
			case schema.RoleUser:
				if hasSubstantiveText(message) {
					metadata.UserTurns++
				}
			case schema.RoleAgent:
				if hasSubstantiveText(message) {
					metadata.AgentTurns++
				}
			}
		}
	}
	for model := range models {
		metadata.Models = append(metadata.Models, model)
	}
	sort.Strings(metadata.Models)
	return metadata
}

func ParseFrontmatter(content []byte) (Metadata, []byte, error) {
	if !bytes.HasPrefix(content, []byte(frontmatterDelimiter)) {
		return Metadata{}, content, fmt.Errorf("frontmatter is missing")
	}
	rest := content[len(frontmatterDelimiter):]
	end := bytes.Index(rest, []byte("\n---\n"))
	if end < 0 {
		return Metadata{}, content, fmt.Errorf("frontmatter closing delimiter is missing")
	}

	var metadata Metadata
	if err := yaml.Unmarshal(rest[:end], &metadata); err != nil {
		return Metadata{}, content, fmt.Errorf("parse frontmatter: %w", err)
	}
	body := rest[end+len("\n---\n"):]
	body = bytes.TrimPrefix(body, []byte("\n"))
	return metadata, body, nil
}

func RenderFrontmatter(metadata Metadata) (string, error) {
	metadata.Tags = normalizeTags(metadata.Tags)
	data, err := yaml.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("marshal frontmatter: %w", err)
	}
	return frontmatterDelimiter + string(data) + frontmatterDelimiter + "\n", nil
}

func ExtractAnnotations(content []byte) Annotations {
	metadata, _, err := ParseFrontmatter(content)
	if err != nil {
		return Annotations{}
	}
	return Annotations{Outcome: metadata.Outcome, Tags: metadata.Tags}
}

func ApplyAnnotations(metadata Metadata, annotations Annotations) Metadata {
	metadata.Outcome = annotations.Outcome
	metadata.Tags = normalizeTags(annotations.Tags)
	return metadata
}

func normalizeTags(tags []string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		result = append(result, tag)
	}
	sort.Strings(result)
	return result
}

func deriveTitle(data *schema.SessionData) string {
	for _, exchange := range data.Exchanges {
		for _, message := range exchange.Messages {
			if message.Role != schema.RoleUser || isSidechain(message) || isInternalMessage(message) {
				continue
			}
			for _, part := range message.Content {
				if part.Type != schema.ContentTypeText {
					continue
				}
				text := strings.Join(strings.Fields(part.Text), " ")
				if text == "" {
					continue
				}
				const maxTitleRunes = 80
				runes := []rune(text)
				if len(runes) > maxTitleRunes {
					text = strings.TrimSpace(string(runes[:maxTitleRunes])) + "…"
				}
				return text
			}
		}
	}
	return ""
}

func hasSubstantiveText(message schema.Message) bool {
	for _, part := range message.Content {
		if part.Type == schema.ContentTypeText && strings.TrimSpace(part.Text) != "" {
			return true
		}
	}
	return false
}

func isSidechain(message schema.Message) bool {
	value, ok := message.Metadata["isSidechain"].(bool)
	return ok && value
}

func isInternalMessage(message schema.Message) bool {
	if value, ok := message.Metadata["isMeta"].(bool); ok && value {
		return true
	}
	for _, part := range message.Content {
		if part.Type != schema.ContentTypeText {
			continue
		}
		text := strings.ToLower(strings.TrimSpace(part.Text))
		for _, prefix := range []string{"<command-name>", "<local-command-caveat>", "<textblock>"} {
			if strings.HasPrefix(text, prefix) {
				return true
			}
		}
	}
	return false
}
