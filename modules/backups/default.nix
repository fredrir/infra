# restic -> S3 with per-service job options (ADR 011). Option surface only in
# phase 1; phase 2 implements systemd timers, pg_dump pre-hooks, restore docs.
{ lib, ... }:
{
  options.llunde.backups = {
    enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Enable the restic backup engine (repo password via sops, ADR 007).";
    };
    repository = lib.mkOption {
      type = lib.types.str;
      example = "s3:s3.amazonaws.com/llunde-pyparser-bucket/restic";
      description = "restic repository URL (S3-compatible).";
    };
    jobs = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            paths = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
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
      default = { };
      description = "Per-service backup jobs (llunde-backend in phase 2, pyparser in phase 3).";
    };
  };
}
