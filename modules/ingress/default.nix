# Caddy ingress under the `edge` user (uid 2000) — ADR 006, with Let's Encrypt
# certificates renewed over DNS-01 (ADR 017 as amended).
#
# Since phase-4 E5/E6 no traffic reaches this host's 80/443: llunde.no, www and
# api are proxied CNAMEs onto the llunde tunnel, and the cloud firewall admits
# nothing. Caddy still binds those ports deliberately — they are the break-glass
# path, live again the moment the firewall re-opens.
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
# modules/observability. There is no public surface: the ports Caddy binds are
# firewalled off at the cloud edge (E6) and traffic arrives via the tunnel.
{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.llunde.ingress;
  quadlet = import ../quadlet/mk-quadlet.nix {inherit lib;};

  acmeEmail = "fhansteen@gmail.com";

  # Built by this repo (images/caddy) because upstream ships no DNS-01 provider
  # and caddy cannot load plugins at runtime — H1, ADR 017 amended.
  # Digest-pinned like cloudflared; CI asserts the version and the presence of
  # dns.providers.cloudflare on every build before this digest may move.
  # Referenced by BOTH the quadlet and the reconcile check below, so the config
  # can never be validated against a different caddy than the one that serves.
  caddyImage = "ghcr.io/fredrir/llunde-caddy@sha256:4c9de100f63866a9e7efdfbb69a97902e62b14d577f2ceea818ef7973ef524ee";

  # A format-valid but obviously fake token: the cloudflare provider rejects an
  # EMPTY one at provision time, which would make `caddy validate` fail for a
  # reason that has nothing to do with the Caddyfile. Validation never calls the
  # API, so a real credential would buy nothing and put a live token in a
  # world-readable command line.
  acmeCheckDummyToken = "0123456789abcdef0123456789abcdef01234567";

  # The trailing `*` is load-bearing — do NOT "tidy" these back to exact paths.
  # Caddy's `path` matcher is exact, and its cleanPath (caddyhttp.go) deliberately
  # PRESERVES a trailing slash, so `/metrics/` cleans to `/metrics/` and misses a
  # bare `path /metrics`. The route is skipped and reverse_proxy then forwards the
  # RAW, unnormalised path upstream. Verified against caddy v2.11.4: `/metrics/`,
  # `/health/`, `/ready/`, `/metrics%2f`, `/metrics//`, `/metrics;x=1`, `/metrics.`
  # all reached the backend through the bare form; all 403 with the glob. Nothing
  # leaks today only because Ktor routes `/metrics` and `/metrics/` separately by
  # default — one `install(IgnoreTrailingSlash)` in the (out-of-repo, :latest)
  # backend would publish the whole scrape with no diff here. A deny-list on an
  # exact matcher is fail-open by construction; the glob makes it fail closed.
  #
  # `path` (not `path_regexp`) is deliberate too: `path` is case-INsensitive, so
  # `/METRICS` is covered, and Ktor's routing is likewise case-insensitive. A bare
  # `path_regexp ^/(metrics|health|ready)(/|$)` is case-SENSITIVE and would reopen
  # exactly that vector.
  opsBlock = ''
    @ops path /metrics* /health* /ready*
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

  # Tunnel listener (phase-4 workstream E, ADR 017): cloudflared (host-network)
  # dials this plain-HTTP loopback site; Cloudflare's edge terminated TLS. The
  # three load-bearing properties from the plan review, in one place:
  #   (a) X-Forwarded-Proto is forced to https — this hop is plain HTTP and
  #       would otherwise report http, breaking Secure cookies + CSRF origin
  #       checks in the backend;
  #   (b) X-Forwarded-For is REPLACED with CF-Connecting-IP, making it the
  #       rightmost (trusted) entry the backend keys rate-limit buckets and
  #       audit rows on — unmapped, everything would key on cloudflared's
  #       127.0.0.1; client-supplied XFF is discarded by the replacement, and
  #       the mapping lives ONLY on this listener because on the public :443
  #       path (until E6 closes it) CF-Connecting-IP is attacker-supplied;
  #   (c) the same @ops 403s as the public vhosts — the tunnel must not
  #       re-expose /metrics & friends.
  tunnelHeaderUp = ''
    header_up X-Forwarded-Proto https
    			header_up X-Forwarded-For {header.CF-Connecting-IP}'';

  tunnelHostBlock = name: vhost: let
    m = "@t_" + lib.replaceStrings ["." "-"] ["_" "_"] name;
  in
    if vhost.redirectTo != null
    then ''
      ${m} host ${name}
      handle ${m} {
      	redir ${vhost.redirectTo}{uri} permanent
      }
    ''
    else ''
      	${m} host ${name}
      	handle ${m} {
      ${lib.optionalString vhost.blockOpsEndpoints "\t\t@ops_${lib.replaceStrings ["."] ["_"] name} path /metrics* /health* /ready*\n\t\trespond @ops_${lib.replaceStrings ["."] ["_"] name} 403\n"}		reverse_proxy ${vhost.upstream} {
      			${tunnelHeaderUp}
      		}
      	}
    '';

  # NB: the address is HOSTLESS (http://:8085) with an explicit loopback bind —
  # an address like http://127.0.0.1:8085 would make "127.0.0.1" the site's
  # HOST matcher, and every tunnel request (Host: api.llunde.no etc.) would
  # match no site at all, yielding Caddy's default empty 200 (E-workstream
  # execution finding).
  tunnelSite = ''
    http://:8085 {
    	bind 127.0.0.1
    	# M1 (phase-4 review): reject any request that reaches this listener
    	# without a CF-Connecting-IP. cloudflared always sets it; its absence means
    	# the request is not tunnel-origin. Otherwise the header_up below Sets an
    	# EMPTY X-Forwarded-For, Ktor falls back to the loopback peer ("localhost"),
    	# and every such request collapses into ONE rate-limit bucket. `handle`
    	# blocks match in written order, so this guard MUST precede the host blocks.
    	@nocf {
    		not header_regexp CF-Connecting-IP .
    	}
    	handle @nocf {
    		respond 400
    	}
    ${lib.concatStringsSep "\n" (lib.mapAttrsToList tunnelHostBlock cfg.virtualHosts)}
    	handle {
    		respond 404
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

  # ACME challenge selection (H1, ADR 017 amended — that ADR originally DECLINED
  # DNS-01). Once E6 closes 80/443 there is no inbound path for http-01 or
  # tls-alpn-01, so renewals for all three names would fail from ~2026-10-08
  # (certs issued 2026-08-09, expire 2026-11-07). DNS-01 needs no open port.
  #
  # `acme_dns` in the global block makes it the default challenge for every site
  # here. The token argument is REQUIRED — a bare `acme_dns cloudflare` is
  # rejected at parse time — and the provider validates the token's FORMAT at
  # provision time, so `caddy validate` needs a plausible value in the
  # environment even though it never calls the API.
  acmeDnsLine = lib.optionalString (cfg.acmeDnsTokenFile != null) ''
    	acme_dns cloudflare {env.CF_API_TOKEN}
  '';

  caddyfile =
    ''
      {
      	email ${acmeEmail}
      ${acmeDnsLine}}

    ''
    + lib.concatStringsSep "\n" (lib.mapAttrsToList vhostBlock cfg.virtualHosts)
    + "\n"
    + metricsSite
    + lib.optionalString cfg.tunnel.enable ("\n" + tunnelSite);
in {
  options.llunde.ingress = {
    enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Run Caddy as the host's sole public ingress/TLS terminator.";
    };
    tunnel = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Serve the vhosts through a Cloudflare Tunnel (ADR 017): cloudflared quadlet + the :8085 loopback listener.";
      };
      tokenFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = "EnvironmentFile with TUNNEL_TOKEN= (sops-rendered). Never rotated casually — the standing tunnel rule.";
      };
    };
    acmeDnsTokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        EnvironmentFile providing CF_API_TOKEN for the ACME DNS-01 challenge
        (H1, ADR 017 amended). Setting it switches every vhost's default
        challenge to DNS-01, which is the only kind that survives 80/443 being
        closed. A SEPARATE, host-scoped Zone:DNS:Edit token — never the laptop's
        ops token, which also carries account-wide tunnel rights.
      '';
    };
    caddyfileCheck = lib.mkOption {
      type = lib.types.str;
      readOnly = true;
      description = ''
        Pre-restart validation command for the gitops reconcile map. Lives here
        rather than in the host file so it cannot drift from the image digest it
        must run.

        It validates using the CONTAINER — the exact binary that will serve —
        not a nixpkgs caddy. That is not a stylistic choice: with `acme_dns
        cloudflare` in the config, a nixpkgs caddy carrying the plugin would
        VALIDATE a config the running image cannot parse, and reconcile would
        then restart the front door into a crash-loop. Checking with the binary
        that will run is the only variant that fails safe on that ordering.
        Proven on llunde-01: exit 0 against the current config, exit 1 against a
        DNS-01 config on a plugin-less image.
      '';
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
              description = "Deny /metrics, /health, /ready and any path under them publicly (tailnet-only per ADR 013).";
            };
          };
        }
      );
      default = {};
      description = "Public vhosts, e.g. llunde.no and api.llunde.no.";
    };
  };

  config = lib.mkIf cfg.enable {
    # B4 (phase-4 pre-cutover review): register the `edge` user so the
    # switch-time reload hook (modules/quadlet `llundeQuadletReload`, which
    # iterates `serviceUsers`) actually reaches caddy + cloudflared. Without
    # this edge (2000) is absent from the list, so a rebuild rewrites the
    # Caddyfile / cloudflared unit, exits 0, and the front door keeps running
    # the OLD unit until reboot or a manual restart — a rebuild that lies.
    # autoUpdate = false is deliberate and load-bearing: the public front door
    # never auto-pulls — BOTH images are digest-pinned now; image bumps are
    # deliberate edits, never a 5-minute registry poll. (Until H1 this comment
    # claimed caddy was ":2-pinned". A floating major tag is not a pin: it held
    # still only because quadlet's Pull=missing let the local image cache hide
    # the drift, so a reprovision or a `podman image prune` could have changed
    # the front-door binary with no diff in this repo.)
    # NB the hook only `daemon-reload`s; a Caddyfile CONTENT change is
    # bind-mounted and resolved at container CREATION, so a deploy touching it
    # still needs an explicit `systemctl --user -M edge@ restart caddy`
    # (documented in runbook §6).
    llunde.quadlet.serviceUsers = [
      {
        name = "edge";
        uid = 2000;
        autoUpdate = false;
      }
    ];

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

    # Rendered here (not in the host file) so it always names the same image the
    # quadlet runs. runuser needs a cwd the target user can read — gitops-pull's
    # unit sets WorkingDirectory=/, which satisfies that; run by hand from /root
    # it fails with "cannot chdir". --network=none because validation must never
    # reach the network, least of all Cloudflare's API.
    llunde.ingress.caddyfileCheck = let
      podman = config.virtualisation.podman.package;
    in
      lib.concatStringsSep " " [
        "${pkgs.util-linux}/bin/runuser -u edge --"
        "${pkgs.coreutils}/bin/env XDG_RUNTIME_DIR=/run/user/2000"
        "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2000/bus"
        "${podman}/bin/podman run --rm --network=none"
        "-e CF_API_TOKEN=${acmeCheckDummyToken}"
        "-v /etc/llunde/caddy/Caddyfile:/cf:ro"
        caddyImage
        "caddy validate --adapter caddyfile --config /cf"
      ];

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
          Image = caddyImage;
          Network = "host";
          # CF_API_TOKEN for the DNS-01 challenge. In [Container], NOT [Service]:
          # caddy reads it INSIDE the container. (REGISTRY_AUTH_FILE below is the
          # opposite case — podman itself reads that, before any container
          # exists, so it belongs in [Service].)
          EnvironmentFile = lib.optionals (cfg.acmeDnsTokenFile != null) [cfg.acmeDnsTokenFile];
          Volume = [
            "/etc/llunde/caddy/Caddyfile:/etc/caddy/Caddyfile:ro"
            # Named volumes: certificate storage must survive container replacement.
            "caddy-data:/data"
            "caddy-config:/config"
          ];
        };
        service = {
          # Private image, so the pull needs credentials — edge's own, not the
          # shared ghcr group secret (hosts/llunde-01/secrets.nix explains why).
          Environment = "REGISTRY_AUTH_FILE=/run/secrets/llunde-caddy-ghcr.json";
          Restart = "always";
          MemoryMax = "256M";
          # The first start after this switch PULLS a new image; the user
          # manager's 90 s default would kill it mid-pull (llunde-backend and
          # the whole observability stack carry the same bound for the same
          # reason). Reconcile calls `systemctl restart`, which blocks on this.
          TimeoutStartSec = 300;
        };
        install.WantedBy = ["default.target"];
      }
      // {
        "llunde/caddy/Caddyfile".text = caddyfile;
      }
      // lib.optionalAttrs cfg.tunnel.enable (quadlet.mkContainerUnit {
        name = "cloudflared";
        uid = 2000;
        unit.Description = "Cloudflare tunnel connector for the llunde vhosts (ADR 017)";
        container = {
          ContainerName = "llunde-cloudflared";
          # Host networking: it dials Caddy's 127.0.0.1:8085 — a podman-networked
          # container's localhost is its own (phase-3.5 cutover lesson).
          Network = "host";
          # Digest-pinned (second-opinion requirement): the front door never
          # rides an unpinned :latest; updates are deliberate digest bumps.
          Image = "docker.io/cloudflare/cloudflared@sha256:e39ee8da81ad5e05d77f38d2f51c60ca51bf2a8450ac3abab50c17fdb91d91bf";
          Exec = "tunnel --no-autoupdate run";
          EnvironmentFile = [cfg.tunnel.tokenFile];
        };
        service = {
          Restart = "always";
          MemoryMax = "256M";
        };
        install.WantedBy = ["default.target"];
      });
  };
}
