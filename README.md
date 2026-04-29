# tracer

`tracer` archives terminal coding sessions to Markdown on Linux.

This fork focuses on local-first operation for:
- Claude Code (`claude`)
- Codex CLI (`codex`)

## Features
- Historical backfill with `tracer sync`
- Continuous archive updates with `tracer watch`
- Per-session Markdown output in `provider/project/session.md`
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
- `tracer list [provider-id] [project] [--json] [--sessions] [--no-pager]`
- `tracer config init [--force]`
- `tracer config check [provider-id] [-c <provider-command>]`
- `tracer version`

## Configuration Docs
Detailed configuration and Linux integration docs:
- [Configuration Guide](docs/configuration.md)

## Attribution
This project includes software derived from the SpecStory CLI:
https://github.com/specstoryai/getspecstory

The codebase has been rewritten and scoped for Linux local archiving workflows.

## License
Apache-2.0. See [LICENSE](LICENSE).
