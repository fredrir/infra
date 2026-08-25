# restic -> S3 with per-service job options
{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.llunde.backups;

  textfileDir = "/var/lib/node-exporter-text";
  successStamp = pkgs.writeShellScript "restic-success-stamp" ''
    set -euo pipefail
    job="$1"
    ${pkgs.coreutils}/bin/mkdir -p ${textfileDir}
    ${pkgs.coreutils}/bin/printf 'restic_last_success_timestamp{job="%s"} %s\n' \
      "$job" "$(${pkgs.coreutils}/bin/date +%s)" > "${textfileDir}/.restic_$job.prom.tmp"
    ${pkgs.coreutils}/bin/mv "${textfileDir}/.restic_$job.prom.tmp" "${textfileDir}/restic_$job.prom"
  '';
in {
  imports = [
    ./llunde-backend.nix
    ./pyparser.nix
  ];

  options.llunde.backups = {
    enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
    };
    repository = lib.mkOption {
      type = lib.types.str;
      default = "s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/restic/llunde-01";
    };
    passwordFile = lib.mkOption {
      type = lib.types.str;
      default = "/run/secrets/restic-password";
    };
    environmentFile = lib.mkOption {
      type = lib.types.str;
      default = "/run/secrets/restic-env";
    };
    jobs = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            paths = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [];
            };
            schedule = lib.mkOption {
              type = lib.types.str;
              default = "weekly";
            };
            preHook = lib.mkOption {
              type = lib.types.lines;
              default = "";
            };
          };
        }
      );
      default = {};
    };
  };

  config = lib.mkIf cfg.enable {
    services.restic.backups =
      lib.mapAttrs (_name: job: {
        initialize = true;
        repository = cfg.repository;
        passwordFile = cfg.passwordFile;
        environmentFile = cfg.environmentFile;
        paths = job.paths;
        backupPrepareCommand = job.preHook;
        timerConfig = {
          OnCalendar = job.schedule;
          Persistent = true;
          RandomizedDelaySec = "1h";
        };

        pruneOpts = [
          "--keep-weekly 4"
          "--keep-monthly 6"
        ];
      })
      cfg.jobs;

    systemd.services =
      lib.mapAttrs' (
        name: _job:
          lib.nameValuePair "restic-backups-${name}" {
            serviceConfig.ExecStartPost = "${successStamp} ${name}";
          }
      )
      cfg.jobs
      // {
        # Monthly, independent
        restic-check = {
          description = "restic repository integrity check";
          serviceConfig = {
            Type = "oneshot";
            EnvironmentFile = cfg.environmentFile;
          };
          environment = {
            RESTIC_REPOSITORY = cfg.repository;
            RESTIC_PASSWORD_FILE = cfg.passwordFile;
          };
          script = "${pkgs.restic}/bin/restic check";
        };
      };
    systemd.timers."restic-check" = {
      wantedBy = ["timers.target"];
      timerConfig = {
        OnCalendar = "monthly";
        Persistent = true;
        RandomizedDelaySec = "2h";
      };
    };
  };
}
