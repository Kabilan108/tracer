# Tracer CLI Fork and Rewrite Plan

## Goal
Build a streamlined Go CLI and daemon for extracting agent sessions, with:
- providers limited to `claude` and `codex`
- no product analytics
- local activity telemetry/statistics retained
- global archive output with incremental updates

## Final Scope Decisions
- Keep providers: `claude`, `codex`.
- Remove providers: `cursor`, `gemini`, `droid`.
- Keep both entry points:
  - `ingest` (one-shot full historical backfill)
  - `daemon run` (foreground continuous watch)
- Share one core engine between `ingest` and `daemon run`.
- Markdown write policy:
  - one file per session
  - update in place
  - small debounce
- Output layout: `provider/project/session.md`.
- Exclusions configured in config file (source of truth), not DB.
- Never delete markdown files if source sessions disappear.
- Remove app/product analytics entirely.
- Keep local activity telemetry and `statistics.json`.
- OTEL optional, off by default.
- Do not port existing SpecStory cloud sync/auth code.
- Use `justfile` (not `makefile`).
- Add Nix scaffolding and Home Manager module, including a user service for daemon.

## Current SpecStory Cloud Behavior (For Reference Only)
This is informational only and will not be ported as-is.
- Auth endpoints:
  - `POST /api/v1/device-login`
  - `POST /api/v1/device-refresh`
  - `POST /api/v1/device-logout`
- Sync endpoints:
  - `GET /api/v1/projects/{projectID}/sessions/sizes`
  - `HEAD /api/v1/projects/{projectID}/sessions/{sessionID}`
  - `PUT /api/v1/projects/{projectID}/sessions/{sessionID}`
- Upload payload includes markdown, raw session data, project/session identifiers, and client/device metadata.

## Implementation Phases

### Phase 0: Bootstrap Fork in This Repo
- Copy only required source from vendor into project root:
  - `go.mod`, `go.sum`, `main.go`, `main_test.go`, `pkg/**`
- Drop upstream docs/repo metadata from active codebase.
- Keep/copy `AGENTS.md` and `CLAUDE.md` for local workflow context.
- Create initial baseline commit.

Exit criteria:
- Code builds in this repo.
- Baseline tests execute.

### Phase 1: Provider Scope Reduction
- Remove `cursor`, `gemini`, `droid` provider packages and registrations.
- Keep and validate `claudecode` and `codexcli` only.
- Update help text, command examples, and config docs/templates accordingly.

Exit criteria:
- `go test ./...` passes for reduced provider set.
- CLI only advertises Claude/Codex.

### Phase 2: Remove Product Analytics, Keep Activity Stats/Telemetry
- Remove `pkg/analytics` package and all event tracking calls.
- Keep:
  - `pkg/session/statistics.go` and `statistics.json` flow
  - OTEL telemetry package and flags
- Ensure OTEL remains opt-in and disabled by default.

Exit criteria:
- No analytics imports/usages remain.
- Stats + telemetry behavior validated by tests.

### Phase 3: Shared Engine for Ingest + Daemon
- Create core engine that handles:
  - historical ingest of all sessions
  - incremental updates from provider watchers
  - markdown generation/write with debounce
  - statistics updates
- Add persistent state DB (SQLite) for runtime dedupe/checkpoints only.

Exit criteria:
- `ingest` and `daemon run` both call the same engine paths.
- No markdown duplication, stable idempotent updates.

### Phase 4: UX and Configuration
- Add/shape commands:
  - `tracer ingest`
  - `tracer daemon run` (foreground)
- Implement single-instance locking for daemon.
- Add global output root + `provider/project/session.md` mapping.
- Implement include/exclude logic from config.

Exit criteria:
- First run ingests all historical sessions.
- Daemon continuously updates/new sessions into archive.

### Phase 5: Nix and Home Manager Scaffolding
- Add `flake.nix` following existing `atlas`/`raindrop` patterns:
  - `buildGoModule`
  - dev shell
  - package output
- Add `nix/hm-module.nix`:
  - program settings
  - daemon service options
- Add systemd user service wiring for daemon auto-run.
- Add `justfile` for common tasks.

Exit criteria:
- Nix build works.
- Home Manager module can install/configure and run daemon.

### Phase 6: Tests and Integration
- First: migrate/adapt provider/session tests for Claude/Codex.
- Then add daemon/integration tests:
  - historical backfill
  - incremental updates
  - debounce behavior
  - exclusion rules
  - single-instance locking

Exit criteria:
- CI-quality test pass for core workflows.

## High-Level Target CLI Shape
- `tracer ingest [--config ...]`
- `tracer daemon run [--config ...]`
- `tracer daemon status` (optional follow-up)
- `tracer daemon stop` (optional follow-up)

## Config Direction (Initial)
Use config file as source of truth:
- enabled providers (`claude`, `codex`)
- global output root
- exclusion rules (`projects`, `path_globs`)
- telemetry options (OTEL endpoint, prompt inclusion)

SQLite is runtime/internal only:
- seen session checkpoints
- content hash/version metadata
- debounce/bookkeeping state

## Known Follow-Ups
- User referenced `~/repos/dictator` for scaffolding patterns; path was not found during lookup and may need corrected path if still needed.
