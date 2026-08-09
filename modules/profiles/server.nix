# Base profile every llunde host imports (ADR 003: hosts carry only diffs).
{ ... }:
{
  # The Hetzner VM boots UEFI (go-live finding — the old docs' BIOS assumption
  # was wrong): systemd-boot on the ESP, no EFI variables (bootctl installs the
  # EFI/BOOT fallback path, so firmware NVRAM persistence doesn't matter).
  boot.loader.systemd-boot.enable = true;
  boot.loader.efi.canTouchEfiVariables = false;
  boot.loader.grub.enable = false;

  time.timeZone = "UTC";

  # Root login stays enabled (keys only) — management is root-over-tailnet
  # (ADR 008/015). sshd listens, but only the tailnet reaches it: 22 is not
  # in the public firewall here or in Hetzner's (runbook §11 to re-open).
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

  # podman's per-user wait-network-online polls the SYSTEM network-online.target
  # (podman#22197); nothing pulls it in by default with dhcpcd, so every user
  # quadlet queues forever without this (go-live finding).
  systemd.targets.network-online.wantedBy = [ "multi-user.target" ];

  # Declarative-only user database (ADR 001): every user/group comes from this
  # config; imperative useradd on the box is rejected at the next switch.
  users.mutableUsers = false;
}
