# Pure fragment for the backend container unit — consumed by BOTH the service
# module and the flake's golden render check. Every value comes from
# docs/init/plans/phase-2/contract.md; changing one is a lead decision.
#
# Secret path contract with modules/secrets (stream A2):
#   /run/secrets/doppler.env            DOPPLER_TOKEN=...   (owner llunde-backend)
#   /run/secrets/llunde-backend-db.env  POSTGRES_PASSWORD=... DB_PASSWORD=...
{ lib }:
let
  quadlet = import ../../modules/quadlet/mk-quadlet.nix { inherit lib; };
  uid = 2001;
in
rec {
  args = {
    name = "llunde-backend";
    inherit uid;
    unit = {
      After = [
        "llunde-postgres.service"
        "llunde-valkey.service"
      ];
      Requires = [
        "llunde-postgres.service"
        "llunde-valkey.service"
      ];
    };
    container = {
      AutoUpdate = "registry";
      ContainerName = "llunde-backend";
      Environment = [
        "APP_ENV=prod"
        "DB_HOST=llunde-postgres"
        "DB_PORT=5432"
        "DB_NAME=llunde"
        "DB_USER=llunde"
        "VALKEY_HOST=llunde-valkey"
        "VALKEY_PORT=6379"
        "CORS_ALLOWED_ORIGINS=https://llunde.no"
        "JAVA_OPTS=-Xmx640m"
      ];
      EnvironmentFile = [
        "/run/secrets/doppler.env"
        "/run/secrets/llunde-backend-db.env"
      ];
      Image = "ghcr.io/fredrir/llunde-backend:latest";
      # Data network for postgres/valkey DNS + default network for egress
      # (Doppler, GHCR). Repeated Network= is Quadlet's multi-network syntax.
      Network = [
        "llunde-backend-data.network"
        "podman"
      ];
      PublishPort = [ "127.0.0.1:8080:8080" ];
    };
    service = {
      # Auth for pulling the private GHCR image (podman-process env, not container env).
      Environment = "REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json";
      MemoryMax = "1G";
      Restart = "always";
      TimeoutStartSec = 300;
    };
    install.WantedBy = [ "default.target" ];
  };

  fragment = quadlet.mkContainerUnit args;

  text = fragment."containers/systemd/users/${toString uid}/llunde-backend.container".text;
}
