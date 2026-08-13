# Per-host observability sources (ADR 013 exporters; ADR 018 collection).
#
# No public scrape surface: Hetzner's cloud firewall (tofu) has no inbound
# rules, and the rule below opens 9100 on tailscale0 ONLY — the exporter's
# wildcard bind stays unreachable if that ever loosens. Caddy blocks /metrics
# on the public vhost and serves scrapes on its tailnet-only :9101
# (modules/ingress); a loopback bind would not be reachable off-box.
#
# Collector: services/observability on llunde-parser (ADR 018). Prometheus
# scrapes 9100 over the tailnet; lokiUrl starts this host's journal shipper.
{
  config,
  lib,
  ...
}: let
  cfg = config.llunde.observability;
  nodeExporterPort = 9100;
  # *.prom drop dir: anything a root job wants Prometheus to see — restic
  # freshness stamps (modules/backups) today.
  textfileDir = "/var/lib/node-exporter-text";
in {
  options.llunde.observability = {
    enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "node_exporter + journald-centric logging on this host.";
    };
    tailnetOnly = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Scrape endpoints reachable over the tailnet only — never public (ADR 013).";
    };
    lokiUrl = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "http://100.92.219.50:3100";
      description = ''
        Base URL of the central Loki (services/observability on llunde-parser,
        reached over the tailnet). Non-null enables the promtail journal
        shipper on this host; null keeps it off (ADR 018).
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    services.prometheus.exporters.node = {
      enable = true;
      port = nodeExporterPort;
      enabledCollectors = ["systemd"];
      extraFlags = ["--collector.textfile.directory=${textfileDir}"];
    };

    # Root-owned, world-readable: writers are root jobs (modules/backups),
    # the reader is the exporter's unprivileged user.
    systemd.tmpfiles.rules = ["d ${textfileDir} 0755 root root -"];

    # promtail only tails the journal — still the single log home — and pushes
    # to the central Loki over the tailnet. max_age bounds backfill to roughly
    # the current boot instead of re-shipping a week of history on first start.
    services.promtail = lib.mkIf (cfg.lokiUrl != null) {
      enable = true;
      configuration = {
        server = {
          # Push-only: no inbound surface beyond loopback introspection.
          http_listen_address = "127.0.0.1";
          http_listen_port = 9080;
          grpc_listen_port = 0;
        };
        clients = [{url = "${cfg.lokiUrl}/loki/api/v1/push";}];
        scrape_configs = [
          {
            job_name = "journal";
            journal = {
              max_age = "12h";
              labels = {
                job = "systemd-journal";
                host = config.networking.hostName;
              };
            };
            relabel_configs = [
              {
                source_labels = ["__journal__systemd_unit"];
                target_label = "unit";
              }
            ];
          }
        ];
      };
    };

    # ADR 018: every observability unit carries MemoryMax; ~100 MB per shipper.
    systemd.services.promtail = lib.mkIf (cfg.lokiUrl != null) {
      serviceConfig.MemoryMax = "128M";
    };

    networking.firewall = lib.mkMerge [
      (lib.mkIf cfg.tailnetOnly {
        interfaces."tailscale0".allowedTCPPorts = [nodeExporterPort];
      })
      (lib.mkIf (!cfg.tailnetOnly) {
        allowedTCPPorts = [nodeExporterPort];
      })
    ];

    # journald is the single log home (containers log JSON to stdout); capped so
    # logs cannot crowd the 80 GB disk.
    services.journald.extraConfig = ''
      SystemMaxUse=1G
    '';
  };
}
