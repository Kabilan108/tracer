# HANDOFF

## Status
Session was focused on discovery and planning, not implementation.  
The user confirmed final product direction for a fork/rewrite of vendored `specstory-cli` into this repo, with reduced provider scope and daemon-first UX.  
We are at pre-implementation: architecture and phase plan are settled, and implementation should start next.

User's last direction:
- proceed with the plan
- do not port SpecStory cloud adapter code
- use `justfile` (not makefile)
- write down the plan and prepare a handoff for the implementing agent

## Completed
- Investigated SpecStory watcher/runtime behavior and provider internals in:
  - `/vault/experiments/2026-03-03-tracer-cli/vendor/getspecsctory/specstory-cli`
- Verified current watch behavior:
  - watch updates markdown continuously on session events
  - does not wait for explicit stop/end signal
- Verified provider architecture constraints:
  - SPI is project-path scoped
  - several provider watchers use package-level global state
- Verified tests exist in vendor:
  - 49 `*_test.go` files total
- Investigated cloud package behavior and endpoints in:
  - `/vault/experiments/2026-03-03-tracer-cli/vendor/getspecsctory/specstory-cli/pkg/cloud/auth.go`
  - `/vault/experiments/2026-03-03-tracer-cli/vendor/getspecsctory/specstory-cli/pkg/cloud/sync.go`
- Reviewed local scaffolding patterns in:
  - `/home/kabilan/repos/cli/raindrop/flake.nix`
  - `/home/kabilan/repos/cli/raindrop/nix/hm-module.nix`
  - `/home/kabilan/repos/cli/atlas/flake.nix`
  - `/home/kabilan/repos/cli/atlas/nix/hm-module.nix`
- Created implementation plan doc:
  - `/vault/experiments/2026-03-03-tracer-cli/FORK_REWRITE_PLAN.md`
- Created this handoff:
  - `/vault/experiments/2026-03-03-tracer-cli/HANDOFF.md`

Git state observed:
- Branch: `main`
- No commits yet on branch
- Working tree had untracked `.gitignore` before this session; now also includes docs added in this session.

## In Progress
No code implementation currently in progress.  
Immediate next step is Phase 0 bootstrap from plan: extract required source from vendor into repo root and establish baseline test run.

## Discussion Summary
- User wants tool to ingest all projects automatically and continue ingesting new activity, without command wrapping UX.
- We discussed current SpecStory watch behavior, which is event-driven autosave and can behave daemon-like in foreground mode.
- We discussed tradeoffs of wrapper mode vs standalone daemon and agreed on separate `ingest` + `daemon run` entry points sharing core engine.
- User requested UX/config simplification:
  - provider enable list
  - exclusions in config
  - global output archive layout
- User explicitly rejected carrying over SpecStory app analytics and requested keeping activity telemetry/statistics.
- User decided not to port current SpecStory cloud adapter code for v1.
- User requested Nix + Home Manager scaffolding patterns similar to their other Go CLIs.

## Decisions Made
- **Decision**: Provider scope is Claude + Codex only.  
  **Rationale**: User only uses those tools and wants less code/surface area.

- **Decision**: Keep both `ingest` and `daemon run` entry points with one shared engine.  
  **Rationale**: Supports both initial historical import and continuous operation without duplicating logic.

- **Decision**: Write one markdown file per session, update in place, with debounce.  
  **Rationale**: Stable archive files + reduced write churn during active sessions.

- **Decision**: Output layout is `provider/project/session.md` under a global output root.  
  **Rationale**: Clean archive organization across all projects/tools.

- **Decision**: Exclusions live in config file; SQLite is runtime state only.  
  **Rationale**: User-editable source of truth plus efficient operational bookkeeping.

- **Decision**: Never delete markdown if source session disappears.  
  **Rationale**: Preserve permanent archive history.

- **Decision**: Remove app/product analytics entirely.  
  **Rationale**: User does not want product analytics.

- **Decision**: Keep local activity telemetry/statistics; OTEL optional and off by default.  
  **Rationale**: Retains useful operational observability with privacy-safe default.

- **Decision**: Do not port existing SpecStory cloud adapter in this phase.  
  **Rationale**: User plans own backend and expects different sync design.

- **Decision**: Use `justfile` over `makefile`.  
  **Rationale**: User preference.

- **Decision**: Add flake + Home Manager module + daemon service scaffolding.  
  **Rationale**: Align with user’s existing workflow and deployment pattern.

## Session Learnings
- SpecStory `watch` command writes markdown on each callback via autosave path, with unchanged-content skip.
- Current provider SPI requires `projectPath`, so native “all projects” ingestion requires interface/runtime refactor.
- Claude/Codex/Gemini watchers currently rely on package-level globals for callback/lifecycle state, which is risky for multi-project daemon orchestration.
- Cursor watcher is instance-based and better aligned with daemon patterns, but Cursor support is out of scope for this fork.
- SpecStory cloud sync is a REST API flow (device auth + project/session sync), not OTLP telemetry ingest.

## Remaining Work
1. Phase 0 bootstrap:
   - Copy required source from vendor `specstory-cli` into repo root (`go.mod`, `go.sum`, `main.go`, `main_test.go`, `pkg/**`).
   - Remove non-essential upstream docs/metadata from active codebase.
   - Keep/copy `AGENTS.md` and `CLAUDE.md`.
2. Phase 1 provider reduction:
   - Remove cursor/gemini/droid packages, registry wiring, and related command/config references.
   - Ensure CLI surface only references Claude/Codex.
3. Phase 2 analytics removal:
   - Delete `pkg/analytics` and all tracking calls.
   - Keep statistics and optional telemetry paths working.
4. Phase 3 shared engine:
   - Implement ingest + daemon run over one core pipeline.
   - Add SQLite runtime state store and markdown debounce updates.
5. Phase 4 UX/config:
   - Add global output root and `provider/project/session` layout.
   - Implement config-driven exclusion rules and provider enables.
   - Add single-instance lock for daemon.
6. Phase 5 Nix/HM/service:
   - Add `flake.nix`, `nix/hm-module.nix`, and user service configuration for daemon.
   - Add `justfile`.
7. Phase 6 test migration and additions:
   - Port/adjust Claude+Codex provider/session tests first.
   - Add daemon/integration tests for historical ingest + continuous updates.

## Blockers
- No technical blocker for starting implementation.

## Context
- Repo root: `/vault/experiments/2026-03-03-tracer-cli`
- Vendor source used for analysis: `/vault/experiments/2026-03-03-tracer-cli/vendor/getspecsctory/specstory-cli`
- Plan doc for implementer: `/vault/experiments/2026-03-03-tracer-cli/FORK_REWRITE_PLAN.md`
- No running services started in this session.

## Reproduction
```bash
cd /vault/experiments/2026-03-03-tracer-cli
git status --short

# Read finalized plan and handoff
sed -n '1,260p' FORK_REWRITE_PLAN.md
sed -n '1,320p' HANDOFF.md

# Inspect vendor source to begin Phase 0 extraction
ls -la vendor/getspecsctory/specstory-cli
find vendor/getspecsctory/specstory-cli/pkg -maxdepth 3 -type f | head
```
