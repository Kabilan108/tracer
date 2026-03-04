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
  daemonExec = lib.concatStringsSep " " (
    [
      (lib.getExe tracerPkg)
      "daemon"
      "run"
    ]
    ++ (map lib.escapeShellArg cfg.daemon.extraArgs)
  );
in
{
  options.programs.tracer = {
    enable = lib.mkEnableOption "tracer CLI and daemon";

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
          archive.root_dir = "~/.specstory/archive";
          ingest.enabled_providers = [ "claude" "codex" ];
          ingest.exclude_projects = [ "scratch-playground" ];
        }
      '';
      description = "Configuration written to ~/.specstory/cli/config.toml";
    };

    daemon = {
      enable = lib.mkEnableOption "tracer daemon user service";

      workingDirectory = lib.mkOption {
        type = lib.types.str;
        default = "%h";
        description = "Working directory for the daemon process.";
      };

      extraArgs = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        example = [
          "--debounce"
          "1s"
          "--archive-root"
          "~/.specstory/archive"
        ];
        description = "Additional arguments passed to `tracer daemon run`.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.package != null || !cfg.daemon.enable;
        message = "programs.tracer.daemon.enable requires programs.tracer.package to be set.";
      }
    ];

    home.packages = lib.mkIf (tracerPkg != null) [ tracerPkg ];

    home.file.".specstory/cli/config.toml" = lib.mkIf (cfg.settings != { }) {
      source = tomlFormat.generate "tracer-config" cfg.settings;
    };

    systemd.user.services.tracer-daemon = lib.mkIf cfg.daemon.enable {
      Unit = {
        Description = "Tracer session archive daemon";
        After = [ "default.target" ];
      };

      Service = {
        Type = "simple";
        WorkingDirectory = cfg.daemon.workingDirectory;
        ExecStart = daemonExec;
        Restart = "always";
        RestartSec = 5;
      };

      Install = {
        WantedBy = [ "default.target" ];
      };
    };
  };
}
