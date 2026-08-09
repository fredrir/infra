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
          subUidStart = lib.mkOption {
            type = lib.types.nullOr lib.types.int;
            default = null;
            description = ''
              Explicit subuid/subgid range start. Set on multi-rootless-user
              hosts: NixOS auto-allocation begins at 100000 and collides with
              any explicitly-ranged neighbor (phase-3.5 contract).
            '';
          };
          subUidCount = lib.mkOption {
            type = lib.types.int;
            default = 65536;
            description = "Explicit subuid/subgid range size (with subUidStart).";
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
    users.users = lib.mapAttrs (
      name: svc:
      {
        uid = svc.uid;
        isNormalUser = true;
        group = name;
        linger = svc.linger;
      }
      // (
        if svc.subUidStart != null then
          {
            autoSubUidGidRange = false;
            subUidRanges = [
              {
                startUid = svc.subUidStart;
                count = svc.subUidCount;
              }
            ];
            subGidRanges = [
              {
                startGid = svc.subUidStart;
                count = svc.subUidCount;
              }
            ];
          }
        else
          { autoSubUidGidRange = true; }
      )
    ) cfg;

    users.groups = lib.mapAttrs (_name: svc: {
      gid = svc.uid;
    }) cfg;
  };
}
