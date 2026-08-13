# Caddy ingress under the `edge` user (uid 2000) — ADR 006, with Let's Encrypt
# certificates renewed over DNS-01 (ADR 017 as amended). No traffic reaches this
# host's 80/443: llunde.no, www and api are proxied CNAMEs onto the llunde
# tunnel and the cloud firewall admits nothing. Caddy binds those ports anyway,
# deliberately — they are the break-glass path, live again the moment the
# firewall re-opens.
#
# Network=host is required; a bridge-networked container shape does not work
# here. Every upstream is published on host loopback ONLY (127.0.0.1:8080/8081,
# ADR 005) and a rootless bridge container cannot reach that:
# host.containers.internal resolves to a routable host address, which
# 127.0.0.1-bound sockets deliberately do not answer. Host
# networking makes the loopback upstreams work verbatim and binds 80/443
# directly (ip_unprivileged_port_start=80, set in the base profile). Isolation
# holds — other users' podman networks live in their own rootless network
# namespaces, unreachable from the host namespace, so the only cross-user
# surface stays the deliberately published loopback ports. PublishPort is
# meaningless under host networking and therefore absent.
#
# Ops endpoints (/metrics, /health, /ready) are blocked on the public vhosts
# (ADR 013) and reachable off-box only through the tailnet-scoped :9101 listener
# below; node-level metrics stay with modules/observability.
{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.llunde.ingress;
  quadlet = import ../quadlet/mk-quadlet.nix {inherit lib;};

  acmeEmail = "fhansteen@gmail.com";

  # Shared with modules/backups' restic stamps, read by node_exporter's textfile
  # collector (modules/observability). Named rather than imported so this module
  # carries no dependency on observability; the stamp script mkdir -p's it.
  textfileDir = "/var/lib/node-exporter-text";

  # Built by this repo (images/caddy): upstream ships no DNS-01 provider and
  # caddy cannot load plugins at runtime (ADR 017 amended). Digest-pinned like
  # cloudflared; CI asserts the version and dns.providers.cloudflare on every
  # build before this digest may move. Referenced by BOTH the quadlet and the
  # reconcile check below, so the config is never validated against another caddy.
  caddyImage = "ghcr.io/fredrir/llunde-caddy@sha256:4c9de100f63866a9e7efdfbb69a97902e62b14d577f2ceea818ef7973ef524ee";

  # Format-valid but obviously fake: the cloudflare provider rejects an EMPTY
  # token at provision time, failing `caddy validate` for a reason unrelated to
  # the Caddyfile. Validation never calls the API, so a real credential would buy
  # nothing and put a live token in a world-readable command line.
  acmeCheckDummyToken = "0123456789abcdef0123456789abcdef01234567";

  # The trailing `*` is load-bearing — do NOT "tidy" these back to exact paths.
  # `path` is an exact matcher and Caddy's cleanPath (caddyhttp.go) deliberately
  # PRESERVES a trailing slash, so on caddy v2.11.4 `/metrics/`, `/metrics//`,
  # `/metrics%2f`, `/metrics;x=1`, `/metrics.` (likewise /health, /ready) all miss
  # a bare `path /metrics`: the route is skipped and reverse_proxy forwards the
  # RAW, unnormalised path upstream. The glob 403s every one of them — a
  # deny-list on an exact matcher is fail-open by construction. Nothing leaks
  # today only because Ktor routes `/metrics` and `/metrics/` separately by
  # default; one `install(IgnoreTrailingSlash)` in the (out-of-repo, :latest)
  # backend would publish the whole scrape with no diff here.
  # `path` not `path_regexp`: `path` is case-INsensitive, so `/METRICS` is
  # covered, matching Ktor's case-insensitive routing, while a bare
  # `path_regexp ^/(metrics|health|ready)(/|$)` is case-SENSITIVE and reopens it.
  opsBlock = ''
    @ops path /metrics* /health* /ready*
    respond @ops 403
  '';

  # Backend ops listener for the tailnet. ADR 013's "scraped over the tailnet
  # directly against 127.0.0.1-published ports" cannot work — a loopback bind is
  # by definition unreachable off-box, so nothing collected backend metrics
  # remotely until this site. Caddy already runs host-network and reaches the
  # backend loopback, so it gains a second plain-HTTP site on :9101, exposed
  # on the node_exporter model (modules/observability): wildcard bind, 9101 never
  # in publicTCPPorts, only trusted tailscale0 admitting traffic. EXACTLY two
  # paths proxy to the backend, /metrics (Prometheus scrape) and /ready (blackbox
  # full-readiness probe); everything else answers 403, mirroring the public
  # vhosts' @ops block.
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

  # Tunnel listener (ADR 017): cloudflared (host-network) dials this plain-HTTP
  # loopback site; Cloudflare's edge already terminated TLS. X-Forwarded-Proto is
  # forced to https because this hop is plain HTTP and would otherwise report
  # http, breaking Secure cookies and CSRF origin checks in the backend.
  # X-Forwarded-For is REPLACED with CF-Connecting-IP, making it the rightmost
  # (trusted) entry the backend keys rate-limit buckets and audit rows on —
  # unmapped, everything keys on cloudflared's 127.0.0.1 — and the replacement
  # discards client-supplied XFF. That mapping lives ONLY on this listener:
  # on the break-glass :443 path CF-Connecting-IP is attacker-supplied. The host
  # blocks below repeat the public vhosts' @ops 403s, since the tunnel must not
  # re-expose /metrics & friends.
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

  # The address must stay HOSTLESS (http://:8085) with an explicit loopback bind:
  # http://127.0.0.1:8085 would make "127.0.0.1" the site's HOST matcher, and
  # every tunnel request (Host: api.llunde.no etc.) would match no site at all,
  # yielding Caddy's default empty 200.
  tunnelSite = ''
    http://:8085 {
    	bind 127.0.0.1
    	# Reject requests without a CF-Connecting-IP: cloudflared always sets it,
    	# so its absence means the request is not tunnel-origin. Otherwise header_up
    	# Sets an EMPTY X-Forwarded-For, Ktor falls back to the loopback peer
    	# ("localhost") and every such request collapses into ONE rate-limit bucket.
    	# `handle` blocks match in written order: this guard MUST precede the hosts.
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

  # ACME challenge selection (ADR 017 amended — that ADR originally DECLINED
  # DNS-01): with 80/443 closed nothing inbound can satisfy http-01 or
  # tls-alpn-01, so renewals for all three names would fail; DNS-01 needs no open
  # port. `acme_dns` in the global block makes it the default challenge for every
  # site here. The token argument is REQUIRED — a bare `acme_dns cloudflare` is
  # rejected at parse time — and the provider validates the token's FORMAT at
  # provision time, so `caddy validate` needs a plausible value in the environment
  # even though it never calls the API.
  # ⚠️ The leading TAB is Caddyfile content, not Nix indentation, and `alejandra`
  # (i.e. `nix fmt`) strips it: an indented-string literal's common prefix is
  # removed at parse time, so re-indenting the literal REWRITES the rendered
  # config — harmless to Caddy, but a silent edit to a generated artifact. The
  # Caddyfile golden catches it (`nix flake check` fails with "Caddyfile
  # drifted"). Leave the tab.
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

  # Origin-certificate expiry stamp, the ONLY watch on Caddy's own certificates:
  # cloudflared dials :8085 in PLAIN HTTP, so no production request ever sees an
  # origin certificate, and the `probe_ssl_earliest_cert_expiry` the public
  # blackbox probes report is CLOUDFLARE's edge certificate. A failed DNS-01
  # renewal would otherwise surface only at break-glass, exactly when runbook §11
  # promises warm certificates and an instant re-open.
  #
  # A real TLS handshake against loopback :443, not Caddy's admin API or /data
  # store: the handshake is what a browser does the moment the cloud firewall
  # re-opens, and the firewall is closed while the listener is not, so nothing
  # has to be re-opened to ask. Not a blackbox probe from llunde-parser, where
  # every other probe lives, because the tailnet ACL scopes tag:server ->
  # tag:server to 9100,9101,3100,4317,4318 (tailscale/policy.hujson) and cannot
  # reach :443 here; widening it to watch a certificate would trade real
  # blast-radius reduction for a monitor, and a textfile stamp needs no ACL
  # change at all.
  #
  # An ABSOLUTE deadline, not a freshness counter: if this timer itself breaks,
  # the last value written keeps counting down and the alert still fires on time.
  # A per-name handshake failure is deliberately NOT this stamp's job — that
  # means the vhost is not served at all, which the public probes
  # (blackbox-public*) already catch far faster.
  certStampScript = pkgs.writeShellApplication {
    name = "caddy-cert-expiry-stamp";
    runtimeInputs = [pkgs.openssl pkgs.coreutils];
    text = ''
      mkdir -p ${textfileDir}
      tmp="${textfileDir}/.caddy_cert_expiry.prom.tmp"
      rc=0

      {
        echo '# HELP caddy_cert_expiry_timestamp Unix time the certificate Caddy serves on this host for this name expires.'
        echo '# TYPE caddy_cert_expiry_timestamp gauge'
      } > "$tmp"

      for name in ${lib.concatStringsSep " " (lib.attrNames cfg.virtualHosts)}; do
        # -servername picks the vhost; </dev/null keeps s_client from hanging on
        # stdin after the handshake.
        if end=$(openssl s_client -connect 127.0.0.1:443 -servername "$name" </dev/null 2>/dev/null \
                   | openssl x509 -noout -enddate 2>/dev/null) \
           && epoch=$(date -d "''${end#notAfter=}" +%s 2>/dev/null); then
          printf 'caddy_cert_expiry_timestamp{name="%s"} %s\n' "$name" "$epoch" >> "$tmp"
        else
          echo "no usable certificate served for $name on 127.0.0.1:443" >&2
          rc=1
        fi
      done

      # Write-then-rename so the exporter never reads a torn file (restic
      # precedent). Partial results still land: a failed name is missing, the
      # others keep counting down.
      mv "$tmp" "${textfileDir}/caddy_cert_expiry.prom"
      exit "$rc"
    '';
  };
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
        (ADR 017). Setting it switches every vhost's default challenge to
        DNS-01, which is the only kind that survives 80/443 being closed. A SEPARATE, host-scoped Zone:DNS:Edit token — never the laptop's
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
    # Register the `edge` user so the switch-time reload hook (modules/quadlet
    # `llundeQuadletReload`, which iterates `serviceUsers`) reaches caddy and
    # cloudflared. Without edge (2000) in that list a rebuild rewrites the
    # Caddyfile / cloudflared unit, exits 0, and the front door keeps running the
    # OLD unit until reboot or a manual restart — a rebuild that lies.
    # autoUpdate = false is deliberate and load-bearing: the public front door
    # never auto-pulls, BOTH images are digest-pinned, and bumps are deliberate
    # edits rather than a 5-minute registry poll. A floating major tag is not a
    # pin — quadlet's Pull=missing lets the image cache hide the drift, so a
    # reprovision or `podman image prune` can change the front-door binary with no
    # diff here. NB the hook only `daemon-reload`s: a Caddyfile CONTENT change is
    # bind-mounted and resolved at container CREATION, so a deploy touching it
    # still needs `systemctl --user -M edge@ restart caddy` (runbook §6).
    llunde.quadlet.serviceUsers = [
      {
        name = "edge";
        uid = 2000;
        autoUpdate = false;
      }
    ];

    # Contract defaults; hosts may override.
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

    # Rendered here, not in the host file, so it always names the image the
    # quadlet runs. runuser needs a cwd the target user can read: gitops-pull's
    # unit sets WorkingDirectory=/, while by hand from /root it fails with
    # "cannot chdir". --network=none — validation must never reach the network,
    # least of all Cloudflare's API.
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

    # Same scoping as node_exporter's 9100 (modules/observability): tailscale0 is
    # already a trusted interface, but the explicit rule documents the intended
    # reach of :9101 and survives if that blanket trust is ever narrowed.
    networking.firewall.interfaces."tailscale0".allowedTCPPorts = [9101];

    # Hourly, not daily: the deadline moves once every ~60 days, but a loopback
    # handshake costs nothing and hourly keeps a freshly-switched host's no-stamp
    # window short. Persistent so a box down over a window stamps on boot.
    systemd.services.caddy-cert-expiry = {
      description = "Stamp the expiry of the origin certificates Caddy serves on :443 (ADR 017)";
      serviceConfig = {
        Type = "oneshot";
        ExecStart = lib.getExe certStampScript;
      };
    };

    systemd.timers.caddy-cert-expiry = {
      wantedBy = ["timers.target"];
      timerConfig = {
        OnCalendar = "hourly";
        Persistent = true;
      };
    };

    environment.etc =
      quadlet.mkContainerUnit {
        name = "caddy";
        uid = 2000;
        unit.Description = "Caddy public ingress (ADR 006)";
        container = {
          Image = caddyImage;
          Network = "host";
          # CF_API_TOKEN for DNS-01. In [Container], NOT [Service]: caddy reads it
          # INSIDE the container. REGISTRY_AUTH_FILE below is the opposite case —
          # podman reads that before any container exists, so it goes in [Service].
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
          # The first start after a switch PULLS a new image; the user manager's
          # 90 s default would kill it mid-pull (llunde-backend and the whole
          # observability stack carry the same bound). Reconcile calls `systemctl
          # restart`, which blocks on this.
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
          # Host networking: it dials Caddy's 127.0.0.1:8085, and a
          # podman-networked container's localhost is its own.
          Network = "host";
          # Digest-pinned: the front door never rides an unpinned :latest;
          # updates are deliberate digest bumps.
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
