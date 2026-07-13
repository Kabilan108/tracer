package session

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tracer-ai/tracer-cli/pkg/spi/schema"
)

func TestDeriveMetadata(t *testing.T) {
	data := &schema.SessionData{
		Provider:      schema.ProviderInfo{ID: "codex"},
		SessionID:     "session-1",
		CreatedAt:     "2026-07-13T09:00:00Z",
		UpdatedAt:     "2026-07-13T10:30:00Z",
		WorkspaceRoot: "/work/tracer",
		Exchanges: []schema.Exchange{{
			StartTime: "2026-07-13T10:00:00Z",
			EndTime:   "2026-07-13T10:20:00Z",
			Messages: []schema.Message{
				{Role: schema.RoleUser, Content: []schema.ContentPart{{Type: schema.ContentTypeText, Text: "internal"}}, Metadata: map[string]interface{}{"isMeta": true}},
				{Role: schema.RoleUser, Content: []schema.ContentPart{{Type: schema.ContentTypeText, Text: "<command-name>/review</command-name>"}}},
				{Role: schema.RoleUser, Content: []schema.ContentPart{{Type: schema.ContentTypeText, Text: "  Implement archive metadata  "}}},
				{Role: schema.RoleAgent, Model: "gpt-5", Content: []schema.ContentPart{{Type: schema.ContentTypeText, Text: "Working"}}},
				{Role: schema.RoleAgent, Model: "gpt-5", Tool: &schema.ToolInfo{Name: "read"}},
				{Role: schema.RoleAgent, Model: "gpt-4", Content: []schema.ContentPart{{Type: schema.ContentTypeText, Text: "sidechain"}}, Metadata: map[string]interface{}{"isSidechain": true}},
				{Role: schema.RoleAgent, Tool: &schema.ToolInfo{Name: "task"}, Metadata: map[string]interface{}{"isSidechain": true}},
			},
		}},
	}

	got := DeriveMetadata(data, "jacurutu")
	if got.Title != "Implement archive metadata" || got.Host != "jacurutu" || got.UserTurns != 1 || got.AgentTurns != 1 || got.ToolCalls != 2 {
		t.Fatalf("DeriveMetadata() = %+v", got)
	}
	if got.Started != "2026-07-13T10:00:00Z" || got.Ended != "2026-07-13T10:20:00Z" {
		t.Fatalf("timestamps = %s to %s", got.Started, got.Ended)
	}
	if !reflect.DeepEqual(got.Models, []string{"gpt-4", "gpt-5"}) {
		t.Fatalf("models = %v", got.Models)
	}
}

func TestFrontmatterRoundTrip(t *testing.T) {
	metadata := Metadata{SessionID: "session-1", Title: "Title", Provider: "codex", Models: []string{}, Tags: []string{"gold", "gold"}}
	frontmatter, err := RenderFrontmatter(metadata)
	if err != nil {
		t.Fatalf("RenderFrontmatter() error = %v", err)
	}
	parsed, body, err := ParseFrontmatter([]byte(frontmatter + "# Body\n"))
	if err != nil {
		t.Fatalf("ParseFrontmatter() error = %v", err)
	}
	if !reflect.DeepEqual(parsed.Tags, []string{"gold"}) || string(body) != "# Body\n" {
		t.Fatalf("parsed = %+v, body = %q", parsed, body)
	}
	if !strings.HasPrefix(frontmatter, "---\n") || !strings.HasSuffix(frontmatter, "---\n\n") {
		t.Fatalf("frontmatter delimiters missing: %q", frontmatter)
	}
}

func TestGenerateMarkdownWithMetadata(t *testing.T) {
	data := &schema.SessionData{Provider: schema.ProviderInfo{Name: "Codex"}, SessionID: "s", CreatedAt: "2026-07-13T10:00:00Z"}
	markdown, err := GenerateMarkdownWithMetadata(data, Metadata{SessionID: "s", Title: "A title", Provider: "codex", Models: []string{}}, false, true)
	if err != nil {
		t.Fatalf("GenerateMarkdownWithMetadata() error = %v", err)
	}
	if !strings.Contains(markdown, "session_id: s") || !strings.Contains(markdown, "# A title") {
		t.Fatalf("markdown = %q", markdown)
	}
}
