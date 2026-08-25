{
  config,
  lib,
  ...
}: let
  cfg = config.llunde.tailscale;
in {
  options.llunde.tailscale = {
    enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
    };
    authKeyFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
    };
  };

  config = lib.mkIf cfg.enable {
    services.tailscale = {
      enable = true;
      authKeyFile = cfg.authKeyFile;
      useRoutingFeatures = "client";
    };
  };
}
