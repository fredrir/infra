{
  lib,
  pkgs,
  ...
}: {
  boot.kernelPackages = lib.mkDefault pkgs.linuxPackages_6_18;
  services.tailscale.package = import ./tailscale-package.nix {inherit pkgs;};
}
