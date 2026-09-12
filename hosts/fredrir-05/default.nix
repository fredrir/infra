{
  config,
  lib,
  ...
}: let
  cfg = config.platform.fredrir05;
  tokenNames = ["k3s-server-token" "k3s-agent-token"];
in {
  imports = [
    ../../modules/profiles/server.nix
    ../../modules/users
    ../../modules/tailscale
    ../../modules/secrets
  ];

  options.platform.fredrir05.secretsFile = lib.mkOption {
    type = lib.types.nullOr lib.types.path;
    default = null;
  };

  config = {
    networking.hostName = lib.mkForce "llunde-01";
    system.stateVersion = "25.11";

    llunde.users.services = {
      edge = {
        uid = 2000;
        subUidStart = 165536;
      };
      llunde-backend = {
        uid = 2001;
        subUidStart = 100000;
      };
      llunde-frontend = {
        uid = 2002;
        subUidStart = 231072;
      };
    };
    users.groups.ghcr.members = ["llunde-backend" "llunde-frontend"];

    llunde.tailscale.authKeyFile = config.sops.secrets.tailscale-auth-key.path;
    platform.transport.authKeyFile = config.sops.secrets.tailscale-auth-key.path;

    platform.k3s = {
      enable = lib.mkDefault false;
      role = "server";
      nodeName = "fredrir-05";
      nodeIP = "10.60.0.5";
      privateIP = "10.60.0.5";
      apiBackends = ["10.60.0.5" "10.60.0.7" "10.60.0.8"];
      adminKeys = [
        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH0jzc3S05J0DFj3W+Gv6J4Hc9fxvUjIOEuTWKfVnVY9 fhansteen@gmail.com"
      ];
    };

    assertions = lib.optionals config.platform.k3s.enable [
      {
        assertion = cfg.secretsFile != null;
        message = "The control plane requires boot-provisioned K3s credentials.";
      }
      {
        assertion = config.platform.k3s.serverTokenFile == "/run/secrets/k3s-server-token" && config.platform.k3s.agentTokenFile == "/run/secrets/k3s-agent-token";
        message = "The control plane must use its declared K3s secret paths.";
      }
    ];

    networking.firewall.trustedInterfaces = lib.mkIf config.platform.k3s.enable (lib.mkForce ["lo"]);

    sops = {
      useSystemdActivation = true;
      secrets =
        {
          tailscale-auth-key = {
            sopsFile = ../../secrets/tailscale.yaml;
            key = "auth_key";
          };
        }
        // lib.optionalAttrs (cfg.secretsFile != null) (lib.genAttrs tokenNames (_: {
          sopsFile = cfg.secretsFile;
          owner = "root";
          group = "root";
          mode = "0400";
        }));
    };

    systemd.services.tailscaled = {
      requires = ["sops-install-secrets.service"];
      after = ["sops-install-secrets.service"];
    };

    systemd.services.k3s = lib.mkIf config.platform.k3s.enable {
      requires = ["sops-install-secrets.service"];
      after = ["sops-install-secrets.service"];
    };
  };
}
