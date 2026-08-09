# Data stores (Postgres, Valkey) as quadlets inside their owning service user
# (ADR 005: private per-user network, nothing published — the API port is the
# backend unit's business). Values from docs/init/plans/phase-2/contract.md.
#
# Volumes are host bind mounts under the owner's home (not named volumes) so
# restic (ADR 011) backs up plain paths; the `U` option makes podman chown the
# tree into the container user's subuid mapping (postgres/valkey run as an
# in-container non-root uid).
{
  config,
  lib,
  ...
}:
let
  cfg = config.llunde.data;
  quadlet = import ../quadlet/mk-quadlet.nix { inherit lib; };

  # Fixed owner per contract: postgres/valkey belong to llunde-backend.
  owner = "llunde-backend";
  uid = 2001;
  dataRoot = "/home/${owner}/data";

  networkUnit = quadlet.mkNetworkUnit {
    name = "llunde-backend-data";
    inherit uid;
    # Internal=true: no external routing; aardvark DNS still resolves container
    # names on the network (verify at go-live — documented in the runbook).
    network.Internal = true;
    install.WantedBy = [ "default.target" ];
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
      EnvironmentFile = [ "/run/secrets/llunde-backend-db.env" ];
      Exec = "postgres -c shared_buffers=${cfg.postgres.sharedBuffers}";
      Image = "docker.io/library/postgres:17";
      Network = [ "llunde-backend-data.network" ];
      Volume = [ "${dataRoot}/postgres:/var/lib/postgresql/data:U" ];
    };
    service = {
      MemoryMax = "768M";
      Restart = "always";
    };
    install.WantedBy = [ "default.target" ];
  };

  valkeyUnit = quadlet.mkContainerUnit {
    name = "llunde-valkey";
    inherit uid;
    container = {
      ContainerName = "llunde-valkey";
      # noeviction: sessions must fail loudly rather than silently evict (ADR 005).
      Exec = "valkey-server --appendonly yes --maxmemory ${cfg.valkey.maxMemory} --maxmemory-policy noeviction";
      Image = "docker.io/valkey/valkey:8";
      Network = [ "llunde-backend-data.network" ];
      Volume = [ "${dataRoot}/valkey:/data:U" ];
    };
    service = {
      MemoryMax = "384M";
      Restart = "always";
    };
    install.WantedBy = [ "default.target" ];
  };
in
{
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
      [ networkUnit ]
      ++ lib.optional cfg.postgres.enable postgresUnit
      ++ lib.optional cfg.valkey.enable valkeyUnit;

    systemd.tmpfiles.rules =
      [ "d ${dataRoot} 0750 ${owner} ${owner} -" ]
      ++ lib.optional cfg.postgres.enable "d ${dataRoot}/postgres 0700 ${owner} ${owner} -"
      ++ lib.optional cfg.valkey.enable "d ${dataRoot}/valkey 0700 ${owner} ${owner} -";
  };
}
