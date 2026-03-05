{
  description = "tracer CLI (session archive sync + watch) — x86_64-linux only";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);

      mkPackage =
        pkgs:
        (pkgs.buildGoModule.override { go = pkgs.go_1_26; }) rec {
          pname = "tracer";
          version = "0.0.1";
          src = self;
          vendorHash = "sha256-LfdLTz97uYgAqgR/UVdL4ARcKz7uwXXel+lf2UKcQds=";

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

          meta = with pkgs.lib; {
            description = "Tracer CLI and watcher for Claude/Codex session archiving";
            homepage = "https://github.com/tracer-ai/tracer-cli";
            license = licenses.asl20;
            platforms = [ "x86_64-linux" ];
            mainProgram = "tracer";
          };
        };
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          default = mkPackage pkgs;
          tracer = mkPackage pkgs;
        }
      );

      overlays.default = final: prev: {
        tracer = mkPackage final;
      };

      homeManagerModules.default = import ./nix/hm-module.nix;

      devShells = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          default = pkgs.mkShell {
            buildInputs = with pkgs; [
              go_1_26
              gopls
              git
              just
              nixfmt
            ];
            shellHook = ''
              export PATH="$PATH:$PWD/bin"
            '';
          };
        }
      );
    };
}
