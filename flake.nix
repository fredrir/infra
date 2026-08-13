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

    # Golden render proof for mkQuadlet (ADR 004), asserted at EVAL time: drift
    # fails `nix flake check` on every system, building nothing but a writeText.
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

    # The two pyparser units whose SHAPE is load-bearing: review (auto-update +
    # migrate coupling) and migrate (oneshot ordering).
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

    # Prometheus unit — retention flag, version pin, wildcard publish and the
    # absence of AutoUpdate are all load-bearing — plus the scrape config, the
    # stack's topology contract.
    observabilityUnits = import ./services/observability/unit.nix {inherit lib;};
    renderedObsPrometheus = observabilityUnits.prometheus.text;
    renderedObsScrapeConfig = observabilityUnits.prometheusConfig;
    # Locks the grafana auth posture (anon=Viewer, basic-auth off) so it cannot
    # silently regress to anonymous admin.
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
        # 9100 is tailscale0-scoped on both hosts (modules/observability), and
        # honor_labels keeps restic's own job="<backup job>" textfile-stamp label
        # off exported_job (modules/backups).
        - job_name: node
          honor_labels: true
          static_configs:
            - targets: ["100.109.80.121:9100"]
              labels:
                host: llunde-01
            - targets: ["100.92.219.50:9100"]
              labels:
                host: llunde-parser

        # Backend JVM/HTTP via Caddy's tailnet-only :9101 (modules/ingress): the
        # app binds 127.0.0.1:8080, so that listener is the only off-box path.
        - job_name: llunde-backend
          metrics_path: /metrics
          static_configs:
            - targets: ["100.109.80.121:9101"]
              labels:
                host: llunde-01

        # Blackbox indirection: the probed URL moves into ?target=, instance keeps
        # the URL, the scrape hits the exporter by CONTAINER NAME on the shared
        # network (aardvark DNS), never a host port. blackbox-public covers the
        # WHOLE public chain the tailnet checks miss: CF edge egress, then tunnel.
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

        # Same URL as blackbox-public, a DIFFERENT question: not "is the site up"
        # but "is it reached THROUGH Cloudflare". Two jobs because one could not
        # tell "site down" from "site up, bypassing the edge" — and bypassing is
        # silent by construction. Named into blackbox-public.*: PublicEdgeDown pages.
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

        # 403 on the backend's ops path IS the healthy signal: Caddy's @ops block
        # answering proves DNS, tunnel and Caddy alive (http_403 accepts only 403).
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

        # Full backend readiness (DB, valkey, …) via the same Caddy listener.
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

    # Rendered artifacts where a silent drift is dangerous: the Caddyfile
    # (ops-block globs, XFF mapping, CF-Connecting-IP guard, bind 127.0.0.1),
    # the cloudflared unit (digest pin, EnvironmentFile, Network=host), the
    # valkey unit (--save "" flag order, digest pin, no :U). Read via self from
    # the REAL host config, so the golden IS the deployed text; too tab-heavy to
    # inline, so they live in tests/golden/. Regenerate on an intended change:
    #   nix eval --raw '.#nixosConfigurations.llunde-01.config.environment.etc."llunde/caddy/Caddyfile".text' > tests/golden/llunde-01.Caddyfile
    hostEtc = self.nixosConfigurations.llunde-01.config.environment.etc;
    renderedCaddyfile = hostEtc."llunde/caddy/Caddyfile".text;
    renderedCloudflared = hostEtc."containers/systemd/users/2000/cloudflared.container".text;
    # The Caddyfile golden proves what caddy is told to do; this proves WHICH
    # caddy — a digest pin carrying a compiled-in ACME provider. A silent revert
    # to a floating tag, or a lost REGISTRY_AUTH_FILE, would only surface at a
    # renewal weeks later.
    renderedCaddy = hostEtc."containers/systemd/users/2000/caddy.container".text;
    renderedValkey = hostEtc."containers/systemd/users/2001/llunde-valkey.container".text;
    goldenCaddyfile = builtins.readFile ./tests/golden/llunde-01.Caddyfile;
    goldenCaddy = builtins.readFile ./tests/golden/caddy.container;
    goldenCloudflared = builtins.readFile ./tests/golden/cloudflared.container;
    goldenValkey = builtins.readFile ./tests/golden/llunde-valkey.container;
    goldenObsGrafana = builtins.readFile ./tests/golden/observability-grafana.container;

    # The module lands with enable = false on both hosts, so nothing else in the
    # gate instantiates its writeShellApplication — and shellcheck runs at BUILD
    # time. This throwaway enabled config forces the script derivation via the
    # ExecStart string's context, so CI lints it. x86_64-linux only: the runtime
    # closure (iproute2) does not evaluate on darwin.
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

    # Config-SYNTAX gate for the stack's two hand-written configs: the goldens
    # prove the rendered text did not DRIFT, not that it is VALID.
    # `fail_if_header_not_matched`, the plausible misspelling of `..._matches`,
    # is golden-clean and exporter-fatal — blackbox refuses to start on an
    # unknown key, so the pull loop gets a failed reconcile and ZERO probes,
    # alerted rather than caught by CI. Both binaries were verified to exit
    # non-zero on a bad file: a gate that cannot fail is worse than no gate.
    # Version skew (nixpkgs blackbox 0.27.0 / promtool 3.7.2 vs deployed v0.28.0
    # / v3.13.2) is fine for syntax — the trade caddy's `validate` check makes.
    obsConfigCheck = system: let
      pkgs = nixpkgs.legacyPackages.${system};
    in
      pkgs.runCommand "obs-config-ok" {} ''
        ${pkgs.prometheus-blackbox-exporter}/bin/blackbox_exporter \
          --config.file=${./services/observability/blackbox.yml} --config.check
        ${pkgs.prometheus.cli}/bin/promtool check config \
          ${pkgs.writeText "prometheus.yml" renderedObsScrapeConfig}
        # Grafana ships no offline validator, so a YAML PARSE is the ceiling; it
        # cannot check the rule schema. Still worth it: alerting.yaml is
        # hand-written nested YAML, a mis-indented rule is the failure that
        # happens, and observability-grafana's reconcile entry watches it with NO
        # `check` — a malformed edit crash-loops Grafana and stalls the deploy on
        # a sticky reconcile_failed. yq exits 1 on unparseable YAML, 0 on valid.
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

    # Rehearsal variant: the host config with the identity guards ON — no tunnel
    # connector (a second one would take real production traffic), no production
    # tailnet identity, no backups into the prod restic prefix — and public SSH
    # open (scratch boxes have no cloud firewall; without it, unreachable).
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
          # No live alerting/probing from a scratch box.
          llunde.observability.stack.enable = false;
          services.openssh.openFirewall = lib.mkForce true;
        }
      ];
    };

    checks.x86_64-linux.mkquadlet-render = mkRenderCheck "x86_64-linux";
    checks.x86_64-linux.gitops-pull-script = gitopsPullScriptCheck "x86_64-linux";
    checks.x86_64-linux.obs-config = obsConfigCheck "x86_64-linux";
    checks.aarch64-linux.mkquadlet-render = mkRenderCheck "aarch64-linux";
    # The laptop is aarch64-darwin: without these, a local `nix flake check`
    # skips the goldens as "incompatible" and passes VACUOUSLY.
    checks.aarch64-darwin.mkquadlet-render = mkRenderCheck "aarch64-darwin";
    checks.aarch64-darwin.obs-config = obsConfigCheck "aarch64-darwin";
  };
}
