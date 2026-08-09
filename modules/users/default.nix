# Per-service rootless podman users (ADR 005). Option surface only in phase 1;
# phase 2 implements: user + subuid/subgid ranges, linger, quadlet dir wiring.
{ lib, ... }:
{
  options.llunde.users.services = lib.mkOption {
    type = lib.types.attrsOf (
      lib.types.submodule {
        options = {
          uid = lib.mkOption {
            type = lib.types.int;
            description = "Fixed uid — also the /etc/containers/systemd/users/<uid>/ key.";
          };
          linger = lib.mkOption {
            type = lib.types.bool;
            default = true;
            description = "Start the user's systemd instance at boot (rootless quadlets).";
          };
        };
      }
    );
    default = { };
    description = ''
      Rootless service users (llunde-backend, llunde-frontend, edge). Cross-user
      traffic is loopback-only: rootless podman networks cannot span users.
    '';
  };
}
