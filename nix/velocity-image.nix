# The Velocity base image.
#
# The second consumer of oci-common, and the reason it was extracted: the
# numeric user, the entrypoint shebang rewrite and the layered-image frame are
# shared with nix/paper-image.nix by construction, not by two files agreeing.
{ bash
, buildEnv
, coreutils
, jdk25_headless
, runCommand
, velocity
, spawnery-config
, imageVersion
, oci-common
}:

let
  velocityHome = runCommand "velocity-home" { } ''
    mkdir -p $out/opt/velocity
    cp ${velocity.jar} $out/opt/velocity/velocity.jar
    chmod -R a-w $out/opt/velocity
  '';
in
oci-common.layeredImage {
  name = "ghcr.io/spawnery/velocity";
  tag = "${velocity.velocityVersion}-${imageVersion}";

  # Ordered by rate of change, the same rationale as the Paper image: the JRE
  # and the pinned jar are large and almost static; spawnery-config and the
  # entrypoint are small and change per commit. Milestone 3c adds the agent
  # plugin as another small layer without touching either.
  contents = [
    (buildEnv {
      name = "velocity-tools";
      # bash because the entrypoint's shebang points at it; coreutils for the
      # mkdir/cp the entrypoint uses to place the agent jar.
      paths = [ bash coreutils jdk25_headless ];
      pathsToLink = [ "/bin" ];
    })
    oci-common.passwd
    oci-common.group
    velocityHome
    (oci-common.binIn { package = spawnery-config; name = "spawnery-config"; })
    (oci-common.entrypointFrom ../image/velocity-entrypoint.sh)
  ];

  config = {
    Env = [
      "HOME=/data"
      "PATH=/bin:/usr/local/bin"
      "VELOCITY_HOME=/opt/velocity"
    ];
    ExposedPorts = { "25565/tcp" = { }; };
    Entrypoint = [ "/usr/local/bin/spawnery-entrypoint" ];
    Labels = {
      "org.opencontainers.image.title" = "Spawnery Velocity base image";
      "org.opencontainers.image.version" = "${velocity.velocityVersion}-${imageVersion}";
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
      # The Velocity build number lives here rather than in the tag, so an
      # upstream rebuild does not force every sample manifest to be touched.
      "cloud.spawnery.velocity-build" = velocity.velocityBuild;
    };
  };
}
