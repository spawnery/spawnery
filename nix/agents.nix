# The Spawnery in-game agents, built from one Gradle build.
#
# Each platform's jar is installed under its own flavour directory. The
# platform APIs come not from Maven but from the pinned Paper and Velocity
# builds, so neither plugin can drift from the thing that loads it.
{ lib
, stdenv
, gradle
, paper
, velocity
, unzip
, jdk25_headless
, imageVersion
}:

stdenv.mkDerivation (finalAttrs: {
  pname = "spawnery-agents";

  version = imageVersion;

  src = ../agent;

  nativeBuildInputs = [ gradle unzip ];

  mitmCache = gradle.fetchDeps {
    pkg = finalAttrs.finalPackage;
    data = ../agent/deps.json;
  };

  # Inside the subprojects: their build files resolve these paths relative to themselves.
  postPatch = ''
    ln -sfn ${paper.repo} paper/paper-repo
    # :common takes Brigadier from the same repository; see its build file.
    ln -sfn ${paper.repo} common/paper-repo
    ln -sfn ${velocity.jar} velocity/velocity.jar
  '';

  # Velocity 4's classes are major 69, which Gradle's own Java 21 refuses to load.
  gradleFlags = [
    "-PagentVersion=${finalAttrs.version}"
    "-PtestJava=${jdk25_headless}/bin/java"
  ];

  # Unqualified: Gradle runs a bare task name in every project that has it, and
  # only the agent subprojects have a shadowJar.
  gradleBuildTask = "shadowJar";

  doCheck = true;

  installPhase = ''
    runHook preInstall
    install -Dm644 paper/build/libs/spawnery-paper-agent-${finalAttrs.version}.jar \
      $out/share/spawnery/paper/spawnery-agent.jar
    install -Dm644 velocity/build/libs/spawnery-velocity-agent-${finalAttrs.version}.jar \
      $out/share/spawnery/velocity/spawnery-agent.jar
    runHook postInstall
  '';

  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    # Through bash: the sandbox has no /usr/bin/env for the shebang. $PWD is the
    # Gradle root, so the check reaches :common's sources and the flavour's.
    bash ${../hack/agent-jar-check.sh} $out/share/spawnery/paper/spawnery-agent.jar "$PWD" paper
    bash ${../hack/agent-jar-check.sh} $out/share/spawnery/velocity/spawnery-agent.jar "$PWD" velocity
    runHook postInstallCheck
  '';

  meta = {
    description = "Spawnery in-game agents";
    platforms = lib.platforms.all;
  };
})
