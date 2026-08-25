{lib}: let
  quadlet = import ../../modules/quadlet/mk-quadlet.nix {inherit lib;};
  uid = 2002;

  llunde01 = "100.109.80.121";
  llundeParser = "100.92.219.50";

  etcDir = "/etc/llunde/observability";

  pullTimeout = 300;

  mkContainer = args: rec {
    inherit args;
    fragment = quadlet.mkContainerUnit args;
    text = fragment."containers/systemd/users/${toString uid}/${args.name}.container".text;
  };
in rec {
  prometheusConfig = ''
    global:
      scrape_interval: 30s
      evaluation_interval: 30s

    scrape_configs:
      - job_name: node
        honor_labels: true
        static_configs:
          - targets: ["${llunde01}:9100"]
            labels:
              host: llunde-01
          - targets: ["${llundeParser}:9100"]
            labels:
              host: llunde-parser

      - job_name: llunde-backend
        metrics_path: /metrics
        static_configs:
          - targets: ["${llunde01}:9101"]
            labels:
              host: llunde-01

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
      Exec = "--config.file=/etc/prometheus/prometheus.yml --storage.tsdb.path=/prometheus --storage.tsdb.retention.time=60d";
      Image = "docker.io/prom/prometheus:v3.13.2";
      Network = ["observability.network"];
      PublishPort = ["0.0.0.0:9090:9090"];
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
        "GF_AUTH_ANONYMOUS_ENABLED=true"
        "GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer"
        "GF_AUTH_DISABLE_LOGIN_FORM=true"
        "GF_AUTH_BASIC_ENABLED=false"
        "GF_ANALYTICS_REPORTING_ENABLED=false"
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
      # OTLP gRPC + HTTP for the backend's tracing wiring
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
      Image = "docker.io/prom/blackbox-exporter:v0.28.0";
      Network = ["observability.network"];
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
