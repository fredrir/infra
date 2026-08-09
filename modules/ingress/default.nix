# Caddy ingress under the `edge` user (uid 2000) — the host's only public
# 80/443 (ADR 006), automatic Let's Encrypt TLS, grey-cloud DNS (ADR 012).
#
# Networking choice: the Caddy container runs with Network=host. The upstream
# contract publishes every service on host loopback ONLY (127.0.0.1:8080/8081,
# ADR 005), and a rootless bridge-networked container cannot reach host
# loopback — host.containers.internal resolves to a routable host address,
# which 127.0.0.1-bound sockets deliberately do not answer. Host networking
# makes the loopback upstreams work verbatim and binds 80/443 directly
# (allowed by ip_unprivileged_port_start=80, set in the base profile).
# Isolation is intact: other users' podman networks live inside their own
# rootless network namespaces and are unreachable from the host namespace, so
# the only cross-user surface remains the deliberately published loopback
# ports — exactly the ADR 005 contract. PublishPort is meaningless under host
# networking and therefore absent.
#
# Ops endpoints (/metrics, /health, /ready) are blocked on the public vhosts
# (ADR 013 / backend phase-3 finding). Off-box they are reachable ONLY via
# the tailnet-scoped :9101 listener below (phase 4 — Prometheus scrapes
# /metrics, blackbox probes /ready); node-level metrics stay with
# modules/observability. The public surface remains 80/443.
{
  config,
  lib,
  ...
}: let
  cfg = config.llunde.ingress;
  quadlet = import ../quadlet/mk-quadlet.nix {inherit lib;};

  acmeEmail = "fhansteen@gmail.com";

  opsBlock = ''
    @ops path /metrics /health /ready
    respond @ops 403
  '';

  # Backend ops listener for the tailnet (phase-4 workstream O, plan-review
  # finding): ADR 013 claimed the app's /metrics is "scraped over the tailnet
  # directly against 127.0.0.1-published ports" — but a loopback bind is by
  # definition unreachable off-box, so until this listener that claim was
  # aspirational; nothing could actually collect backend metrics remotely.
  # Caddy already runs host-network and reaches the backend loopback, so it
  # gains a second, plain-HTTP site on :9101. Exposure is the proven
  # node_exporter model (modules/observability): the socket binds the
  # wildcard, the public firewall never opens 9101 (publicTCPPorts), and only
  # trusted tailscale0 admits traffic — tailnet-only reachability with zero
  # public surface. EXACTLY two paths proxy to the backend — /metrics
  # (Prometheus scrape) and /ready (blackbox full-readiness probe); everything
  # else answers 403, mirroring the public vhosts' @ops block.
  metricsSite = ''
    http://:9101 {
    	@scrape path /metrics /ready
    	handle @scrape {
    		reverse_proxy 127.0.0.1:8080
    	}
    	handle {
    		respond 403
    	}
    }
  '';

  vhostBlock = name: vhost:
    if vhost.redirectTo != null
    then ''
      ${name} {
      	redir ${vhost.redirectTo}{uri} permanent
      }
    ''
    else ''
      ${name} {
      	encode gzip
      ${lib.optionalString vhost.blockOpsEndpoints opsBlock}	reverse_proxy ${vhost.upstream}
      }
    '';

  caddyfile =
    ''
      {
      	email ${acmeEmail}
      }

    ''
    + lib.concatStringsSep "\n" (lib.mapAttrsToList vhostBlock cfg.virtualHosts)
    + "\n"
    + metricsSite;
in {
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
              default = "";
              example = "127.0.0.1:8080";
              description = "Loopback upstream this vhost proxies to (ADR 005 wiring).";
            };
            redirectTo = lib.mkOption {
              type = lib.types.nullOr lib.types.str;
              default = null;
              example = "https://llunde.no";
              description = "Redirect-only vhost (e.g. www -> apex); upstream is ignored.";
            };
            blockOpsEndpoints = lib.mkOption {
              type = lib.types.bool;
              default = true;
              description = "Deny /metrics, /health, /ready publicly (tailnet-only per ADR 013).";
            };
          };
        }
      );
      default = {};
      description = "Public vhosts, e.g. llunde.no and api.llunde.no.";
    };
  };

  config = lib.mkIf cfg.enable {
    # Contract defaults (docs/init/plans/phase-2/contract.md); hosts may override.
    llunde.ingress.virtualHosts = {
      "llunde.no" = {
        upstream = lib.mkDefault "127.0.0.1:8081";
        # A SPA answers every path itself; only the API has real ops endpoints.
        blockOpsEndpoints = lib.mkDefault false;
      };
      "api.llunde.no" = {
        upstream = lib.mkDefault "127.0.0.1:8080";
        blockOpsEndpoints = lib.mkDefault true;
      };
      "www.llunde.no" = {
        redirectTo = lib.mkDefault "https://llunde.no";
      };
    };

    # Same belt-and-suspenders scoping as node_exporter's 9100
    # (modules/observability): tailscale0 is already a trusted interface, but
    # the explicit rule documents the intended reach of :9101 and survives if
    # that blanket trust is ever narrowed.
    networking.firewall.interfaces."tailscale0".allowedTCPPorts = [9101];

    environment.etc =
      quadlet.mkContainerUnit {
        name = "caddy";
        uid = 2000;
        unit.Description = "Caddy public ingress (ADR 006)";
        container = {
          Image = "docker.io/library/caddy:2";
          Network = "host";
          Volume = [
            "/etc/llunde/caddy/Caddyfile:/etc/caddy/Caddyfile:ro"
            # Named volumes: certificate storage must survive container replacement.
            "caddy-data:/data"
            "caddy-config:/config"
          ];
        };
        service = {
          Restart = "always";
          MemoryMax = "256M";
        };
        install.WantedBy = ["default.target"];
      }
      // {
        "llunde/caddy/Caddyfile".text = caddyfile;
      };
  };
}
