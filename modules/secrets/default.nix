# Bootstrap secrets via sops-nix (ADR 007), decrypted with the host's ed25519
# key into /run/secrets. Three bootstrap secrets plus the infra-internal DB env
# file (contract.md lead decision extending ADR 007: postgres/valkey cannot wrap
# Doppler, so their creds ride sops too).
{ config, lib, ... }:
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
    dbEnvFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "EnvironmentFile with Postgres/Valkey internal credentials (contract.md).";
    };
  };

  config = {
    # Placeholder secret files until go-live populates real sops-encrypted
    # values (tasks.md 2.2); real recipients replace .sops.yaml placeholders
    # first, then this flips to true.
    sops.validateSopsFiles = false;

    sops.age.sshKeyPaths = [ "/etc/ssh/ssh_host_ed25519_key" ];

    sops.secrets = {
      # Env-file form (DOPPLER_TOKEN=...): consumed as EnvironmentFile by the
      # backend quadlet, hence the explicit .env path (stream B2 contract).
      "doppler-token" = {
        sopsFile = ../../secrets/doppler.yaml;
        key = "doppler_token";
        owner = "llunde-backend";
        path = "/run/secrets/doppler.env";
      };
      "tailscale-auth-key" = {
        sopsFile = ../../secrets/tailscale.yaml;
        key = "auth_key";
      };
      "restic-password" = {
        sopsFile = ../../secrets/restic.yaml;
        key = "password";
      };
      # Env-file form: AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY/AWS_DEFAULT_REGION
      # for restic's S3 access (contract amendment, stream D2 finding).
      "restic-env" = {
        sopsFile = ../../secrets/restic.yaml;
        key = "env";
      };
      # Env-file form (POSTGRES_PASSWORD=... DB_PASSWORD=...), shared by the
      # postgres unit and the backend (contract.md secrets flow).
      "llunde-backend-db-env" = {
        sopsFile = ../../secrets/llunde-backend-db.yaml;
        key = "env";
        owner = "llunde-backend";
        path = "/run/secrets/llunde-backend-db.env";
      };
    };

    llunde.secrets = {
      dopplerTokenFile = config.sops.secrets."doppler-token".path;
      tailscaleAuthKeyFile = config.sops.secrets."tailscale-auth-key".path;
      resticPasswordFile = config.sops.secrets."restic-password".path;
      dbEnvFile = config.sops.secrets."llunde-backend-db-env".path;
    };

    llunde.tailscale.authKeyFile = lib.mkDefault config.sops.secrets."tailscale-auth-key".path;
  };
}
