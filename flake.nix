{
  description = "fs — task coordination for isolated agents, over shared memstore";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      eachSystem = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      perSystem = f: nixpkgs.lib.genAttrs systems (system: f system nixpkgs.legacyPackages.${system});
    in {
      # go.mod requires go 1.27, and buildGoModule sets GOTOOLCHAIN=local, so the
      # toolchain has to be named: without it `go build` would try to fetch one,
      # and the build would stop being reproducible (or fail with no network).
      #
      # Dependencies are vendored and committed, with vendorHash = null, because a
      # build delegated to the nix daemon cannot reach a proxy configured only in
      # the client environment. Vendoring keeps the build hermetic and offline. If
      # go.mod changes, regenerate the tree from the module cache:
      #   nix develop -c sh -c 'GOPROXY=off GOFLAGS=-mod=mod go mod vendor'
      # then commit the result — nix copies only tracked files.
      packages = perSystem (system: pkgs:
        let
          fs = (pkgs.buildGoModule.override { go = pkgs.go_1_27; }) {
            pname = "fs";
            version = "0.1.0";
            src = ./.;
            vendorHash = null;
            # The command, not the library at the module root.
            subPackages = [ "cmd" ];
            # Go names an installed binary after its package directory — here
            # `cmd`; the tool is `fs`.
            postInstall = "mv $out/bin/cmd $out/bin/fs";
            meta = {
              description = "Task coordination for isolated agents over shared memstore";
              mainProgram = "fs";
            };
          };
        in {
          default = fs;
          inherit fs;
        });

      devShells = eachSystem (pkgs: {
        default = pkgs.mkShell { packages = [ pkgs.go_1_27 pkgs.gopls ]; };
      });
    };
}
