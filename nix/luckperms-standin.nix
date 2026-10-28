# Test-only and in no image: a LuckPerms jar for each platform that is present
# and never loaded, for hack/agent-test.sh. It carries only the API classes the
# agents probe, and LuckPermsProvider.get() throws as the real one does until
# LuckPerms has enabled.
{ lib
, stdenvNoCC
, jdk25_headless
, paper
}:

stdenvNoCC.mkDerivation {
  pname = "luckperms-standin";
  version = "0.1.0";

  src = ../hack/luckperms-standin;

  nativeBuildInputs = [ jdk25_headless ];

  buildPhase = ''
    runHook preBuild
    paperClasspath=$(find ${paper.repo}/libraries -name '*.jar' | tr '\n' ':')
    mkdir -p classes/paper classes/velocity
    javac --release 21 -d classes/paper -cp "$paperClasspath" \
      $(find api paper -name '*.java')
    cp paper/plugin.yml classes/paper/
    javac --release 21 -d classes/velocity $(find api velocity -name '*.java')
    cp velocity/velocity-plugin.json classes/velocity/
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    mkdir -p $out
    jar --create --file $out/luckperms-standin-paper.jar -C classes/paper .
    jar --create --file $out/luckperms-standin-velocity.jar -C classes/velocity .
    runHook postInstall
  '';

  meta.platforms = lib.platforms.all;
}
