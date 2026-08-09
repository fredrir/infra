# The three bootstrap secrets — everything else lives in Doppler (ADR 007).
# Option surface only in phase 1; phase 2 wires sops-nix files + owners.
{ lib, ... }:
{
  options.llunde.secrets = {
    dopplerTokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Doppler service token (sops-nix), consumed by service users' quadlet wrappers.";
    };
    tailscaleAuthKeyFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Tailscale auth key (sops-nix).";
    };
    resticPasswordFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "restic repository password (sops-nix).";
    };
  };
}
