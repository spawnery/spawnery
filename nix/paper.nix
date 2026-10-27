# The pinned Paper artifacts. PaperMC's jar is a paperclip bootstrap that
# downloads and patches Mojang's server jar on first start; patching here
# keeps every pod from downloading and extracting 166 MB on each start.
{ fetchurl
, jdk25_headless
, stdenvNoCC
}:

# hack/paper-pin.sh writes both versions and both hashes; `make
# paper-pin-check` compares without changing anything.
rec {
  paperVersion = "26.3";
  paperBuild = "226";

  paperJar = fetchurl {
    url = "https://fill-data.papermc.io/v1/objects/1373c651510d4aeee7e7903f3cfd7003919c1a364528dbe124a4ae40e11a766e/paper-${paperVersion}-${paperBuild}.jar";
    hash = "sha256-E3PGUVENSu7n55A/PP1wA5GcGjZFKNvhJKSuQOEadm4=";
  };

  # URL and hash come from META-INF/download-context inside paperJar, not from
  # the host that serves the artifact.
  mojangJar = fetchurl {
    url = "https://piston-data.mojang.com/v1/objects/33680f5f2ac32864d6d7cf5e56a705fdb3e05f4c/server.jar";
    hash = "sha256-0FLxTXoXNzT7pVNxHltXAWLi8qMTJn7jGiG5daZ5vmQ=";
  };

  # cache/ ships along although nothing reads it after this build: Paperclip
  # touches it before deciding whether to patch and fails on a read-only path.
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
