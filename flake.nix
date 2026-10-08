{
  description = "vm development environment";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      # Everything `go tool task <target>` needs that is not a Go program.
      # Go tools (task, golangci-lint, repocheck, generators) are pinned by
      # go.mod `tool` directives and deliberately absent here: two pins drift.
      devShells = forSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.git
              pkgs.shellcheck
              pkgs.actionlint
            ];
            # go.mod's go directive picks the toolchain; nixpkgs' go only
            # bootstraps it.
            GOTOOLCHAIN = "auto";
          };
        }
      );

      formatter = forSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
