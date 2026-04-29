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

[ingest]
enabled_providers = ["claude", "codex"]
exclude_projects = []
exclude_path_globs = []
```

## Sections

### `[archive]`
- `root_dir`: archive output root

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
