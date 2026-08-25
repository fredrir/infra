# Per-service rootless podman users:
{
  config,
  lib,
  ...
}: let
  cfg = config.llunde.users.services;
in {
  options.llunde.users.services = lib.mkOption {
    type = lib.types.attrsOf (
      lib.types.submodule {
        options = {
          uid = lib.mkOption {
            type = lib.types.int;
          };
          linger = lib.mkOption {
            type = lib.types.bool;
            default = true;
          };
          subUidStart = lib.mkOption {
            type = lib.types.nullOr lib.types.int;
            default = null;
          };
          subUidCount = lib.mkOption {
            type = lib.types.int;
            default = 65536;
          };
        };
      }
    );
    default = {};
  };

  config = {
    users.users =
      lib.mapAttrs (
        name: svc:
          {
            uid = svc.uid;
            isNormalUser = true;
            group = name;
            linger = svc.linger;
          }
          // (
            if svc.subUidStart != null
            then {
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
            else {autoSubUidGidRange = true;}
          )
      )
      cfg;

    users.groups =
      lib.mapAttrs (_name: svc: {
        gid = svc.uid;
      })
      cfg;
  };
}
