# Host = diffs only (ADR 003): what llunde-parser IS — the pyparser stack
# under its own rootless user, the portfolio tenant slot, zero public ports
# (tunnel ingress + tailnet SSH, ADR 015/016). Phase-3.5 contract values.
{ modulesPath, pkgs, ... }:
{
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    ./disko.nix
    ./secrets.nix
    ../../modules/profiles/server.nix
    ../../modules/users
    ../../modules/tailscale
    ../../modules/secrets
    ../../modules/quadlet
    ../../modules/tenants
    ../../modules/backups
    ../../modules/observability
    ../../services/pyparser
  ];

  networking.hostName = "llunde-parser";

  # No public inbound at all (ADR 015): web ingress is the Cloudflare tunnel
  # (outbound), SSH rides the tailnet. publicTCPPorts stays its default [].

  # Contract uids. Explicit subuids on BOTH rootless users: auto-allocation
  # starts at 100000 and would collide with portfolio's declared range.
  llunde.users.services.pyparser = {
    uid = 2001;
    subUidStart = 165536;
  };

  llunde.tenants.portfolio = {
    uid = 3000;
    subUidStart = 100000;
    packages = with pkgs; [
      cosign
      doppler
      python3
      rsync
    ];
    # bootstrap.sh parity (its install.sh expects these to exist).
    homeDirectories = [
      ".config/containers/systemd"
      ".config/portfolio"
      "caddy"
    ];
  };

  llunde.backups = {
    enable = true;
    repository = "s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/restic/llunde-parser";
  };
  llunde.observability.enable = true;

  system.stateVersion = "25.11";
}
