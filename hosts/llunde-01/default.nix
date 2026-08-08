# Host = diffs only (ADR 003): everything shared lives in modules/profiles.
{ ... }:
{
  imports = [
    ./disko.nix
    ../../modules/profiles/server.nix
  ];

  networking.hostName = "llunde-01";

  system.stateVersion = "25.11";
}
