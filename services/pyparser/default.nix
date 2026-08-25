{
  config,
  lib,
  pkgs,
  ...
}: let
  units = import ./unit.nix {inherit lib;};
in {
  options.llunde.pyparser.tunnel.enable = lib.mkOption {
    type = lib.types.bool;
    default = true;
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
      serviceUsers = [
        {
          name = "pyparser";
          uid = 2001;
        }
      ];
    };

    systemd.services.pyparser-secrets-render = {
      description = "Render Doppler pyparser/prd to /run/pyparser/secrets.env";
      wantedBy = ["multi-user.target"];
      before = ["user@2001.service"];
      wants = ["network-online.target"];
      after = ["network-online.target"];
      environment.HOME = "/root";
      serviceConfig = {
        Type = "oneshot";
        EnvironmentFile = config.llunde.secrets.dopplerTokenFile;
        UMask = "0027";
      };
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
