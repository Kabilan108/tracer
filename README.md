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
- Native one-shot cross-host archive pushes that preserve receiver annotations
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
- `tracer list [--project <name>] [--provider <id>] [--outcome <value>] [--tag <tag> ...]`
- `tracer get <session-id> [--provider <provider-id>] [--path] [--tool-output <mode>] [--turns user,agent]`
- `tracer outcome <session-id-or-path> <done|abandoned|clear>`
- `tracer tag <session-id-or-path> gold`
- `tracer untag <session-id-or-path> gold`
- `tracer push <remote-name> [--dry-run]`
- `tracer receive --dest <path> --stdin`
- `tracer config init [--force]`
- `tracer config check [provider-id] [-c <provider-command>]`
- `tracer version`

`tracer get` reads the finished archive for the requested session and prints the Markdown transcript to stdout. Use `tracer get <session-id> -P` to print only the archived Markdown path, or `tracer get <session-id> -p claude` / `-p codex` to limit lookup to one provider.

Tool-output filtering is applied to the archived Markdown only when it is read; reads never modify the archive. `--tool-output=full` is the byte-identical default, `--tool-output=none` keeps each tool tag and summary as a stub, and `--tool-output=truncate:N` keeps the first `N` output lines per tool call with an 8 KiB hard cap. `--turns=user,agent` removes tool-use and thinking blocks entirely.

```bash
tracer get 019abc --tool-output=none
tracer get 019abc --tool-output=truncate:20
tracer get 019abc --turns=user,agent
```

`tracer list` reads archived Markdown rather than provider session stores. JSON output is a recency-sorted array containing the frontmatter fields and absolute transcript path. Configure `archive.additional_roots` to include synchronized, read-only archives in the same query.

To opt an additional root into annotation writes by bare session ID, also list it in `archive.annotatable_roots`. Annotation lookup collects matches across the primary archive and every annotatable root, then rejects duplicate IDs with the candidate paths; explicit transcript paths continue to work for any path. Only enable this for roots fed by `tracer push`, which preserves receiver annotations. Raw `rsync` feeds can clobber receiver-side annotations. This opt-in affects only `outcome`, `tag`, and `untag`; sync, watch, ingest, and regeneration never write additional roots.

Repeat `--tag` to require every specified tag. Prefix a tag with `!` to require that the tag is absent. Quote negated tags with single quotes so shells that interpret `!` pass it through unchanged. The `!` prefix is reserved for negation: a tag whose name itself starts with `!` cannot be positively matched, so avoid `!` when naming tags.

```bash
tracer list --tag gold --tag ready
tracer list --json --since 168h --tag '!wiki:compiled'
```

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

## Cross-host archive push

Configure one or more named SSH destinations:

```toml
[[push.remotes]]
name = "sietch"
host = "sietch"
dest = "/vault/userdata/tracer-ingest/jacurutu"
```

Remote names must start with an ASCII letter or digit and contain only letters, digits, `_`, and `-`. Hosts must start with a letter or digit and contain only letters, digits, `.`, `_`, and `-`. Destinations must be absolute and contain only letters, digits, `.`, `_`, `/`, `@`, and `-`. These restrictions keep both the local SSH invocation and its remote shell command unambiguous.

Preview or send changed files from the primary archive:

```bash
tracer push sietch --dry-run
tracer push sietch
```

Each push invokes `ssh <host> tracer receive --dest <dest> --stdin`, streams a tar archive, and exits. There is no receiving daemon. The receiver must run the Tracer release that introduced `receive` or newer.

Only one push to a given remote can run at a time. A concurrent attempt exits immediately with an “already in progress” error. Deleted or renamed sender paths are pruned from that remote's cursor after a successful push. Files that change during a push are skipped or left uncheckpointed so the next push retries them.

When a transcript already exists at the destination, Tracer applies the incoming transcript body and derived metadata while merging annotations. Tags are the normalized union of both copies, and a non-empty receiver outcome wins over the sender outcome. Because tags are a union, removing a tag only on the receiver is not permanent: it reappears on a later push while the sender still has that tag.

The receiver rejects unsafe paths, symlinks, links, devices, directories, and other non-regular tar entries. An existing transcript with invalid frontmatter is replaced by the valid incoming transcript. Invalid incoming transcripts and per-file write failures are skipped while the rest of the stream is processed; `receive` then exits nonzero so the sender does not advance its cursor.

## Configuration Docs

Detailed configuration and Linux integration docs:
- [Configuration Guide](docs/configuration.md)

## Attribution

This project includes software derived from the SpecStory CLI:
https://github.com/specstoryai/getspecstory

The codebase has been rewritten and scoped for Linux local archiving workflows.

## License

Apache-2.0. See [LICENSE](LICENSE).
