# The pinned Purpur artifacts, patched at build time exactly the way
# nix/paper.nix patches Paper's.
#
# Purpur is a fork of Paper and ships the same paperclip bootstrap: its jar
# carries META-INF/license/paperclip-LICENSE.txt and a META-INF/download-context
# in the identical format, so the patch-only build below is Paper's unchanged.
{ fetchurl
, jdk25_headless
, stdenvNoCC
, mojangJar
}:

# Both values a Purpur bump moves -- the build number and the hash -- are what
# hack/purpur-pin.sh computes and writes, the same way hack/paper-pin.sh does
# for Paper. It differs in one respect worth knowing: PaperMC's API publishes a
# SHA-256 for its launcher and Purpur's publishes an MD5, so the check the pin
# script makes on its first download is weaker. What actually freezes the input
# is the hash below, which is the same either way.
rec {
  purpurVersion = "26.2";
  purpurBuild = "2628";

  purpurJar = fetchurl {
    url = "https://api.purpurmc.org/v2/purpur/${purpurVersion}/${purpurBuild}/download";
    hash = "sha256-dbnEn/0J8mGA+0qyhdhA2oBvebNH8v4iVq3iaR2hVJI=";
  };

  # Mojang's server jar arrives as an argument, and the flake passes Paper's.
  # Measured on 2026-08-31: Purpur 26.2 build 2628 names exactly the object
  # nix/paper.nix already pins --
  # 823e2250d24b3ddac457a60c92a6a941943fcd6a -- because both forks are the same
  # Minecraft version and there is only one such jar.
  #
  # Sharing it is safe rather than convenient, and the reason is that paperclip
  # verifies the cached original against the hash in its own
  # META-INF/download-context before patching. A Purpur and a Paper pin that
  # ever drift onto different Minecraft versions therefore fail this build
  # loudly, in the sandbox, rather than producing a server patched against the
  # wrong original.
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
