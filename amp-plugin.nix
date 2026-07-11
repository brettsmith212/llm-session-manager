{ stdenvNoCC
, lib
}:

stdenvNoCC.mkDerivation {
  pname = "llmux-amp-plugin";
  version = "0.1.0";

  src = ./plugins/amp/llmux-state.ts;

  dontUnpack = true;
  dontConfigure = true;
  dontBuild = true;

  installPhase = ''
    runHook preInstall
    mkdir -p $out/share/amp/plugins
    cp $src $out/share/amp/plugins/llmux-state.ts
    runHook postInstall
  '';

  meta = {
    description = "Amp plugin that integrates Amp thread state and commands with llmux";
    license = lib.licenses.mit;
    platforms = lib.platforms.unix;
  };
}
