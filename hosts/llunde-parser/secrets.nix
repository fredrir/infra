# llunde-parser's bootstrap secrets: no sops DB password — POSTGRES_PASSWORD is
# single-sourced from the Doppler render. Mechanism lives in modules/secrets;
# recipients in .sops.yaml (per-host rules).
{
  config,
  lib,
  ...
}: {
  sops.secrets = {
    # Env-file form (DOPPLER_TOKEN=...): consumed by the root-level env-render
    # oneshot (services/pyparser), never by containers directly.
    "pyparser-doppler" = {
      sopsFile = ../../secrets/pyparser-doppler.yaml;
      key = "doppler_token";
      path = "/run/secrets/pyparser-doppler.env";
    };
    "tailscale-auth-key" = {
      sopsFile = ../../secrets/tailscale.yaml;
      key = "auth_key";
    };
    "restic-password" = {
      sopsFile = ../../secrets/pyparser-restic.yaml;
      key = "password";
    };
    "restic-env" = {
      sopsFile = ../../secrets/pyparser-restic.yaml;
      key = "env";
    };
    # Same read-only GHCR PAT as llunde-01 (shared file): pyparser-review is a
    # private package; the pyparser user pulls with REGISTRY_AUTH_FILE.
    "ghcr-auth" = {
      sopsFile = ../../secrets/ghcr.yaml;
      key = "auth_json";
      mode = "0640";
      group = "ghcr";
      path = "/run/secrets/ghcr-auth.json";
    };
  };

  # Grafana's SES SMTP credential (GF_SMTP_USER/GF_SMTP_PASSWORD env form):
  # send-only IAM user, From-pinned to alerts@llunde.no.
  sops.secrets."observability-smtp" = {
    sopsFile = ../../secrets/observability-smtp.yaml;
    key = "env";
    owner = "observability";
    path = "/run/secrets/observability-smtp.env";
  };

  # Grafana admin password (GF_SECURITY_ADMIN_PASSWORD env form) — defense in
  # depth: basic auth is off and the login form hidden, so no auth path to admin
  # exists in normal ops, but this kills the admin:admin default if login is
  # ever re-enabled for break-glass. Read it with
  # `sops -d secrets/observability-grafana.yaml`.
  sops.secrets."observability-grafana" = {
    sopsFile = ../../secrets/observability-grafana.yaml;
    key = "env";
    owner = "observability";
    path = "/run/secrets/observability-grafana.env";
  };

  users.groups.ghcr.members = ["pyparser"];

  llunde.secrets = {
    dopplerTokenFile = config.sops.secrets."pyparser-doppler".path;
    tailscaleAuthKeyFile = config.sops.secrets."tailscale-auth-key".path;
    resticPasswordFile = config.sops.secrets."restic-password".path;
    ghcrAuthFile = config.sops.secrets."ghcr-auth".path;
  };

  llunde.tailscale.authKeyFile = lib.mkDefault config.sops.secrets."tailscale-auth-key".path;

  # GitOps pull (ADR 020): read-only deploy key + this host's external heartbeat
  # URL. The heartbeat matters MOST here — the obs stack lives on this box, so
  # nothing else is watching.
  sops.secrets."gitops-deploy-key" = {
    sopsFile = ../../secrets/gitops-llunde-parser.yaml;
    key = "deploy_key";
  };
  sops.secrets."gitops-heartbeat-url" = {
    sopsFile = ../../secrets/gitops-llunde-parser.yaml;
    key = "heartbeat_url";
  };
}
