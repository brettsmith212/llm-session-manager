{ stdenvNoCC
, lib
}:

stdenvNoCC.mkDerivation {
  pname = "llmux-codex-plugin";
  version = "0.1.0";

  src = ./plugins/codex/hooks.json;

  dontUnpack = true;
  dontConfigure = true;
  dontBuild = true;

  installPhase = ''
    runHook preInstall
    mkdir -p $out/share/codex
    cp $src $out/share/codex/hooks.json
    runHook postInstall
  '';

  meta = {
    description = "Codex hooks that report agent state to llmux";
    license = lib.licenses.mit;
    platforms = lib.platforms.unix;
  };
}
