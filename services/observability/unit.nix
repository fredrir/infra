# Pure fragments for the observability stack — consumed by BOTH the service
# module and the flake's golden render checks (pyparser pattern). Values from
# the phase-4 workstream-O contract (docs/init/plans/phase-4/tasks.md O1–O7,
# ADR 018): uid 2002 on llunde-parser, own `observability` network, images
# pinned by VERSION TAG with NO AutoUpdate labels (a broken upstream release
# must never take the estate's only alerting down — bumps are deliberate),
# MemoryMax on every unit (stack total ≤2 G).
#
# Exposure model — the proven node_exporter shape (modules/observability):
# every PublishPort binds the WILDCARD, the public firewall opens nothing
# (llunde-parser's publicTCPPorts stays []), and tailscale0 is trusted, so
# each port is tailnet-only reachable. Wildcard rather than the tailnet IP
# because the IP may not exist yet when the user manager starts units at boot.
#
# Configs arrive as environment.etc files bind-mounted :ro (default.nix).
# NixOS /etc entries are store symlinks; podman resolves them at mount time
# (caddy precedent), so a config change needs a container restart to land.
# No :z anywhere — NixOS runs no SELinux.
{lib}: let
  quadlet = import ../../modules/quadlet/mk-quadlet.nix {inherit lib;};
  uid = 2002;

  # Tailnet addresses (ADR 008/015: the management + scrape plane).
  llunde01 = "100.109.80.121";
  llundeParser = "100.92.219.50";

  etcDir = "/etc/llunde/observability";

  # First boot pulls five images; the user manager's 90 s default start
  # timeout would kill the pulls (llunde-backend precedent).
  pullTimeout = 300;

  mkContainer = args: rec {
    inherit args;
    fragment = quadlet.mkContainerUnit args;
    text = fragment."containers/systemd/users/${toString uid}/${args.name}.container".text;
  };
in rec {
  # Rendered to /etc/llunde/observability/prometheus.yml by default.nix and
  # golden-checked by the flake (O2 proof): the scrape topology is the
  # load-bearing contract of the whole stack.
  prometheusConfig = ''
    global:
      scrape_interval: 30s
      evaluation_interval: 30s

    scrape_configs:
      # Host metrics over the tailnet: 9100 is interface-scoped to tailscale0
      # on both hosts (modules/observability). honor_labels because the restic
      # textfile stamps carry their own job="<backup job>" label
      # (modules/backups) which must survive the scrape instead of being
      # renamed exported_job.
      - job_name: node
        honor_labels: true
        static_configs:
          - targets: ["${llunde01}:9100"]
            labels:
              host: llunde-01
          - targets: ["${llundeParser}:9100"]
            labels:
              host: llunde-parser

      # Backend JVM/HTTP metrics via Caddy's tailnet-only :9101 listener
      # (modules/ingress): the app binds 127.0.0.1:8080 on llunde-01, so that
      # listener is the only off-box path (phase-4 plan-review finding).
      - job_name: llunde-backend
        metrics_path: /metrics
        static_configs:
          - targets: ["${llunde01}:9101"]
            labels:
              host: llunde-01

      # Blackbox probes (O6) address the exporter by CONTAINER NAME on the
      # shared network (aardvark DNS) — never a host port. Standard blackbox
      # indirection: the probed URL moves into ?target=, instance keeps the
      # URL, and the scrape itself hits the exporter.
      #
      # blackbox-public exercises the WHOLE public chain the tailnet-side
      # checks can't see: egress to the CF edge and back through the tunnel.
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

      # M3: the cutover assertion. Same URL as blackbox-public, a DIFFERENT
      # question — not "is the site up" but "is the site reached THROUGH
      # Cloudflare". Two jobs on purpose: one probe answering both questions
      # could not tell "site down" from "site up but bypassing the edge", and
      # bypassing the edge is silent by construction. Was deliberately red until E5
      # flipped DNS, and named outside the blackbox-public.* family so
      # PublicEdgeDown would not page on a state we chose. E5 landed 2026-08-13
      # and green is now the correct state, so it joins the family and alerts.
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

      # The public edge answering 403 on the backend's ops path IS the healthy
      # signal — Caddy's @ops block responding proves DNS, tunnel and Caddy
      # are all alive (module http_403 accepts only 403).
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
      # NOT Internal (pyparser go-live fix 7 applies doubly here): blackbox
      # needs real egress to the CF edge and an Internal network's aardvark
      # answers all DNS without upstream. Isolation holds via the user
      # boundary; inbound is only the published ports.
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
      # Exec replaces the image CMD entirely, so config/storage paths must be
      # restated alongside the retention bound (contract: 60 d).
      Exec = "--config.file=/etc/prometheus/prometheus.yml --storage.tsdb.path=/prometheus --storage.tsdb.retention.time=60d";
      Image = "docker.io/prom/prometheus:v3.13.2";
      Network = ["observability.network"];
      PublishPort = ["0.0.0.0:9090:9090"];
      # :U chowns the volume to the image's runtime user (nobody) — the
      # pyparser-postgres precedent.
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
        # C1 (phase-4 review): the tailnet is the perimeter, but it is NOT
        # single-user — ephemeral tag:ci runners join it (ADR 015) and the
        # portfolio tenant has a shell ON this box — so anonymous ADMIN was a
        # privilege-escalation primitive (SSRF pivot via datasource proxy + an
        # SES relay via the contact-point test). Anonymous is now read-only
        # Viewer; basic auth is OFF (kills the admin:admin default that was live
        # independent of the hidden login form); the admin password is
        # sops-provided (observability-grafana.env) as a break-glass backstop.
        # Alerting + dashboards are fully file-provisioned — nothing legitimate
        # needs Admin. Dashboards stay one click away (anonymous Viewer).
        "GF_AUTH_ANONYMOUS_ENABLED=true"
        "GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer"
        "GF_AUTH_DISABLE_LOGIN_FORM=true"
        "GF_AUTH_BASIC_ENABLED=false"
        "GF_ANALYTICS_REPORTING_ENABLED=false"
        # SES SMTP (plan-default delivery; owner-confirmed). User/password
        # are the scoped llunde-alerts-smtp IAM credential, sops-rendered.
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
      # 3100 published for the promtails pushing from BOTH hosts over the
      # tailnet (modules/observability lokiUrl) — not just for operators.
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
      # OTLP gRPC + HTTP, published for the backend's future tracing wiring
      # (ADR 018: the collector exists from day one; the Java-agent hookup is
      # a backend-repo follow-up).
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
      # Published so an operator can hand-run probes from the tailnet;
      # Prometheus itself scrapes it by container name on the shared network.
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
