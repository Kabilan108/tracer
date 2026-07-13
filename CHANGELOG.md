# Changelog

## Unreleased

### Added

- Add YAML frontmatter to archived transcripts with session identity, title, host, workspace, provider, models, timestamps, turn counts, and tool-call counts.
- Add archive-backed `tracer list` JSON output with recency sorting and filters for time, project, provider, outcome, and tags.
- Add read-only `archive.additional_roots` support for querying synchronized archives alongside the primary archive.
- Add explicit `outcome`, `tag`, and `untag` commands for annotating archived sessions.
- Add unit, integration, race, and VHS coverage for metadata generation, archive discovery, annotation preservation, and CLI workflows.

### Changed

- Derive session titles from the first substantive user message while ignoring sidechain and internal Claude messages.
- Preserve manual outcomes and tags when sync or watch regenerates a transcript.
- Write transcripts atomically and coordinate transcript generation with metadata mutation using cross-process locks.
- Make archive discovery tolerate missing or inaccessible read-only roots while reporting malformed timestamps used with `--since`.
- Change session-ID annotation lookup to target only the writable primary archive; synchronized copies require an explicit path.
- Update `tracer-digest` discovery to consume `tracer list --json` metadata while retaining path-based deduplication for late rsync arrivals and active sessions.

### Migration

- Run `tracer sync` after upgrading to regenerate available sessions with frontmatter. Legacy Markdown whose provider source no longer exists remains outside `tracer list` results.
