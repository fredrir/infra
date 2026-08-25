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
      description = "Enable the restic backup engine (repo password via sops, ADR 007/011).";
    };
    repository = lib.mkOption {
      type = lib.types.str;
      default = "s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/restic/llunde-01";
      description = "restic repository URL (S3-compatible).";
    };
    passwordFile = lib.mkOption {
      type = lib.types.str;
      default = "/run/secrets/restic-password";
      description = "Path to the restic repository password (sops-rendered).";
    };
    environmentFile = lib.mkOption {
      type = lib.types.str;
      default = "/run/secrets/restic-env";
      description = "Env file with AWS credentials for the S3 repository (sops-rendered).";
    };
    jobs = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            paths = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [];
              description = "Paths snapshotted for this service.";
            };
            schedule = lib.mkOption {
              type = lib.types.str;
              default = "weekly";
              description = "systemd OnCalendar expression (weekly to start, per owner).";
            };
            preHook = lib.mkOption {
              type = lib.types.lines;
              default = "";
              description = "Runs before snapshot, e.g. pg_dump into a staged path.";
            };
          };
        }
      );
      default = {};
      description = "Per-service backup jobs (llunde-backend, pyparser).";
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
        # Retention enforced after every backup
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
