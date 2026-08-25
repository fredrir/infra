{
  description = "llunde infrastructure — Git describes desired state";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";
    disko = {
      url = "github:nix-community/disko";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    sops-nix = {
      url = "github:Mic92/sops-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = {
    self,
    nixpkgs,
    disko,
    sops-nix,
  }: let
    lib = nixpkgs.lib;
    quadlet = import ./modules/quadlet/mk-quadlet.nix {inherit lib;};

    renderedContainer =
      (quadlet.mkContainerUnit {
        name = "dummy";
        uid = 991;
        container = {
          AutoUpdate = "registry";
          Image = "ghcr.io/example/dummy:latest";
          PublishPort = [
            "127.0.0.1:8080:8080"
            "127.0.0.1:9090:9090"
          ];
          ReadOnly = true;
        };
        service.Restart = "always";
        install.WantedBy = ["default.target"];
      })."containers/systemd/users/991/dummy.container".text;

    goldenContainer = ''
      [Container]
      AutoUpdate=registry
      Image=ghcr.io/example/dummy:latest
      PublishPort=127.0.0.1:8080:8080
      PublishPort=127.0.0.1:9090:9090
      ReadOnly=true

      [Service]
      Restart=always

      [Install]
      WantedBy=default.target
    '';

    renderedNetwork =
      (quadlet.mkNetworkUnit {
        name = "dummy";
        uid = 991;
        network = {
          Internal = true;
          Subnet = "10.89.0.0/24";
        };
        install.WantedBy = ["default.target"];
      })."containers/systemd/users/991/dummy.network".text;

    goldenNetwork = ''
      [Network]
      Internal=true
      Subnet=10.89.0.0/24

      [Install]
      WantedBy=default.target
    '';

    # The backend container as deployed, from the pure fragment the module uses.
    renderedBackend = (import ./services/llunde-backend/unit.nix {inherit lib;}).text;

    goldenBackend = ''
      [Unit]
      After=llunde-postgres.service
      After=llunde-valkey.service
      Requires=llunde-postgres.service
      Requires=llunde-valkey.service

      [Container]
      AutoUpdate=registry
      ContainerName=llunde-backend
      Environment=APP_ENV=prod
      Environment=DB_HOST=llunde-postgres
      Environment=DB_PORT=5432
      Environment=DB_NAME=llunde
      Environment=DB_USER=llunde
      Environment=VALKEY_HOST=llunde-valkey
      Environment=VALKEY_PORT=6379
      Environment=CORS_ALLOWED_ORIGINS=https://llunde.no
      Environment=JAVA_OPTS=-Xmx640m
      Environment=HOME=/tmp
      EnvironmentFile=/run/secrets/doppler.env
      EnvironmentFile=/run/secrets/llunde-backend-db.env
      Image=ghcr.io/fredrir/llunde-backend:latest
      Network=llunde-backend-data.network
      Network=podman
      PublishPort=127.0.0.1:8080:8080

      [Service]
      Environment=REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json
      MemoryMax=1G
      Restart=always
      TimeoutStartSec=300

      [Install]
      WantedBy=default.target
    '';

    pyparserUnits = import ./services/pyparser/unit.nix {inherit lib;};
    renderedPyparserReview = pyparserUnits.review.text;
    renderedPyparserMigrate = pyparserUnits.migrate.text;

    goldenPyparserReview = ''
      [Unit]
      After=pyparser-migrate.service
      Requires=pyparser-migrate.service

      [Container]
      AutoUpdate=registry
      ContainerName=pyparser-review
      Environment=PYPARSER_ENV=production
      Environment=PYPARSER_DOCLING_NUM_THREADS=1
      EnvironmentFile=/run/pyparser/secrets.env
      HealthCmd=curl -fsS http://localhost:8081/healthz
      Image=ghcr.io/fredrir/pyparser-review:latest
      Network=pyparser.network
      NetworkAlias=review
      Volume=pyparser-files:/app/.local/files

      [Service]
      Environment=REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json
      Restart=always
      TimeoutStartSec=300

      [Install]
      WantedBy=default.target
    '';

    goldenPyparserMigrate = ''
      [Unit]
      After=pyparser-postgres.service
      PartOf=pyparser-review.service
      Requires=pyparser-postgres.service

      [Container]
      ContainerName=pyparser-migrate
      EnvironmentFile=/run/pyparser/secrets.env
      Exec=alembic upgrade head
      Image=ghcr.io/fredrir/pyparser-review:latest
      Network=pyparser.network

      [Service]
      Environment=REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json
      RemainAfterExit=true
      TimeoutStartSec=600
      Type=oneshot
    '';

    observabilityUnits = import ./services/observability/unit.nix {inherit lib;};
    renderedObsPrometheus = observabilityUnits.prometheus.text;
    renderedObsScrapeConfig = observabilityUnits.prometheusConfig;
    renderedObsGrafana = observabilityUnits.grafana.text;

    goldenObsPrometheus = ''
      [Container]
      ContainerName=observability-prometheus
      Exec=--config.file=/etc/prometheus/prometheus.yml --storage.tsdb.path=/prometheus --storage.tsdb.retention.time=60d
      Image=docker.io/prom/prometheus:v3.13.2
      Network=observability.network
      PublishPort=0.0.0.0:9090:9090
      Volume=/etc/llunde/observability/prometheus.yml:/etc/prometheus/prometheus.yml:ro
      Volume=prometheus-data:/prometheus:U

      [Service]
      MemoryMax=512M
      Restart=always
      TimeoutStartSec=300

      [Install]
      WantedBy=default.target
    '';

    goldenObsScrapeConfig = ''
      global:
        scrape_interval: 30s
        evaluation_interval: 30s

      scrape_configs:
        - job_name: node
          honor_labels: true
          static_configs:
            - targets: ["100.109.80.121:9100"]
              labels:
                host: llunde-01
            - targets: ["100.92.219.50:9100"]
              labels:
                host: llunde-parser

        - job_name: llunde-backend
          metrics_path: /metrics
          static_configs:
            - targets: ["100.109.80.121:9101"]
              labels:
                host: llunde-01

        - job_name: blackbox-public
          metrics_path: /probe
          params:
            module: [http_2xx]
          static_configs:
            - targets: ["https://llunde.no"]
          relabel_configs:
            - source_labels: [__address__]
              target_label: __param_target
            - source_labels: [__param_target]
              target_label: instance
            - target_label: __address__
              replacement: observability-blackbox:9115

        - job_name: blackbox-public-cfray
          metrics_path: /probe
          params:
            module: [http_2xx_cfray]
          static_configs:
            - targets: ["https://llunde.no"]
          relabel_configs:
            - source_labels: [__address__]
              target_label: __param_target
            - source_labels: [__param_target]
              target_label: instance
            - target_label: __address__
              replacement: observability-blackbox:9115

        - job_name: blackbox-public-403
          metrics_path: /probe
          params:
            module: [http_403]
          static_configs:
            - targets: ["https://api.llunde.no/health"]
          relabel_configs:
            - source_labels: [__address__]
              target_label: __param_target
            - source_labels: [__param_target]
              target_label: instance
            - target_label: __address__
              replacement: observability-blackbox:9115

        - job_name: blackbox-ready
          metrics_path: /probe
          params:
            module: [http_2xx]
          static_configs:
            - targets: ["http://100.109.80.121:9101/ready"]
          relabel_configs:
            - source_labels: [__address__]
              target_label: __param_target
            - source_labels: [__param_target]
              target_label: instance
            - target_label: __address__
              replacement: observability-blackbox:9115
    '';

    hostEtc = self.nixosConfigurations.llunde-01.config.environment.etc;
    renderedCaddyfile = hostEtc."llunde/caddy/Caddyfile".text;
    renderedCloudflared = hostEtc."containers/systemd/users/2000/cloudflared.container".text;

    renderedCaddy = hostEtc."containers/systemd/users/2000/caddy.container".text;
    renderedValkey = hostEtc."containers/systemd/users/2001/llunde-valkey.container".text;
    goldenCaddyfile = builtins.readFile ./tests/golden/llunde-01.Caddyfile;
    goldenCaddy = builtins.readFile ./tests/golden/caddy.container;
    goldenCloudflared = builtins.readFile ./tests/golden/cloudflared.container;
    goldenValkey = builtins.readFile ./tests/golden/llunde-valkey.container;
    goldenObsGrafana = builtins.readFile ./tests/golden/observability-grafana.container;

    gitopsPullScriptCheck = system: let
      sys = nixpkgs.lib.nixosSystem {
        inherit system;
        modules = [
          ./modules/gitops-pull
          {
            networking.hostName = "gitops-check";
            llunde.gitopsPull = {
              enable = true;
              sshKeyFile = "/run/secrets/gitops-deploy-key";
              requireHeartbeat = false;
              heartbeatUrlFile = "/run/secrets/gitops-heartbeat-url";
              probePeer = "100.64.0.1";
              reconcile.dummy = {
                uid = 999;
                units.dummy = {
                  watch = ["/etc/dummy.container"];
                  check = "true";
                };
              };
            };
          }
        ];
      };
    in
      nixpkgs.legacyPackages.${system}.writeText "gitops-pull-script-ok"
      sys.config.systemd.services.gitops-pull.serviceConfig.ExecStart;

    obsConfigCheck = system: let
      pkgs = nixpkgs.legacyPackages.${system};
    in
      pkgs.runCommand "obs-config-ok" {} ''
        ${pkgs.prometheus-blackbox-exporter}/bin/blackbox_exporter \
          --config.file=${./services/observability/blackbox.yml} --config.check
        ${pkgs.prometheus.cli}/bin/promtool check config \
          ${pkgs.writeText "prometheus.yml" renderedObsScrapeConfig}
        ${pkgs.yq-go}/bin/yq eval '.' \
          ${./services/observability/grafana/alerting.yaml} >/dev/null
        touch $out
      '';

    mkRenderCheck = system:
      assert lib.assertMsg (renderedObsPrometheus == goldenObsPrometheus)
      "observability prometheus unit drifted from golden:\n---rendered---\n${renderedObsPrometheus}\n---golden---\n${goldenObsPrometheus}";
      assert lib.assertMsg (renderedObsScrapeConfig == goldenObsScrapeConfig)
      "observability scrape config drifted from golden:\n---rendered---\n${renderedObsScrapeConfig}\n---golden---\n${goldenObsScrapeConfig}";
      assert lib.assertMsg (renderedPyparserReview == goldenPyparserReview)
      "pyparser review unit drifted from golden:\n---rendered---\n${renderedPyparserReview}\n---golden---\n${goldenPyparserReview}";
      assert lib.assertMsg (renderedPyparserMigrate == goldenPyparserMigrate)
      "pyparser migrate unit drifted from golden:\n---rendered---\n${renderedPyparserMigrate}\n---golden---\n${goldenPyparserMigrate}";
      assert lib.assertMsg (renderedContainer == goldenContainer)
      "mkContainerUnit rendering drifted from golden:\n---rendered---\n${renderedContainer}\n---golden---\n${goldenContainer}";
      assert lib.assertMsg (renderedNetwork == goldenNetwork)
      "mkNetworkUnit rendering drifted from golden:\n---rendered---\n${renderedNetwork}\n---golden---\n${goldenNetwork}";
      assert lib.assertMsg (renderedBackend == goldenBackend)
      "backend unit rendering drifted from golden:\n---rendered---\n${renderedBackend}\n---golden---\n${goldenBackend}";
      assert lib.assertMsg (renderedCaddyfile == goldenCaddyfile)
      "llunde-01 Caddyfile drifted from golden (regenerate tests/golden/llunde-01.Caddyfile):\n---rendered---\n${renderedCaddyfile}\n---golden---\n${goldenCaddyfile}";
      assert lib.assertMsg (renderedCaddy == goldenCaddy)
      "caddy unit drifted from golden — image digest / REGISTRY_AUTH_FILE / TimeoutStartSec are load-bearing (regenerate tests/golden/caddy.container):\n---rendered---\n${renderedCaddy}\n---golden---\n${goldenCaddy}";
      assert lib.assertMsg (renderedCloudflared == goldenCloudflared)
      "cloudflared unit drifted from golden:\n---rendered---\n${renderedCloudflared}\n---golden---\n${goldenCloudflared}";
      assert lib.assertMsg (renderedValkey == goldenValkey)
      "valkey unit drifted from golden:\n---rendered---\n${renderedValkey}\n---golden---\n${goldenValkey}";
      assert lib.assertMsg (renderedObsGrafana == goldenObsGrafana)
      "grafana unit drifted from golden (anonymous/basic-auth posture — regenerate tests/golden/observability-grafana.container):\n---rendered---\n${renderedObsGrafana}\n---golden---\n${goldenObsGrafana}";
        nixpkgs.legacyPackages.${system}.writeText "mkquadlet-render-ok" renderedContainer;
  in {
    nixosConfigurations.llunde-01 = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        disko.nixosModules.disko
        sops-nix.nixosModules.sops
        ./hosts/llunde-01
      ];
    };

    nixosConfigurations.llunde-parser = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        disko.nixosModules.disko
        sops-nix.nixosModules.sops
        ./hosts/llunde-parser
      ];
    };

    nixosConfigurations.llunde-parser-rehearsal = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        disko.nixosModules.disko
        sops-nix.nixosModules.sops
        ./hosts/llunde-parser
        {
          networking.hostName = lib.mkForce "llunde-parser-rehearsal";
          llunde.pyparser.tunnel.enable = false;
          llunde.tailscale.enable = lib.mkForce false;
          llunde.backups.enable = lib.mkForce false;
          llunde.observability.stack.enable = false;
          services.openssh.openFirewall = lib.mkForce true;
        }
      ];
    };

    checks.x86_64-linux.mkquadlet-render = mkRenderCheck "x86_64-linux";
    checks.x86_64-linux.gitops-pull-script = gitopsPullScriptCheck "x86_64-linux";
    checks.x86_64-linux.obs-config = obsConfigCheck "x86_64-linux";
    checks.aarch64-linux.mkquadlet-render = mkRenderCheck "aarch64-linux";
    checks.aarch64-darwin.mkquadlet-render = mkRenderCheck "aarch64-darwin";
    checks.aarch64-darwin.obs-config = obsConfigCheck "aarch64-darwin";
  };
}
