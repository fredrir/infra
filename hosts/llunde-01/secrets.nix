# llunde-01's bootstrap secrets (ADR 007): the five sops files this host can
# decrypt, exactly as deployed at the phase-2 go-live. Mechanism (options,
# host-key decryption) lives in modules/secrets; this file is content only
# (phase-3.5 per-host split).
{
  config,
  lib,
  ...
}: {
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
    # containers-auth.json for private GHCR pulls; group-readable by the
    # image-pulling service users (REGISTRY_AUTH_FILE points here).
    "ghcr-auth" = {
      sopsFile = ../../secrets/ghcr.yaml;
      key = "auth_json";
      mode = "0640";
      group = "ghcr";
      path = "/run/secrets/ghcr-auth.json";
    };
  };

  # Env-file form (TUNNEL_TOKEN=...): the llunde tunnel's connector token
  # (phase-4 workstream E, ADR 017). Never rotated casually.
  sops.secrets."llunde-tunnel" = {
    sopsFile = ../../secrets/llunde-tunnel.yaml;
    key = "env";
    owner = "edge";
    path = "/run/secrets/llunde-tunnel.env";
  };

  # GitOps pull (ADR 020, workstream A phase 2c): the read-only deploy key and
  # this host's external heartbeat URL. In sops on purpose — NOT the obs
  # deadman's out-of-band /var/lib file pattern (review B6).
  sops.secrets."gitops-deploy-key" = {
    sopsFile = ../../secrets/gitops-llunde-01.yaml;
    key = "deploy_key";
  };
  sops.secrets."gitops-heartbeat-url" = {
    sopsFile = ../../secrets/gitops-llunde-01.yaml;
    key = "heartbeat_url";
  };

  users.groups.ghcr.members = [
    "llunde-backend"
    "llunde-frontend"
  ];

  llunde.secrets = {
    dopplerTokenFile = config.sops.secrets."doppler-token".path;
    tailscaleAuthKeyFile = config.sops.secrets."tailscale-auth-key".path;
    resticPasswordFile = config.sops.secrets."restic-password".path;
    dbEnvFile = config.sops.secrets."llunde-backend-db-env".path;
    ghcrAuthFile = config.sops.secrets."ghcr-auth".path;
  };

  llunde.tailscale.authKeyFile = lib.mkDefault config.sops.secrets."tailscale-auth-key".path;
}
