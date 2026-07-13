# tracer

`tracer` archives terminal coding sessions to Markdown on Linux.

This fork focuses on local-first operation for:
- Claude Code (`claude`)
- Codex CLI (`codex`)

## Features

- Historical backfill with `tracer sync`
- Continuous archive updates with `tracer watch`
- Per-session Markdown output in `provider/project/session.md`
- YAML frontmatter with host, project, model, timing, and turn metadata
- Archive-backed session listing with JSON and metadata filters
- Explicit outcomes and quality tags that survive transcript regeneration
- Config-driven provider filters and project/path exclusions
- Built-in config bootstrap and validation commands
- Optional Nix package and Home Manager module

## Quick Install (Linux)

### Option 1: Build from source

```bash
go build -o ./bin/tracer .
install -Dm755 ./bin/tracer ~/.local/bin/tracer
```

### Option 2: Build with Nix

```bash
nix build .#tracer
install -Dm755 ./result/bin/tracer ~/.local/bin/tracer
```

## Quick Setup

1. Initialize config:
```bash
tracer config init
```
2. Validate config and provider installs:
```bash
tracer config check
```
3. Backfill existing sessions:
```bash
tracer sync
```
4. Run continuous watcher:
```bash
tracer watch
```

## Commands

- `tracer sync [provider-id]`
- `tracer watch [provider-id]`
- `tracer list [--json] [--since <timestamp|duration>] [--limit N]`
- `tracer list [--project <name>] [--provider <id>] [--outcome <value>] [--tag <tag>]`
- `tracer get <session-id> [--provider <provider-id>] [--path]`
- `tracer outcome <session-id-or-path> <done|abandoned|clear>`
- `tracer tag <session-id-or-path> gold`
- `tracer untag <session-id-or-path> gold`
- `tracer config init [--force]`
- `tracer config check [provider-id] [-c <provider-command>]`
- `tracer version`

`tracer get` refreshes the archive for the requested session and prints the Markdown transcript to stdout. Use `tracer get <session-id> -P` to print only the archived Markdown path, or `tracer get <session-id> -p claude` / `-p codex` to limit lookup to one provider.

`tracer list` reads archived Markdown rather than provider session stores. JSON output is a recency-sorted array containing the frontmatter fields and absolute transcript path. Configure `archive.additional_roots` to include synchronized, read-only archives in the same query.

Run `tracer sync` after upgrading so sessions still present in provider storage are regenerated with frontmatter. Older Markdown files whose source sessions are no longer available remain unchanged and are not returned by `tracer list`.

Generated transcripts begin with metadata like:

```yaml
---
session_id: 019...
title: Add archive-backed session listing
host: jacurutu
cwd: /home/kabilan/dotfiles
provider: codex
models:
  - gpt-5.6
started: 2026-07-13T10:00:00Z
ended: 2026-07-13T10:42:00Z
user_turns: 8
agent_turns: 11
tool_calls: 23
outcome: done
tags:
  - gold
---
```

Derived fields are refreshed by sync and watch. User-set `outcome` and `tags` values are preserved.

Outcome and tag commands resolve session IDs only in the writable primary archive. To annotate a synchronized transcript under an additional root, pass its explicit Markdown path; a later rsync may replace annotations made to that copy.

## Configuration Docs

Detailed configuration and Linux integration docs:
- [Configuration Guide](docs/configuration.md)

## Attribution

This project includes software derived from the SpecStory CLI:
https://github.com/specstoryai/getspecstory

The codebase has been rewritten and scoped for Linux local archiving workflows.

## License

Apache-2.0. See [LICENSE](LICENSE).
