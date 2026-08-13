# llunde-frontend slice (ADR 009): the static-server container under the
# llunde-frontend user (uid 2002); importing this module IS enabling it. Serves
# container port 8080, published on host loopback 8081 — only Caddy makes it
# public.
{lib, ...}: let
  quadlet = import ../../modules/quadlet/mk-quadlet.nix {inherit lib;};
in {
  # Registering the user does two things: the switch-time reload hook reaches
  # uid 2002, and the auto-update timer's ConditionUser matches 2002 — without
  # which the `AutoUpdate = "registry"` label below is INERT and :latest pushes
  # never deploy. autoUpdate defaults true: pull-based deploy per ADR 009/010,
  # like backend + pyparser. Unlike edge, the frontend SHOULD ride :latest — a
  # static SPA server, not the TLS front door.
  llunde.quadlet.serviceUsers = [
    {
      name = "llunde-frontend";
      uid = 2002;
    }
  ];

  environment.etc = quadlet.mkContainerUnit {
    name = "llunde-frontend";
    uid = 2002;
    unit.Description = "llunde frontend (static server)";
    container = {
      Image = "ghcr.io/fredrir/llunde-frontend:latest";
      AutoUpdate = "registry";
      PublishPort = ["127.0.0.1:8081:8080"];
    };
    service = {
      # Pull auth for the private GHCR image: podman-process env, not container env.
      Environment = "REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json";
      Restart = "always";
      MemoryMax = "256M";
    };
    install.WantedBy = ["default.target"];
  };
}
