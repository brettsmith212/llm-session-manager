{ pkgs, lib, ... }:

pkgs.buildGoModule rec {
  pname = "llm-session-manager";
  version = "0.1.0";

  src = ./.;

  vendorHash = "sha256-/B76UBP2FC3GPIioq+TME8So7yRmaXWDX0Qc+8fieKs=";

  ldflags = [ "-s" "-w" ];

  postInstall = ''
    mv $out/bin/llm-session-manager $out/bin/llmux
  '';

  meta = {
    description = "LLM-agnostic tmux session manager";
    platforms = lib.platforms.unix;
    mainProgram = "llmux";
  };
}
