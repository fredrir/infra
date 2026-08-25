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
