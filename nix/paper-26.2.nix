# Paper 26.2's pins, kept for the Mojang jar the Purpur 26.2 image patches
# with, and deleted with that image. Frozen: no pin script reads this file.
{ fetchurl
, jdk25_headless
, stdenvNoCC
}:

rec {
  paperVersion = "26.2";
  paperBuild = "119";

  paperJar = fetchurl {
    url = "https://fill-data.papermc.io/v1/objects/a8c9140c3075bd7c04973e9cdc491b21bfe6bad472b674ef932a4ae0fec19629/paper-${paperVersion}-${paperBuild}.jar";
    hash = "sha256-qMkUDDB1vXwElz6c3EkbIb/mutRytnTvkypK4P7Blik=";
  };

  mojangJar = fetchurl {
    url = "https://piston-data.mojang.com/v1/objects/823e2250d24b3ddac457a60c92a6a941943fcd6a/server.jar";
    hash = "sha256-zazfsliY3l5LSw5d3MJyL3cGfkZgVwnC2IbAAOu2PsU=";
  };

  repo = stdenvNoCC.mkDerivation {
    pname = "paper-repo";
    version = "${paperVersion}+${paperBuild}";

    dontUnpack = true;
    nativeBuildInputs = [ jdk25_headless ];

    buildPhase = ''
      runHook preBuild

      mkdir -p work/cache
      cp ${mojangJar} work/cache/mojang_${paperVersion}.jar
      cd work
      java -Dpaperclip.patchonly=true -DbundlerRepoDir=. -jar ${paperJar}
      cd ..

      runHook postBuild
    '';

    installPhase = ''
      runHook preInstall

      mkdir -p $out
      cp -r work/versions work/libraries work/cache $out/
      chmod -R a-w $out

      runHook postInstall
    '';

    meta.description = "Paper ${paperVersion} build ${paperBuild}, patched at build time";
  };
}
