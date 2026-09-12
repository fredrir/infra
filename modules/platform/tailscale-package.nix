{pkgs}: let
  architectures = {
    x86_64-linux = {
      name = "amd64";
      hash = "1ffqy184ln5nass0yi4bjhcw0dnsn1b4r7zijnvbaq2y0kqqsx2h";
    };
    aarch64-linux = {
      name = "arm64";
      hash = "0gscdi7kd598kk77iq0lpajfvbcrwbznfc8hl2pbn550jajydlcx";
    };
  };
  architecture = architectures.${pkgs.stdenv.hostPlatform.system};
in
  pkgs.stdenvNoCC.mkDerivation {
    pname = "tailscale";
    version = "1.102.4";
    src = pkgs.fetchurl {
      url = "https://pkgs.tailscale.com/stable/tailscale_1.102.4_${architecture.name}.tgz";
      sha256 = architecture.hash;
    };
    nativeBuildInputs = [pkgs.makeWrapper];
    installPhase = ''
      runHook preInstall
      install -Dm755 tailscale "$out/bin/tailscale"
      install -Dm755 tailscaled "$out/bin/tailscaled"
      wrapProgram "$out/bin/tailscaled" --prefix PATH : ${pkgs.lib.makeBinPath [pkgs.iproute2 pkgs.iptables pkgs.getent pkgs.procps]}
      runHook postInstall
    '';
    meta = {
      platforms = builtins.attrNames architectures;
      mainProgram = "tailscale";
    };
  }
