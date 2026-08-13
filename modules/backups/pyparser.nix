# The pyparser backup job (ADR 011). Same pg_dump strategy as
# llunde-backend.nix — root timer, `runuser -u pyparser` into the lingering
# user's podman (uid 2001) — but different storage: dumps stage into
# /var/backup/pyparser (root-owned) and are snapshotted with the pyparser-files
# NAMED volume, not a bind mount, so restic reads its _data directory directly.
#
# Replaces the compose-era db-backup/backup-ship sleep-loop containers; the old
# s3://…/pg-backups/ prefix stays as a frozen archive.
{
  config,
  lib,
  pkgs,
  ...
}: let
  # pyparser-files; must match the Volume= name in services/pyparser. A rootless
  # user's graphroot defaults to ~/.local/share/containers/storage.
  filesVolumeData = "/home/pyparser/.local/share/containers/storage/volumes/pyparser-files/_data";
  dumpDir = "/var/backup/pyparser";
  asPyparserUser = "${pkgs.util-linux}/bin/runuser -u pyparser -- env XDG_RUNTIME_DIR=/run/user/2001 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2001/bus";
  podman = "/run/current-system/sw/bin/podman";
in {
  # Presence-gated to hosts that actually run pyparser: backups.enable alone
  # would run `runuser -u pyparser` on llunde-01, where uid 2001 is
  # llunde-backend.
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
