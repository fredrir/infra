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
        zstd
      ];
    checks = system: let
      pkgs = nixpkgs.legacyPackages.${system};
    in
      with pkgs; [
        actionlint
        age
        ansible
        bash
        coreutils
        git
        gnutar
        jq
        kubectl
        kubernetes-helm
        kustomize
        opentofu
        (python3.withPackages (python: [python.pyyaml python.jsonschema]))
        sops
        stdenv.cc.cc.lib
        uv
        yq-go
        zstd
      ];
  in {
    devShells = forAllSystems (system: let
      pkgs = nixpkgs.legacyPackages.${system};
    in {
      default = pkgs.mkShell {
        packages = toolchain system;
        LD_LIBRARY_PATH = pkgs.lib.makeLibraryPath (pkgs.lib.optionals pkgs.stdenv.isLinux [pkgs.stdenv.cc.cc.lib]);
      };
    });
    packages = forAllSystems (system: {
      attic-client = nixpkgs.legacyPackages.${system}.attic-client;
      check = nixpkgs.legacyPackages.${system}.buildEnv {
        name = "infra-check";
        paths = checks system;
        pathsToLink = ["/bin" "/lib" "/share"];
      };
      default = (nixpkgs.legacyPackages.${system}.buildEnv {
        name = "infra-toolchain";
        paths = toolchain system;
        pathsToLink = ["/bin" "/share"];
      }).overrideAttrs (_: {allowSubstitutes = true;});
    });
    formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.alejandra);
  };
}
