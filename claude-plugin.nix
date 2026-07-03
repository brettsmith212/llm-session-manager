{ stdenvNoCC
, lib
}:

stdenvNoCC.mkDerivation {
  pname = "llmux-claude-plugin";
  version = "0.1.0";

  src = ./plugins/claude;

  dontConfigure = true;
  dontBuild = true;

  installPhase = ''
    runHook preInstall
    mkdir -p $out
    cp -rT $src $out
    runHook postInstall
  '';

  meta = {
    description = "Claude Code plugin that pushes LLM lifecycle events to llmux tmux session state";
    license = lib.licenses.mit;
    platforms = lib.platforms.unix;
  };
}
