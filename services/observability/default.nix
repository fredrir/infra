# Observability slice (ADR 018): Prometheus + Grafana + Loki + OTel collector +
# blackbox-exporter as rootless quadlets under the observability user (uid 2002)
# on llunde-parser, plus the root-level external dead-man. Unit text lives in
# ./unit.nix for golden-testing; the configs beside this file land as
# environment.etc entries the units bind-mount.
{
  config,
  lib,
  pkgs,
  ...
}: let
  units = import ./unit.nix {inherit lib;};
in {
  # Rehearsal guard (mirrors the pyparser tunnel flag): a scratch box booting
  # this slice fires real alerts and probes the real public URLs. NEVER true on
  # a rehearsal machine.
  options.llunde.observability.stack.enable = lib.mkOption {
    type = lib.types.bool;
    default = true;
    description = "Run the collection stack (prometheus/grafana/loki/otel/blackbox + dead-man).";
  };

  config = lib.mkIf config.llunde.observability.stack.enable {
    llunde.quadlet = {
      units = units.fragments;
      # autoUpdate = false is the point: no AutoUpdate labels anywhere in this
      # stack (see unit.nix); image bumps are deliberate edits.
      serviceUsers = [
        {
          name = "observability";
          uid = 2002;
          autoUpdate = false;
        }
      ];
    };

    environment.etc = {
      "llunde/observability/prometheus.yml".text = units.prometheusConfig;
      "llunde/observability/blackbox.yml".source = ./blackbox.yml;
      "llunde/observability/loki.yaml".source = ./loki.yaml;
      "llunde/observability/otel-collector.yaml".source = ./otel-collector.yaml;
      "llunde/observability/grafana/datasources.yaml".source = ./grafana/datasources.yaml;
      "llunde/observability/grafana/dashboards.yaml".source = ./grafana/dashboards.yaml;
      "llunde/observability/grafana/alerting.yaml".source = ./grafana/alerting.yaml;
      "llunde/observability/grafana/overview.json".source = ./grafana/overview.json;
    };

    # External dead-man (ADR 018): llunde-parser observes llunde-01, so if this
    # stack is dark all alerting is dark and only something OUTSIDE the estate
    # can say so. Every 5 minutes: verify Prometheus AND Grafana answer their
    # health endpoints (published ports reach host loopback), then ping the
    # heartbeat URL in /var/lib/deadman/url — a pending owner input (a
    # healthchecks.io check); until that file exists the unit logs one line and
    # exits 0. Gate test: stop the stack → checks fail → pings stop →
    # healthchecks.io notifies the owner within 15 minutes.
    systemd.services.observability-deadman = {
      description = "Dead-man heartbeat: ping the external monitor while the stack is healthy";
      serviceConfig.Type = "oneshot";
      script = ''
        set -euo pipefail
        ${pkgs.curl}/bin/curl -fsS --max-time 10 http://127.0.0.1:9090/-/healthy >/dev/null
        ${pkgs.curl}/bin/curl -fsS --max-time 10 http://127.0.0.1:3000/api/health >/dev/null
        if [ -r /var/lib/deadman/url ]; then
          ${pkgs.curl}/bin/curl -fsS --max-time 10 --retry 2 "$(cat /var/lib/deadman/url)" >/dev/null
        else
          echo "stack healthy, but /var/lib/deadman/url is absent - external heartbeat not configured yet (owner input pending)"
        fi
      '';
    };

    systemd.timers.observability-deadman = {
      wantedBy = ["timers.target"];
      timerConfig = {
        OnCalendar = "*:0/5";
        # No Persistent=: a missed window while the box is down is the signal
        # the external service exists to catch.
      };
    };

    systemd.tmpfiles.rules = ["d /var/lib/deadman 0700 root root -"];
  };
}
