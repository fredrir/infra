# Data stores (Postgres, Valkey) as quadlets inside their owning service user
# (ADR 005: private per-user network, nothing loopback-published except APIs).
# Option surface only in phase 1; phase 2 implements the units + caps.
{ lib, ... }:
{
  options.llunde.data = {
    postgres = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Postgres quadlet inside the owning service user.";
      };
      sharedBuffers = lib.mkOption {
        type = lib.types.str;
        default = "256MB";
        description = "Declared cap — the 4 GB box stays honest (ADR 005).";
      };
    };
    valkey = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Valkey quadlet (appendonly yes) inside the owning service user.";
      };
      maxMemory = lib.mkOption {
        type = lib.types.str;
        default = "256mb";
        description = "Declared cap for valkey maxmemory.";
      };
    };
  };
}
