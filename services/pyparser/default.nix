# pyparser service slice (phase 3.5): the whole compose stack as rootless
# quadlets under the pyparser user (uid 2001), plus the root-level Doppler
# render every container consumes. Values from
# docs/init/plans/phase-3.5/contract.md; unit text lives in ./unit.nix so the
# flake can golden-test it.
{
  config,
  lib,
  pkgs,
  ...
}: let
  units = import ./unit.nix {inherit lib;};
in {
  # Identity guard for rehearsal machines (plan tasks 2.1): a second live
  # connector on the pyparser tunnel would receive real production traffic,
  # so scratch installs disable the tunnel unit entirely.
  options.llunde.pyparser.tunnel.enable = lib.mkOption {
    type = lib.types.bool;
    default = true;
    description = "Run the cloudflared connector. NEVER true on a rehearsal box while prod lives.";
  };

  config = {
    llunde.quadlet = {
      units = map (u: u.fragment) (
        [
          units.network
          units.postgres
          units.migrate
          units.review
          units.workerExtract
          units.workerLight
        ]
        ++ lib.optional config.llunde.pyparser.tunnel.enable units.cloudflared
      );
      # autoUpdate=true (the default) enrolls uid 2001 in the shared
      # auto-update timer (modules/quadlet); only the units carrying
      # AutoUpdate=registry — review and the two workers — get restarted by it.
      serviceUsers = [
        {
          name = "pyparser";
          uid = 2001;
        }
      ];
    };

    # Doppler pyparser/prd → /run/pyparser/secrets.env: the quadlets'
    # EnvironmentFile=, replacing compose's env_file. Single source for
    # POSTGRES_PASSWORD, TUNNEL_TOKEN and the app secrets (contract).
    #
    # ORDERING IS THE POINT: user units cannot After= system units, so the
    # SYSTEM manager orders this render Before=user@2001.service — the pyparser
    # user manager (linger) does not bring up its quadlets until the env file
    # exists. This is the reboot test's explicit proof. sops secrets need no
    # explicit ordering here: sops-nix installs them in the setupSecrets
    # activation script, which completes before systemd starts any unit.
    #
    # Re-render on demand: `systemctl start pyparser-secrets-render` — a
    # oneshot WITHOUT RemainAfterExit returns to inactive after each run, so a
    # plain `start` re-runs it (with RemainAfterExit it would be a no-op).
    systemd.services.pyparser-secrets-render = {
      description = "Render Doppler pyparser/prd to /run/pyparser/secrets.env";
      wantedBy = ["multi-user.target"];
      before = ["user@2001.service"];
      wants = ["network-online.target"];
      after = ["network-online.target"];
      # The doppler CLI wants a writable config dir even in token mode
      # (llunde-backend go-live finding); systemd does not set HOME without User=.
      environment.HOME = "/root";
      serviceConfig = {
        Type = "oneshot";
        # DOPPLER_TOKEN=… — project/config are baked into the service token,
        # so the download needs no further scoping (same as deploy-remote.sh).
        EnvironmentFile = config.llunde.secrets.dopplerTokenFile;
        UMask = "0027";
      };
      # Atomic write: the script runs with `set -e`, so a failed or empty fetch
      # aborts before the mv and leaves the last-good secrets.env in place.
      script = ''
        install -d -m 0750 -o root -g pyparser /run/pyparser
        tmp="$(mktemp /run/pyparser/.secrets.env.XXXXXX)"
        ${pkgs.doppler}/bin/doppler secrets download --no-file --format docker > "$tmp"
        test -s "$tmp"
        chown root:pyparser "$tmp"
        chmod 0640 "$tmp"
        mv "$tmp" /run/pyparser/secrets.env
      '';
    };
  };
}
