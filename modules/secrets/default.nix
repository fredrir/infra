# Bootstrap-secrets mechanism
{lib, ...}: {
  options.llunde.secrets = {
    dopplerTokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
    };
    tailscaleAuthKeyFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
    };
    resticPasswordFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
    };
    dbEnvFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
    };
    ghcrAuthFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
    };
  };

  config = {
    sops.validateSopsFiles = true;
    sops.age.sshKeyPaths = ["/etc/ssh/ssh_host_ed25519_key"];
  };
}
