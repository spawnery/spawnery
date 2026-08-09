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
, unzip
, imageVersion
}:

stdenv.mkDerivation (finalAttrs: {
  pname = "spawnery-paper-agent";
  version = "${paper.paperVersion}-${imageVersion}";

  src = ../agent/paper;

  nativeBuildInputs = [ gradle unzip ];

  mitmCache = gradle.fetchDeps {
    pkg = finalAttrs.finalPackage;
    data = ../agent/paper/deps.json;
  };

  # Gradle resolves the Paper API through this link, not through a repository.
  postPatch = ''
    ln -sfn ${paper.repo} paper-repo
  '';

  gradleFlags = [ "-PagentVersion=${finalAttrs.version}" ];
  gradleBuildTask = "shadowJar";

  doCheck = true;

  installPhase = ''
    runHook preInstall
    install -Dm644 build/libs/spawnery-paper-agent-${finalAttrs.version}.jar $out/share/spawnery/spawnery-agent.jar
    runHook postInstall
  '';

  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    # Invoked through bash rather than executed directly: the store path's
    # "#!/usr/bin/env bash" shebang has nothing to resolve against inside the
    # build sandbox, which carries no /usr/bin/env, and fails with "bad
    # interpreter" before the check ever runs.
    bash ${../hack/agent-jar-check.sh} $out/share/spawnery/spawnery-agent.jar
    runHook postInstallCheck
  '';

  meta = {
    description = "Spawnery agent plugin for Paper";
    platforms = lib.platforms.all;
  };
})
