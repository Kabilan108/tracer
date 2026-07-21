# Configuration Guide (Linux)

## Config File Location

`tracer` reads user config from:
- `$XDG_CONFIG_HOME/tracer/config.toml` when `XDG_CONFIG_HOME` is set
- otherwise `~/.config/tracer/config.toml`

Initialize the file:
```bash
tracer config init
```

Validate it:
```bash
tracer config check
```

## Minimal Example

```toml
[archive]
root_dir = "~/.local/share/tracer/archive"
additional_roots = ["/vault/userdata/tracer-ingest"]
annotatable_roots = ["/vault/userdata/tracer-ingest"]

[ingest]
enabled_providers = ["claude", "codex"]
exclude_projects = []
exclude_path_globs = []

[[push.remotes]]
name = "sietch"
host = "sietch"
dest = "/vault/userdata/tracer-ingest/jacurutu"
```

## Sections


### `[archive]`

- `root_dir`: archive output root
- `additional_roots`: read-only archive roots included by `tracer list`
- `annotatable_roots`: additional roots searched by session ID for `outcome`, `tag`, and `untag`

Only `root_dir` receives sync and watch output. Additional roots are recursively scanned for archived transcripts and may point at host-specific rsync destinations.

Every `annotatable_roots` entry must also be listed in `additional_roots`. For annotation commands, bare session IDs collect matches across the primary root and all annotatable roots. If the same ID exists in more than one searched root, Tracer reports every candidate path and requires an explicit transcript path. Explicit-path annotation works for any path.

Only enable annotation writes on a root fed by `tracer push`, whose receiver preserves annotations while merging incoming transcripts. Do not enable them on a root fed by raw `rsync`: a later sync can replace the transcript and clobber receiver-side annotations. Additional roots remain read-only for sync, watch, ingest, and transcript regeneration.

The `tag` and `untag` commands accept arbitrary non-empty tag names without whitespace or commas; a leading `!` is reserved for list negation. Tags are lowercased on write, and removal is case-insensitive, so names such as `gold` and `wiki:compiled` round-trip through annotation and list filters.

Default output layout:
- `provider/project/session.md`

### `[ingest]`

- `enabled_providers`: limit active providers (`claude`, `codex`)
- `exclude_projects`: skip specific project names
- `exclude_path_globs`: skip matching workspace paths (filepath globs)

### `[logging]`

- `debug_dir`
- `log`
- `debug`
- `console`
- `silent`

### `[local_sync]`

- `local_time_zone`

### `[[push.remotes]]`

Each push remote requires:

- `name`: unique name passed to `tracer push`; must match `^[A-Za-z0-9][A-Za-z0-9_-]*$`
- `host`: SSH destination, including an alias resolved by the user's SSH config; must match `^[A-Za-z0-9][A-Za-z0-9._-]*$`
- `dest`: receiver-side archive directory; must match `^/[A-Za-z0-9._/@-]+$`

The restricted character sets prevent a host from being interpreted as a local SSH option and prevent the destination from injecting or breaking the remote shell command. Push remote validation is performed when `tracer push` resolves a remote, so an unused malformed push entry does not prevent unrelated commands such as `sync`, `watch`, `get`, or `receive` from running.

Only the primary `archive.root_dir` is scanned. Additional archive roots are never pushed. Tracer hashes the complete Markdown file bytes, so annotation-only sender edits are included.

Preview pending files without connecting:

```bash
tracer push sietch --dry-run
```

Push pending files:

```bash
tracer push sietch
```

For each non-empty push, the sender runs:

```bash
ssh sietch tracer receive --dest /vault/userdata/tracer-ingest/jacurutu --stdin
```

The receiving command reads one tar stream, merges it, writes files atomically, and exits. No daemon or persistent listener is required. The receiver must have the Tracer release that introduced `receive` or a newer version installed in the non-interactive SSH path.

A per-remote nonblocking lock covers scanning, sending, and cursor updates. A second push to the same remote exits immediately with `push to <name> already in progress`. After a successful push, cursor rows for deleted or renamed sender paths are pruned transactionally. A file that changes between scanning and tar production is skipped; a file detected changing while streamed is not checkpointed, so it is retried on the next push. Dry runs read an existing cursor database read-only and do not create one.

If a destination transcript exists, its tags are unioned with the incoming tags and normalized. A non-empty receiver outcome wins; otherwise the incoming outcome is used. The incoming transcript body and other frontmatter fields replace the old copy. A tag removed only at the receiver reappears if a later changed sender transcript still carries that tag.

The receiver accepts only regular tar entries and verifies that every resolved parent directory remains inside the resolved destination root. Unsafe paths, links, devices, directories, and other special entries fail without being written. If an existing destination transcript has invalid frontmatter, it is replaced by the valid incoming file and processing continues. Invalid incoming frontmatter or a per-file write failure is counted and skipped; remaining entries are still processed, but `receive` exits nonzero at the end so the sender cursor remains unchanged.

## CLI Flag Overrides

Persistent flags override config values at runtime, for example:
- `--archive-root`
- `--debug-dir`
- `--console`
- `--log`
- `--debug`
- `--silent`
- `--local-time-zone`

## Home Manager (Linux)

This repo exposes a Home Manager module at `nix/hm-module.nix`.

Example module usage:
```nix
{
  imports = [
    tracer.homeManagerModules.default
  ];

  programs.tracer = {
    enable = true;
    package = tracer.packages.${pkgs.system}.tracer;

    settings = {
      archive.root_dir = "~/.local/share/tracer/archive";
      archive.additional_roots = [ "/vault/userdata/tracer-ingest" ];
      archive.annotatable_roots = [ "/vault/userdata/tracer-ingest" ];
      push.remotes = [
        {
          name = "sietch";
          host = "sietch";
          dest = "/vault/userdata/tracer-ingest/jacurutu";
        }
      ];
      ingest.enabled_providers = [ "claude" "codex" ];
      ingest.exclude_projects = [ ];
      ingest.exclude_path_globs = [ ];
    };

    watch = {
      enable = true;
      workingDirectory = "%h";
      extraArgs = [ ];
    };
  };
}
```

## NixOS

Typical NixOS setup is to use Home Manager for user-scoped `tracer` config and service.

If you are not using Home Manager, you can still:
1. Build/install the package from this flake.
2. Create `~/.config/tracer/config.toml` via `tracer config init`.
3. Run `tracer watch` from a user-level systemd service.

## Verification

After setup:
```bash
tracer config check
tracer sync
tracer watch
```
