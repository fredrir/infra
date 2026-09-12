{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.platform.k3s;
  server = cfg.role == "server";
  tokenFile =
    if server
    then cfg.serverTokenFile
    else cfg.agentTokenFile;
  preflight = pkgs.writeShellApplication {
    name = "platform-preflight";
    runtimeInputs = [pkgs.coreutils pkgs.gnugrep pkgs.iproute2 pkgs.util-linux pkgs.kmod];
    text = ''
      test "$(id -u)" = 0
      test -d /run/systemd/system
      test -c /dev/net/tun
      test "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs
      for controller in cpu memory pids io; do
        grep -qw "$controller" /sys/fs/cgroup/cgroup.controllers
      done
      test "$(cat /proc/sys/net/ipv4/ip_forward)" = 1
      test -z "$(swapon --noheadings --show=NAME)"
      ip address show dev ${lib.escapeShellArg cfg.transportInterface} | grep -q 'inet '
      ip address show | grep -Fq ${lib.escapeShellArg cfg.nodeIP}
      test -s ${lib.escapeShellArg tokenFile}
      test "$(stat -c %u ${lib.escapeShellArg tokenFile})" = 0
      stat -c %a ${lib.escapeShellArg tokenFile} | grep -Eq '^(400|600)$'
      test "$(findmnt -T /var/lib/rancher -no FSTYPE)" != overlay
      ${lib.optionalString server ''
        test -s ${lib.escapeShellArg cfg.agentTokenFile}
        test "$(stat -c %u ${lib.escapeShellArg cfg.agentTokenFile})" = 0
        stat -c %a ${lib.escapeShellArg cfg.agentTokenFile} | grep -Eq '^(400|600)$'
        if cmp -s ${lib.escapeShellArg cfg.serverTokenFile} ${lib.escapeShellArg cfg.agentTokenFile}; then
          echo 'Server and agent tokens must differ' >&2
          exit 1
        fi
      ''}
    '';
  };
  backup = pkgs.writeShellApplication {
    name = "platform-etcd-backup";
    runtimeInputs = [pkgs.coreutils pkgs.findutils pkgs.gnutar pkgs.restic pkgs.k3s_1_36];
    text = builtins.readFile ./etcd-backup.sh;
  };
in {
  options.platform.k3s = {
    enable = lib.mkEnableOption "K3s platform host";
    role = lib.mkOption {
      type = lib.types.enum ["server" "agent"];
      default = "agent";
    };
    nodeName = lib.mkOption {
      type = lib.types.str;
      default = "";
    };
    nodeIP = lib.mkOption {
      type = lib.types.str;
      default = "";
    };
    privateIP = lib.mkOption {
      type = lib.types.str;
      default = "";
    };
    privateInterface = lib.mkOption {
      type = lib.types.str;
      default = "";
    };
    transportInterface = lib.mkOption {
      type = lib.types.str;
      default = "tailscale0";
    };
    apiName = lib.mkOption {
      type = lib.types.str;
      default = "k3s-api.internal";
    };
    apiBackends = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [];
    };
    initialize = lib.mkOption {
      type = lib.types.bool;
      default = false;
    };
    preflightApproved = lib.mkOption {
      type = lib.types.bool;
      default = false;
    };
    serverTokenFile = lib.mkOption {
      type = lib.types.str;
      default = "/run/secrets/k3s-server-token";
    };
    agentTokenFile = lib.mkOption {
      type = lib.types.str;
      default = "/run/secrets/k3s-agent-token";
    };
    podCIDR = lib.mkOption {
      type = lib.types.str;
      default = "10.42.0.0/16";
    };
    serviceCIDR = lib.mkOption {
      type = lib.types.str;
      default = "10.43.0.0/16";
    };
    adminKeys = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [];
    };
    backupEnvironmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.preflightApproved;
        message = "Platform enrollment requires reviewed host and network preflight.";
      }
      {
        assertion = cfg.nodeName != "" && cfg.nodeIP != "" && cfg.adminKeys != [];
        message = "Platform host identity, node IP and admin keys are required.";
      }
      {
        assertion = builtins.length cfg.apiBackends == 3 && builtins.length (lib.unique cfg.apiBackends) == 3;
        message = "Platform API requires three distinct backends.";
      }
      {
        assertion = !server || (cfg.privateIP != "" && cfg.privateInterface != "" && cfg.nodeIP == cfg.privateIP);
        message = "K3s servers require a private node IP and interface.";
      }
      {
        assertion = !cfg.initialize || server;
        message = "Only a server can initialize etcd.";
      }
      {
        assertion = cfg.serverTokenFile != cfg.agentTokenFile;
        message = "Server and agent token paths must differ.";
      }
      {
        assertion = lib.all (p: lib.hasPrefix "/run/" p || lib.hasPrefix "/var/lib/" p) [cfg.serverTokenFile cfg.agentTokenFile];
        message = "K3s token paths must refer to runtime files.";
      }
      {
        assertion = !(lib.elem cfg.transportInterface config.networking.firewall.trustedInterfaces);
        message = "Platform transport must use explicit firewall rules.";
      }
    ];

    networking.hostName = cfg.nodeName;
    networking.firewall = {
      enable = true;
      checkReversePath = "loose";
      interfaces =
        {
          ${cfg.transportInterface} = {
            allowedTCPPorts = [22 6443 9100 10250];
            allowedUDPPorts = [8472];
          };
        }
        // lib.optionalAttrs server {
          ${cfg.privateInterface}.allowedTCPPorts = [2379 2380 6443 9100 10250];
        };
      extraForwardRules = ''
        iifname "cni0" accept
        oifname "cni0" accept
        iifname "flannel.1" accept
        oifname "flannel.1" accept
      '';
      extraInputRules = ''
        ip saddr ${cfg.podCIDR} tcp dport { 6443, 9100, 10250 } accept
      '';
    };
    networking.nftables.enable = true;
    boot.kernelModules = ["overlay" "br_netfilter" "tun" "vxlan"];
    boot.kernel.sysctl = {
      "net.ipv4.ip_forward" = 1;
      "net.bridge.bridge-nf-call-iptables" = 1;
      "net.bridge.bridge-nf-call-ip6tables" = 1;
      "vm.overcommit_memory" = 1;
      "vm.panic_on_oom" = 0;
      "kernel.panic" = 10;
      "kernel.panic_on_oops" = 1;
    };
    services.chrony.enable = true;
    services.prometheus.exporters.node = {
      enable = true;
      openFirewall = false;
      listenAddress = cfg.nodeIP;
      enabledCollectors = ["systemd" "textfile"];
      extraFlags = ["--collector.textfile.directory=/var/lib/platform/metrics"];
    };
    services.openssh = {
      enable = true;
      openFirewall = false;
      settings = {
        PasswordAuthentication = false;
        KbdInteractiveAuthentication = false;
        PermitRootLogin = "prohibit-password";
      };
    };
    users.users.root.openssh.authorizedKeys.keys = cfg.adminKeys;
    environment.systemPackages = [preflight] ++ lib.optional server backup;
    environment.etc."platform/k3s-etcd-expected.prom" = lib.mkIf server {
      text = ''
        restic_expected_job{backup_job="k3s-etcd"} 1
        restic_max_age_seconds{backup_job="k3s-etcd"} 21600
      '';
    };
    systemd.tmpfiles.rules =
      [
        "d /var/lib/rancher 0700 root root -"
        "d /var/lib/platform 0755 root root -"
        "d /var/lib/platform/metrics 0755 root root -"
      ]
      ++ lib.optional server "L+ /var/lib/platform/metrics/k3s-etcd-expected.prom - - - - /etc/platform/k3s-etcd-expected.prom";
    services.k3s = {
      enable = true;
      package = pkgs.k3s_1_36;
      role = cfg.role;
      nodeName = cfg.nodeName;
      nodeIP = cfg.nodeIP;
      tokenFile = tokenFile;
      agentTokenFile =
        if server
        then cfg.agentTokenFile
        else null;
      serverAddr =
        if cfg.initialize
        then ""
        else "https://${cfg.apiName}:7443";
      disable = lib.optionals server ["traefik" "servicelb"];
      nodeTaint = lib.optionals server ["node-role.kubernetes.io/control-plane=true:NoSchedule"];
      gracefulNodeShutdown.enable = true;
      extraKubeletConfig = {
        cgroupDriver = "systemd";
        protectKernelDefaults = true;
        systemReserved = {
          cpu = "250m";
          memory = "256Mi";
          "ephemeral-storage" = "1Gi";
        };
        kubeReserved = {
          cpu = "250m";
          memory =
            if server
            then "768Mi"
            else "512Mi";
          "ephemeral-storage" = "2Gi";
        };
        evictionHard = {
          "memory.available" = "256Mi";
          "nodefs.available" = "10%";
          "imagefs.available" = "15%";
        };
        podPidsLimit = 1024;
        containerLogMaxSize = "10Mi";
        containerLogMaxFiles = 3;
      };
      extraFlags =
        ["--flannel-iface=${cfg.transportInterface}"]
        ++ lib.optionals server [
          "--advertise-address=${cfg.privateIP}"
          "--tls-san=${cfg.apiName}"
          "--secrets-encryption"
          "--write-kubeconfig-mode=0600"
          "--cluster-cidr=${cfg.podCIDR}"
          "--service-cidr=${cfg.serviceCIDR}"
          "--flannel-backend=vxlan"
          "--kube-apiserver-arg=authorization-mode=Node,RBAC"
          "--kube-apiserver-arg=enable-admission-plugins=NodeRestriction"
          "'--etcd-snapshot-schedule-cron=0 */6 * * *'"
          "--etcd-snapshot-retention=20"
        ]
        ++ lib.optional cfg.initialize "--cluster-init";
    };
    systemd.services.k3s = {
      requires = ["tailscaled.service" "platform-api-proxy.service"];
      after = ["tailscaled.service" "platform-api-proxy.service" "network-online.target"];
      serviceConfig.ExecStartPre = ["${preflight}/bin/platform-preflight"];
    };
    systemd.services.platform-etcd-backup = lib.mkIf (server && cfg.backupEnvironmentFile != null) {
      requires = ["k3s.service"];
      after = ["k3s.service"];
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${backup}/bin/platform-etcd-backup";
        EnvironmentFile = cfg.backupEnvironmentFile;
        UMask = "0077";
      };
    };
    systemd.timers.platform-etcd-backup = lib.mkIf (server && cfg.backupEnvironmentFile != null) {
      wantedBy = ["timers.target"];
      timerConfig = {
        OnCalendar = "*-*-* 00,04,08,12,16,20:20:00";
        RandomizedDelaySec = "10m";
        Persistent = true;
      };
    };
  };
}
