# Tailscale as the single management overlay (ADR 008): auto-join via the
# sops-provided auth key. MagicDNS picks up networking.hostName, so the box is
# `ssh llunde-01` on the tailnet; port 22 closure is staged post-gate (runbook).
{ config, lib, ... }:
let
  cfg = config.llunde.tailscale;
in
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

  config = lib.mkIf cfg.enable {
    services.tailscale = {
      enable = true;
      authKeyFile = cfg.authKeyFile;
      useRoutingFeatures = "client";
    };
  };
}
