# Base profile every llunde host imports (ADR 003: hosts carry only diffs).
# Phase-2 streams extend this: users, tailscale, sysctls, linger, observability.
{ ... }:
{
  # Grub device list comes from disko (EF02 partition registers the disk).
  boot.loader.grub.enable = true;

  services.openssh = {
    enable = true;
    settings = {
      PasswordAuthentication = false;
      KbdInteractiveAuthentication = false;
    };
  };

  networking.firewall.enable = true;

  nix.settings.experimental-features = [
    "nix-command"
    "flakes"
  ];
}
