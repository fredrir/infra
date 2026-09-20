{pkgs}:
pkgs.buildGo127Module {
  pname = "infra";
  version = "0.1.0";
  src = pkgs.lib.fileset.toSource {
    root = ../.;
    fileset = pkgs.lib.fileset.unions [../go.mod ../go.sum ../cmd ../internal];
  };
  vendorHash = "sha256-vjGhS7nhjXlms1ZsRG6Qy/7ategUIgwiBwk943cz9Lk=";
  subPackages = ["cmd/infra"];
  env.CGO_ENABLED = 0;
  nativeCheckInputs = [pkgs.git];
  checkPhase = ''
    runHook preCheck
    test -z "$(gofmt -l cmd internal)"
    go vet ./...
    go test ./...
    runHook postCheck
  '';
  meta.mainProgram = "infra";
}
