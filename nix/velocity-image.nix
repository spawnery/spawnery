# The Velocity base image.
{ bash
, buildEnv
, coreutils
, findutils
, velocity-jre
, runCommand
, velocity
, spawnery-config
, agents
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

  # Ordered by rate of change.
  contents = [
    (buildEnv {
      name = "velocity-tools";
      # findutils as in paper-image.nix.
      paths = [ bash coreutils findutils velocity-jre ];
      pathsToLink = [ "/bin" ];
    })
    oci-common.passwd
    oci-common.group
    velocityHome
    # Its own layer: it changes on every commit, the pinned proxy jar rarely.
    (runCommand "velocity-agent" { } ''
      install -Dm644 ${agents}/share/spawnery/velocity/spawnery-agent.jar \
        $out/opt/velocity/agent/spawnery-agent.jar
    '')
    (oci-common.binIn { package = spawnery-config; name = "spawnery-config"; })
    (oci-common.entrypointFrom ../image/velocity-entrypoint.sh)
  ];

  config = {
    Env = [
      "HOME=/data"
      "PATH=/bin:/usr/local/bin"
      "SPAWNERY_VELOCITY_HOME=/opt/velocity"
    ];
    ExposedPorts = { "25565/tcp" = { }; };
    Entrypoint = [ "/usr/local/bin/spawnery-entrypoint" ];
    Labels = {
      "org.opencontainers.image.title" = "Spawnery Velocity base image";
      "org.opencontainers.image.version" = "${velocity.velocityVersion}-${imageVersion}";
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
      # The build number lives here rather than in the tag, so an upstream rebuild
      # does not touch every sample manifest.
      "cloud.spawnery.velocity-build" = velocity.velocityBuild;
    };
  };
}
