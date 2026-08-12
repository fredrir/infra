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

  # containers-auth.json for `edge` alone (H1, ADR 017 amended): the front door
  # now runs an image built by THIS repo, ghcr.io/fredrir/llunde-caddy, which is
  # private like the repo. Deliberately NOT the shared `ghcr` group secret above:
  # that credential is also the backend's and frontend's, and the internet-facing
  # user should hold its own revocable copy rather than the one everything else
  # depends on. GHCR accepts only CLASSIC PATs and `read:packages` cannot be
  # scoped per-package, so the reach is the same either way — what this buys is
  # independent rotation, and no other scopes on the token.
  sops.secrets."llunde-caddy-ghcr" = {
    sopsFile = ../../secrets/llunde-caddy-ghcr.yaml;
    key = "auth_json";
    owner = "edge";
    path = "/run/secrets/llunde-caddy-ghcr.json";
  };

  # Env-file form (CF_API_TOKEN=...): the ACME DNS-01 credential (H1, ADR 017
  # amended). Scoped Zone:DNS:Edit on llunde.no and NOTHING else — verified to
  # create and delete a TXT record, and verified to be REFUSED account tunnel
  # access. Deliberately not the laptop's ops token, which also carries
  # account-wide Cloudflare Tunnel rights over the portfolio and pyparser
  # tenants' tunnels (ADR 016 boundary) and must never reach a host.
  #
  # Residual, accepted with eyes open (ADR 017): this is zone-write authority
  # living on the internet-facing box, so a compromise of `edge` is takeover of
  # llunde.no — including MX/SPF/DKIM, i.e. email. Cloudflare cannot scope DNS
  # edit below the zone.
  sops.secrets."llunde-caddy-acme" = {
    sopsFile = ../../secrets/llunde-caddy-acme.yaml;
    key = "env";
    owner = "edge";
    path = "/run/secrets/llunde-caddy-acme.env";
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
