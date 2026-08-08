# The pinned Paper artifacts, and the patching that has to happen at build time
# rather than in every pod.
#
# The jar PaperMC publishes is not a server: it is a paperclip bootstrap that
# downloads Mojang's server jar on first start and patches it. Leaving that to
# runtime would break the main design's promise that nothing is downloaded at
# runtime, and would extract 166 MB into every ephemeral pod's emptyDir on
# every single start.
{ fetchurl
, jdk25_headless
, stdenvNoCC
}:

rec {
  paperVersion = "26.2";
  paperBuild = "111";

  # The launcher. Its hash was computed from a download and checked in here;
  # that does not make the source trustworthy, it makes the artifact frozen —
  # a changed upstream breaks the build instead of substituting a jar quietly.
  paperJar = fetchurl {
    url = "https://fill-data.papermc.io/v1/objects/3ec81e3ea50cc6090b94aab024491846a202702e8a874308a5d7510f6b3aa012/paper-${paperVersion}-${paperBuild}.jar";
    hash = "sha256-PsgePqUMxgkLlKqwJEkYRqICcC6Kh0MIpddRD2s6oBI=";
  };

  # Mojang's server jar. This URL and this hash both come from
  # META-INF/download-context inside paperJar, which is itself pinned above.
  # The checksum therefore does not come from the host that serves the
  # artifact — which is what the main design asks for and what no other hash in
  # this project manages.
  mojangJar = fetchurl {
    url = "https://piston-data.mojang.com/v1/objects/823e2250d24b3ddac457a60c92a6a941943fcd6a/server.jar";
    hash = "sha256-zazfsliY3l5LSw5d3MJyL3cGfkZgVwnC2IbAAOu2PsU=";
  };

  # The patched server, produced offline: every input is already fetched, so
  # the sandbox needs no network.
  #
  # cache/ ships along, 61 MB that nothing reads after this build. Paperclip
  # touches the cache directory before it decides whether patching is needed at
  # all, and on a read-only path it fails there with a FileSystemException.
  # Measured, not assumed. Dropping it would mean a writable cache directory in
  # every pod, which is worse.
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
