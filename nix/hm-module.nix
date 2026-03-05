{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.programs.tracer;
  tomlFormat = pkgs.formats.toml { };
  tracerPkg = cfg.package;
  watchExec = lib.concatStringsSep " " (
    [
      (lib.getExe tracerPkg)
      "watch"
    ]
    ++ (map lib.escapeShellArg cfg.watch.extraArgs)
  );
in
{
  options.programs.tracer = {
    enable = lib.mkEnableOption "tracer CLI";

    package = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default = null;
      description = "The tracer package to install. If null, install separately.";
    };

    settings = lib.mkOption {
      type = tomlFormat.type;
      default = { };
      example = lib.literalExpression ''
        {
          archive.root_dir = "~/.local/share/tracer/archive";
          ingest.enabled_providers = [ "claude" "codex" ];
          ingest.exclude_projects = [ "scratch-playground" ];
        }
      '';
      description = "Configuration written to ~/.config/tracer/config.toml";
    };

    watch = {
      enable = lib.mkEnableOption "tracer watch user service";

      workingDirectory = lib.mkOption {
        type = lib.types.str;
        default = "%h";
        description = "Working directory for the watch process.";
      };

      extraArgs = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        example = [
          "--debounce"
          "1s"
          "--archive-root"
          "~/.local/share/tracer/archive"
        ];
        description = "Additional arguments passed to `tracer watch`.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.package != null || !cfg.watch.enable;
        message = "programs.tracer.watch.enable requires programs.tracer.package to be set.";
      }
    ];

    home.packages = lib.mkIf (tracerPkg != null) [ tracerPkg ];

    home.file.".config/tracer/config.toml" = lib.mkIf (cfg.settings != { }) {
      source = tomlFormat.generate "tracer-config" cfg.settings;
    };

    systemd.user.services.tracer-watch = lib.mkIf cfg.watch.enable {
      Unit = {
        Description = "Tracer session archive watcher";
        After = [ "default.target" ];
      };

      Service = {
        Type = "simple";
        WorkingDirectory = cfg.watch.workingDirectory;
        ExecStart = watchExec;
        Restart = "always";
        RestartSec = 5;
      };

      Install = {
        WantedBy = [ "default.target" ];
      };
    };
  };
}
