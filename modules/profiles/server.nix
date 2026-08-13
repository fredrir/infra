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
      TCP ports open on public interfaces. BOTH hosts leave this empty
      (ADR 017): all web ingress arrives through Cloudflare
      tunnels the hosts dial outbound, and SSH rides the tailnet (ADR 015).
      The estate has zero public inbound. The tailnet is always trusted
      regardless, which is what keeps 9100/9101 reachable for scraping.
    '';
  };

  config = {
    # The Hetzner VM boots UEFI: systemd-boot on the ESP, no EFI variables —
    # bootctl installs the EFI/BOOT fallback path, so firmware NVRAM
    # persistence does not matter.
    boot.loader.systemd-boot.enable = true;
    boot.loader.efi.canTouchEfiVariables = false;
    boot.loader.grub.enable = false;
    # The ESP is 512M (disko): ~10 mixed-kernel generations would overflow it.
    # 5 keeps the boot menu — the boot-plane recovery path (ADR 020 residual) —
    # usable. Deeper rollback rides `git push -f deploy`, not the bootloader.
    boot.loader.systemd-boot.configurationLimit = 5;

    time.timeZone = "UTC";

    # Root login stays enabled (keys only): management is root-over-tailnet
    # (ADR 008/015). sshd listens, but 22 is in neither this firewall nor
    # Hetzner's — runbook §11 re-opens it.
    services.openssh = {
      enable = true;
      # The module default would punch 22 into the host firewall; tailnet SSH
      # arrives on the trusted tailscale0 interface instead (ADR 015).
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

    # Rootless Caddy (uid 2000) binds 80/443 directly (ADR 005/006). Still
    # required with zero public inbound: the sockets stay bound but unreachable,
    # and break-glass re-opens the firewall rather than reconfiguring Caddy.
    boot.kernel.sysctl."net.ipv4.ip_unprivileged_port_start" = 80;

    virtualisation.podman = {
      enable = true;
      dockerCompat = false;
    };

    # No system.autoUpgrade, and no manual deploys either: modules/gitops-pull
    # (ADR 020) applies the gate-green `deploy` pointer on each host, whereas
    # autoUpgrade would track a channel/flakeref tip with none of the
    # gate/promoter/deadman machinery around it.

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

    # podman's per-user wait-network-online polls the SYSTEM
    # network-online.target (podman#22197), which nothing pulls in by default
    # under dhcpcd — without this, every user quadlet queues forever.
    systemd.targets.network-online.wantedBy = ["multi-user.target"];

    # Declarative-only user database (ADR 001): every user/group comes from this
    # config, and an imperative useradd is undone at the next switch.
    users.mutableUsers = false;
  };
}
