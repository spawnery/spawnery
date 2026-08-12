# The pinned Velocity artifact.
#
# Unlike Paper, this needs no build-time patching: Velocity ships as a fat jar
# with nothing to download on first start. The hash was computed from a
# download and checked in here; that does not make the source trustworthy, it
# makes the artifact frozen — a changed upstream breaks the build instead of
# substituting a jar quietly.
#
# 3.5.1 rather than the newer 4.0.0: PaperMC marks 3.5.1 as the RECOMMENDED
# channel and 4.0.0 only as STABLE, and Velocity 4 breaks the plugin API that
# milestone 3c's agent is written against.
{ fetchurl }:

rec {
  velocityVersion = "3.5.1";
  velocityBuild = "615";

  jar = fetchurl {
    url = "https://fill-data.papermc.io/v1/objects/b4e3164df5377346854dc6cb9e6a78022b1946ff69e89676313f5f6f1c6f0fb3/velocity-${velocityVersion}-${velocityBuild}.jar";
    hash = "sha256-tOMWTfU3c0aFTcbLnmp4AisZRv9p6JZ2MT9fbxxvD7M=";
  };

  # config-version = "2.8", measured out of the pinned jar above with:
  #
  #   JAR=$(nix build .#velocity-jar --no-link --print-out-paths)
  #   jar xf "$JAR" default-velocity.toml && cat default-velocity.toml
  #
  # (default-velocity.toml sits at the jar root, not under META-INF; `unzip`
  # is not on PATH in the dev shell, so `jar xf` extracts it instead.)
  #
  # This is what velocityBuild 615 validates and migrates a rendered
  # velocity.toml against. Task 5 writes this exact value into the rendered
  # file; a version bump that does not re-run the command above and update
  # this comment produces a config Velocity migrates out from under the
  # renderer on first start.
  #
  # That same extracted file is checked in as
  # internal/render/testdata/velocity.default.toml, which is what
  # TestVelocityWritesTheKeysVelocityItselfReads compares the renderer's key
  # names against and what TestVelocityWritesThePinnedConfigVersion compares
  # this version to. A bump has to refresh it in the same change.
}
