{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.llunde.quadlet;
  autoUsers = lib.filter (u: u.autoUpdate) cfg.serviceUsers;
in {
  options.llunde.quadlet = {
    units = lib.mkOption {
      type = with lib.types; listOf (attrsOf anything);
      default = [];
    };

    serviceUsers = lib.mkOption {
      type = with lib.types;
        listOf (submodule {
          options = {
            name = lib.mkOption {type = lib.types.str;};
            uid = lib.mkOption {type = lib.types.int;};
            autoUpdate = lib.mkOption {
              type = lib.types.bool;
              default = true;
            };
          };
        });
      default = [];
    };
  };

  config = {
    environment.etc = lib.mkMerge cfg.units;

    systemd.user.services.llunde-auto-update = lib.mkIf (autoUsers != []) {
      description = "podman auto-update for llunde service users";
      unitConfig.ConditionUser = map (u: "|${toString u.uid}") autoUsers;
      serviceConfig = {
        Type = "oneshot";
        Environment = "REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json";
        ExecStart = "${pkgs.podman}/bin/podman auto-update";
      };
    };

    systemd.user.timers.llunde-auto-update = lib.mkIf (autoUsers != []) {
      description = "Poll registries and restart updated llunde containers";
      unitConfig.ConditionUser = map (u: "|${toString u.uid}") autoUsers;
      timerConfig = {
        OnCalendar = "*:0/5";
        RandomizedDelaySec = 60;
      };
      wantedBy = ["timers.target"];
    };

    system.activationScripts.llundeQuadletReload.text =
      lib.concatMapStringsSep "\n" (u: ''
        if [ -d /run/user/${toString u.uid} ]; then
          ${pkgs.systemd}/bin/systemctl --machine=${u.name}@ --user daemon-reload || true
        fi
      '')
      cfg.serviceUsers;
  };
}
