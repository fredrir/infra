{lib}: let
  quadlet = import ../../modules/quadlet/mk-quadlet.nix {inherit lib;};
  uid = 2001;
in rec {
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
        "HOME=/tmp"
      ];
      EnvironmentFile = [
        "/run/secrets/doppler.env"
        "/run/secrets/llunde-backend-db.env"
      ];
      Image = "ghcr.io/fredrir/llunde-backend:latest";
      Network = [
        "llunde-backend-data.network"
        "podman"
      ];
      PublishPort = ["127.0.0.1:8080:8080"];
    };
    service = {
      Environment = "REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json";
      MemoryMax = "1G";
      Restart = "always";
      TimeoutStartSec = 300;
    };
    install.WantedBy = ["default.target"];
  };

  fragment = quadlet.mkContainerUnit args;

  text = fragment."containers/systemd/users/${toString uid}/llunde-backend.container".text;
}
