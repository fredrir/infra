# Minimal host observability now; collection stack is phase 4 (ADR 013).
# Option surface only in phase 1; phase 2 implements node_exporter + journald
# exposure over the tailnet only.
{ lib, ... }:
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
      description = "Scrape endpoints bind/allow tailnet only — never public (ADR 013).";
    };
  };
}
