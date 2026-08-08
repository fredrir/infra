# Caddy ingress under the `edge` user — the only public 80/443 (ADR 006).
# Option surface only in phase 1; phase 2 implements the Caddy quadlet +
# sysctl net.ipv4.ip_unprivileged_port_start=80 + ops-endpoint blocking.
{ lib, ... }:
{
  options.llunde.ingress = {
    enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Run Caddy as the host's sole public ingress/TLS terminator.";
    };
    virtualHosts = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            upstream = lib.mkOption {
              type = lib.types.str;
              example = "127.0.0.1:8080";
              description = "Loopback upstream this vhost proxies to (ADR 005 wiring).";
            };
            blockOpsEndpoints = lib.mkOption {
              type = lib.types.bool;
              default = true;
              description = "Deny /metrics, /health, /ready publicly (tailnet-only per ADR 013).";
            };
          };
        }
      );
      default = { };
      description = "Public vhosts, e.g. llunde.no and api.llunde.no.";
    };
  };
}
