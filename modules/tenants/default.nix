# Workloads that deploy themselves (TODO: fold these projects into to this repo)
{
  config,
  lib,
  ...
}: let
  cfg = config.llunde.tenants;
in {
  options.llunde.tenants = lib.mkOption {
    type = lib.types.attrsOf (
      lib.types.submodule {
        options = {
          uid = lib.mkOption {
            type = lib.types.int;
          };
          subUidStart = lib.mkOption {
            type = lib.types.int;
          };
          subUidCount = lib.mkOption {
            type = lib.types.int;
            default = 65536;
          };
          packages = lib.mkOption {
            type = lib.types.listOf lib.types.package;
            default = [];
          };
          sshAccess = lib.mkOption {
            type = lib.types.bool;
            default = true;
          };
          homeDirectories = lib.mkOption {
            type = lib.types.listOf lib.types.str;
            default = [];
          };
        };
      }
    );
    default = {};
  };

  config = lib.mkIf (cfg != {}) {
    users.users =
      lib.mapAttrs (name: t: {
        uid = t.uid;
        isNormalUser = true;
        group = name;
        linger = true;
        autoSubUidGidRange = false;
        subUidRanges = [
          {
            startUid = t.subUidStart;
            count = t.subUidCount;
          }
        ];
        subGidRanges = [
          {
            startGid = t.subUidStart;
            count = t.subUidCount;
          }
        ];
        packages = t.packages;
      })
      cfg;

    users.groups =
      lib.mapAttrs (_name: t: {
        gid = t.uid;
      })
      cfg;

    services.envfs.enable = true;

    systemd.tmpfiles.rules = lib.concatLists (
      lib.mapAttrsToList (
        name: t: map (d: "d /home/${name}/${d} 0700 ${name} ${name} -") t.homeDirectories
      )
      cfg
    );
  };
}
