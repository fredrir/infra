# Minimal host observability now; collection stack is phase 4 (ADR 013).
#
# Exposure model (three rings, no public scrape surface):
#   1. Hetzner cloud firewall (tofu) admits only 22/80/443.
#   2. This host firewall opens 9100 ONLY on tailscale0 (interface-scoped rule
#      below) — the exporter may bind the wildcard, but nothing outside the
#      tailnet can reach it even if ring 1 ever loosens.
#   3. The app's /metrics is blocked at Caddy on the public vhost (stream C2)
#      and scraped over the tailnet directly against 127.0.0.1-published ports
#      via the tailnet — phase 4 places the collector.
#
# Phase-4 mapping: node_exporter (here) + app /metrics + journald JSON logs
# become the sources for the Prometheus/Loki-or-Grafana-Cloud decision
# (docs/init/plans/phase-4/README.md §3). Nothing here needs to change for
# that — the collector just gains a tailnet route to 9100.
{
  config,
  lib,
  ...
}:
let
  cfg = config.llunde.observability;
  nodeExporterPort = 9100;
in
{
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
  };

  config = lib.mkIf cfg.enable {
    services.prometheus.exporters.node = {
      enable = true;
      port = nodeExporterPort;
      enabledCollectors = [ "systemd" ];
    };

    networking.firewall = lib.mkMerge [
      (lib.mkIf cfg.tailnetOnly {
        interfaces."tailscale0".allowedTCPPorts = [ nodeExporterPort ];
      })
      (lib.mkIf (!cfg.tailnetOnly) {
        allowedTCPPorts = [ nodeExporterPort ];
      })
    ];

    # Containers log JSON to stdout -> journald is the single log home; cap it
    # so logs cannot crowd the 80 GB disk.
    services.journald.extraConfig = ''
      SystemMaxUse=1G
    '';
  };
}
