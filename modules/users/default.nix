# Per-service rootless podman users (ADR 005): fixed uid, own group (gid = uid),
# lingering user manager so quadlets start at boot, auto subuid/subgid ranges for
# rootless podman. Cross-user traffic is loopback-only — rootless podman networks
# cannot span users.
{ config, lib, ... }:
let
  cfg = config.llunde.users.services;
in
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

  config = {
    users.users = lib.mapAttrs (name: svc: {
      uid = svc.uid;
      isNormalUser = true;
      group = name;
      linger = svc.linger;
      autoSubUidGidRange = true;
    }) cfg;

    users.groups = lib.mapAttrs (_name: svc: {
      gid = svc.uid;
    }) cfg;
  };
}
