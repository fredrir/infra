{
  config,
  lib,
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
  };

  config = lib.mkIf cfg.enable {
    services.prometheus.exporters.node = {
      enable = true;
      port = nodeExporterPort;
      enabledCollectors = ["systemd"];
      extraFlags = ["--collector.textfile.directory=${textfileDir}"];
    };

    systemd.tmpfiles.rules = ["d ${textfileDir} 0755 root root -"];

    services.promtail = lib.mkIf (cfg.lokiUrl != null) {
      enable = true;
      configuration = {
        server = {
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

    services.journald.extraConfig = ''
      SystemMaxUse=1G
    '';
  };
}
