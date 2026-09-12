{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.platform.k3s;
  gvisor = import ./gvisor-package.nix {inherit pkgs;};
in {
  options.platform.k3s.sandbox.enable = lib.mkEnableOption "gVisor runtime";
  config = lib.mkIf (cfg.enable && cfg.sandbox.enable) {
    assertions = [
      {
        assertion = cfg.role == "agent";
        message = "CI runtime belongs on workers.";
      }
    ];
    environment.systemPackages = [gvisor];
    systemd.services.k3s.path = [gvisor pkgs.iproute2 pkgs.iptables pkgs.procps];
    environment.etc."containerd/runsc.toml".text = ''
      [runsc_config]
        platform = "systrap"
    '';
    systemd.tmpfiles.rules = [
      "L+ /var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl - - - - ${pkgs.writeText "platform-containerd-v3.tmpl" ''
        {{ template "base" . }}
        [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runsc]
          runtime_type = "io.containerd.runsc.v1"
        [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runsc.options]
          TypeUrl = "io.containerd.runsc.v1.options"
          ConfigPath = "/etc/containerd/runsc.toml"
      ''}"
    ];
  };
}
