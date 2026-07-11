{
  description = "LLM-agnostic tmux session manager (llmux) and agent plugins";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forEachSystem = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forEachSystem (system:
        let pkgs = nixpkgs.legacyPackages.${system}; in
        {
          default = pkgs.callPackage ./default.nix { };
          claude-plugin = pkgs.callPackage ./claude-plugin.nix { };
          amp-plugin = pkgs.callPackage ./amp-plugin.nix { };
          opencode-plugin = pkgs.callPackage ./opencode-plugin.nix { };
        });

      overlays = forEachSystem (system: (final: prev: {
        llmux = self.packages.${system}.default;
        llmux-claude-plugin = self.packages.${system}.claude-plugin;
        llmux-amp-plugin = self.packages.${system}.amp-plugin;
        llmux-opencode-plugin = self.packages.${system}.opencode-plugin;
      }));
    };
}
