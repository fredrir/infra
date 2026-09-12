{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.platform.watchdog;
in {
  options.platform.watchdog = {
    enable = lib.mkEnableOption "External infrastructure checks";
    configFile = lib.mkOption {
      type = lib.types.str;
      default = "/run/secrets/platform-watchdog";
    };
  };
  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = !config.platform.k3s.enable;
        message = "External watchdog must remain outside K3s.";
      }
      {
        assertion = lib.hasPrefix "/run/" cfg.configFile || lib.hasPrefix "/var/lib/" cfg.configFile;
        message = "Watchdog credentials require a runtime path.";
      }
    ];
    systemd.services.platform-watchdog = {
      serviceConfig = {
        Type = "oneshot";
        TimeoutStartSec = "180s";
        DynamicUser = true;
        StateDirectory = "platform-watchdog";
        StateDirectoryMode = "0700";
        LoadCredential = "config:${cfg.configFile}";
        ExecStart = "${pkgs.python3}/bin/python ${./watchdog.py} --config %d/config --state /var/lib/platform-watchdog/status.json";
        NoNewPrivileges = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ProtectKernelTunables = true;
        ProtectControlGroups = true;
        RestrictAddressFamilies = ["AF_UNIX" "AF_INET" "AF_INET6"];
        MemoryMax = "96M";
        CPUQuota = "20%";
      };
    };
    systemd.timers.platform-watchdog = {
      wantedBy = ["timers.target"];
      timerConfig = {
        OnBootSec = "2m";
        OnUnitActiveSec = "5m";
        RandomizedDelaySec = "15s";
      };
    };
  };
}
