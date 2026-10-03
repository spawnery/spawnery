# The pinned Velocity artifact, a fat jar that needs no build-time patching.
# The 4.x line because 3.x never learned Minecraft 26.3; its classes are
# major 69, so the agents' tests need Java 25 (see nix/agents.nix).
{ fetchurl }:

rec {
  velocityVersion = "4.2.0";
  velocityBuild = "30";

  jar = fetchurl {
    url = "https://fill-data.papermc.io/v1/objects/35a5596a5468a035d8a32c8de5ebb0dc6b8d8f0cc3ff5169d514aca762af8aa8/velocity-${velocityVersion}-${velocityBuild}.jar";
    hash = "sha256-NaVZalRooDXYoyyN5euw3GuNjwzD/1Fp1RSsp2Kviqg=";
  };

  # On a version bump, refresh internal/render/defaults/velocity.default.toml
  # (and the config-version the renderer writes) from the pinned jar:
  #
  #   JAR=$(nix build .#velocity-jar --no-link --print-out-paths)
  #   jar xf "$JAR" default-velocity.toml && cat default-velocity.toml
  #
  # Velocity otherwise migrates a rendered velocity.toml on first start.
}
