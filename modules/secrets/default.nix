# Bootstrap-secrets MECHANISM via sops-nix (ADR 007): options + host-key
# decryption only. Each host declares its own sops.secrets set in
# hosts/<host>/secrets.nix and fills these paths (phase-3.5 stream A —
# llunde-01's five and llunde-parser's four differ, so the sets cannot
# live here).
{lib, ...}: {
  options.llunde.secrets = {
    dopplerTokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Doppler service token (sops-nix), env-file form.";
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
    dbEnvFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "EnvironmentFile with internal DB credentials (llunde-01 only — llunde-parser's DB password rides the Doppler render).";
    };
    ghcrAuthFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "containers-auth.json with a read:packages GHCR PAT — images are private by decision.";
    };
  };

  config = {
    sops.validateSopsFiles = true;
    sops.age.sshKeyPaths = ["/etc/ssh/ssh_host_ed25519_key"];
  };
}
