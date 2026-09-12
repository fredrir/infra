{...}: {
  imports = [
    ./security-baseline.nix
    ./k3s.nix
    ./transport.nix
    ./api-proxy.nix
    ./sandbox.nix
    ./watchdog.nix
  ];
}
