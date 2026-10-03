# The pinned Purpur artifacts. Purpur ships Paper's paperclip bootstrap, so
# the patch-only build is nix/paper.nix's unchanged.
{ fetchurl
, jdk25_headless
, stdenvNoCC
, mojangJar
}:

# hack/purpur-pin.sh writes the build number and the hash. Purpur's API
# publishes only an MD5 to check the download against; the hash below is
# what freezes the input.
rec {
  purpurVersion = "26.3";
  purpurBuild = "2642";

  purpurJar = fetchurl {
    url = "https://api.purpurmc.org/v2/purpur/${purpurVersion}/${purpurBuild}/download";
    hash = "sha256-zAdiFPyFb1XlL3/oPhL7Q2HeCWd/XIncrwJKKlgs21A=";
  };

  # Paper's Mojang jar: same Minecraft version, same object. paperclip verifies
  # it against its own download-context, so a drifted pair fails the build.
  repo = stdenvNoCC.mkDerivation {
    pname = "purpur-repo";
    version = "${purpurVersion}+${purpurBuild}";

    dontUnpack = true;
    nativeBuildInputs = [ jdk25_headless ];

    buildPhase = ''
      runHook preBuild

      mkdir -p work/cache
      cp ${mojangJar} work/cache/mojang_${purpurVersion}.jar
      cd work
      java -Dpaperclip.patchonly=true -DbundlerRepoDir=. -jar ${purpurJar}
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

    meta.description = "Purpur ${purpurVersion} build ${purpurBuild}, patched at build time";
  };
}
