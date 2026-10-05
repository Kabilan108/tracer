# Changelog

## Unreleased

### Changed

- `tracer watch` batches write events per transcript and parses off the event loop, at most once per second and less often for transcripts that are slow to parse. Previously every write event re-parsed the whole file, so a growing 100 MB transcript cost about a CPU-second per append (#14).
- The Home Manager `tracer-watch` service runs with `Nice=10`, `CPUWeight=20`, and `IOSchedulingClass=idle` by default so archiving yields to interactive work.
- `tracer watch` reads only the bytes appended to an active session file instead of re-reading and re-decoding the whole file on every write. A partially written last line is left for the next read, and a complete final record that has no newline yet is shown without being cached. A file is re-read from the start when it was truncated or replaced, or rewritten in place: it kept its size but its mtime changed, or the bytes just before the read position differ. Decoded records are cached per file and dropped after 10 minutes without writes or when the file is removed. Per append, a 110 MB Codex rollout went from 1.1 s of CPU and 110 MB of reads in 0.2.2 to 8 ms and 3 KB, and a 72 MB Claude transcript from 1.3 s to 29 ms (#14).
- `tracer watch` startup skips session sources whose files are unchanged since they were archived and whose Markdown still exists, using per-source fingerprints (path, size, mtime, inode, and a parser version) stored in `runtime-state.db`. With `--local-time-zone` the fingerprint also covers the zone's UTC offsets, so switching to a zone with different offsets re-parses everything once. A source that cannot be read is counted as an error, and the next start parses it again. `tracer sync` still re-parses everything. On a 2.7 GB history, restarting the watcher went from about 5 minutes of CPU and 4.5 GB peak RSS to 0.1 s and 20 MB.
- Historical ingest streams sessions one source at a time instead of holding each provider's full history in memory.
- Once its watches are registered, each watcher re-ingests the sources that changed since startup ingest, so a write made between the two is archived even when it never produces an event. The Codex watcher no longer re-parses every existing day directory, or today's, when it starts.
- Building Claude session DAGs no longer scans every record per node. That made assembling large projects quadratic: a full parse of a 73 to 101 MB Claude project averaged 45 s and now takes about 1.2 s. `tracer sync` of a 2.7 GB history went from 4 minutes to 12 seconds.

### Removed

- Remove `AgentChatSession.RawData` and the watcher's per-callback session fingerprint. The engine's rendered-markdown hash already skips unchanged sessions, and building the raw copy doubled memory and re-encoded every record on each update.

## 0.2.2

### Added

- Add `-v`/`--version` root flag as an alias for `tracer version`.

### Fixed

- Restore the allocation-free hashing path for tar writing; the frontmatter probe buffer is only built during archive scans where it is used.

## 0.2.1

### Fixed

- Skip transcripts without parseable frontmatter when scanning for `tracer push`, matching how `list` and `get` already ignore them. Legacy pre-frontmatter files previously made every push fail permanently: the receiver rejected them per-file, the run exited nonzero, and the cursor never advanced. Skipped files are counted as `invalid` in the push summary.

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
