{
  config,
  lib,
  ...
}: let
  cfg = config.platform.transport;
  server = config.platform.k3s.role == "server";
  roleTag = if server then "tag:platform-control" else "tag:platform-worker";
  backendRoutes = map (address: "${address}/32") config.platform.k3s.apiBackends;
  routeFlags = [
    "--accept-routes=${if server then "false" else "true"}"
    "--advertise-routes=${lib.concatStringsSep "," cfg.advertiseRoutes}"
    "--advertise-exit-node=false"
    "--ssh=false"
  ];
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
      {
        assertion = cfg.advertiseRoutes == [] || (server && lib.sort builtins.lessThan cfg.advertiseRoutes == lib.sort builtins.lessThan backendRoutes);
        message = "Only control planes may advertise the complete private API backend /32 routes.";
      }
    ];
    services.tailscale = {
      enable = true;
      authKeyFile = cfg.authKeyFile;
      useRoutingFeatures =
        if cfg.advertiseRoutes == []
        then "client"
        else "server";
      openFirewall = true;
      extraUpFlags = routeFlags ++ ["--advertise-tags=${roleTag}"];
      extraSetFlags = routeFlags;
    };
  };
}
