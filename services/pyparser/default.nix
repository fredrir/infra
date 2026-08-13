# pyparser service slice: the compose stack as rootless quadlets under the
# pyparser user (uid 2001), plus the root-level Doppler render every container
# consumes. Unit text lives in ./unit.nix so the flake can golden-test it.
{
  config,
  lib,
  pkgs,
  ...
}: let
  units = import ./unit.nix {inherit lib;};
in {
  # Identity guard for rehearsal machines: a second live connector on the
  # pyparser tunnel takes real production traffic, so scratch installs disable
  # the tunnel unit entirely.
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
      # autoUpdate=true (default) enrolls uid 2001 in the shared auto-update
      # timer (modules/quadlet); only AutoUpdate=registry units (review, both
      # workers) get restarted by it.
      serviceUsers = [
        {
          name = "pyparser";
          uid = 2001;
        }
      ];
    };

    # Doppler pyparser/prd → /run/pyparser/secrets.env: the quadlets'
    # EnvironmentFile=, replacing compose's env_file, single source for
    # POSTGRES_PASSWORD, TUNNEL_TOKEN and the app secrets.
    #
    # ORDERING IS THE POINT: user units cannot After= system units, so the SYSTEM
    # manager orders this render Before=user@2001.service — the pyparser user
    # manager (linger) holds its quadlets until the env file exists. sops needs no
    # ordering: sops-nix installs secrets in the setupSecrets activation script,
    # which completes before any unit starts. Re-render with `systemctl start
    # pyparser-secrets-render`: a oneshot WITHOUT RemainAfterExit returns to
    # inactive, so a plain `start` re-runs it (with it, a no-op).
    systemd.services.pyparser-secrets-render = {
      description = "Render Doppler pyparser/prd to /run/pyparser/secrets.env";
      wantedBy = ["multi-user.target"];
      before = ["user@2001.service"];
      wants = ["network-online.target"];
      after = ["network-online.target"];
      # doppler wants a writable config dir even in token mode, and systemd sets
      # no HOME without User=.
      environment.HOME = "/root";
      serviceConfig = {
        Type = "oneshot";
        # DOPPLER_TOKEN=… — project/config are baked into the service token, so
        # the download needs no further scoping (as in deploy-remote.sh).
        EnvironmentFile = config.llunde.secrets.dopplerTokenFile;
        UMask = "0027";
      };
      # Atomic write: `set -e` aborts a failed or empty fetch before the mv,
      # leaving the last-good secrets.env in place.
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
