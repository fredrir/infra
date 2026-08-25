{lib}: let
  quadlet = import ../../modules/quadlet/mk-quadlet.nix {inherit lib;};
  uid = 2001;

  appImage = "ghcr.io/fredrir/pyparser-review:latest";

  secretsEnv = "/run/pyparser/secrets.env";

  registryAuth = "REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json";

  filesVolume = "pyparser-files:/app/.local/files";

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
      NetworkAlias = "postgres";
      Environment = [
        "POSTGRES_USER=pyparser"
        "POSTGRES_DB=pyparser_llunde"
      ];
      EnvironmentFile = [secretsEnv];
      HealthCmd = "pg_isready -U pyparser -d pyparser_llunde";
      HealthInterval = "5s";
      HealthRetries = 10;
      HealthTimeout = "5s";
      Image = "docker.io/library/postgres:17-alpine";
      Network = ["pyparser.network"];
      Notify = "healthy";
      ShmSize = "256m";
      User = "postgres";
      Volume = ["pyparser-pgdata:/var/lib/postgresql/data:U"];
    };
    service.Restart = "always";
    install.WantedBy = ["default.target"];
  };

  # Deploy ordering
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
      Image = appImage;
      Network = ["pyparser.network"];
    };
    service = {
      Environment = registryAuth;
      RemainAfterExit = true;
      # Pull + migration
      TimeoutStartSec = 600;
      Type = "oneshot";
    };
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
      NetworkAlias = "review";
      Environment = [
        "PYPARSER_ENV=production"
        "PYPARSER_DOCLING_NUM_THREADS=1"
      ];
      EnvironmentFile = [secretsEnv];
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

  # EXTRACT lane:
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

  # Token-only connector:
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
