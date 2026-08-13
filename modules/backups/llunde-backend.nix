# The llunde-backend backup job (ADR 011). postgres is rootless under
# llunde-backend (uid 2001), so the root timer enters that user's podman with
# `runuser -u llunde-backend` plus its XDG_RUNTIME_DIR — no sudo rules, no
# user-level timer, works because the user lingers. Dumps stage into
# /var/backup/llunde-backend (root-owned), snapshotted with valkey's appendonly
# volume.
{
  config,
  lib,
  pkgs,
  ...
}: let
  # Valkey's volume; must match the Volume= path in services/llunde-backend.
  # A host bind mount, not a named volume, precisely so restic sees plain files.
  valkeyVolumeData = "/home/llunde-backend/data/valkey";
  dumpDir = "/var/backup/llunde-backend";
  asBackendUser = "${pkgs.util-linux}/bin/runuser -u llunde-backend -- env XDG_RUNTIME_DIR=/run/user/2001 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2001/bus";
  podman = "/run/current-system/sw/bin/podman";
in {
  # Presence-gated to hosts that actually run llunde-backend: backups.enable
  # alone would run `runuser -u llunde-backend` on llunde-parser, where uid 2001
  # is pyparser.
  config = lib.mkIf (config.llunde.backups.enable && (config.llunde.users.services ? llunde-backend)) {
    systemd.tmpfiles.rules = ["d ${dumpDir} 0700 root root -"];

    llunde.backups.jobs.llunde-backend = {
      schedule = "weekly";
      paths = [
        dumpDir
        valkeyVolumeData
      ];
      preHook = ''
        set -euo pipefail
        # Custom-format dump so restores go through pg_restore selectively.
        ${asBackendUser} ${podman} exec llunde-postgres pg_dump -U llunde -Fc llunde \
          > ${dumpDir}/llunde.dump
        # Valkey: ask for a fresh AOF rewrite point, then snapshot the volume live.
        ${asBackendUser} ${podman} exec llunde-valkey valkey-cli BGREWRITEAOF || true
      '';
    };
  };
}
