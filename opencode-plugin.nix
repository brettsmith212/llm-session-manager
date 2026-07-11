{ stdenvNoCC
, bun
, lib
}:

stdenvNoCC.mkDerivation {
  pname = "llmux-opencode-plugin";
  version = "1.0.0";

  src = ./plugins/opencode/plugin.ts;

  dontUnpack = true;
  dontConfigure = true;
  nativeBuildInputs = [ bun ];

  buildPhase = ''
    runHook preBuild
    bun build $src --outfile tmux-session-manager.js
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    mkdir -p $out/share/opencode/plugins
    cp tmux-session-manager.js $out/share/opencode/plugins/
    runHook postInstall
  '';

  meta = {
    description = "OpenCode plugin that reports agent state to llmux";
    license = lib.licenses.mit;
    platforms = lib.platforms.unix;
  };
}
