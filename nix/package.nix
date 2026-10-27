{ lib, buildGoModule }:

buildGoModule rec {
  pname = "tracer";
  version = "0.4.0";
  src = ../.;
  vendorHash = "sha256-MMrLdQC5HUunc9qkcAGDXe5/JRReVdNI1jEV5p72gTk=";

  env.CGO_ENABLED = 0;

  buildPhase = ''
    runHook preBuild
    go build -trimpath -ldflags "-X main.version=${version}" -o tracer .
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    install -Dm755 tracer $out/bin/tracer
    runHook postInstall
  '';

  doCheck = false;

  meta = with lib; {
    description = "Tracer CLI and watcher for Claude/Codex session archiving";
    homepage = "https://github.com/tracer-ai/tracer-cli";
    license = licenses.asl20;
    platforms = [ "x86_64-linux" ];
    mainProgram = "tracer";
  };
}
