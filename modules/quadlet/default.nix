# Quadlet plumbing (ADR 004): collects mkQuadlet fragments into environment.etc
# and owns the per-user machinery — the auto-update timer and the switch-time
# daemon-reload poke. User creation/subuid/linger live in modules/users.
{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.llunde.quadlet;
  autoUsers = lib.filter (u: u.autoUpdate) cfg.serviceUsers;
  # `|` prefix = triggering condition: the unit runs for a user iff at least one
  # ConditionUser matches, so one shared /etc/systemd/user unit is per-user gated
  # without touching unrelated users (e.g. openclaw).
in {
  options.llunde.quadlet = {
    units = lib.mkOption {
      type = with lib.types; listOf (attrsOf anything);
      default = [];
      description = "environment.etc fragments produced by mkQuadlet (mkContainerUnit/mkNetworkUnit).";
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
              description = "Whether this user's containers use AutoUpdate=registry.";
            };
          };
        });
      default = [];
      description = "Service users owning quadlet units (drives auto-update gating and reload pokes).";
    };
  };

  config = {
    environment.etc = lib.mkMerge cfg.units;

    # NixOS owns /etc/systemd/user — raw environment.etc entries there fail the
    # etc build — so the auto-update pair goes through systemd.user.*;
    # ConditionUser still does the per-user gating.
    systemd.user.services.llunde-auto-update = lib.mkIf (autoUsers != []) {
      description = "podman auto-update for llunde service users";
      unitConfig.ConditionUser = map (u: "|${toString u.uid}") autoUsers;
      serviceConfig = {
        Type = "oneshot";
        Environment = "REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json";
        ExecStart = "${pkgs.podman}/bin/podman auto-update";
      };
    };

    # Merge-to-main-is-deploy (ADR 010): poll GHCR every 5 minutes.
    systemd.user.timers.llunde-auto-update = lib.mkIf (autoUsers != []) {
      description = "Poll registries and restart updated llunde containers";
      unitConfig.ConditionUser = map (u: "|${toString u.uid}") autoUsers;
      timerConfig = {
        OnCalendar = "*:0/5";
        RandomizedDelaySec = 60;
      };
      wantedBy = ["timers.target"];
    };

    # Best-effort (ADR 004's documented wrinkle): after switch, poke each running
    # user manager so the quadlet generator re-reads unit files. RELOAD only —
    # restarting changed containers is auto-update's job, or a manual
    # `systemctl --user restart`. Users whose manager is down are skipped; on
    # first boot linger starts it fresh anyway.
    system.activationScripts.llundeQuadletReload.text =
      lib.concatMapStringsSep "\n" (u: ''
        if [ -d /run/user/${toString u.uid} ]; then
          ${pkgs.systemd}/bin/systemctl --machine=${u.name}@ --user daemon-reload || true
        fi
      '')
      cfg.serviceUsers;
  };
}
