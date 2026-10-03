{
  description = "llama-swap with the fleet patches: peer listing forward, peer model settings, selector capabilities, peer health";

  inputs = {
    flake-parts.url = "github:hercules-ci/flake-parts";
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    inputs@{
      self,
      flake-parts,
      nixpkgs,
      ...
    }:
    let
      # What the package needs to know about this checkout. `self` is the git
      # tree, so ignored files are already absent; the filter matters for a
      # `path:` build of a working copy, which would otherwise carry result
      # links, the Go build directory, and the UI's node_modules into the source.
      source = {
        src = nixpkgs.lib.cleanSourceWith {
          name = "llama-swap-source";
          src = self.outPath;
          filter =
            path: _type:
            let
              base = baseNameOf path;
            in
            !(
              nixpkgs.lib.hasPrefix "result" base
              || builtins.elem base [
                "node_modules"
                "build"
                "dist"
                ".direnv"
                ".jj"
                ".git"
              ]
            );
        };
        rev = self.rev or self.dirtyRev or "unknown";
        lastModifiedDate = self.lastModifiedDate or "19700101000000";
      };
    in
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];

      # Consumers apply this to their own nixpkgs, so the fork is built with
      # the same package set as everything else and evaluated once.
      flake.overlays.default = final: _prev: {
        llama-swap = final.callPackage ./nix/package.nix source;
      };

      perSystem =
        { config, pkgs, ... }:
        {
          packages = {
            llama-swap = pkgs.callPackage ./nix/package.nix source;
            llama-swap-ui = config.packages.llama-swap.passthru.ui;
            default = config.packages.llama-swap;
          };

          # The package's check phase runs the Go test suite, so building it is
          # the check.
          checks = {
            inherit (config.packages) llama-swap;
          };

          devShells.default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
              pkgs.nodejs
            ];
          };

          formatter = pkgs.nixfmt-tree;
        };
    };
}
