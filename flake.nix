{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = {nixpkgs, ...}: let
    systems = ["aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux"];
    forAllSystems = nixpkgs.lib.genAttrs systems;
    toolchain = system: let
      pkgs = nixpkgs.legacyPackages.${system};
    in
      with pkgs; [
        actionlint
        age
        alejandra
        ansible
        attic-client
        cosign
        curl
        fluxcd
        gh
        git
        gitleaks
        jq
        kubectl
        kubernetes-helm
        kustomize
        nix
        openssh
        opentofu
        (python3.withPackages (python: [python.pyyaml python.jsonschema]))
        restic
        sops
        uv
        yq-go
      ];
  in {
    devShells = forAllSystems (system: {
      default = nixpkgs.legacyPackages.${system}.mkShell {
        packages = toolchain system;
      };
    });
    packages = forAllSystems (system: {
      attic-client = nixpkgs.legacyPackages.${system}.attic-client;
      default = nixpkgs.legacyPackages.${system}.buildEnv {
        name = "infra-toolchain";
        paths = toolchain system;
        pathsToLink = ["/bin" "/share"];
      };
    });
    formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.alejandra);
  };
}
