# Pure fragments for the observability stack — used by the service module and
# the flake goldens. ADR 018: uid 2002 on llunde-parser, own `observability`
# network, MemoryMax per unit (stack ≤2 G), images pinned by VERSION TAG with NO
# AutoUpdate labels — a broken upstream release must never take the estate's only
# alerting down. PublishPort binds the WILDCARD (the tailnet IP may not exist when
# units start at boot); reachability stays tailnet-only via the firewall
# (publicTCPPorts is []) and trusted tailscale0. Configs are environment.etc store
# symlinks bind-mounted :ro (default.nix), resolved at mount time — changes need a
# container restart. No :z: NixOS runs no SELinux.
{lib}: let
  quadlet = import ../../modules/quadlet/mk-quadlet.nix {inherit lib;};
  uid = 2002;

  # Tailnet addresses (ADR 008/015: the management + scrape plane).
  llunde01 = "100.109.80.121";
  llundeParser = "100.92.219.50";

  etcDir = "/etc/llunde/observability";

  # First boot pulls five images; the 90 s default start timeout would kill them.
  pullTimeout = 300;

  mkContainer = args: rec {
    inherit args;
    fragment = quadlet.mkContainerUnit args;
    text = fragment."containers/systemd/users/${toString uid}/${args.name}.container".text;
  };
in rec {
  # Rendered by default.nix to /etc/llunde/observability/prometheus.yml, golden-checked.
  prometheusConfig = ''
    global:
      scrape_interval: 30s
      evaluation_interval: 30s

    scrape_configs:
      # 9100 is tailscale0-scoped on both hosts (modules/observability), and
      # honor_labels keeps restic's own job="<backup job>" textfile-stamp label
      # off exported_job (modules/backups).
      - job_name: node
        honor_labels: true
        static_configs:
          - targets: ["${llunde01}:9100"]
            labels:
              host: llunde-01
          - targets: ["${llundeParser}:9100"]
            labels:
              host: llunde-parser

      # Backend JVM/HTTP via Caddy's tailnet-only :9101 (modules/ingress): the
      # app binds 127.0.0.1:8080, so that listener is the only off-box path.
      - job_name: llunde-backend
        metrics_path: /metrics
        static_configs:
          - targets: ["${llunde01}:9101"]
            labels:
              host: llunde-01

      # Blackbox indirection: the probed URL moves into ?target=, instance keeps
      # the URL, the scrape hits the exporter by CONTAINER NAME on the shared
      # network (aardvark DNS), never a host port. blackbox-public covers the
      # WHOLE public chain the tailnet checks miss: CF edge egress, then tunnel.
      - job_name: blackbox-public
        metrics_path: /probe
        params:
          module: [http_2xx]
        static_configs:
          - targets: ["https://llunde.no"]
        relabel_configs:
          - source_labels: [__address__]
            target_label: __param_target
          - source_labels: [__param_target]
            target_label: instance
          - target_label: __address__
            replacement: observability-blackbox:9115

      # Same URL as blackbox-public, a DIFFERENT question: not "is the site up"
      # but "is it reached THROUGH Cloudflare". Two jobs because one could not
      # tell "site down" from "site up, bypassing the edge" — and bypassing is
      # silent by construction. Named into blackbox-public.*: PublicEdgeDown pages.
      - job_name: blackbox-public-cfray
        metrics_path: /probe
        params:
          module: [http_2xx_cfray]
        static_configs:
          - targets: ["https://llunde.no"]
        relabel_configs:
          - source_labels: [__address__]
            target_label: __param_target
          - source_labels: [__param_target]
            target_label: instance
          - target_label: __address__
            replacement: observability-blackbox:9115

      # 403 on the backend's ops path IS the healthy signal: Caddy's @ops block
      # answering proves DNS, tunnel and Caddy alive (http_403 accepts only 403).
      - job_name: blackbox-public-403
        metrics_path: /probe
        params:
          module: [http_403]
        static_configs:
          - targets: ["https://api.llunde.no/health"]
        relabel_configs:
          - source_labels: [__address__]
            target_label: __param_target
          - source_labels: [__param_target]
            target_label: instance
          - target_label: __address__
            replacement: observability-blackbox:9115

      # Full backend readiness (DB, valkey, …) via the same Caddy listener.
      - job_name: blackbox-ready
        metrics_path: /probe
        params:
          module: [http_2xx]
        static_configs:
          - targets: ["http://${llunde01}:9101/ready"]
        relabel_configs:
          - source_labels: [__address__]
            target_label: __param_target
          - source_labels: [__param_target]
            target_label: instance
          - target_label: __address__
            replacement: observability-blackbox:9115
  '';

  network = rec {
    args = {
      name = "observability";
      inherit uid;
      # NOT Internal: blackbox needs real egress to the CF edge, and Internal
      # aardvark answers all DNS without upstream. Isolation is the user
      # boundary; inbound is the published ports.
      network = {};
      install.WantedBy = ["default.target"];
    };
    fragment = quadlet.mkNetworkUnit args;
    text = fragment."containers/systemd/users/${toString uid}/observability.network".text;
  };

  prometheus = mkContainer {
    name = "observability-prometheus";
    inherit uid;
    container = {
      ContainerName = "observability-prometheus";
      # Exec replaces the image CMD entirely: config/storage paths must be
      # restated alongside the 60 d retention bound.
      Exec = "--config.file=/etc/prometheus/prometheus.yml --storage.tsdb.path=/prometheus --storage.tsdb.retention.time=60d";
      Image = "docker.io/prom/prometheus:v3.13.2";
      Network = ["observability.network"];
      PublishPort = ["0.0.0.0:9090:9090"];
      # :U chowns the volume to the image's runtime user (nobody).
      Volume = [
        "${etcDir}/prometheus.yml:/etc/prometheus/prometheus.yml:ro"
        "prometheus-data:/prometheus:U"
      ];
    };
    service = {
      MemoryMax = "512M";
      Restart = "always";
      TimeoutStartSec = pullTimeout;
    };
    install.WantedBy = ["default.target"];
  };

  grafana = mkContainer {
    name = "observability-grafana";
    inherit uid;
    container = {
      ContainerName = "observability-grafana";
      Environment = [
        # The tailnet is the perimeter but NOT single-user (ephemeral tag:ci
        # runners, ADR 015; portfolio-tenant shell on this box), so anonymous
        # Admin would be a privesc primitive: SSRF via the datasource proxy, SES
        # relay via the contact-point test. Viewer keeps dashboards one click
        # away; basic auth OFF kills the admin:admin default that survives a
        # hidden login form; the sops admin password (observability-grafana.env)
        # is break-glass. Provisioning is file-based: nothing needs Admin.
        "GF_AUTH_ANONYMOUS_ENABLED=true"
        "GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer"
        "GF_AUTH_DISABLE_LOGIN_FORM=true"
        "GF_AUTH_BASIC_ENABLED=false"
        "GF_ANALYTICS_REPORTING_ENABLED=false"
        # SES SMTP; user/password are the scoped llunde-alerts-smtp IAM
        # credential, sops-rendered.
        "GF_SMTP_ENABLED=true"
        "GF_SMTP_HOST=email-smtp.eu-north-1.amazonaws.com:587"
        "GF_SMTP_FROM_ADDRESS=alerts@llunde.no"
        "GF_SMTP_FROM_NAME=llunde alerts"
      ];
      EnvironmentFile = [
        "/run/secrets/observability-smtp.env"
        "/run/secrets/observability-grafana.env"
      ];
      Image = "docker.io/grafana/grafana:13.1.3";
      Network = ["observability.network"];
      PublishPort = ["0.0.0.0:3000:3000"];
      Volume = [
        "${etcDir}/grafana/datasources.yaml:/etc/grafana/provisioning/datasources/datasources.yaml:ro"
        "${etcDir}/grafana/dashboards.yaml:/etc/grafana/provisioning/dashboards/dashboards.yaml:ro"
        "${etcDir}/grafana/alerting.yaml:/etc/grafana/provisioning/alerting/alerting.yaml:ro"
        "${etcDir}/grafana/overview.json:/etc/grafana/dashboards/overview.json:ro"
        "grafana-data:/var/lib/grafana:U"
      ];
    };
    service = {
      MemoryMax = "384M";
      Restart = "always";
      TimeoutStartSec = pullTimeout;
    };
    install.WantedBy = ["default.target"];
  };

  loki = mkContainer {
    name = "observability-loki";
    inherit uid;
    container = {
      ContainerName = "observability-loki";
      Exec = "-config.file=/etc/loki/loki.yaml";
      Image = "docker.io/grafana/loki:3.7.6";
      Network = ["observability.network"];
      # 3100 published for the promtails pushing from BOTH hosts over the tailnet
      # (modules/observability lokiUrl), not just for operators.
      PublishPort = ["0.0.0.0:3100:3100"];
      Volume = [
        "${etcDir}/loki.yaml:/etc/loki/loki.yaml:ro"
        "loki-data:/loki:U"
      ];
    };
    service = {
      MemoryMax = "512M";
      Restart = "always";
      TimeoutStartSec = pullTimeout;
    };
    install.WantedBy = ["default.target"];
  };

  otelCollector = mkContainer {
    name = "observability-otel";
    inherit uid;
    container = {
      ContainerName = "observability-otel";
      # Config mounts over the image's default --config path, so no Exec.
      Image = "docker.io/otel/opentelemetry-collector-contrib:0.158.0";
      Network = ["observability.network"];
      # OTLP gRPC + HTTP for the backend's tracing wiring (ADR 018: collector
      # from day one, Java-agent hookup a backend-repo follow-up).
      PublishPort = [
        "0.0.0.0:4317:4317"
        "0.0.0.0:4318:4318"
      ];
      Volume = ["${etcDir}/otel-collector.yaml:/etc/otelcol-contrib/config.yaml:ro"];
    };
    service = {
      MemoryMax = "256M";
      Restart = "always";
      TimeoutStartSec = pullTimeout;
    };
    install.WantedBy = ["default.target"];
  };

  blackbox = mkContainer {
    name = "observability-blackbox";
    inherit uid;
    container = {
      ContainerName = "observability-blackbox";
      # Config mounts over the image's default --config.file path, so no Exec.
      Image = "docker.io/prom/blackbox-exporter:v0.28.0";
      Network = ["observability.network"];
      # Published so operators can hand-run probes from the tailnet; Prometheus
      # itself uses the container name.
      PublishPort = ["0.0.0.0:9115:9115"];
      Volume = ["${etcDir}/blackbox.yml:/etc/blackbox_exporter/config.yml:ro"];
    };
    service = {
      MemoryMax = "64M";
      Restart = "always";
      TimeoutStartSec = pullTimeout;
    };
    install.WantedBy = ["default.target"];
  };

  fragments = map (u: u.fragment) [
    network
    prometheus
    grafana
    loki
    otelCollector
    blackbox
  ];
}
