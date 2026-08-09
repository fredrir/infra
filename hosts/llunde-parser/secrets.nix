# llunde-parser's bootstrap secrets (phase-3.5 contract: FOUR files — no sops
# DB password; POSTGRES_PASSWORD is single-sourced from the Doppler render).
# Mechanism lives in modules/secrets; recipients in .sops.yaml (per-host rules).
{ config, lib, ... }:
{
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

  users.groups.ghcr.members = [ "pyparser" ];

  llunde.secrets = {
    dopplerTokenFile = config.sops.secrets."pyparser-doppler".path;
    tailscaleAuthKeyFile = config.sops.secrets."tailscale-auth-key".path;
    resticPasswordFile = config.sops.secrets."restic-password".path;
    ghcrAuthFile = config.sops.secrets."ghcr-auth".path;
  };

  llunde.tailscale.authKeyFile = lib.mkDefault config.sops.secrets."tailscale-auth-key".path;
}
