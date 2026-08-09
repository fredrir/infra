# Tenant slots (ADR 016): workloads that deploy THEMSELVES. llunde-infra
# declares only the slot — user, uid, explicit subuid range, linger, and the
# host binaries the tenant documents needing. Everything inside $HOME
# (quadlets in ~/.config/containers/systemd/, env files, deploy scripts,
# tunnel tokens) is the tenant's own, placed by the tenant's own pipeline.
# This module never references tenant internals.
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
            description = "Fixed uid (contract: tenants use the 3000+ block).";
          };
          subUidStart = lib.mkOption {
            type = lib.types.int;
            description = "Explicit subuid/subgid range start (never auto — collision hazard).";
          };
          subUidCount = lib.mkOption {
            type = lib.types.int;
            default = 65536;
            description = "Subuid/subgid range size.";
          };
          packages = lib.mkOption {
            type = lib.types.listOf lib.types.package;
            default = [];
            description = "Host binaries the tenant's documented tooling needs (on its PATH).";
          };
          sshAccess = lib.mkOption {
            type = lib.types.bool;
            default = true;
            description = "Tenant may log in over SSH (tailnet-only like everything else); its own authorized_keys governs.";
          };
          homeDirectories = lib.mkOption {
            type = lib.types.listOf lib.types.str;
            default = [];
            description = ''
              $HOME-relative directories pre-created (0700, tenant-owned) —
              the declarative twin of what the tenant's imperative bootstrap
              script would mkdir. Content stays tenant-owned.
            '';
          };
        };
      }
    );
    default = {};
    description = "Self-deploying tenant slots (ADR 016). portfolio is the first.";
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

    # Tenant scripts are FHS-shaped (#!/bin/bash — including sshd forced
    # commands, which exec the script directly, so "run it via bash" is not
    # an option). envfs resolves /bin and /usr/bin shebangs from PATH
    # (cutover finding).
    services.envfs.enable = true;

    systemd.tmpfiles.rules = lib.concatLists (
      lib.mapAttrsToList (
        name: t: map (d: "d /home/${name}/${d} 0700 ${name} ${name} -") t.homeDirectories
      )
      cfg
    );
  };
}
