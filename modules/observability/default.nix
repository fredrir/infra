{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.llunde.observability;
  nodeExporterPort = 9100;
  textfileDir = "/var/lib/node-exporter-text";
in {
  options.llunde.observability = {
    enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
    };
    tailnetOnly = lib.mkOption {
      type = lib.types.bool;
      default = true;
    };
    lokiUrl = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "http://100.92.219.50:3100";
    };
    legacyPositionsFile = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = "/var/lib/promtail/positions.yaml";
    };
    journalMaxAge = lib.mkOption {
      type = lib.types.str;
      default = "12h";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.lokiUrl == null || lib.versionAtLeast pkgs.grafana-alloy.version "1.16.0";
        message = "Alloy journal cursor migration requires Alloy 1.16.0 or newer";
      }
    ];

    services.prometheus.exporters.node = {
      enable = true;
      port = nodeExporterPort;
      enabledCollectors = ["systemd"];
      extraFlags = ["--collector.textfile.directory=${textfileDir}"];
    };

    systemd.tmpfiles.rules = ["d ${textfileDir} 0755 root root -"];

    services.alloy = lib.mkIf (cfg.lokiUrl != null) {
      enable = true;
      extraFlags = [
        "--server.http.listen-addr=127.0.0.1:12345"
        "--storage.path=/var/lib/alloy/data"
        "--disable-reporting"
      ];
    };

    environment.etc."alloy/journal.alloy" = lib.mkIf (cfg.lokiUrl != null) {
      text = ''
        loki.relabel "journal" {
          forward_to = []
          rule {
            source_labels = ["__journal__systemd_unit"]
            target_label = "unit"
          }
          rule {
            target_label = "job"
            replacement = "systemd-journal"
          }
        }
        loki.source.journal "journal" {
          max_age = ${builtins.toJSON cfg.journalMaxAge}
          labels = { host = ${builtins.toJSON config.networking.hostName} }
          relabel_rules = loki.relabel.journal.rules
          forward_to = [loki.write.central.receiver]
          ${lib.optionalString (cfg.legacyPositionsFile != null) ''
            legacy_position {
              file = "/var/lib/alloy/promtail-positions.yaml"
              name = "journal"
            }
          ''}
        }
        loki.write "central" {
          endpoint {
            url = ${builtins.toJSON "${cfg.lokiUrl}/loki/api/v1/push"}
          }
        }
      '';
    };

    systemd.services.alloy = lib.mkIf (cfg.lokiUrl != null) {
      conflicts = ["promtail.service"];
      after = ["promtail.service"];
      serviceConfig = {
        MemoryMax = "256M";
        ExecStartPre = lib.optional (cfg.legacyPositionsFile != null)
          "+${pkgs.writeShellScript "alloy-journal-cursor" ''
            set -eu
            target=/var/lib/alloy/promtail-positions.yaml
            if [ ! -f "$target" ]; then
              test -s ${lib.escapeShellArg cfg.legacyPositionsFile}
              ${pkgs.coreutils}/bin/install -o alloy -g alloy -m 0600 \
                ${lib.escapeShellArg cfg.legacyPositionsFile} "$target"
            fi
          ''}";
      };
    };

    networking.firewall = lib.mkMerge [
      (lib.mkIf cfg.tailnetOnly {
        interfaces."tailscale0".allowedTCPPorts = [nodeExporterPort];
      })
      (lib.mkIf (!cfg.tailnetOnly) {
        allowedTCPPorts = [nodeExporterPort];
      })
    ];

    services.journald.extraConfig = ''
      SystemMaxUse=1G
    '';
  };
}
