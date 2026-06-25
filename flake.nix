{
  description = "tinyland-cleanup: Cross-platform disk cleanup daemon with graduated thresholds";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-24.11";
    flake-parts.url = "github:hercules-ci/flake-parts";
    gloriousflywheel = {
      url = "github:tinyland-inc/GloriousFlywheel";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" "x86_64-darwin" ];

      perSystem = { pkgs, self', system, ... }: let
        buildVersion = pkgs.lib.fileContents ./VERSION;
        buildCommit = inputs.self.rev or "dirty";
        buildDate = inputs.self.lastModifiedDate or "unknown";
      in {
        packages.default = pkgs.buildGoModule {
          pname = "tinyland-cleanup";
          version = buildVersion;
          src = ./.;
          vendorHash = null;

          ldflags = [
            "-s" "-w"
            "-X main.version=${buildVersion}"
            "-X main.commit=${buildCommit}"
            "-X main.date=${buildDate}"
          ];

          meta = with pkgs.lib; {
            description = "Cross-platform disk cleanup daemon with graduated thresholds";
            homepage = "https://github.com/Jesssullivan/tinyland-cleanup";
            license = licenses.mit;
            platforms = platforms.unix;
            mainProgram = "tinyland-cleanup";
          };
        };

        # Documentation site (MkDocs Material), built hermetically through Nix.
        # Mirrors the pure-Bazel //docs:site target; SOURCE_DATE_EPOCH keeps the
        # output byte-identical across Nix and Bazel.
        packages.docs = pkgs.stdenvNoCC.mkDerivation {
          pname = "tinyland-cleanup-docs";
          version = buildVersion;
          src = ./.;
          nativeBuildInputs = [
            (pkgs.python3.withPackages (ps: with ps; [
              mkdocs
              mkdocs-material
              pymdown-extensions
            ]))
          ];
          SOURCE_DATE_EPOCH = "315532800";
          dontBuild = true;
          installPhase = ''
            runHook preInstall
            mkdocs build --strict --config-file docs/mkdocs.yml --site-dir "$out"
            runHook postInstall
          '';
        };

        devShells.default = pkgs.mkShell {
          inputsFrom = [ self'.packages.default ];
          packages = with pkgs; [
            bazelisk
            buildifier
            go_1_23
            gopls
            golangci-lint
            just
            ripgrep
            inputs.gloriousflywheel.packages.${system}.gloriousflywheel-frontdoor-tools
          ];
        };
      };

      flake = {
        overlays.default = final: prev: {
          tinyland-cleanup = inputs.self.packages.${final.system}.default;
        };
      };
    };
}
