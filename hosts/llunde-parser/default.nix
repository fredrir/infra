{
  modulesPath,
  pkgs,
  ...
}: {
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    ./disko.nix
    ./secrets.nix
    ../../modules/profiles/server.nix
    ../../modules/users
    ../../modules/tailscale
    ../../modules/secrets
    ../../modules/quadlet
    ../../modules/tenants
    ../../modules/backups
    ../../modules/observability
    ../../modules/gitops-pull
    ../../services/pyparser
    ../../services/observability
  ];

  networking.hostName = "llunde-parser";

  environment.systemPackages = [pkgs.cosign];

  llunde.users.services.pyparser = {
    uid = 2001;
    subUidStart = 165536;
  };

  llunde.users.services.observability = {
    uid = 2002;
    subUidStart = 231072;
  };

  llunde.tenants.portfolio = {
    uid = 3000;
    subUidStart = 100000;
    packages = with pkgs; [
      cosign
      doppler
      python3
      rsync
    ];
    homeDirectories = [
      ".config"
      ".config/containers"
      ".config/containers/systemd"
      ".config/portfolio"
      "caddy"
    ];
  };

  llunde.backups = {
    enable = true;
    repository = "s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/restic/llunde-parser";
  };
  llunde.observability = {
    enable = true;
    lokiUrl = "http://100.92.219.50:3100";
  };

  llunde.gitopsPull = {
    enable = true;
    sshKeyFile = "/run/secrets/gitops-deploy-key";
    heartbeatUrlFile = "/run/secrets/gitops-heartbeat-url";
    probePeer = "100.109.80.121"; # llunde-01
    applyDelaySeconds = 1800;
    reconcile = {
      pyparser = {
        uid = 2001;
        units = {
          pyparser-review.watch = [
            "/etc/containers/systemd/users/2001/pyparser-review.container"
            "/etc/containers/systemd/users/2001/pyparser.network"
          ];
          pyparser-migrate.watch = [
            "/etc/containers/systemd/users/2001/pyparser-migrate.container"
            "/etc/containers/systemd/users/2001/pyparser.network"
          ];
          pyparser-postgres.watch = [
            "/etc/containers/systemd/users/2001/pyparser-postgres.container"
            "/etc/containers/systemd/users/2001/pyparser.network"
          ];
          pyparser-cloudflared.watch = [
            "/etc/containers/systemd/users/2001/pyparser-cloudflared.container"
            "/etc/containers/systemd/users/2001/pyparser.network"
          ];
          pyparser-worker-extract.watch = [
            "/etc/containers/systemd/users/2001/pyparser-worker-extract.container"
            "/etc/containers/systemd/users/2001/pyparser.network"
          ];
          pyparser-worker-light.watch = [
            "/etc/containers/systemd/users/2001/pyparser-worker-light.container"
            "/etc/containers/systemd/users/2001/pyparser.network"
          ];
        };
      };
      observability = {
        uid = 2002;
        units = {
          observability-prometheus.watch = [
            "/etc/containers/systemd/users/2002/observability-prometheus.container"
            "/etc/containers/systemd/users/2002/observability.network"
            "/etc/llunde/observability/prometheus.yml"
          ];
          observability-grafana.watch = [
            "/etc/containers/systemd/users/2002/observability-grafana.container"
            "/etc/containers/systemd/users/2002/observability.network"
            "/etc/llunde/observability/grafana/alerting.yaml"
            "/etc/llunde/observability/grafana/dashboards.yaml"
            "/etc/llunde/observability/grafana/datasources.yaml"
            "/etc/llunde/observability/grafana/overview.json"
          ];
          observability-loki.watch = [
            "/etc/containers/systemd/users/2002/observability-loki.container"
            "/etc/containers/systemd/users/2002/observability.network"
            "/etc/llunde/observability/loki.yaml"
          ];
          observability-otel.watch = [
            "/etc/containers/systemd/users/2002/observability-otel.container"
            "/etc/containers/systemd/users/2002/observability.network"
            "/etc/llunde/observability/otel-collector.yaml"
          ];
          observability-blackbox.watch = [
            "/etc/containers/systemd/users/2002/observability-blackbox.container"
            "/etc/containers/systemd/users/2002/observability.network"
            "/etc/llunde/observability/blackbox.yml"
          ];
        };
      };
    };
  };

  system.stateVersion = "25.11";
}
