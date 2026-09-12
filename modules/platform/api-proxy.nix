{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.platform.k3s;
  proxyConfig = pkgs.writeText "platform-api-proxy.cfg" ''
    global
      maxconn 256
    defaults
      mode tcp
      timeout connect 5s
      timeout client 1h
      timeout server 1h
    frontend api
      bind 127.0.0.1:7443
      default_backend control_planes
    backend control_planes
      balance roundrobin
      option tcp-check
      ${lib.concatImapStringsSep "\n  " (i: address: "server control-${toString i} ${address}:6443 check inter 2s fall 3 rise 2") cfg.apiBackends}
  '';
in {
  config = lib.mkIf cfg.enable {
    networking.hosts."127.0.0.1" = [cfg.apiName];
    systemd.services.platform-api-proxy = {
      wantedBy = ["multi-user.target"];
      after = ["network-online.target"];
      wants = ["network-online.target"];
      serviceConfig = {
        ExecStartPre = "${pkgs.haproxy}/bin/haproxy -c -f ${proxyConfig}";
        ExecStart = "${pkgs.haproxy}/bin/haproxy -W -db -f ${proxyConfig}";
        Restart = "always";
        DynamicUser = true;
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
      };
    };
  };
}
