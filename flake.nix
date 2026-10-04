{
  description = "tinyland-cleanup: Cross-platform disk cleanup daemon with graduated thresholds";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-24.11";
    flake-parts.url = "github:hercules-ci/flake-parts";
  };

  outputs = inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" "x86_64-darwin" ];

      perSystem = { pkgs, self', system, ... }: let
        buildVersion = pkgs.lib.fileContents ./VERSION;
        buildCommit = inputs.self.rev or "dirty";
        buildDate = inputs.self.lastModifiedDate or "unknown";
        mkCleanup = { pname, tags ? [ ], doCheck ? true }: pkgs.buildGoModule {
          inherit pname tags doCheck;
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
      in {
        packages.default = mkCleanup { pname = "tinyland-cleanup"; };

        # TIN-3342 consumer-test hook: the same source built with the
        # tinyland_sim tag, which reads disk statistics from
        # TINYLAND_CLEANUP_SIM_DISKSTATS instead of statfs (see
        # docs/operator-workflow.md). The Go and Bazel lanes run its tests;
        # this package is the binary a consumer check runs. For consumer
        # checks only; never install or deploy it.
        packages.sim = mkCleanup {
          pname = "tinyland-cleanup-sim";
          tags = [ "tinyland_sim" ];
          doCheck = false;
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
