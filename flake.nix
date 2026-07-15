{
  description = "Parrot Coder";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/e7a3ca8092b61ff85b6a45bf863ea2b2d6a661b3";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.buildGoModule {
            pname = "parrot";
            version = "0.0.0-dev";
            src = ./.;
            vendorHash = "sha256-IjEJqtZfn/iEhxTu/KSelcqoIwQ4T584nqv9+ikGNcs=";
            subPackages = [ "cmd/parrot" ];
            ldflags = [
              "-X main.version=0.0.0-dev"
            ];
            # Keep DWARF so core dumps are usable with dlv/gdb.
            dontStrip = true;
            meta.mainProgram = "parrot";
          };
        }
      );

      checks = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          go = pkgs.go_1_25;
        in
        {
          inherit (self.packages.${system}) default;

          format =
            pkgs.runCommand "parrot-format"
              {
                nativeBuildInputs = [ go ];
                src = ./.;
              }
              ''
                cp -R "$src" source
                chmod -R u+w source
                cd source
                unformatted="$(gofmt -l .)"
                if [ -n "$unformatted" ]; then
                  printf 'unformatted Go files:\n%s\n' "$unformatted" >&2
                  exit 1
                fi
                touch "$out"
              '';

          vet =
            pkgs.runCommand "parrot-vet"
              {
                nativeBuildInputs = [ go ];
                src = ./.;
              }
              ''
                export HOME="$TMPDIR"
                cp -R "$src" source
                chmod -R u+w source
                cd source
                go vet ./...
                touch "$out"
              '';

          test =
            pkgs.runCommand "parrot-test"
              {
                nativeBuildInputs = [ go ];
                src = ./.;
              }
              ''
                export HOME="$TMPDIR"
                cp -R "$src" source
                chmod -R u+w source
                cd source
                go test ./...
                touch "$out"
              '';
        }
      );

      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go_1_25
              gopls
              gotools
              govulncheck
              nil
              alejandra
            ];
          };
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.alejandra);
    };
}
