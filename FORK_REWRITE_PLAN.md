# Tracer CLI Rewrite Plan

## Scope
Linux-first CLI for archiving Claude Code and Codex CLI sessions to Markdown.

## Current Status
All planned rewrite phases (0-6) are complete.

Implemented:
- Provider scope reduced to `claude` and `codex`
- `sync` and `watch` workflow over a shared ingest/watch engine
- Per-session archive layout: `provider/project/session.md`
- Runtime dedupe/checkpoint state in SQLite
- Product cloud/auth stack removed
- Config-driven provider filtering and project/path exclusions
- TTY-aware sync progress reporting
- `config` command tree:
  - `tracer config init`
  - `tracer config check [provider-id]`
- Nix flake package and Home Manager module
- Test suite updated for the rewritten flow

## Command Surface
- `tracer sync [provider-id]`
- `tracer watch [provider-id]`
- `tracer list [provider-id] [project] [--json] [--sessions]`
- `tracer config init [--force]`
- `tracer config check [provider-id] [-c <provider-command>]`
- `tracer version`

## Active Follow-Ups
- Keep CLI output and docs aligned with current command surface
- Continue hard-removing dead paths during feature work
- Expand integration tests as new behavior is added

