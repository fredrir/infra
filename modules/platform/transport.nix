{
  config,
  lib,
  ...
}: let
  cfg = config.platform.transport;
in {
  options.platform.transport = {
    authKeyFile = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
    };
    advertiseRoutes = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [];
    };
  };
  config = lib.mkIf config.platform.k3s.enable {
    assertions = [
      {
        assertion = config.platform.k3s.transportInterface == "tailscale0";
        message = "The transport adapter requires tailscale0.";
      }
    ];
    services.tailscale = {
      enable = true;
      authKeyFile = cfg.authKeyFile;
      useRoutingFeatures =
        if cfg.advertiseRoutes == []
        then "client"
        else "both";
      openFirewall = true;
      extraSetFlags = ["--accept-routes=true" "--ssh=false"] ++ lib.optional (cfg.advertiseRoutes != []) "--advertise-routes=${lib.concatStringsSep "," cfg.advertiseRoutes}";
    };
  };
}
