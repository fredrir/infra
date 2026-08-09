# llunde-frontend service slice (ADR 009): the static-server container under
# the llunde-frontend user (uid 2002). Importing this module IS enabling it.
# Serves on container port 8080; published on host loopback 8081 per the
# phase-2 contract — only Caddy (edge) makes it public.
{ lib, ... }:
let
  quadlet = import ../../modules/quadlet/mk-quadlet.nix { inherit lib; };
in
{
  environment.etc = quadlet.mkContainerUnit {
    name = "llunde-frontend";
    uid = 2002;
    unit.Description = "llunde frontend (static server)";
    container = {
      Image = "ghcr.io/fredrir/llunde-frontend:latest";
      AutoUpdate = "registry";
      PublishPort = [ "127.0.0.1:8081:8080" ];
    };
    service = {
      # Auth for pulling the private GHCR image (podman-process env, not container env).
      Environment = "REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json";
      Restart = "always";
      MemoryMax = "256M";
    };
    install.WantedBy = [ "default.target" ];
  };
}
