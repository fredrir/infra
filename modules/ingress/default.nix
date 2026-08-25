{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.llunde.ingress;
  quadlet = import ../quadlet/mk-quadlet.nix {inherit lib;};

  acmeEmail = "fhansteen@gmail.com";

  textfileDir = "/var/lib/node-exporter-text";

  caddyImage = "ghcr.io/fredrir/llunde-caddy@sha256:4c9de100f63866a9e7efdfbb69a97902e62b14d577f2ceea818ef7973ef524ee";

  acmeCheckDummyToken = "0123456789abcdef0123456789abcdef01234567";

  opsBlock = ''
    @ops path /metrics* /health* /ready*
    respond @ops 403
  '';

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

  tunnelSite = ''
    http://:8085 {
    	bind 127.0.0.1
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
        if end=$(openssl s_client -connect 127.0.0.1:443 -servername "$name" </dev/null 2>/dev/null \
                   | openssl x509 -noout -enddate 2>/dev/null) \
           && epoch=$(date -d "''${end#notAfter=}" +%s 2>/dev/null); then
          printf 'caddy_cert_expiry_timestamp{name="%s"} %s\n' "$name" "$epoch" >> "$tmp"
        else
          echo "no usable certificate served for $name on 127.0.0.1:443" >&2
          rc=1
        fi
      done

      mv "$tmp" "${textfileDir}/caddy_cert_expiry.prom"
      exit "$rc"
    '';
  };
in {
  options.llunde.ingress = {
    enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
    };
    tunnel = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
      };
      tokenFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
      };
    };
    acmeDnsTokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
    };
    caddyfileCheck = lib.mkOption {
      type = lib.types.str;
      readOnly = true;
    };
    virtualHosts = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            upstream = lib.mkOption {
              type = lib.types.str;
              default = "";
              example = "127.0.0.1:8080";
            };
            redirectTo = lib.mkOption {
              type = lib.types.nullOr lib.types.str;
              default = null;
              example = "https://llunde.no";
            };
            blockOpsEndpoints = lib.mkOption {
              type = lib.types.bool;
              default = true;
            };
          };
        }
      );
      default = {};
    };
  };

  config = lib.mkIf cfg.enable {
    llunde.quadlet.serviceUsers = [
      {
        name = "edge";
        uid = 2000;
        autoUpdate = false;
      }
    ];

    llunde.ingress.virtualHosts = {
      "llunde.no" = {
        upstream = lib.mkDefault "127.0.0.1:8081";
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

    networking.firewall.interfaces."tailscale0".allowedTCPPorts = [9101];

    systemd.services.caddy-cert-expiry = {
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
        container = {
          Image = caddyImage;
          Network = "host";
          EnvironmentFile = lib.optionals (cfg.acmeDnsTokenFile != null) [cfg.acmeDnsTokenFile];
          Volume = [
            "/etc/llunde/caddy/Caddyfile:/etc/caddy/Caddyfile:ro"
            "caddy-data:/data"
            "caddy-config:/config"
          ];
        };
        service = {
          Environment = "REGISTRY_AUTH_FILE=/run/secrets/llunde-caddy-ghcr.json";
          Restart = "always";
          MemoryMax = "256M";
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
        container = {
          ContainerName = "llunde-cloudflared";
          Network = "host";
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
