# Host = diffs only (ADR 003): everything shared lives in modules/; this file
# says what llunde-01 IS — its services, its users, its disk.
{ modulesPath, ... }:
{
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
  llunde.backups.enable = true;
  llunde.observability.enable = true;

  system.stateVersion = "25.11";
}
