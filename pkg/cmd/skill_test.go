package cmd

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCreateSkillCommand_Output(t *testing.T) {
	const version = "0.2.0-test"
	var output bytes.Buffer
	command := CreateSkillCommand(version)
	command.SetOut(&output)

	if err := command.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := output.String()
	parts := strings.SplitN(got, "---", 3)
	if len(parts) != 3 || parts[0] != "" {
		t.Fatalf("output does not contain delimited YAML frontmatter: %q", got)
	}
	var frontmatter struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &frontmatter); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}
	if frontmatter.Name != "tracer" {
		t.Errorf("frontmatter name = %q, want tracer", frontmatter.Name)
	}
	if strings.TrimSpace(frontmatter.Description) == "" {
		t.Error("frontmatter description must not be empty")
	}
	if !strings.Contains(parts[2], "\n# Tracer CLI "+version+"\n") {
		t.Errorf("output does not contain exact version heading for %q", version)
	}
}

func TestCreateSkillCommand_RejectsArguments(t *testing.T) {
	command := CreateSkillCommand("test")
	command.SetArgs([]string{"unexpected"})

	if err := command.Execute(); err == nil {
		t.Fatal("Execute() should reject arguments")
	}
}
