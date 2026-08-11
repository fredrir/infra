# Base profile every llunde host imports (ADR 003: hosts carry only diffs).
{
  config,
  lib,
  ...
}: {
  options.llunde.profile.publicTCPPorts = lib.mkOption {
    type = lib.types.listOf lib.types.port;
    default = [];
    description = ''
      TCP ports open on public interfaces. llunde-01 opens 80/443 for Caddy;
      llunde-parser opens nothing (tunnel ingress, tailnet SSH — ADR 015).
      The tailnet is always trusted regardless.
    '';
  };

  config = {
    # The Hetzner VM boots UEFI (go-live finding — the old docs' BIOS assumption
    # was wrong): systemd-boot on the ESP, no EFI variables (bootctl installs the
    # EFI/BOOT fallback path, so firmware NVRAM persistence doesn't matter).
    boot.loader.systemd-boot.enable = true;
    boot.loader.efi.canTouchEfiVariables = false;
    boot.loader.grub.enable = false;
    # The ESP is 512M (disko): ~10 mixed-kernel generations would overflow it.
    # 5 keeps the boot menu (the boot-plane recovery path, ADR 020 residual)
    # usable without filling the partition. Rollback depth beyond that rides
    # `git push -f deploy`, not the bootloader.
    boot.loader.systemd-boot.configurationLimit = 5;

    time.timeZone = "UTC";

    # Root login stays enabled (keys only) — management is root-over-tailnet
    # (ADR 008/015). sshd listens, but only the tailnet reaches it: 22 is not
    # in the public firewall here or in Hetzner's (runbook §11 to re-open).
    services.openssh = {
      enable = true;
      # sshd listens but the module must NOT punch 22 into the host firewall
      # (its default does): tailnet SSH arrives via the trusted tailscale0
      # interface; public 22 stays closed at both layers (ADR 015).
      openFirewall = false;
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
      allowedTCPPorts = config.llunde.profile.publicTCPPorts;
      trustedInterfaces = ["tailscale0"];
    };

    # Rootless Caddy (uid 2000) binds 80/443 directly (ADR 005/006).
    boot.kernel.sysctl."net.ipv4.ip_unprivileged_port_start" = 80;

    virtualisation.podman = {
      enable = true;
      dockerCompat = false;
    };

    # No system.autoUpgrade on purpose — but the estate is NOT manually
    # deployed either: modules/gitops-pull (ADR 020) applies the gate-green
    # `deploy` pointer on each host. autoUpgrade would track a channel/flakeref
    # tip with none of the gate/promoter/deadman machinery around it.

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
    systemd.targets.network-online.wantedBy = ["multi-user.target"];

    # Declarative-only user database (ADR 001): every user/group comes from this
    # config; imperative useradd on the box is rejected at the next switch.
    users.mutableUsers = false;
  };
}
