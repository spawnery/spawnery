# The Spawnery in-game agents, built from one Gradle build.
#
# `agent/` is a multi-project build: `:common` holds the session machinery and
# the generated stubs, and one subproject per platform holds the code that
# touches that platform's API. This derivation builds every agent jar in it and
# installs each under its own flavour directory, so a consumer names the
# platform it wants rather than trusting that there is only one jar.
#
# Dependencies come through the nixpkgs Gradle setup hook, whose lockfile
# (agent/deps.json) carries one SHA-256 per artifact and is checked in.
# The platform APIs do not: each is symlinked in from the already-pinned bundle
# or jar, so neither plugin can drift from the thing that loads it.
{ lib
, stdenv
, gradle
, paper
, velocity
, unzip
, imageVersion
}:

stdenv.mkDerivation (finalAttrs: {
  pname = "spawnery-agents";

  # imageVersion alone, where this used to be "${paper.paperVersion}-${imageVersion}".
  # That old string said something true about an artifact that belonged to one
  # platform, and would say something false about one that belongs to two: a
  # Paper version has nothing to do with the Velocity jar built beside it. The
  # Paper *image* still carries the combined tag, because an image really is
  # per-platform; this derivation is not.
  version = imageVersion;

  src = ../agent;

  nativeBuildInputs = [ gradle unzip ];

  mitmCache = gradle.fetchDeps {
    pkg = finalAttrs.finalPackage;
    data = ../agent/deps.json;
  };

  # Gradle resolves each platform API through these links, not through a
  # repository. They go inside their own subprojects and not at the source
  # root, because the fileTree in agent/paper/build.gradle.kts and the
  # files("velocity.jar") in agent/velocity/build.gradle.kts are both resolved
  # relative to the subproject directory.
  #
  # Paper needs a whole repo tree because its API is spread over a libraries
  # directory; Velocity's fat jar contains its own plugin API, so one file is
  # the whole of it.
  postPatch = ''
    ln -sfn ${paper.repo} paper/paper-repo
    # :common compiles its command tree against Brigadier, which it takes from
    # the same repository rather than from Maven -- see its build file for why.
    ln -sfn ${paper.repo} common/paper-repo
    ln -sfn ${velocity.jar} velocity/velocity.jar
  '';

  gradleFlags = [ "-PagentVersion=${finalAttrs.version}" ];

  # Unqualified on purpose. Gradle resolves a bare task name in every project
  # that has such a task, and only the agent subprojects have a shadowJar, so
  # this builds each of them and nothing else. Naming them explicitly
  # (":paper:shadowJar :velocity:shadowJar") would depend on how the nixpkgs
  # hook joins a multi-task value into its command line; one name depends on
  # nothing.
  gradleBuildTask = "shadowJar";

  doCheck = true;

  # One directory per flavour, and the same file name inside each. The
  # consumers -- nix/paper-image.nix and nix/velocity-image.nix -- therefore
  # name the platform they want in the path rather than distinguishing two jars
  # by their Gradle archivesName, which is a build detail neither image should
  # have to know.
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
    # Invoked through bash rather than executed directly: the store path's
    # "#!/usr/bin/env bash" shebang has nothing to resolve against inside the
    # build sandbox, which carries no /usr/bin/env, and fails with "bad
    # interpreter" before the check ever runs.
    # The second argument is the unpacked source root, which installCheckPhase
    # still has as its working directory -- now the Gradle root rather than one
    # subproject, so the check reaches both :common's sources and the
    # flavour's. The third names the flavour, which is what tells the check
    # which subproject and which plugin descriptor belong to this jar.
    bash ${../hack/agent-jar-check.sh} $out/share/spawnery/paper/spawnery-agent.jar "$PWD" paper
    bash ${../hack/agent-jar-check.sh} $out/share/spawnery/velocity/spawnery-agent.jar "$PWD" velocity
    runHook postInstallCheck
  '';

  meta = {
    description = "Spawnery in-game agents";
    platforms = lib.platforms.all;
  };
})
