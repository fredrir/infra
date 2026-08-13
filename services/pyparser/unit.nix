# Pure fragments for the pyparser stack — used by the service module and the
# flake goldens; the values map llunde-pyparser's docker-compose.prod.yml onto
# quadlets, so changing one is a lead decision. Secret paths (host secrets.nix
# plus the render oneshot in default.nix):
#   /run/pyparser/secrets.env     Doppler pyparser/prd render — compose's
#                                 env_file; POSTGRES_PASSWORD and TUNNEL_TOKEN
#                                 arrive ONLY here (single-sourced).
#   /run/secrets/ghcr-auth.json   read-only GHCR PAT (pyparser-review is private).
{lib}: let
  quadlet = import ../../modules/quadlet/mk-quadlet.nix {inherit lib;};
  uid = 2001;

  # One image for review, both workers AND migrate: that same-image invariant
  # makes `alembic upgrade head` produce the schema the code expects.
  appImage = "ghcr.io/fredrir/pyparser-review:latest";

  # Every container consumes the Doppler render (compose's env_file).
  secretsEnv = "/run/pyparser/secrets.env";

  # Pull auth for the private GHCR image: podman-process env, not container env.
  registryAuth = "REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json";

  filesVolume = "pyparser-files:/app/.local/files";

  # Cold starts pull a multi-GB ML image; the 90s default timeout would kill it.
  pullTimeout = 300;

  mkContainer = args: rec {
    inherit args;
    fragment = quadlet.mkContainerUnit args;
    text = fragment."containers/systemd/users/${toString uid}/${args.name}.container".text;
  };
in rec {
  network = rec {
    args = {
      name = "pyparser";
      inherit uid;
      # NOT Internal: Internal aardvark answers all DNS without upstream,
      # killing GHCR/Cloudflare lookups. Isolation holds via the user boundary
      # and zero published ports.
      network = {};
      install.WantedBy = ["default.target"];
    };
    fragment = quadlet.mkNetworkUnit args;
    text = fragment."containers/systemd/users/${toString uid}/pyparser.network".text;
  };

  postgres = mkContainer {
    name = "pyparser-postgres";
    inherit uid;
    container = {
      ContainerName = "pyparser-postgres";
      # PYPARSER_LLUNDE_DATABASE_URL (Doppler) says @postgres:5432, compose's
      # service name; quadlet DNS resolves ContainerName, so alias it rather
      # than fork Doppler — else alembic cannot resolve 'postgres' and no app
      # starts.
      NetworkAlias = "postgres";
      Environment = [
        "POSTGRES_USER=pyparser"
        "POSTGRES_DB=pyparser_llunde"
        # POSTGRES_PASSWORD comes from secrets.env (single-sourced).
      ];
      EnvironmentFile = [secretsEnv];
      HealthCmd = "pg_isready -U pyparser -d pyparser_llunde";
      HealthInterval = "5s";
      HealthRetries = 10;
      HealthTimeout = "5s";
      # PINNED, deliberately NO AutoUpdate: unattended DB image swaps are a data
      # hazard. Bumping the major is a lead decision.
      Image = "docker.io/library/postgres:17-alpine";
      Network = ["pyparser.network"];
      # sd_notify READY only once the healthcheck passes, so After= here means
      # compose's `condition: service_healthy`, not just "spawned".
      Notify = "healthy";
      ShmSize = "256m";
      # Run as the image's postgres user from the start: the entrypoint's PGDATA
      # setup happens as that uid, so :U must chown the volume to THAT uid, not
      # container-root (modules/data).
      User = "postgres";
      Volume = ["pyparser-pgdata:/var/lib/postgresql/data:U"];
    };
    service.Restart = "always";
    install.WantedBy = ["default.target"];
  };

  # Deploy ordering: this oneshot is PartOf= review, so every stop/restart of
  # pyparser-review — including the `systemctl --user restart` podman
  # auto-update issues on a new image — restarts migrate IN THE SAME
  # TRANSACTION. A oneshot's start job completes only when Exec exits, so the
  # apps' Requires=+After= start them strictly after `alembic upgrade head`
  # returned 0, and on failure keep them down rather than serving a half-migrated
  # schema. Wrinkle: auto-update restarts one unit at a time, so a worker may
  # briefly run new code on the old schema until review's restart re-runs migrate.
  migrate = mkContainer {
    name = "pyparser-migrate";
    inherit uid;
    unit = {
      After = ["pyparser-postgres.service"];
      PartOf = ["pyparser-review.service"];
      Requires = ["pyparser-postgres.service"];
    };
    container = {
      ContainerName = "pyparser-migrate";
      EnvironmentFile = [secretsEnv];
      Exec = "alembic upgrade head";
      # Same image as review; NO AutoUpdate label — oneshot --rm containers
      # confuse `podman auto-update`; it rides along via PartOf=.
      Image = appImage;
      Network = ["pyparser.network"];
    };
    service = {
      Environment = registryAuth;
      # Stays active after alembic exits so the apps' Requires= holds for the
      # rest of the boot; PartOf= resets it on every app deploy.
      RemainAfterExit = true;
      # Pull + migration both live inside this start job.
      TimeoutStartSec = 600;
      Type = "oneshot";
    };
    # No [Install]: only ever pulled in by the app units' Requires=.
  };

  review = mkContainer {
    name = "pyparser-review";
    inherit uid;
    unit = {
      After = ["pyparser-migrate.service"];
      Requires = ["pyparser-migrate.service"];
    };
    container = {
      AutoUpdate = "registry";
      ContainerName = "pyparser-review";
      # The tunnel's remote-managed ingress targets http://review:8081,
      # compose's service name — the postgres alias's twin.
      NetworkAlias = "review";
      Environment = [
        "PYPARSER_ENV=production"
        "PYPARSER_DOCLING_NUM_THREADS=1"
      ];
      EnvironmentFile = [secretsEnv];
      # compose `expose: 8081` — the port stays on the network, no PublishPort
      # anywhere: ingress is cloudflared dialing pyparser-review:8081 here.
      HealthCmd = "curl -fsS http://localhost:8081/healthz";
      Image = appImage;
      Network = ["pyparser.network"];
      Volume = [filesVolume];
    };
    service = {
      Environment = registryAuth;
      Restart = "always";
      TimeoutStartSec = pullTimeout;
    };
    install.WantedBy = ["default.target"];
  };

  # EXTRACT lane: memory-capped so a pathological PDF OOM-kills THIS container
  # only (restart + reaper recover the job), cpu-capped so a burst never starves
  # review/cloudflared. TimeoutStopSec must exceed the drain timeout, or deploys
  # leave a SIGKILL orphan instead of a fenced PENDING row.
  workerExtract = mkContainer {
    name = "pyparser-worker-extract";
    inherit uid;
    unit = {
      After = ["pyparser-migrate.service"];
      Requires = ["pyparser-migrate.service"];
    };
    container = {
      AutoUpdate = "registry";
      ContainerName = "pyparser-worker-extract";
      Environment = [
        "PYPARSER_ENV=production"
        "PYPARSER_WORKER_STAGES=EXTRACT"
        "PYPARSER_WORKER_CONCURRENCY=1"
        "PYPARSER_WORKER_DRAIN_TIMEOUT_S=60"
        "PYPARSER_DOCLING_NUM_THREADS=3"
        "PYPARSER_VLM_MODELS="
        "PYPARSER_LLM_MODELS="
        "PYPARSER_DOCTR_KEEP_RESIDENT=true"
      ];
      EnvironmentFile = [secretsEnv];
      Exec = "pyparser-worker";
      # compose `healthcheck: disable` — the image's baked-in server healthcheck
      # is meaningless in worker mode; `none` switches it off.
      HealthCmd = "none";
      Image = appImage;
      Network = ["pyparser.network"];
      Volume = [filesVolume];
    };
    service = {
      CPUQuota = "300%";
      Environment = registryAuth;
      MemoryMax = "6G";
      Restart = "always";
      TimeoutStartSec = pullTimeout;
      TimeoutStopSec = 90;
    };
    install.WantedBy = ["default.target"];
  };

  workerLight = mkContainer {
    name = "pyparser-worker-light";
    inherit uid;
    unit = {
      After = ["pyparser-migrate.service"];
      Requires = ["pyparser-migrate.service"];
    };
    container = {
      AutoUpdate = "registry";
      ContainerName = "pyparser-worker-light";
      Environment = [
        "PYPARSER_ENV=production"
        "PYPARSER_WORKER_STAGES=CLASSIFY,BATCH,SPLIT,STRUCTURE_TASK"
        "PYPARSER_REAPER_ENABLED=true"
        "PYPARSER_VLM_MODELS="
        "PYPARSER_LLM_MODELS="
      ];
      EnvironmentFile = [secretsEnv];
      Exec = "pyparser-worker";
      HealthCmd = "none";
      Image = appImage;
      Network = ["pyparser.network"];
      Volume = [filesVolume];
    };
    service = {
      Environment = registryAuth;
      MemoryMax = "2G";
      Restart = "always";
      TimeoutStartSec = pullTimeout;
      TimeoutStopSec = 45;
    };
    install.WantedBy = ["default.target"];
  };

  # Token-only connector; TUNNEL_TOKEN arrives via secrets.env. No AutoUpdate,
  # self-update disabled — connector bumps are deliberate. Masked on rehearsal
  # boxes (ops runbook, not code).
  cloudflared = mkContainer {
    name = "pyparser-cloudflared";
    inherit uid;
    unit.After = ["pyparser-review.service"];
    container = {
      ContainerName = "pyparser-cloudflared";
      EnvironmentFile = [secretsEnv];
      Exec = "tunnel --no-autoupdate run";
      Image = "docker.io/cloudflare/cloudflared:latest";
      Network = ["pyparser.network"];
    };
    service.Restart = "always";
    install.WantedBy = ["default.target"];
  };

  fragments = map (u: u.fragment) [
    network
    postgres
    migrate
    review
    workerExtract
    workerLight
    cloudflared
  ];
}
