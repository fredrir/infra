{pkgs}: let
  architecture =
    {
      x86_64-linux = {
        name = "x86_64";
        hash = "81416511897ab8abd4e723d66823c5b0461a2ee3311cfa70d152404ef9b860cf";
      };
      aarch64-linux = {
        name = "aarch64";
        hash = "2b162adb35860f598ab2f89b9d752bff2c7ee6175c05d9cc532174a336cfb38c";
      };
    }.${
      pkgs.stdenv.hostPlatform.system
    };
in
  pkgs.stdenvNoCC.mkDerivation {
    pname = "gvisor";
    version = "20260907.0";
    src = pkgs.fetchurl {
      url = "https://github.com/google/gvisor/releases/download/release-20260907.0/gvisor-${architecture.name}.tar.bz2";
      sha256 = architecture.hash;
    };
    sourceRoot = ".";
    dontStrip = true;
    installPhase = ''
      runHook preInstall
      mkdir -p "$out/bin"
      cp -R runsc containerd-shim-runsc-v1 gvisor-bin "$out/bin/"
      runHook postInstall
    '';
    meta.platforms = ["x86_64-linux" "aarch64-linux"];
  }
