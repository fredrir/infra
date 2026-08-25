{
  config,
  lib,
  pkgs,
  ...
}: let
  valkeyVolumeData = "/home/llunde-backend/data/valkey";
  dumpDir = "/var/backup/llunde-backend";
  asBackendUser = "${pkgs.util-linux}/bin/runuser -u llunde-backend -- env XDG_RUNTIME_DIR=/run/user/2001 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2001/bus";
  podman = "/run/current-system/sw/bin/podman";
in {
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
        ${asBackendUser} ${podman} exec llunde-postgres pg_dump -U llunde -Fc llunde \
          > ${dumpDir}/llunde.dump
        ${asBackendUser} ${podman} exec llunde-valkey valkey-cli BGREWRITEAOF || true
      '';
    };
  };
}
