{
  config,
  modulesPath,
  ...
}: {
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    ./disko.nix
    ./secrets.nix
    ../../modules/profiles/server.nix
    ../../modules/users
    ../../modules/tailscale
    ../../modules/secrets
    ../../modules/quadlet
    ../../modules/data
    ../../modules/ingress
    ../../modules/backups
    ../../modules/observability
    ../../modules/gitops-pull
    ../../services/llunde-backend
    ../../services/llunde-frontend
  ];

  networking.hostName = "llunde-01";

  llunde.profile.publicTCPPorts = [];

  llunde.users.services = {
    edge.uid = 2000;
    llunde-backend.uid = 2001;
    llunde-frontend.uid = 2002;
  };

  llunde.ingress.enable = true;
  llunde.ingress.tunnel = {
    enable = true;
    tokenFile = "/run/secrets/llunde-tunnel.env";
  };
  llunde.ingress.acmeDnsTokenFile = "/run/secrets/llunde-caddy-acme.env";
  llunde.gitopsPull = {
    enable = true;
    sshKeyFile = "/run/secrets/gitops-deploy-key";
    heartbeatUrlFile = "/run/secrets/gitops-heartbeat-url";
    probePeer = "100.92.219.50"; # llunde-parser
    reconcile = {
      edge = {
        uid = 2000;
        units = {
          caddy = {
            watch = [
              "/etc/containers/systemd/users/2000/caddy.container"
              "/etc/llunde/caddy/Caddyfile"
            ];
            check = config.llunde.ingress.caddyfileCheck;
          };
          cloudflared.watch = ["/etc/containers/systemd/users/2000/cloudflared.container"];
        };
      };
      llunde-backend = {
        uid = 2001;
        units = {
          llunde-backend.watch = [
            "/etc/containers/systemd/users/2001/llunde-backend.container"
            "/etc/containers/systemd/users/2001/llunde-backend-data.network"
          ];
          llunde-postgres.watch = [
            "/etc/containers/systemd/users/2001/llunde-postgres.container"
            "/etc/containers/systemd/users/2001/llunde-backend-data.network"
          ];
          llunde-valkey.watch = [
            "/etc/containers/systemd/users/2001/llunde-valkey.container"
            "/etc/containers/systemd/users/2001/llunde-backend-data.network"
          ];
        };
      };
      llunde-frontend = {
        uid = 2002;
        units.llunde-frontend.watch = ["/etc/containers/systemd/users/2002/llunde-frontend.container"];
      };
    };
  };

  llunde.backups.enable = true;
  llunde.observability = {
    enable = true;
    lokiUrl = "http://100.92.219.50:3100";
  };

  system.stateVersion = "25.11";
}
