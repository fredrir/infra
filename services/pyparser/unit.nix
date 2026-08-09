# Pure fragments for the pyparser stack — consumed by BOTH the service module
# and the flake's golden render checks. Every value comes from
# docs/init/plans/phase-3.5/contract.md (compose → quadlet mapping table);
# changing one is a lead decision. Source being ported:
# llunde-pyparser's docker-compose.prod.yml.
#
# Secret path contract (host secrets.nix + the render oneshot in default.nix):
#   /run/pyparser/secrets.env     Doppler pyparser/prd render — compose's
#                                 env_file; POSTGRES_PASSWORD and TUNNEL_TOKEN
#                                 arrive ONLY this way (single-sourced).
#   /run/secrets/ghcr-auth.json   read-only GHCR PAT (pyparser-review is private).
{lib}: let
  quadlet = import ../../modules/quadlet/mk-quadlet.nix {inherit lib;};
  uid = 2001;

  # One app image for review, both workers AND migrate — the contract's
  # same-image invariant is what makes `alembic upgrade head` migrate to
  # exactly the schema the code expects.
  appImage = "ghcr.io/fredrir/pyparser-review:latest";

  # Every container consumes the Doppler render (compose's env_file did the
  # same for every service).
  secretsEnv = "/run/pyparser/secrets.env";

  # Auth for pulling the private GHCR image (podman-process env, not container env).
  registryAuth = "REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json";

  filesVolume = "pyparser-files:/app/.local/files";

  # Cold starts pull a multi-GB ML image; the user manager's 90s default
  # start timeout would kill the pull (llunde-backend precedent).
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
      # NOT Internal (go-live fix 7): an Internal network's aardvark answers
      # all DNS without upstream, killing GHCR/Cloudflare lookups. Isolation
      # holds via the user boundary and zero published ports.
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
      # The Doppler-rendered PYPARSER_LLUNDE_DATABASE_URL says @postgres:5432 —
      # compose's service name, which prod compose keeps using until cutover.
      # Quadlet DNS resolves ContainerName, so alias the old name rather than
      # fork the Doppler value (rehearsal finding: alembic could not resolve
      # 'postgres' and the whole app chain failed its dependency start).
      NetworkAlias = "postgres";
      Environment = [
        "POSTGRES_USER=pyparser"
        "POSTGRES_DB=pyparser_llunde"
        # POSTGRES_PASSWORD comes from secrets.env (contract: single-sourced).
      ];
      EnvironmentFile = [secretsEnv];
      HealthCmd = "pg_isready -U pyparser -d pyparser_llunde";
      HealthInterval = "5s";
      HealthRetries = 10;
      HealthTimeout = "5s";
      # PINNED, deliberately NO AutoUpdate: unattended DB image swaps are a
      # data hazard (contract). Bumping the major is a lead decision.
      Image = "docker.io/library/postgres:17-alpine";
      Network = ["pyparser.network"];
      # sd_notify READY only once the healthcheck passes, so After= on this
      # unit means compose's `condition: service_healthy`, not just "spawned".
      Notify = "healthy";
      ShmSize = "256m";
      # Run as the image's postgres user from the start: the entrypoint's
      # PGDATA setup happens as that uid, so :U must chown the volume to
      # THAT uid, not container-root (go-live fix 5, modules/data pattern).
      User = "postgres";
      Volume = ["pyparser-pgdata:/var/lib/postgresql/data:U"];
    };
    service.Restart = "always";
    install.WantedBy = ["default.target"];
  };

  # Deploy-ordering mechanism (contract): this oneshot is PartOf= review, so
  # every stop/restart of pyparser-review — including the `systemctl --user
  # restart` podman auto-update issues on a new image — propagates a restart
  # to migrate IN THE SAME TRANSACTION. The app units' Requires=+After= on
  # this unit then do the rest: a oneshot's start job only completes when
  # Exec exits, so review (and any worker restarting alongside) starts
  # strictly after `alembic upgrade head` returned 0; if it fails, Requires=
  # keeps the apps down instead of serving against a half-migrated schema.
  # Known wrinkle: auto-update restarts units one at a time, so a worker
  # restarted before review may briefly run new code on the old schema until
  # review's restart re-runs migrate.
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
      # confuse `podman auto-update` (contract); it rides along via PartOf=.
      Image = appImage;
      Network = ["pyparser.network"];
    };
    service = {
      Environment = registryAuth;
      # Stays active after alembic exits so the apps' Requires= is satisfied
      # for the rest of the boot; PartOf= resets it on every app deploy.
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
      # The tunnel's remote-managed ingress targets http://review:8081 —
      # compose's service name (cutover finding, the postgres alias's twin:
      # Access's edge 302 masked it until the un-Access'd share origin 502'd).
      NetworkAlias = "review";
      Environment = [
        "PYPARSER_ENV=production"
        "PYPARSER_DOCLING_NUM_THREADS=1"
      ];
      EnvironmentFile = [secretsEnv];
      # compose `expose: 8081` — the port stays on the pod network only, no
      # PublishPort anywhere (contract): ingress is cloudflared dialing
      # pyparser-review:8081 over this network.
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
  # only (restart + reaper recover the job), cpu-capped so a burst never
  # starves review/cloudflared. TimeoutStopSec must exceed the drain timeout
  # so deploys end with a fenced PENDING row, not a SIGKILL orphan.
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
      # compose `healthcheck: disable` — the image's baked-in server
      # healthcheck is meaningless in worker mode; `none` switches it off.
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

  # Token-only connector; TUNNEL_TOKEN arrives via secrets.env. No AutoUpdate
  # (contract) and self-update disabled — connector bumps are deliberate.
  # Masked on rehearsal boxes (ops runbook, not code).
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
