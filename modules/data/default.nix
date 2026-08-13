# Postgres/Valkey quadlets inside their owning service user (ADR 005: private
# per-user network, nothing published — the API port is the backend unit's).
# Volumes are host bind mounts under the owner's home, not named volumes, so
# restic (ADR 011) sees plain paths; `:U` (where used) chowns the tree into the
# container user's subuid mapping — both images run non-root in-container.
{
  config,
  lib,
  ...
}: let
  cfg = config.llunde.data;
  quadlet = import ../quadlet/mk-quadlet.nix {inherit lib;};

  # Fixed owner: postgres/valkey belong to llunde-backend.
  owner = "llunde-backend";
  uid = 2001;
  dataRoot = "/home/${owner}/data";

  networkUnit = quadlet.mkNetworkUnit {
    name = "llunde-backend-data";
    inherit uid;
    # NOT Internal — an Internal network's aardvark answers all DNS without
    # upstream, killing api.doppler.com and GHCR lookups for the backend that
    # shares it. Isolation: the user boundary and zero published ports.
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
      # The entrypoint's PGDATA mkdir runs as uid 999, so :U must chown the bind
      # mount to THAT uid, not container-root.
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
      # noeviction: sessions fail loudly rather than evict silently (ADR 005).
      # save "": no RDB. Its temp file lands in /data's ROOT, owned by the
      # service user rather than the container's mapped uid, so BGSAVE fails
      # "Permission denied" and — default stop-writes-on-bgsave-error — takes
      # ALL writes down (symptom: auth writes 500 with MISCONF). AOF is the
      # durability path instead, writing into appendonlydir/, which the
      # container DOES own; sessions are TTL'd ephemeral data anyway.
      Exec = ''valkey-server --appendonly yes --save "" --maxmemory ${cfg.valkey.maxMemory} --maxmemory-policy noeviction'';
      # Digest-pinned valkey 8.1.9: a data store must not ride a floating tag —
      # bumps are deliberate edits.
      Image = "docker.io/valkey/valkey@sha256:495e4fecdc98ee48a20b207726caa5ab6451e0fac3642a9be10d9e70b3068df6";
      Network = ["llunde-backend-data.network"];
      # No :U — it chowns the mount to the container's mapped root (host uid
      # ${toString uid}) every start, and the image has no USER, so /data ROOT
      # never returns to the valkey user; only appendonlydir/, which the server
      # creates. AOF *rewrite* then cannot create temp-rewriteaof-*.aof (bare
      # relative path in the ROOT) -> aof_last_bgrewrite_status:err and the INCR
      # AOF never compacts. The entrypoint chowns /data to valkey on start
      # (find . ! -user valkey -exec chown); the create-only tmpfiles rule below
      # keeps systemd-tmpfiles-resetup from undoing that. postgres is immune —
      # it writes under the pgdata/ SUBDIR, not the mount root.
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
      # Create-only (':' prefixes): mode/owner apply at creation, never after. A
      # plain `d ... 0700 owner` re-chowns the LIVE inode to the service user on
      # every systemd-tmpfiles-resetup, i.e. after each switch — which is what
      # stole /data ROOT from the valkey user mid-run. Runtime ownership is the
      # entrypoint's.
      ++ lib.optional cfg.valkey.enable "d ${dataRoot}/valkey :0700 :${owner} :${owner} -";
  };
}
