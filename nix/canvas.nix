# The pinned Canvas artifacts. Canvas ships Paper's paperclip bootstrap, so
# the patch-only build is nix/paper.nix's unchanged.
{ fetchurl
, jdk25_headless
, stdenvNoCC
, mojangJar
}:

rec {
  canvasVersion = "26.3";
  canvasBuild = "991";

  canvasJar = fetchurl {
    url = "https://jenkins.canvasmc.io/job/Canvas/${canvasBuild}/artifact/canvas-server/build/libs/canvas-build.${canvasBuild}.jar";
    hash = "sha256-v+KIU6kIjYdBbUFdz5ML1fuZ7pAZ95I6EMfS7rJq7yc=";
  };

  repo = stdenvNoCC.mkDerivation {
    pname = "canvas-repo";
    version = "${canvasVersion}+${canvasBuild}";

    dontUnpack = true;
    nativeBuildInputs = [ jdk25_headless ];

    buildPhase = ''
      runHook preBuild

      mkdir -p work/cache
      cp ${mojangJar} work/cache/mojang_${canvasVersion}.jar
      cd work
      java -Dpaperclip.patchonly=true -DbundlerRepoDir=. -jar ${canvasJar}
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

    meta.description = "Canvas ${canvasVersion} build ${canvasBuild}, patched at build time";
  };
}
