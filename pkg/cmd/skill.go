// Package cmd contains CLI command implementations for the Tracer CLI.
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

const skillText = `---
name: tracer
description: Archive, list, filter, annotate, and cross-host-sync coding-agent session transcripts from the command line.
---

# Tracer CLI %s

Tracer archives Claude Code and Codex CLI sessions as Markdown at <root>/<provider>/<project>/<session-id>.md. Each transcript has YAML frontmatter fields including session_id, title, host, cwd, provider, models, started, ended, user_turns, agent_turns, tool_calls, and the user-maintained outcome and tags annotations.

## Archive sessions

- tracer sync [provider-id] backfills available sessions once.
- tracer watch [provider-id] backfills and then continuously archives updates in the foreground.

## Find and read sessions

Use tracer list --json for machine-readable, recency-sorted metadata. Filter with --since (a duration such as 168h or an RFC3339 timestamp), --project, --provider, --outcome, and --limit. Repeat --tag to require every tag; prefix a tag with ! to require its absence and single-quote it, for example --tag '!wiki:compiled'.

Use tracer get <session-id> --tool-output=full when exact archived Markdown is required. Use --tool-output=none to retain only tool stubs for maximum token savings, or --tool-output=truncate:N to keep up to the first N output lines per tool call, capped at 8 KiB per call, when some tool context matters. Add --turns=user,agent when only the conversation is needed and tool-use and thinking blocks can be omitted. Use -P/--path to print only the archived Markdown path. Cross-provider session-ID collisions are errors; pass --provider <id> to disambiguate. These reads and read-time filters never modify archive files.

## Annotate sessions

- tracer outcome <session-id-or-path> <done|abandoned|clear> sets or clears the outcome.
- tracer tag <session-id-or-path> <tag> adds an arbitrary tag, for example gold or wiki:compiled.
- tracer untag <session-id-or-path> <tag> removes the named tag case-insensitively, for example gold or wiki:compiled.

Bare session IDs resolve in the writable primary archive and roots explicitly configured in archive.annotatable_roots; duplicate IDs are rejected as ambiguous. Explicit Markdown paths may target any archive, while sync, watch, and transcript regeneration write only the primary archive. Never edit archive files directly: use the annotation commands so annotations survive regeneration and synchronization.

## Cross-host sync

The tracer push <remote> [--dry-run] command sends or previews changed primary-archive files for a config-defined SSH remote and merge-preserves the receiver's outcome and tags.
The tracer receive --dest <path> --stdin command is the one-shot stream receiver normally invoked by push over ssh; no daemon runs on the destination.

## Setup and diagnostics

- tracer version prints the installed version.
- tracer config init creates the default user configuration.
- tracer config check validates configuration and provider availability.
`

// CreateSkillCommand creates the skill command.
// The version string is passed in because it's set at build time in main.go.
func CreateSkillCommand(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "skill",
		Short: "Print agent instructions for this Tracer version",
		Args:  cobra.NoArgs,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Pure skill output intentionally skips logging setup and global logging-flag validation.
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), skillText, version)
			return err
		},
	}
}
