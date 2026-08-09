# Quadlet plumbing (ADR 004): collects mkQuadlet fragments into environment.etc
# and owns the per-user machinery — the auto-update timer and the switch-time
# daemon-reload poke. User creation/subuid/linger live in modules/users (A2).
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.llunde.quadlet;
  autoUsers = lib.filter (u: u.autoUpdate) cfg.serviceUsers;

  # `|` prefix = triggering condition: the unit runs for a user iff at least one
  # ConditionUser matches, so one shared /etc/systemd/user unit is effectively
  # per-user gated without touching unrelated users (e.g. openclaw).
  conditionLines = lib.concatMapStringsSep "\n" (u: "ConditionUser=|${toString u.uid}") autoUsers;

  autoUpdateService = pkgs.writeText "llunde-auto-update.service" ''
    [Unit]
    Description=podman auto-update for llunde service users
    ${conditionLines}

    [Service]
    Type=oneshot
    Environment=REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json
    ExecStart=${pkgs.podman}/bin/podman auto-update
  '';

  # Merge-to-main-is-deploy (ADR 010): poll GHCR every 5 minutes.
  autoUpdateTimer = pkgs.writeText "llunde-auto-update.timer" ''
    [Unit]
    Description=Poll registries and restart updated llunde containers
    ${conditionLines}

    [Timer]
    OnCalendar=*:0/5
    RandomizedDelaySec=60

    [Install]
    WantedBy=timers.target
  '';
in
{
  options.llunde.quadlet = {
    units = lib.mkOption {
      type = with lib.types; listOf (attrsOf anything);
      default = [ ];
      description = "environment.etc fragments produced by mkQuadlet (mkContainerUnit/mkNetworkUnit).";
    };

    serviceUsers = lib.mkOption {
      type =
        with lib.types;
        listOf (submodule {
          options = {
            name = lib.mkOption { type = lib.types.str; };
            uid = lib.mkOption { type = lib.types.int; };
            autoUpdate = lib.mkOption {
              type = lib.types.bool;
              default = true;
              description = "Whether this user's containers use AutoUpdate=registry.";
            };
          };
        });
      default = [ ];
      description = "Service users owning quadlet units (drives auto-update gating and reload pokes).";
    };
  };

  config = {
    environment.etc = lib.mkMerge (
      cfg.units
      ++ lib.optionals (autoUsers != [ ]) [
        {
          "systemd/user/llunde-auto-update.service".source = autoUpdateService;
          "systemd/user/llunde-auto-update.timer".source = autoUpdateTimer;
          # Static enablement; ConditionUser does the per-user gating.
          "systemd/user/timers.target.wants/llunde-auto-update.timer".source = autoUpdateTimer;
        }
      ]
    );

    # Best-effort v1 (documented wrinkle, ADR 004): after switch, poke each
    # running user manager so the quadlet generator re-reads unit files.
    # This RELOADS units; restarting changed containers is auto-update's job
    # (or manual `systemctl --user restart`). Falls back silently when the
    # user manager isn't up (first boot: linger starts it fresh anyway).
    system.activationScripts.llundeQuadletReload.text = lib.concatMapStringsSep "\n" (u: ''
      if [ -d /run/user/${toString u.uid} ]; then
        ${pkgs.systemd}/bin/systemctl --machine=${u.name}@ --user daemon-reload || true
      fi
    '') cfg.serviceUsers;
  };
}
