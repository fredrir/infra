{
  config,
  lib,
  ...
}: {
  sops.secrets = {
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
    "restic-env" = {
      sopsFile = ../../secrets/restic.yaml;
      key = "env";
    };
    "llunde-backend-db-env" = {
      sopsFile = ../../secrets/llunde-backend-db.yaml;
      key = "env";
      owner = "llunde-backend";
      path = "/run/secrets/llunde-backend-db.env";
    };
    "ghcr-auth" = {
      sopsFile = ../../secrets/ghcr.yaml;
      key = "auth_json";
      mode = "0640";
      group = "ghcr";
      path = "/run/secrets/ghcr-auth.json";
    };
  };

  sops.secrets."llunde-tunnel" = {
    sopsFile = ../../secrets/llunde-tunnel.yaml;
    key = "env";
    owner = "edge";
    path = "/run/secrets/llunde-tunnel.env";
  };

  sops.secrets."llunde-caddy-ghcr" = {
    sopsFile = ../../secrets/llunde-caddy-ghcr.yaml;
    key = "auth_json";
    owner = "edge";
    path = "/run/secrets/llunde-caddy-ghcr.json";
  };

  sops.secrets."llunde-caddy-acme" = {
    sopsFile = ../../secrets/llunde-caddy-acme.yaml;
    key = "env";
    owner = "edge";
    path = "/run/secrets/llunde-caddy-acme.env";
  };

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
