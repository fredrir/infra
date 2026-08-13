# Host = diffs only (ADR 003): everything shared lives in modules/; this file
# says what llunde-01 IS — its services, its users, its disk.
{
  config,
  modulesPath,
  ...
}: {
  imports = [
    # Hetzner Cloud is QEMU/KVM: without this the initrd lacks virtio drivers
    # and the box hangs before finding its root disk.
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

  # ZERO public inbound, estate-wide (ADR 017). Caddy still BINDS 80/443, and
  # the listeners stay so that re-opening the cloud firewall is the only step
  # break-glass needs (runbook §7.5); public traffic arrives through the tunnel
  # cloudflared dials outbound. The Hetzner firewall is emptied out of band —
  # the hcloud provider cannot delete a firewall's LAST rules: its update omits
  # the field, no-ops, and still prints "Apply complete".
  llunde.profile.publicTCPPorts = [];

  # Fixed uids: the quadlet paths /etc/containers/systemd/users/<uid>/ depend on
  # them, so these values do not move.
  llunde.users.services = {
    edge.uid = 2000;
    llunde-backend.uid = 2001;
    llunde-frontend.uid = 2002;
  };

  llunde.ingress.enable = true;
  # Serve the vhosts through the llunde tunnel (ADR 017).
  llunde.ingress.tunnel = {
    enable = true;
    tokenFile = "/run/secrets/llunde-tunnel.env";
  };
  # ACME over DNS-01 (ADR 017 amended): certificates keep renewing with 80/443
  # closed, and warm certs make the break-glass grey-flip fast rather than a
  # race with Let's Encrypt.
  llunde.ingress.acmeDnsTokenFile = "/run/secrets/llunde-caddy-acme.env";
  # GitOps pull auto-apply (ADR 020): this host is the CANARY — it applies
  # promoted revs immediately, llunde-parser follows 30 min behind.
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
            # Never bounce the front door onto a Caddyfile that cannot load.
            # The check validates with the CONTAINER IMAGE, not pkgs.caddy (see
            # the option's docs in modules/ingress): version skew breaks the
            # moment the config references a MODULE only one build contains.
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
    # Journal → the collection stack on llunde-parser, over the tailnet (ADR 018).
    lokiUrl = "http://100.92.219.50:3100";
  };

  system.stateVersion = "25.11";
}
