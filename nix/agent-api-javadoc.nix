# The plugin API's Javadoc, built without agents.nix's game-jar machinery.
#
# agent/api depends on nothing but the JDK and, for tests, JUnit -- read from
# agent/api/build.gradle.kts, not assumed -- so `:api:javadoc` needs neither
# the Paper repo nor the Velocity jar that nix/agents.nix symlinks in for the
# other two subprojects. Keeping this derivation separate from `agents` is the
# point of it: `nix build .#agents` builds both plugins against those jars and
# runs two JUnit suites, where `make docs` cost about 43 seconds in CI before
# this file existed. Putting that build in front of every documentation build
# would trade a fast link check for a slow one, on a job that runs on every
# push.
{ lib
, stdenv
, gradle
}:

stdenv.mkDerivation (finalAttrs: {
  pname = "spawnery-api-javadoc";
  version = "0";

  src = ../agent;

  nativeBuildInputs = [ gradle ];

  mitmCache = gradle.fetchDeps {
    pkg = finalAttrs.finalPackage;
    data = ../agent/deps.json;
  };

  # Qualified, unlike agents.nix's `gradleBuildTask`: a bare "javadoc" would
  # also run on :common, :paper and :velocity, which do carry a javadoc task
  # of their own (both apply the Kotlin JVM plugin) and do need the symlinked
  # Paper repo and Velocity jar this derivation deliberately does not provide.
  gradleBuildTask = ":api:javadoc";

  installPhase = ''
    runHook preInstall
    cp -r api/build/docs/javadoc $out
    runHook postInstall
  '';

  meta = {
    description = "Spawnery plugin API Javadoc";
    platforms = lib.platforms.all;
  };
})
