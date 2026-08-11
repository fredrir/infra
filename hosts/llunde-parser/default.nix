# Host = diffs only (ADR 003): what llunde-parser IS — the pyparser stack
# under its own rootless user, the portfolio tenant slot, zero public ports
# (tunnel ingress + tailnet SSH, ADR 015/016). Phase-3.5 contract values.
{
  modulesPath,
  pkgs,
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
    ../../modules/tenants
    ../../modules/backups
    ../../modules/observability
    ../../modules/gitops-pull
    ../../services/pyparser
    ../../services/observability
  ];

  networking.hostName = "llunde-parser";

  # portfolio's install.sh probes `command -v cosign` in a ROOT shell and
  # curl-installs into /usr/local/bin when missing — a path that cannot exist
  # here. System-wide cosign keeps the guard true (cutover finding).
  environment.systemPackages = [pkgs.cosign];

  # No public inbound at all (ADR 015): web ingress is the Cloudflare tunnel
  # (outbound), SSH rides the tailnet. publicTCPPorts stays its default [].

  # Contract uids. Explicit subuids on ALL rootless users: auto-allocation
  # starts at 100000 and would collide with portfolio's declared range.
  llunde.users.services.pyparser = {
    uid = 2001;
    subUidStart = 165536;
  };

  # Phase-4 workstream O (ADR 018): the collection stack's rootless user.
  # 20xx uids are per-host — 2002 means llunde-frontend on llunde-01 and
  # observability here. Range starts where pyparser's ends (165536 + 65536).
  llunde.users.services.observability = {
    uid = 2002;
    subUidStart = 231072;
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
    # bootstrap.sh parity (its install.sh expects these to exist). Every path
    # level is listed: tmpfiles creates unlisted parents as root and then
    # refuses its own unsafe ownership transition (cutover finding).
    homeDirectories = [
      ".config"
      ".config/containers"
      ".config/containers/systemd"
      ".config/portfolio"
      "caddy"
    ];
  };

  llunde.backups = {
    enable = true;
    repository = "s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/restic/llunde-parser";
  };
  llunde.observability = {
    enable = true;
    # Journal → the local Loki quadlet; via the tailnet address so the shape
    # is identical on every host (phase-4 O3).
    lokiUrl = "http://100.92.219.50:3100";
  };

  system.stateVersion = "25.11";
}
