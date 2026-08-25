{
  config,
  lib,
  ...
}: {
  options.llunde.profile.publicTCPPorts = lib.mkOption {
    type = lib.types.listOf lib.types.port;
    default = [];
  };

  config = {
    boot.loader.systemd-boot.enable = true;
    boot.loader.efi.canTouchEfiVariables = false;
    boot.loader.grub.enable = false;
    boot.loader.systemd-boot.configurationLimit = 5;

    time.timeZone = "UTC";

    services.openssh = {
      enable = true;
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

    boot.kernel.sysctl."net.ipv4.ip_unprivileged_port_start" = 80;

    virtualisation.podman = {
      enable = true;
      dockerCompat = false;
    };

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

    systemd.targets.network-online.wantedBy = ["multi-user.target"];

    users.mutableUsers = false;
  };
}
