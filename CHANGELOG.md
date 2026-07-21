# Changelog

## 0.2.0

### Added

- Add CI and release workflows; local builds embed a `dev-<sha>` version.
- Add YAML frontmatter to archived transcripts with session identity, title, host, workspace, provider, models, timestamps, turn counts, and tool-call counts.
- Add archive-backed `tracer list` JSON output with recency sorting and filters for time, project, provider, outcome, and tags.
- Add read-only `archive.additional_roots` support for querying synchronized archives alongside the primary archive.
- Add explicit `outcome`, `tag`, and `untag` commands for annotating archived sessions.
- Add read-time tool-output truncation and conversation-only filtering to `tracer get` without modifying archived Markdown.
- Add unit, integration, race, and VHS coverage for metadata generation, archive discovery, annotation preservation, and CLI workflows.
- Add native `tracer push <remote>` and one-shot `tracer receive` archive synchronization with byte-hash cursors and receiver-side annotation merging.
- Add opt-in `archive.annotatable_roots` for annotation commands to resolve session IDs in merge-preserving received archives, with ambiguity protection.
- Add `tracer skill` to print version-matched instructions for coding agents.

### Changed

- Upgrade `tracer list --tag` to support repeatable AND filters and `!`-prefixed tag negation.
- Allow `tracer tag` and `tracer untag` to manage arbitrary validated tag names, including namespaced tags such as `wiki:compiled`.
- Derive session titles from the first substantive user message while ignoring sidechain and internal Claude messages.
- Preserve manual outcomes and tags when sync or watch regenerates a transcript.
- Write transcripts atomically and coordinate transcript generation with metadata mutation using cross-process locks.
- Make archive discovery tolerate missing or inaccessible read-only roots while reporting malformed timestamps used with `--since`.
- Search the primary archive plus configured annotatable roots for bare-ID annotations, reject ambiguity with candidate paths, and retain explicit paths for non-annotatable roots.
- Update `tracer-digest` discovery to consume `tracer list --json` metadata while retaining path-based deduplication for late rsync arrivals and active sessions.

### Migration

- Run `tracer sync` after upgrading to regenerate available sessions with frontmatter. Legacy Markdown whose provider source no longer exists remains outside `tracer list` results.
