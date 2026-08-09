# The Spawnery Paper agent plugin.
#
# Dependencies come through the nixpkgs Gradle setup hook, whose lockfile
# (agent/paper/deps.json) carries one SHA-256 per artifact and is checked in.
# The Paper API does not: it is symlinked in from the already-pinned Paper
# bundle, so it can never drift from the server that loads the plugin.
{ lib
, stdenv
, gradle
, paper
, imageVersion ? "0.2.0"
}:

stdenv.mkDerivation (finalAttrs: {
  pname = "spawnery-paper-agent";
  version = "${paper.paperVersion}-${imageVersion}";

  src = ../agent/paper;

  nativeBuildInputs = [ gradle ];

  mitmCache = gradle.fetchDeps {
    pkg = finalAttrs.finalPackage;
    data = ../agent/paper/deps.json;
  };

  # Gradle resolves the Paper API through this link, not through a repository.
  postPatch = ''
    ln -sfn ${paper.repo} paper-repo
  '';

  gradleFlags = [ "-PagentVersion=${finalAttrs.version}" ];
  gradleBuildTask = "jar";

  doCheck = true;

  installPhase = ''
    runHook preInstall
    install -Dm644 build/libs/*.jar $out/share/spawnery/spawnery-agent.jar
    runHook postInstall
  '';

  meta = {
    description = "Spawnery agent plugin for Paper";
    platforms = lib.platforms.all;
  };
})
