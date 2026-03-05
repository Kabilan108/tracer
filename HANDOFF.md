# HANDOFF

## Repo State
- Project: `/vault/experiments/2026-03-03-tracer-cli`
- Branch: `main`
- Status: rewrite complete and functional for Linux local archiving

## What This Build Does
- Archives Claude Code and Codex CLI sessions to Markdown
- Supports one-shot backfill with `sync`
- Supports continuous updates with `watch`
- Uses per-session files under `provider/project/session.md`
- Maintains runtime ingest state in SQLite
- Uses TOML config at `~/.config/tracer/config.toml` (or `$XDG_CONFIG_HOME/tracer/config.toml`)

## Current CLI
- `tracer sync [provider-id]`
- `tracer watch [provider-id]`
- `tracer list [provider-id] [project] [--json] [--sessions]`
- `tracer config init [--force]`
- `tracer config check [provider-id] [-c <provider-command>]`
- `tracer version`

## Validation Commands
```bash
go test ./...
tracer --help
tracer config --help
tracer config init
tracer config check
```

## Nix Integration
- Flake package: `flake.nix`
- Home Manager module: `nix/hm-module.nix`
- User service support is exposed through the Home Manager module (`programs.tracer.watch.enable`)

## No Known Blockers
- Current work should continue from feature requests and QA findings.
- Keep docs updated when command/config behavior changes.

