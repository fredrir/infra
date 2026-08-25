{
  config,
  lib,
  pkgs,
  ...
}: let
  units = import ./unit.nix {inherit lib;};
in {
  options.llunde.observability.stack.enable = lib.mkOption {
    type = lib.types.bool;
    default = true;
    description = "Run the collection stack (prometheus/grafana/loki/otel/blackbox + dead-man).";
  };

  config = lib.mkIf config.llunde.observability.stack.enable {
    llunde.quadlet = {
      units = units.fragments;
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

    systemd.services.observability-deadman = {
      description = "Dead-man heartbeat";
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
      };
    };

    systemd.tmpfiles.rules = ["d /var/lib/deadman 0700 root root -"];
  };
}
