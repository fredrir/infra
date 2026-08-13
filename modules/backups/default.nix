# restic -> S3 with per-service job options (ADR 011). Thin layer over upstream
# services.restic.backups — paths, timerConfig, backupPrepareCommand, S3 env and
# forget --prune are already there; this adds the per-service shape and repo
# conventions (retention, monthly check, secrets paths).
#
# restic needs AWS credentials as well as the repo password, so
# secrets/restic.yaml carries TWO sops values: restic/password -> passwordFile
# (plain string) and restic/env -> environmentFile (AWS_ACCESS_KEY_ID,
# AWS_SECRET_ACCESS_KEY, AWS_DEFAULT_REGION=eu-north-1). modules/secrets wires
# sops; this module consumes the rendered paths.
{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.llunde.backups;

  # Backup freshness for Prometheus: each job stamps its last SUCCESS into the
  # node_exporter textfile dir (modules/observability owns the tmpfiles rule;
  # mkdir -p covers hosts where backups outrun it). ExecStartPost, NOT the
  # upstream backupCleanupCommand — that runs from postStop, i.e. after FAILED
  # runs too, and a success stamp must only ever record success.
  # Write-then-rename so the exporter never reads a torn file.
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
    # Root, SYSTEM-level timers on purpose: root reads /run/secrets and can
    # enter rootless-podman containers via runuser (see llunde-backend.nix),
    # which a per-user timer cannot do for postgres.
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
        # Retention enforced after every backup (upstream runs forget --prune).
        pruneOpts = [
          "--keep-weekly 4"
          "--keep-monthly 6"
        ];
      })
      cfg.jobs;

    systemd.services =
      # Stamps onto the upstream-generated units. ExecStartPost fires only once
      # every ExecStart — backup AND forget --prune — exited 0: exactly the
      # "last good backup" ResticStale (services/observability) alerts on.
      lib.mapAttrs' (
        name: _job:
          lib.nameValuePair "restic-backups-${name}" {
            serviceConfig.ExecStartPost = "${successStamp} ${name}";
          }
      )
      cfg.jobs
      // {
        # Monthly, independent of the backup timers so a wedged backup unit
        # cannot silently skip verification.
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
