# Base profile every llunde host imports (ADR 003: hosts carry only diffs).
{ ... }:
{
  # Grub device list comes from disko (EF02 partition registers the disk).
  boot.loader.grub.enable = true;

  time.timeZone = "UTC";

  # Root login stays enabled (keys only) — management is root-over-tailnet
  # (ADR 008); port 22 is break-glass until the phase-2 gate proves Tailscale,
  # then the firewall closes it (runbook).
  services.openssh = {
    enable = true;
    settings = {
      PasswordAuthentication = false;
      KbdInteractiveAuthentication = false;
      PermitRootLogin = "prohibit-password";
    };
  };

  users.users.root.openssh.authorizedKeys.keys = [
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH0jzc3S05J0DFj3W+Gv6J4Hc9fxvUjIOEuTWKfVnVY9 fhansteen@gmail.com"
  ];

  networking.firewall = {
    enable = true;
    allowedTCPPorts = [
      22
      80
      443
    ];
    trustedInterfaces = [ "tailscale0" ];
  };

  # Rootless Caddy (uid 2000) binds 80/443 directly (ADR 005/006).
  boot.kernel.sysctl."net.ipv4.ip_unprivileged_port_start" = 80;

  virtualisation.podman = {
    enable = true;
    dockerCompat = false;
  };

  # No system.autoUpgrade on purpose: NixOS updates flow through git (ADR 001) —
  # a new generation is always an explicit `nixos-rebuild switch --flake`.

  nix = {
    settings = {
      experimental-features = [
        "nix-command"
        "flakes"
      ];
      auto-optimise-store = true;
    };
    gc = {
      automatic = true;
      dates = "weekly";
      options = "--delete-older-than 30d";
    };
  };

  llunde.tailscale.enable = true;

  # Declarative-only user database (ADR 001): every user/group comes from this
  # config; imperative useradd on the box is rejected at the next switch.
  users.mutableUsers = false;
}
