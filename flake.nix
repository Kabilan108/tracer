{
  description = "tracer CLI (session archive sync + watch) — x86_64-linux only";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [ "x86_64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);
    in
    {
      overlays.default = final: prev: {
        tracer = final.callPackage ./nix/package.nix { };
      };

      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        rec {
          tracer = pkgs.callPackage ./nix/package.nix { };
          default = tracer;
        }
      );

      homeManagerModules.default = import ./nix/hm-module.nix;

      devShells = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          default = pkgs.mkShell {
            buildInputs = with pkgs; [
              go
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
