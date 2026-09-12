{
  nixpkgs,
  system,
}: let
  lib = nixpkgs.lib;
  mkHost = overrides:
    nixpkgs.lib.nixosSystem {
      system =
        if system == "aarch64-darwin"
        then "aarch64-linux"
        else system;
      modules = [
        ./default.nix
        {
          system.stateVersion = "26.05";
          boot.loader.grub.devices = ["/dev/vda"];
          fileSystems."/" = {
            device = "/dev/vda1";
            fsType = "ext4";
          };
          platform.k3s =
            {
              enable = true;
              nodeName = "rehearsal-control";
              nodeIP = "192.0.2.10";
              privateIP = "192.0.2.10";
              privateInterface = "ens7";
              apiBackends = ["192.0.2.10" "192.0.2.11" "192.0.2.12"];
              adminKeys = ["rehearsal-key"];
              preflightApproved = true;
            }
            // builtins.removeAttrs overrides ["transport"];
          platform.transport = overrides.transport or {};
        }
      ];
    };
  server =
    (mkHost {
      role = "server";
      initialize = true;
    }).config;
  agent =
    (mkHost {
      role = "agent";
      sandbox.enable = true;
    }).config;
  unapproved = (mkHost {preflightApproved = false;}).config;
  sameToken = (mkHost {agentTokenFile = "/run/secrets/k3s-server-token";}).config;
  router = (mkHost {
    role = "server";
    transport.advertiseRoutes = ["192.0.2.10/32" "192.0.2.11/32" "192.0.2.12/32"];
  }).config;
  broadRoutes = (mkHost {
    role = "server";
    transport.advertiseRoutes = ["192.0.2.0/24"];
  }).config;
  workerRoutes = (mkHost {
    role = "agent";
    transport.advertiseRoutes = ["192.0.2.10/32" "192.0.2.11/32" "192.0.2.12/32"];
  }).config;
  noFailures = cfg: lib.all (x: x.assertion) cfg.assertions;
in
  assert lib.assertMsg (noFailures server && noFailures agent) "Platform fixtures must satisfy host assertions.";
  assert lib.assertMsg (!(noFailures unapproved) && !(noFailures sameToken)) "Unapproved enrollment and shared token paths must fail.";
  assert lib.assertMsg (noFailures router && !(noFailures broadRoutes) && !(noFailures workerRoutes)) "Only servers may advertise the exact backend host routes.";
  assert lib.assertMsg (lib.elem "--accept-routes=false" server.services.tailscale.extraSetFlags && lib.elem "--accept-routes=true" agent.services.tailscale.extraSetFlags) "Control-plane traffic must retain its private route.";
  assert lib.assertMsg (lib.elem "--advertise-tags=tag:platform-control" server.services.tailscale.extraUpFlags && lib.elem "--advertise-tags=tag:platform-worker" agent.services.tailscale.extraUpFlags) "Transport enrollment must use the host role tag.";
  assert lib.assertMsg (lib.elem "--secrets-encryption" server.services.k3s.extraFlags) "Server datastore must encrypt secrets.";
  assert lib.assertMsg (server.services.k3s.nodeTaint != [] && agent.services.k3s.nodeTaint == []) "Control-plane taint must exclude workloads.";
  assert lib.assertMsg (agent.services.k3s.tokenFile == "/run/secrets/k3s-agent-token" && agent.services.k3s.agentTokenFile == null) "Workers must receive only agent credentials.";
  assert lib.assertMsg (lib.all (x: lib.elem x server.services.k3s.disable) ["traefik" "servicelb"]) "K3s must not claim existing ingress ports.";
    nixpkgs.legacyPackages.${system}.writeText "platform-host-contract" "validated\n"
