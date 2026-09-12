{
  nixpkgs,
  system,
  host,
  legacyHost,
}: let
  lib = nixpkgs.lib;
  inventory = builtins.fromJSON (builtins.readFile ../platform/inventory/nodes.json);
  node = lib.findFirst (value: value.id == "fredrir-05") (throw "The control-plane inventory entry is required.") inventory.nodes;
  cfg = host.config;
  previous = legacyHost.config;
  passes = value: lib.all (entry: entry.assertion) value.assertions;
  enabled = overrides: (host.extendModules {modules = [{platform.k3s.enable = true;} overrides];}).config;
  missingEnrollment = enabled {};
  missingSecrets = enabled {
    platform.k3s = {
      preflightApproved = true;
      privateInterface = "fixture0";
    };
  };
  ready = enabled {
    platform.k3s = {
      preflightApproved = true;
      privateInterface = "fixture0";
    };
    platform.fredrir05.secretsFile = ./fixtures/k3s-tokens.yaml;
    sops.validateSopsFiles = lib.mkForce false;
  };
  services = builtins.attrNames cfg.systemd.services;
  timers = builtins.attrNames cfg.systemd.timers;
  etc = builtins.attrNames cfg.environment.etc;
  accounts = {
    edge = {
      uid = 2000;
      start = 165536;
    };
    llunde-backend = {
      uid = 2001;
      start = 100000;
    };
    llunde-frontend = {
      uid = 2002;
      start = 231072;
    };
  };
  boot = value: {
    inherit (value.boot.loader.systemd-boot) enable configurationLimit;
    inherit (value.boot.loader) efi;
    grub = value.boot.loader.grub.enable;
    modules = value.boot.initrd.availableKernelModules;
    disk = value.disko.devices.disk.main.device;
  };
  access = value: {
    inherit (value.services.openssh) enable openFirewall settings;
    inherit (value.users.users.root.openssh) authorizedKeys;
    tailscale = {
      inherit (value.services.tailscale) enable authKeyFile useRoutingFeatures extraUpFlags extraSetFlags;
    };
    inherit (value.networking.firewall) allowedTCPPorts trustedInterfaces;
  };
  preservedAccount = name: expected: let
    user = cfg.users.users.${name};
  in
    user.uid
    == expected.uid
    && cfg.users.groups.${name}.gid == expected.uid
    && user.home == previous.users.users.${name}.home
    && user.linger
    && user.subUidRanges
    == [
      {
        startUid = expected.start;
        count = 65536;
      }
    ]
    && user.subGidRanges
    == [
      {
        startGid = expected.start;
        count = 65536;
      }
    ];
in
  assert lib.assertMsg (passes cfg && !cfg.services.k3s.enable && cfg.platform.k3s.role == "server") "The control-plane candidate must remain valid and inactive.";
  assert lib.assertMsg (cfg.fileSystems == previous.fileSystems && boot cfg == boot previous) "Storage and boot configuration must preserve the existing host.";
  assert lib.assertMsg (cfg.networking.hostName == previous.networking.hostName && cfg.system.stateVersion == previous.system.stateVersion) "Host identity and state version must remain stable.";
  assert lib.assertMsg (cfg.platform.k3s.nodeName == node.id && ready.services.k3s.nodeName == node.id && cfg.platform.k3s.role == node.desiredRole && ready.networking.hostName == previous.networking.hostName) "K3s must use the canonical inventory identity while preserving the OS hostname.";
  assert lib.assertMsg (access cfg == access previous) "Existing administrator access must remain available.";
  assert lib.assertMsg (lib.all (name: preservedAccount name accounts.${name}) (builtins.attrNames accounts)) "Retained service accounts must preserve data ownership and subordinate mappings.";
  assert lib.assertMsg (!lib.any (name: lib.hasPrefix "containers/systemd/" name) etc && !lib.any (name: lib.hasPrefix "llunde-" name || lib.hasPrefix "gitops-" name || lib.hasPrefix "restic-" name) (services ++ timers)) "The control plane must not declare legacy application or reconciliation units.";
  assert lib.assertMsg (!(passes missingEnrollment) && !(passes missingSecrets) && passes ready) "K3s activation requires reviewed network enrollment and declared boot credentials.";
  assert lib.assertMsg (lib.elem "sops-install-secrets.service" ready.systemd.services.k3s.requires && lib.elem "sops-install-secrets.service" ready.systemd.services.k3s.after && ready.sops.useSystemdActivation) "K3s must wait for boot secret provisioning.";
  assert lib.assertMsg (lib.all (name: ready.sops.secrets.${name}.owner == "root" && ready.sops.secrets.${name}.mode == "0400") ["k3s-server-token" "k3s-agent-token"]) "K3s credentials must remain root-readable only.";
    nixpkgs.legacyPackages.${system}.writeText "fredrir-05-host-contract" "validated\n"
