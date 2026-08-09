# The pyparser backup job (ADR 011; phase-3.5 contract/decision 4).
#
# Same pg_dump strategy as llunde-backend.nix: the backup timer is root,
# postgres runs rootless under pyparser (uid 2001). Root enters the user's
# podman with `runuser -u pyparser` + the user's XDG_RUNTIME_DIR — no sudo
# rules, no user-level timer, works because the user lingers. The dump is
# staged into /var/backup/pyparser (root-owned) and snapshotted together with
# the pyparser-files named volume (named volumes here, unlike llunde-backend's
# bind mounts, so restic reads the volume's _data directory directly).
#
# This replaces the compose-era db-backup/backup-ship sleep-loop containers;
# the old s3://…/pg-backups/ prefix stays as a frozen archive.
{
  config,
  lib,
  pkgs,
  ...
}: let
  # Rootless-podman named volume of the pyparser-files unit (stream B). Must
  # match the Volume= name in services/pyparser — reconciled at lead
  # integration. graphroot for a rootless user defaults to
  # ~/.local/share/containers/storage.
  filesVolumeData = "/home/pyparser/.local/share/containers/storage/volumes/pyparser-files/_data";
  dumpDir = "/var/backup/pyparser";
  asPyparserUser = "${pkgs.util-linux}/bin/runuser -u pyparser -- env XDG_RUNTIME_DIR=/run/user/2001 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2001/bus";
  podman = "/run/current-system/sw/bin/podman";
in {
  # Presence-gated (phase-3.5): this job only exists on hosts that actually run
  # pyparser — backups.enable alone would run `runuser -u pyparser` on
  # llunde-01, where uid 2001 is llunde-backend.
  config = lib.mkIf (config.llunde.backups.enable && (config.llunde.users.services ? pyparser)) {
    systemd.tmpfiles.rules = ["d ${dumpDir} 0700 root root -"];

    llunde.backups.jobs.pyparser = {
      schedule = "weekly";
      paths = [
        dumpDir
        filesVolumeData
      ];
      preHook = ''
        set -euo pipefail
        # Custom-format dump so restores go through pg_restore selectively.
        ${asPyparserUser} ${podman} exec pyparser-postgres pg_dump -U pyparser -Fc pyparser_llunde \
          > ${dumpDir}/pyparser_llunde.dump
      '';
    };
  };
}
