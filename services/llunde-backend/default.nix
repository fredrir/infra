# llunde-backend service slice (ADR 002/005): the backend container plus its
# data stores under the llunde-backend user (uid 2001), API on 127.0.0.1:8080.
{lib, ...}: let
  unitDef = import ./unit.nix {inherit lib;};
in {
  llunde.data.postgres.enable = true;
  llunde.data.valkey = {
    enable = true;
    maxMemory = "192mb";
  };

  llunde.quadlet = {
    units = [unitDef.fragment];
    serviceUsers = [
      {
        name = "llunde-backend";
        uid = 2001;
      }
    ];
  };
}
