# Tailscale as the single management overlay (ADR 008). Option surface only in
# phase 1; phase 2 implements auto-join (auth key via sops) and the staged
# port-22 closure documented in the runbook.
{ lib, ... }:
{
  options.llunde.tailscale = {
    enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Join the tailnet at boot using the sops-provided auth key.";
    };
    authKeyFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Path to the Tailscale auth key (sops-nix /run/secrets, ADR 007).";
    };
  };
}
