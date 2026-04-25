{
  description = "tinyland-cleanup: Cross-platform disk cleanup daemon with graduated thresholds";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-24.11";
    flake-parts.url = "github:hercules-ci/flake-parts";
  };

  outputs = inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" "x86_64-darwin" ];

      perSystem = { pkgs, self', system, ... }: {
        packages.default = pkgs.buildGoModule {
          pname = "tinyland-cleanup";
          version = "0.2.0";
          src = ./.;
          vendorHash = null;

          ldflags = [
            "-s" "-w"
            "-X main.version=0.2.0"
          ] ++ pkgs.lib.optionals (inputs.self ? rev) [
            "-X main.commit=${inputs.self.rev}"
          ];

          meta = with pkgs.lib; {
            description = "Cross-platform disk cleanup daemon with graduated thresholds";
            homepage = "https://github.com/Jesssullivan/tinyland-cleanup";
            license = licenses.mit;
            platforms = platforms.unix;
            mainProgram = "tinyland-cleanup";
          };
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
