# Host = diffs only (ADR 003): what llunde-parser IS — the pyparser stack under
# its own rootless user, the portfolio tenant slot, and zero public ports
# (tunnel ingress + tailnet SSH, ADR 015/016).
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

  # portfolio's install.sh probes `command -v cosign` in a ROOT shell and
  # curl-installs into /usr/local/bin when missing — impossible here, so
  # system-wide cosign keeps the guard true.
  environment.systemPackages = [pkgs.cosign];

  # No public inbound (ADR 015): web ingress is the outbound Cloudflare tunnel,
  # SSH rides the tailnet. publicTCPPorts stays its default [].

  # Fixed uids. Explicit subuids on ALL rootless users: auto-allocation starts
  # at 100000 and would collide with portfolio's declared range.
  llunde.users.services.pyparser = {
    uid = 2001;
    subUidStart = 165536;
  };

  # The collection stack's rootless user (ADR 018). 20xx uids are per-host:
  # 2002 means llunde-frontend on llunde-01 and observability here. The range
  # starts where pyparser's ends (165536 + 65536).
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
    # bootstrap.sh parity (install.sh expects these to exist). EVERY path level
    # is listed: tmpfiles creates unlisted parents as root and then refuses its
    # own unsafe ownership transition.
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
    # Journal → the local Loki quadlet, via the tailnet address so the shape is
    # identical on every host.
    lokiUrl = "http://100.92.219.50:3100";
  };

  # GitOps pull auto-apply (ADR 020): follows the llunde-01 canary by 30 min —
  # longer than the deadman window, so a rev that bricks the canary has already
  # self-healed (and alerted) before this host takes it. portfolio (uid 3000) is
  # deliberately absent from reconcile: its quadlets are not infra-rendered (own
  # repo, own deploys).
  llunde.gitopsPull = {
    enable = true;
    sshKeyFile = "/run/secrets/gitops-deploy-key";
    heartbeatUrlFile = "/run/secrets/gitops-heartbeat-url";
    probePeer = "100.109.80.121"; # llunde-01
    applyDelaySeconds = 1800;
    reconcile = {
      pyparser = {
        uid = 2001;
        # The shared .network rides in every dependent unit's watch: a network
        # change must recreate all attached containers. pyparser-migrate is a
        # oneshot — restarting re-runs `alembic upgrade head` (idempotent).
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
