# Per-host observability sources (ADR 013 exporters; ADR 018 collection).
#
# Exposure model (three rings, no public scrape surface):
#   1. Hetzner cloud firewall (tofu) admits at most 22/80/443.
#   2. This host firewall opens 9100 ONLY on tailscale0 (interface-scoped rule
#      below) — the exporter may bind the wildcard, but nothing outside the
#      tailnet can reach it even if ring 1 ever loosens.
#   3. The app's /metrics is blocked at Caddy on the public vhost (stream C2);
#      off-box scraping goes through Caddy's tailnet-only :9101 listener
#      (modules/ingress, phase-4 plan-review finding — the loopback bind was
#      never reachable off-box on its own).
#
# Phase 4 placed the collector (services/observability on llunde-parser,
# ADR 018): Prometheus scrapes 9100 over the tailnet, and lokiUrl below turns
# on the journal shipper feeding the central Loki from every host.
{
  config,
  lib,
  ...
}: let
  cfg = config.llunde.observability;
  nodeExporterPort = 9100;
  # Success stamps from modules/backups (restic freshness) and anything else
  # a root job wants Prometheus to see land here as *.prom files.
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

    # Journal shipper (phase-4 O3): NixOS-native promtail on every host,
    # pushing this host's journal to the central Loki over the tailnet. The
    # journal stays the single log home (ring 3 above) — promtail only tails
    # it; max_age bounds backfill to roughly the current boot instead of
    # re-shipping a week of history on first start.
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

    # Hard cap (ADR 018: every observability unit carries MemoryMax; the
    # shipper budget is ~100 MB per host).
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

    # Containers log JSON to stdout -> journald is the single log home; cap it
    # so logs cannot crowd the 80 GB disk.
    services.journald.extraConfig = ''
      SystemMaxUse=1G
    '';
  };
}
