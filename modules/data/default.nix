{
  config,
  lib,
  ...
}: let
  cfg = config.llunde.data;
  quadlet = import ../quadlet/mk-quadlet.nix {inherit lib;};

  owner = "llunde-backend";
  uid = 2001;
  dataRoot = "/home/${owner}/data";

  networkUnit = quadlet.mkNetworkUnit {
    name = "llunde-backend-data";
    inherit uid;
    network = {};
    install.WantedBy = ["default.target"];
  };

  postgresUnit = quadlet.mkContainerUnit {
    name = "llunde-postgres";
    inherit uid;
    container = {
      ContainerName = "llunde-postgres";
      Environment = [
        "POSTGRES_DB=llunde"
        "POSTGRES_USER=llunde"
        "PGDATA=/var/lib/postgresql/data/pgdata"
      ];
      EnvironmentFile = ["/run/secrets/llunde-backend-db.env"];
      Exec = "postgres -c shared_buffers=${cfg.postgres.sharedBuffers}";
      Image = "docker.io/library/postgres:17";
      Network = ["llunde-backend-data.network"];
      User = "postgres";
      Volume = ["${dataRoot}/postgres:/var/lib/postgresql/data:U"];
    };
    service = {
      MemoryMax = "768M";
      Restart = "always";
    };
    install.WantedBy = ["default.target"];
  };

  valkeyUnit = quadlet.mkContainerUnit {
    name = "llunde-valkey";
    inherit uid;
    container = {
      ContainerName = "llunde-valkey";
      Exec = ''valkey-server --appendonly yes --save "" --maxmemory ${cfg.valkey.maxMemory} --maxmemory-policy noeviction'';
      Image = "docker.io/valkey/valkey@sha256:495e4fecdc98ee48a20b207726caa5ab6451e0fac3642a9be10d9e70b3068df6";
      Network = ["llunde-backend-data.network"];
      Volume = ["${dataRoot}/valkey:/data"];
    };
    service = {
      MemoryMax = "384M";
      Restart = "always";
    };
    install.WantedBy = ["default.target"];
  };
in {
  options.llunde.data = {
    postgres = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Postgres quadlet inside the owning service user.";
      };
      sharedBuffers = lib.mkOption {
        type = lib.types.str;
        default = "256MB";
        description = "Declared cap — the 4 GB box stays honest (ADR 005).";
      };
    };
    valkey = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Valkey quadlet (appendonly yes) inside the owning service user.";
      };
      maxMemory = lib.mkOption {
        type = lib.types.str;
        default = "256mb";
        description = "Declared cap for valkey maxmemory.";
      };
    };
  };

  config = lib.mkIf (cfg.postgres.enable || cfg.valkey.enable) {
    llunde.quadlet.units =
      [networkUnit]
      ++ lib.optional cfg.postgres.enable postgresUnit
      ++ lib.optional cfg.valkey.enable valkeyUnit;

    systemd.tmpfiles.rules =
      ["d ${dataRoot} 0750 ${owner} ${owner} -"]
      ++ lib.optional cfg.postgres.enable "d ${dataRoot}/postgres 0700 ${owner} ${owner} -"
      ++ lib.optional cfg.valkey.enable "d ${dataRoot}/valkey :0700 :${owner} :${owner} -";
  };
}
