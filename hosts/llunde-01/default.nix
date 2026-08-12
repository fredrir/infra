# Host = diffs only (ADR 003): everything shared lives in modules/; this file
# says what llunde-01 IS — its services, its users, its disk.
{
  config,
  modulesPath,
  ...
}: {
  imports = [
    # Hetzner Cloud is QEMU/KVM: without this the initrd lacks virtio drivers
    # and the box hangs before finding its root disk (go-live finding).
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

  # Caddy's public ports (ADR 006); llunde-parser opens none (ADR 015).
  llunde.profile.publicTCPPorts = [
    80
    443
  ];

  # Fixed uids are contract values (docs/init/plans/phase-2/contract.md);
  # quadlet paths /etc/containers/systemd/users/<uid>/ depend on them.
  llunde.users.services = {
    edge.uid = 2000;
    llunde-backend.uid = 2001;
    llunde-frontend.uid = 2002;
  };

  llunde.ingress.enable = true;
  # Phase-4 workstream E (ADR 017): serve the vhosts through the llunde tunnel.
  llunde.ingress.tunnel = {
    enable = true;
    tokenFile = "/run/secrets/llunde-tunnel.env";
  };
  # H1 (ADR 017 amended): ACME moves to DNS-01 so certificates keep renewing
  # once E6 closes 80/443 — warm certs are what make the break-glass grey-flip
  # fast instead of a race with Let's Encrypt.
  llunde.ingress.acmeDnsTokenFile = "/run/secrets/llunde-caddy-acme.env";
  # GitOps pull auto-apply (ADR 020, phase 2c): this host is the CANARY —
  # applies promoted revs immediately; llunde-parser follows 30min behind.
  # Rehearsed end-to-end on a throwaway box first (happy path, build-fail,
  # reconcile, two sever/deadman rollbacks, hold semantics, hcloud reset).
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
            # PR #12's lesson carried into automation: never bounce the front
            # door onto a Caddyfile that cannot load. The command validates
            # using the CONTAINER IMAGE, not pkgs.caddy — see the option's docs
            # in modules/ingress. Version skew used to be "fine for syntax
            # checks"; it stopped being fine the moment the config referenced a
            # MODULE that only one of the two builds contains.
            check = config.llunde.ingress.caddyfileCheck;
          };
          cloudflared.watch = ["/etc/containers/systemd/users/2000/cloudflared.container"];
        };
      };
      llunde-backend = {
        uid = 2001;
        # The shared .network rides in every dependent unit's watch: a network
        # change must recreate all attached containers.
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
    # Journal → the collection stack on llunde-parser over the tailnet
    # (phase-4 O3, ADR 018).
    lokiUrl = "http://100.92.219.50:3100";
  };

  system.stateVersion = "25.11";
}
