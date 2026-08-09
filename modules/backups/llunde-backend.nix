# The llunde-backend backup job (ADR 011; contract.md caps/paths).
#
# pg_dump strategy: the backup timer is root, postgres runs rootless under
# llunde-backend (uid 2001). Root enters the user's podman with
# `runuser -u llunde-backend` + the user's XDG_RUNTIME_DIR — no sudo rules,
# no user-level timer, works because the user lingers (contract). The dump is
# staged into /var/backup/llunde-backend (root-owned) and snapshotted together
# with valkey's appendonly volume.
{
  config,
  lib,
  pkgs,
  ...
}: let
  # Rootless-podman volume of the valkey unit (stream B2). Must match the
  # Volume= name in services/llunde-backend — reconciled at lead integration.
  # Bind mount of the valkey unit (stream B2 chose host paths over named
  # volumes precisely so restic sees plain files).
  valkeyVolumeData = "/home/llunde-backend/data/valkey";
  dumpDir = "/var/backup/llunde-backend";
  asBackendUser = "${pkgs.util-linux}/bin/runuser -u llunde-backend -- env XDG_RUNTIME_DIR=/run/user/2001 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2001/bus";
  podman = "/run/current-system/sw/bin/podman";
in {
  # Presence-gated (phase-3.5): this job only exists on hosts that actually run
  # llunde-backend — backups.enable alone would run `runuser -u llunde-backend`
  # on llunde-parser, where uid 2001 is pyparser.
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
