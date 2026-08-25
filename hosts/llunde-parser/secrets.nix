{
  config,
  lib,
  ...
}: {
  sops.secrets = {
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
    "ghcr-auth" = {
      sopsFile = ../../secrets/ghcr.yaml;
      key = "auth_json";
      mode = "0640";
      group = "ghcr";
      path = "/run/secrets/ghcr-auth.json";
    };
  };

  sops.secrets."observability-smtp" = {
    sopsFile = ../../secrets/observability-smtp.yaml;
    key = "env";
    owner = "observability";
    path = "/run/secrets/observability-smtp.env";
  };

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

  sops.secrets."gitops-deploy-key" = {
    sopsFile = ../../secrets/gitops-llunde-parser.yaml;
    key = "deploy_key";
  };
  sops.secrets."gitops-heartbeat-url" = {
    sopsFile = ../../secrets/gitops-llunde-parser.yaml;
    key = "heartbeat_url";
  };
}
