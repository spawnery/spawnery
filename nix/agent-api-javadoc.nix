# The plugin API's Javadoc, kept apart from agents.nix so a docs build does
# not build both plugins against the game jars and run their test suites.
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

  # Qualified: :common, :paper and :velocity have javadoc tasks too, and they
  # need the Paper repo and Velocity jar this derivation does not provide.
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
