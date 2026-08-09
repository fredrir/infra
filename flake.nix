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

  outputs =
    {
      self,
      nixpkgs,
      disko,
      sops-nix,
    }:
    let
      lib = nixpkgs.lib;
      quadlet = import ./modules/quadlet/mk-quadlet.nix { inherit lib; };

      # Golden render proof for mkQuadlet (ADR 004): asserted at EVAL time, so
      # `nix flake check` fails on rendering drift for every system without
      # needing to build anything beyond a writeText.
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
          install.WantedBy = [ "default.target" ];
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
          install.WantedBy = [ "default.target" ];
        })."containers/systemd/users/991/dummy.network".text;

      goldenNetwork = ''
        [Network]
        Internal=true
        Subnet=10.89.0.0/24

        [Install]
        WantedBy=default.target
      '';

      # Real-unit golden (phase 2): the backend container as deployed, from the
      # same pure fragment the service module consumes.
      renderedBackend = (import ./services/llunde-backend/unit.nix { inherit lib; }).text;

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
        EnvironmentFile=/run/secrets/doppler.env
        EnvironmentFile=/run/secrets/llunde-backend-db.env
        Image=ghcr.io/fredrir/llunde-backend:latest
        Network=llunde-backend-data.network
        Network=podman
        PublishPort=127.0.0.1:8080:8080

        [Service]
        MemoryMax=1G
        Restart=always
        TimeoutStartSec=300

        [Install]
        WantedBy=default.target
      '';

      mkRenderCheck =
        system:
        assert lib.assertMsg (renderedContainer == goldenContainer)
          "mkContainerUnit rendering drifted from golden:\n---rendered---\n${renderedContainer}\n---golden---\n${goldenContainer}";
        assert lib.assertMsg (renderedNetwork == goldenNetwork)
          "mkNetworkUnit rendering drifted from golden:\n---rendered---\n${renderedNetwork}\n---golden---\n${goldenNetwork}";
        assert lib.assertMsg (renderedBackend == goldenBackend)
          "backend unit rendering drifted from golden:\n---rendered---\n${renderedBackend}\n---golden---\n${goldenBackend}";
        nixpkgs.legacyPackages.${system}.writeText "mkquadlet-render-ok" renderedContainer;
    in
    {
      nixosConfigurations.llunde-01 = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          disko.nixosModules.disko
          sops-nix.nixosModules.sops
          ./hosts/llunde-01
        ];
      };

      checks.x86_64-linux.mkquadlet-render = mkRenderCheck "x86_64-linux";
      checks.aarch64-linux.mkquadlet-render = mkRenderCheck "aarch64-linux";
    };
}
