# llunde-01's bootstrap secrets (ADR 007): the sops files this host can decrypt.
# Mechanism (options, host-key decryption) lives in modules/secrets; this file is
# content only.
{
  config,
  lib,
  ...
}: {
  sops.secrets = {
    # Env-file form (DOPPLER_TOKEN=...): consumed as EnvironmentFile by the
    # backend quadlet, hence the explicit .env path.
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
    # for restic's S3 access.
    "restic-env" = {
      sopsFile = ../../secrets/restic.yaml;
      key = "env";
    };
    # Env-file form (POSTGRES_PASSWORD=... DB_PASSWORD=...), shared by the
    # postgres unit and the backend.
    "llunde-backend-db-env" = {
      sopsFile = ../../secrets/llunde-backend-db.yaml;
      key = "env";
      owner = "llunde-backend";
      path = "/run/secrets/llunde-backend-db.env";
    };
    # containers-auth.json for private GHCR pulls; group-readable by the
    # image-pulling service users, whose REGISTRY_AUTH_FILE points here.
    "ghcr-auth" = {
      sopsFile = ../../secrets/ghcr.yaml;
      key = "auth_json";
      mode = "0640";
      group = "ghcr";
      path = "/run/secrets/ghcr-auth.json";
    };
  };

  # Env-file form (TUNNEL_TOKEN=...): the llunde tunnel's connector token
  # (ADR 017). Never rotated casually.
  sops.secrets."llunde-tunnel" = {
    sopsFile = ../../secrets/llunde-tunnel.yaml;
    key = "env";
    owner = "edge";
    path = "/run/secrets/llunde-tunnel.env";
  };

  # containers-auth.json for `edge` alone (ADR 017 amended): the front door runs
  # ghcr.io/fredrir/llunde-caddy, built by THIS repo and private like it.
  # Deliberately NOT the shared `ghcr` group secret above, which is also the
  # backend's and frontend's — the internet-facing user holds its own revocable
  # copy. GHCR takes only CLASSIC PATs and `read:packages` cannot be scoped
  # per-package, so the reach is identical; this buys independent rotation and
  # no other scopes.
  sops.secrets."llunde-caddy-ghcr" = {
    sopsFile = ../../secrets/llunde-caddy-ghcr.yaml;
    key = "auth_json";
    owner = "edge";
    path = "/run/secrets/llunde-caddy-ghcr.json";
  };

  # Env-file form (CF_API_TOKEN=...): the ACME DNS-01 credential (ADR 017
  # amended). Scoped Zone:DNS:Edit on llunde.no and NOTHING else — verified to
  # create and delete a TXT record, and REFUSED account tunnel access.
  # Deliberately not the laptop's ops token, which carries account-wide tunnel
  # rights over the portfolio and pyparser tenants (ADR 016 boundary) and must
  # never reach a host.
  #
  # Accepted residual (ADR 017): zone-write authority on the internet-facing box
  # means a compromise of `edge` is takeover of llunde.no, email (MX/SPF/DKIM)
  # included. Cloudflare cannot scope DNS edit below the zone.
  sops.secrets."llunde-caddy-acme" = {
    sopsFile = ../../secrets/llunde-caddy-acme.yaml;
    key = "env";
    owner = "edge";
    path = "/run/secrets/llunde-caddy-acme.env";
  };

  # GitOps pull (ADR 020): read-only deploy key + this host's external heartbeat
  # URL. In sops on purpose — NOT the obs deadman's out-of-band /var/lib file.
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
