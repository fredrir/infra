{lib, ...}: let
  quadlet = import ../../modules/quadlet/mk-quadlet.nix {inherit lib;};
in {
  llunde.quadlet.serviceUsers = [
    {
      name = "llunde-frontend";
      uid = 2002;
    }
  ];

  environment.etc = quadlet.mkContainerUnit {
    name = "llunde-frontend";
    uid = 2002;
    unit.Description = "llunde frontend (static server)";
    container = {
      Image = "ghcr.io/fredrir/llunde-frontend:latest";
      AutoUpdate = "registry";
      PublishPort = ["127.0.0.1:8081:8080"];
    };
    service = {
      Environment = "REGISTRY_AUTH_FILE=/run/secrets/ghcr-auth.json";
      Restart = "always";
      MemoryMax = "256M";
    };
    install.WantedBy = ["default.target"];
  };
}
