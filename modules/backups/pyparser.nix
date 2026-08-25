{
  config,
  lib,
  pkgs,
  ...
}: let
  filesVolumeData = "/home/pyparser/.local/share/containers/storage/volumes/pyparser-files/_data";
  dumpDir = "/var/backup/pyparser";
  asPyparserUser = "${pkgs.util-linux}/bin/runuser -u pyparser -- env XDG_RUNTIME_DIR=/run/user/2001 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2001/bus";
  podman = "/run/current-system/sw/bin/podman";
in {
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
        ${asPyparserUser} ${podman} exec pyparser-postgres pg_dump -U pyparser -Fc pyparser_llunde \
          > ${dumpDir}/pyparser_llunde.dump
      '';
    };
  };
}
